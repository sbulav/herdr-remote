package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
	"github.com/dcolinmorgan/herdr-remote/internal/auth"
	"github.com/dcolinmorgan/herdr-remote/internal/connector"
	"github.com/dcolinmorgan/herdr-remote/internal/enrollment"
	"github.com/dcolinmorgan/herdr-remote/internal/protocol"
	"github.com/dcolinmorgan/herdr-remote/internal/push"
	"github.com/dcolinmorgan/herdr-remote/internal/pushendpoint"
	"github.com/dcolinmorgan/herdr-remote/internal/store"
)

type ServerConfig struct {
	Origin, StaticDir    string
	UpstreamLogoutURL    string
	Proxy                *auth.Proxy
	Sessions             *auth.Sessions
	Store                *store.Store
	Enrollment           *enrollment.Service
	Push                 *push.Service
	VAPIDPublicKey       string
	OperatorSubject      string
	Logger               *slog.Logger
	Metrics              *Metrics
	SessionCheckInterval time.Duration
}

const browserUnauthorizedCloseCode websocket.StatusCode = 4401

type Server struct {
	cfg         ServerConfig
	hub         *Hub
	pushWorkers chan struct{}
	staticRoot  *os.Root
}
type browserProjection struct {
	mu       sync.Mutex
	epoch    string
	sequence uint64
	invalid  bool
}

func (p *browserProjection) current() (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.epoch, !p.invalid
}
func (p *browserProjection) reset(epoch string) {
	p.mu.Lock()
	p.epoch = epoch
	p.sequence = 0
	p.invalid = false
	p.mu.Unlock()
}
func (p *browserProjection) expectedResync() (string, uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.epoch, p.sequence + 1
}

func NewServer(c ServerConfig, h *Hub) (*Server, error) {
	if c.Origin == "" || c.Proxy == nil || c.Sessions == nil || c.Store == nil || c.Enrollment == nil {
		return nil, errors.New("incomplete control plane configuration")
	}
	u, err := url.Parse(c.Origin)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return nil, errors.New("origin must be an exact HTTPS origin")
	}
	logoutURL, err := url.Parse(c.UpstreamLogoutURL)
	if err != nil || len(c.UpstreamLogoutURL) > 2048 || logoutURL.Scheme != "https" || logoutURL.Host == "" || logoutURL.User != nil || logoutURL.Fragment != "" || logoutURL.Opaque != "" {
		return nil, errors.New("upstream logout URL must be an absolute HTTPS URL without userinfo or fragment")
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.Metrics == nil {
		c.Metrics = &Metrics{}
	}
	if c.SessionCheckInterval <= 0 {
		c.SessionCheckInterval = 5 * time.Second
	}
	if c.Push == nil && c.VAPIDPublicKey != "" {
		return nil, errors.New("VAPID public key requires push service")
	}
	if c.Push != nil {
		if c.OperatorSubject == "" || c.Push.ValidateConfiguration(c.VAPIDPublicKey) != nil {
			return nil, errors.New("push service requires operator and valid VAPID public key")
		}
	}
	s := &Server{cfg: c, hub: h, pushWorkers: make(chan struct{}, 4)}
	if c.StaticDir != "" {
		root, err := os.OpenRoot(c.StaticDir)
		if err != nil {
			return nil, fmt.Errorf("open static root: %w", err)
		}
		s.staticRoot = root
	}
	if c.Push != nil && c.OperatorSubject != "" {
		h.Subscribe(func(kind string, _ any) {
			if kind != "attention" {
				return
			}
			select {
			case s.pushWorkers <- struct{}{}:
			default:
				return
			}
			go func() {
				defer func() { <-s.pushWorkers }()
				ctx, cancel := context.WithTimeout(context.Background(), c.Push.FanoutTimeout(store.MaxPushSubscriptionsPerOperator))
				defer cancel()
				if err := c.Push.Notify(ctx, c.OperatorSubject, "agent_state_changed"); err != nil {
					c.Logger.Warn("web push delivery failed", "error", "delivery failed")
				}
			}()
		})
	}
	return s, nil
}

func (s *Server) BrowserHandler() http.Handler {
	public := http.NewServeMux()
	public.HandleFunc("/healthz", s.health)
	public.HandleFunc("/readyz", s.ready)
	public.HandleFunc("/metrics", s.metrics)
	public.HandleFunc("/v1/enroll", s.enroll)
	protected := http.NewServeMux()
	protected.HandleFunc("/api/v1/session", s.session)
	protected.HandleFunc("/api/v1/csrf", s.csrf)
	protected.HandleFunc("/auth/logout", s.logout)
	protected.HandleFunc("/v1/browser/ws", s.browserWS)
	protected.HandleFunc("/api/v1/actions/", s.actionStatus)
	protected.HandleFunc("/api/v1/enrollments", s.createEnrollment)
	protected.HandleFunc("/api/v1/hosts/", s.hostAction)
	protected.HandleFunc("/api/v1/push/subscriptions", s.pushSubscriptions)
	protected.HandleFunc("/api/v1/push/subscriptions/reconcile", s.reconcilePushSubscription)
	protected.HandleFunc("/api/v1/push/subscriptions/replace", s.replacePushSubscription)
	protected.HandleFunc("/api/v1/push/test", s.testPush)
	if s.cfg.StaticDir != "" {
		protected.Handle("/", s.static())
	}
	public.Handle("/api/", s.cfg.Proxy.Middleware(protected))
	public.Handle("/auth/", s.cfg.Proxy.Middleware(protected))
	public.Handle("/v1/browser/", s.cfg.Proxy.Middleware(protected))
	public.Handle("/", s.cfg.Proxy.Middleware(protected))
	return securityHeaders(public)
}
func (s *Server) ConnectorHandler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("/healthz", s.health)
	m.HandleFunc("/readyz", s.ready)
	m.HandleFunc("/v1/connectors/ws", s.hub.ConnectorHandler)
	m.HandleFunc("/v1/connectors/rotate", s.rotate)
	return m
}

func (s *Server) session(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	identity, ok := auth.IdentityFrom(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if _, err := s.cfg.Sessions.Get(r); err != nil {
		if _, err := s.cfg.Sessions.Issue(w, identity); err != nil {
			http.Error(w, "internal", http.StatusInternalServerError)
			return
		}
	}
	var publicKey *string
	if s.cfg.VAPIDPublicKey != "" {
		value := s.cfg.VAPIDPublicKey
		publicKey = &value
	}
	jsonResponse(w, http.StatusOK, struct {
		Authenticated bool `json:"authenticated"`
		Operator      struct {
			DisplayName string `json:"display_name"`
		} `json:"operator"`
		PushPublicKey *string `json:"push_public_key"`
		LogoutURL     string  `json:"logout_url"`
	}{Authenticated: true, Operator: struct {
		DisplayName string `json:"display_name"`
	}{DisplayName: identity.Subject}, PushPublicKey: publicKey, LogoutURL: s.cfg.UpstreamLogoutURL})
}

func (s *Server) csrf(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	session, err := s.cfg.Sessions.Get(r)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	jsonResponse(w, http.StatusOK, struct {
		Token string `json:"token"`
	}{Token: session.CSRF})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	if !s.requireOrigin(w, r) {
		return
	}
	session, err := s.cfg.Sessions.Get(r)
	if err != nil || !s.cfg.Sessions.RequireCSRF(r, session) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var in struct{}
	if strictBody(r, &in) != nil {
		http.Error(w, "invalid", http.StatusBadRequest)
		return
	}
	s.cfg.Sessions.Delete(session.ID)
	s.cfg.Sessions.ClearCookie(w)
	jsonResponse(w, http.StatusOK, struct {
		LogoutURL string `json:"logout_url"`
	}{LogoutURL: s.cfg.UpstreamLogoutURL})
}

func (s *Server) browserWS(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Origin") != s.cfg.Origin {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	session, err := s.cfg.Sessions.Get(r)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{strings.TrimPrefix(s.cfg.Origin, "https://")}, CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(protocol.MaxFrameBytes)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	var unauthorizedOnce sync.Once
	closeUnauthorized := func() {
		unauthorizedOnce.Do(func() {
			_ = conn.Close(browserUnauthorizedCloseCode, "session unauthorized")
			cancel()
		})
	}
	revoked, unwatch, err := s.cfg.Sessions.Watch(session)
	if err != nil {
		closeUnauthorized()
		return
	}
	defer unwatch()
	go func() {
		ticker := time.NewTicker(s.cfg.SessionCheckInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-revoked:
				closeUnauthorized()
				return
			case <-ticker.C:
				if s.cfg.Sessions.Valid(session) != nil {
					closeUnauthorized()
					return
				}
			}
		}
	}()
	initialEpoch, _ := protocol.NewUUIDv7()
	projection := &browserProjection{epoch: initialEpoch}
	sessionID, _ := protocol.NewUUIDv7()
	out := connector.NewQueue(256)
	var closeOnce sync.Once
	failConnection := func() {
		closeOnce.Do(func() { cancel(); _ = conn.Close(websocket.StatusTryAgainLater, "priority delivery unavailable") })
	}
	enqueue := func(typ string, body any) bool {
		if ctx.Err() != nil || s.cfg.Sessions.Valid(session) != nil {
			return false
		}
		b, err := protocol.MarshalEnvelope(1, typ, body)
		if err != nil {
			failConnection()
			return false
		}
		putCtx, putCancel := context.WithTimeout(ctx, 2*time.Second)
		err = out.Put(putCtx, b)
		putCancel()
		if err != nil {
			failConnection()
			return false
		}
		return true
	}
	enqueueOutput := func(id string, body any) bool {
		if ctx.Err() != nil || s.cfg.Sessions.Valid(session) != nil {
			return false
		}
		b, err := protocol.MarshalEnvelope(1, "output.snapshot", body)
		if err != nil {
			failConnection()
			return false
		}
		out.ReplaceOutput(id, b)
		return true
	}
	if !enqueue("session.snapshot", s.hub.Snapshot(sessionID, initialEpoch)) {
		return
	}
	unsub := s.hub.Subscribe(func(kind string, v any) {
		if s.cfg.Sessions.Valid(session) != nil {
			return
		}
		switch kind {
		case "state.delta":
			event := v.(StateEvent)
			projection.mu.Lock()
			if projection.invalid {
				projection.mu.Unlock()
				return
			}
			projection.sequence++
			body := protocol.StateDelta{SessionID: sessionID, StateEpoch: projection.epoch, Sequence: projection.sequence, Changes: event.Changes}
			for _, change := range event.Changes {
				if change.Operation == "instance.epoch_changed" {
					projection.invalid = true
					break
				}
			}
			projection.mu.Unlock()
			enqueue("state.delta", body)
		case "prompt":
			p := v.(protocol.PromptSnapshot)
			p.SessionID = sessionID
			p.ConnectorEpoch = p.StateEpoch
			epoch, valid := projection.current()
			if !valid {
				return
			}
			p.StateEpoch = epoch
			p.Excerpt, p.ExcerptTruncated = browserExcerpt(p.Excerpt, p.ExcerptTruncated)
			enqueue("prompt.snapshot", p)
		}
	})
	defer unsub()
	activeOutputs := map[string]string{}
	defer cleanupBrowserOutputs(s.hub, activeOutputs)
	writeErr := make(chan error, 1)
	go func() {
		for {
			b, err := out.Next(ctx)
			if err != nil {
				return
			}
			wctx, c := context.WithTimeout(ctx, 10*time.Second)
			if s.cfg.Sessions.Valid(session) != nil {
				c()
				return
			}
			err = conn.Write(wctx, websocket.MessageText, b)
			c()
			if err != nil {
				select {
				case writeErr <- err:
				default:
				}
				return
			}
		}
	}()
	var malformed protocol.MalformedTracker
	for {
		select {
		case <-writeErr:
			return
		default:
		}
		_, frame, err := conn.Read(ctx)
		if err != nil {
			return
		}
		env, msg, err := protocol.DecodeStrict(frame, "browser")
		directionErr := protocol.ValidateDirection(protocol.BrowserToControl, env.Type)
		if err != nil || directionErr != nil {
			s.cfg.Metrics.Malformed.Add(1)
			code := "INVALID_MESSAGE"
			if directionErr != nil || (env.Type != "" && err != nil) {
				code = "UNSUPPORTED_MESSAGE"
			}
			s.protocolError(enqueue, sessionID, env.MessageID, code)
			if malformed.Add(time.Now()) {
				return
			}
			continue
		}
		if s.cfg.Sessions.Check(session) != nil {
			closeUnauthorized()
			return
		}
		switch m := msg.(type) {
		case *protocol.BrowserActionRequest:
			if m.SessionID != sessionID {
				go s.rejectAction(enqueue, sessionID, *m, "UNAUTHORIZED")
				continue
			}
			epoch, valid := projection.current()
			if !valid {
				go s.rejectAction(enqueue, sessionID, *m, "STALE_STATE")
				continue
			}
			go s.browserAction(ctx, session.Identity, sessionID, epoch, *m, enqueue)
		case *protocol.OutputSubscribe:
			if m.SessionID != sessionID {
				continue
			}
			copy := *m
			copy.SessionID = ""
			copy.Target.HostID = m.Target.HostID
			err = s.hub.SubscribeOutput(ctx, copy, func(o protocol.OutputSnapshot) {
				o.SessionID = sessionID
				o.ConnectorEpoch = o.StateEpoch
				epoch, valid := projection.current()
				if !valid {
					return
				}
				o.StateEpoch = epoch
				enqueueOutput(o.SubscriptionID, o)
			})
			if err != nil {
				s.protocolError(enqueue, sessionID, env.MessageID, ErrorCode(err))
			} else {
				activeOutputs[m.SubscriptionID] = m.Target.HostID
			}
		case *protocol.OutputUnsubscribe:
			s.hub.UnsubscribeOutput(ctx, "", m.SubscriptionID)
			delete(activeOutputs, m.SubscriptionID)
		case *protocol.StateResync:
			currentEpoch, nextSequence := projection.expectedResync()
			if m.SessionID != sessionID || m.ExpectedEpoch == nil || *m.ExpectedEpoch != currentEpoch || m.ExpectedSequence == nil || *m.ExpectedSequence != nextSequence {
				s.protocolError(enqueue, sessionID, env.MessageID, "INVALID_MESSAGE")
				continue
			}
			newEpoch, _ := protocol.NewUUIDv7()
			projection.reset(newEpoch)
			enqueue("session.snapshot", s.hub.Snapshot(sessionID, newEpoch))
		}
	}
}
func cleanupBrowserOutputs(h *Hub, active map[string]string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for id, host := range active {
		h.UnsubscribeOutput(ctx, host, id)
	}
}

func (s *Server) browserAction(ctx context.Context, id auth.Identity, sessionID, stateEpoch string, a protocol.BrowserActionRequest, send func(string, any) bool) {
	op := a.Operation.Type
	result := protocol.ActionResult{SessionID: sessionID, ActionID: a.ActionID, OperationType: op, Result: json.RawMessage("null")}
	finish := func(status, code string) {
		result.Status = status
		result.Code = &code
		s.hub.completeAction(context.Background(), a.ActionID, status, &code, time.Now())
		send("action.result", result)
	}
	connection, version, _ := s.hub.ConnectionMetadata(a.Target.HostID)
	textBytes, keyCount := 0, 0
	if a.Operation.Text != nil {
		textBytes = len([]byte(*a.Operation.Text))
	}
	keyCount = len(a.Operation.Keys)
	intent := store.ActionIntent{ActionID: a.ActionID, OperationType: op, Issuer: id.Issuer, Subject: id.Subject, HostID: a.Target.HostID, InstanceID: a.Target.InstanceID, TerminalID: a.Target.TerminalID, ConnectionID: connection, ConnectorVersion: version, ProtocolVersion: 1, TextBytes: textBytes, KeyCount: keyCount, RequestedAt: time.Now()}
	if protocol.IsWrite(op) && s.hub.AuditBlocked() {
		if err := s.hub.RepairAudits(ctx); err != nil {
			result.Status = "rejected"
			code := "AUDIT_UNAVAILABLE"
			result.Code = &code
			send("action.result", result)
			return
		}
	}
	if err := s.cfg.Store.BeginAction(ctx, intent); err != nil {
		result.Status = "rejected"
		code := "AUDIT_UNAVAILABLE"
		if errors.Is(err, store.ErrDuplicate) {
			code = "DUPLICATE_ACTION"
		}
		result.Code = &code
		send("action.result", result)
		return
	}
	if a.Expected.StateEpoch != stateEpoch {
		finish("rejected", "STALE_STATE")
		return
	}
	if err := s.hub.ValidateAction(a); err != nil {
		finish("rejected", ErrorCode(err))
		return
	}
	connectorReq := a.ActionRequest
	connectorReq.Expected.StateEpoch = a.Expected.ConnectorEpoch
	connectorReq.Expected.ConnectorEpoch = ""
	handle, err := s.hub.Dispatch(ctx, connectorReq)
	if err != nil {
		finish("rejected", ErrorCode(err))
		return
	}
	received := false
	receivedCh := handle.Received
	for {
		select {
		case <-ctx.Done():
			status, code := "failed", "CONNECTION_LOST"
			if protocol.IsWrite(op) {
				status, code = "unknown", "OUTCOME_UNKNOWN"
			}
			finish(status, code)
			return
		case <-receivedCh:
			if !received {
				received = true
				send("action.received", protocol.ActionReceived{SessionID: sessionID, ActionID: a.ActionID})
			}
			receivedCh = nil
		case r := <-handle.Result:
			r.SessionID = sessionID
			if r.Status == "succeeded" && op == "agent.read" {
				var rr protocol.ReadResult
				if json.Unmarshal(r.Result, &rr) == nil {
					rr.ConnectorEpoch = a.Expected.ConnectorEpoch
					rr.StateEpoch = stateEpoch
					r.Result = mustJSON(rr)
				}
			}
			send("action.result", r)
			return
		}
	}
}

func (s *Server) rejectAction(send func(string, any) bool, session string, a protocol.BrowserActionRequest, code string) {
	r := protocol.ActionResult{SessionID: session, ActionID: a.ActionID, OperationType: a.Operation.Type, Status: "rejected", Code: &code, Result: json.RawMessage("null")}
	send("action.result", r)
}
func (s *Server) protocolError(send func(string, any) bool, session, reply, code string) {
	var p any = nil
	if protocol.IsUUIDv7(reply) {
		p = reply
	}
	send("protocol.error", map[string]any{"session_id": session, "in_reply_to": p, "code": code, "fatal": false})
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	io.WriteString(w, "{\"status\":\"ok\"}\n")
}
func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	ctx, c := context.WithTimeout(r.Context(), time.Second)
	defer c()
	if err := s.cfg.Store.Ready(ctx); err != nil {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	s.health(w, r)
}
func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintf(w, "# TYPE herdr_connector_connections_total counter\nherdr_connector_connections_total %d\n# TYPE herdr_actions_total counter\nherdr_actions_total %d\n# TYPE herdr_protocol_malformed_total counter\nherdr_protocol_malformed_total %d\n# TYPE herdr_audit_failures_total counter\nherdr_audit_failures_total %d\n", s.cfg.Metrics.ConnectorConnections.Load(), s.cfg.Metrics.Actions.Load(), s.cfg.Metrics.Malformed.Load(), s.cfg.Metrics.AuditFailures.Load())
}

func (s *Server) createEnrollment(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method", 405)
		return
	}
	if !s.requireOrigin(w, r) {
		return
	}
	session, err := s.cfg.Sessions.Get(r)
	if err != nil || !s.cfg.Sessions.RequireCSRF(r, session) {
		http.Error(w, "forbidden", 403)
		return
	}
	var in struct {
		DisplayName string `json:"display_name"`
	}
	if strictBody(r, &in) != nil || !validDisplayName(in.DisplayName) {
		http.Error(w, "invalid", 400)
		return
	}
	token, err := s.cfg.Enrollment.CreateToken(r.Context(), in.DisplayName)
	if err != nil {
		if errors.Is(err, store.ErrHostLimit) {
			http.Error(w, "host limit reached", http.StatusConflict)
			return
		}
		if errors.Is(err, store.ErrInvalidDisplayName) {
			http.Error(w, "invalid", http.StatusBadRequest)
			return
		}
		http.Error(w, "internal", 500)
		return
	}
	jsonResponse(w, 201, token)
}
func validDisplayName(value string) bool {
	if !utf8.ValidString(value) || utf8.RuneCountInString(value) < 1 || utf8.RuneCountInString(value) > 80 {
		return false
	}
	for _, r := range value {
		if r <= 0x1f || (r >= 0x7f && r <= 0x9f) {
			return false
		}
	}
	return true
}
func (s *Server) enroll(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method", 405)
		return
	}
	var in struct {
		Token  string `json:"token"`
		CSRPEM string `json:"csr_pem"`
	}
	if strictBody(r, &in) != nil {
		http.Error(w, "invalid", 400)
		return
	}
	cert, err := s.cfg.Enrollment.Enroll(r.Context(), in.Token, []byte(in.CSRPEM))
	if err != nil {
		http.Error(w, "invalid or expired enrollment", 400)
		return
	}
	jsonResponse(w, 201, cert)
}
func (s *Server) rotate(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method", 405)
		return
	}
	_, record, err := s.hub.authenticateConnector(r)
	if err != nil {
		http.Error(w, "unauthorized", 401)
		return
	}
	var in struct {
		CSRPEM string `json:"csr_pem"`
	}
	if strictBody(r, &in) != nil {
		http.Error(w, "invalid", 400)
		return
	}
	cert, err := s.cfg.Enrollment.Rotate(r.Context(), record.HostID, []byte(in.CSRPEM))
	if err != nil {
		http.Error(w, "invalid", 400)
		return
	}
	jsonResponse(w, 201, cert)
}
func (s *Server) hostAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != "DELETE" || !strings.HasSuffix(r.URL.Path, "/credential") {
		http.Error(w, "method", 405)
		return
	}
	if !s.requireOrigin(w, r) {
		return
	}
	session, err := s.cfg.Sessions.Get(r)
	if err != nil || !s.cfg.Sessions.RequireCSRF(r, session) {
		http.Error(w, "forbidden", 403)
		return
	}
	host := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/hosts/"), "/credential")
	if !protocol.IsUUIDv7(host) {
		http.Error(w, "invalid", 400)
		return
	}
	if err := s.cfg.Enrollment.Revoke(r.Context(), host); err != nil {
		http.Error(w, "internal", 500)
		return
	}
	s.hub.mu.RLock()
	l := s.hub.leases[host]
	s.hub.mu.RUnlock()
	if l != nil {
		_ = l.conn.Close(websocket.StatusPolicyViolation, "credential revoked")
	}
	w.WriteHeader(204)
}

func (s *Server) pushSubscriptions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodDelete {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	if !s.requireOrigin(w, r) {
		return
	}
	session, err := s.cfg.Sessions.Get(r)
	if err != nil || !s.cfg.Sessions.RequireCSRF(r, session) {
		http.Error(w, "forbidden", 403)
		return
	}
	switch r.Method {
	case http.MethodPost:
		if s.cfg.Push == nil {
			jsonResponse(w, http.StatusServiceUnavailable, map[string]bool{"enabled": false})
			return
		}
		var in struct {
			Endpoint string `json:"endpoint"`
			Keys     struct {
				P256DH string `json:"p256dh"`
				Auth   string `json:"auth"`
			} `json:"keys"`
		}
		if strictBody(r, &in) != nil || !validPushEndpoint(in.Endpoint) || len(in.Endpoint) > 2048 || len(in.Keys.P256DH) < 1 || len(in.Keys.P256DH) > 512 || len(in.Keys.Auth) < 1 || len(in.Keys.Auth) > 256 || len(r.UserAgent()) > 256 {
			http.Error(w, "invalid", 400)
			return
		}
		if err := s.cfg.Store.UpsertPush(r.Context(), store.PushSubscription{Subject: session.Identity.Subject, Endpoint: in.Endpoint, P256DH: in.Keys.P256DH, Auth: in.Keys.Auth, UserAgent: r.UserAgent()}); err != nil {
			if errors.Is(err, store.ErrPushLimit) {
				http.Error(w, "push subscription limit reached", http.StatusConflict)
				return
			}
			if errors.Is(err, store.ErrInvalidPushSubscription) {
				http.Error(w, "invalid", http.StatusBadRequest)
				return
			}
			http.Error(w, "internal", 500)
			return
		}
		w.WriteHeader(204)
	case http.MethodDelete:
		var in struct {
			Endpoints []string `json:"endpoints"`
		}
		if strictBody(r, &in) != nil || len(in.Endpoints) < 1 || len(in.Endpoints) > store.MaxPushDeletionEndpoints {
			http.Error(w, "invalid", 400)
			return
		}
		seen := make(map[string]struct{}, len(in.Endpoints))
		for _, endpoint := range in.Endpoints {
			if !validPushEndpoint(endpoint) || len(endpoint) > 2048 {
				http.Error(w, "invalid", http.StatusBadRequest)
				return
			}
			if _, exists := seen[endpoint]; exists {
				http.Error(w, "invalid", http.StatusBadRequest)
				return
			}
			seen[endpoint] = struct{}{}
		}
		if err := s.cfg.Store.DeletePushEndpointsForSubject(r.Context(), session.Identity.Subject, in.Endpoints); err != nil {
			if errors.Is(err, store.ErrInvalidPushSubscription) {
				http.Error(w, "invalid", http.StatusBadRequest)
				return
			}
			http.Error(w, "internal", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(204)
	}
}
func (s *Server) reconcilePushSubscription(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	if !s.requireOrigin(w, r) {
		return
	}
	session, err := s.cfg.Sessions.Get(r)
	if err != nil || !s.cfg.Sessions.RequireCSRF(r, session) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var in struct {
		Endpoint string `json:"endpoint"`
	}
	if strictBody(r, &in) != nil || !validPushEndpoint(in.Endpoint) || len(in.Endpoint) > 2048 {
		http.Error(w, "invalid", http.StatusBadRequest)
		return
	}
	subscribed, err := s.cfg.Store.HasPushSubscription(r.Context(), session.Identity.Subject, in.Endpoint)
	if err != nil {
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	jsonResponse(w, http.StatusOK, map[string]bool{"subscribed": subscribed})
}
func (s *Server) replacePushSubscription(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	if !s.requireOrigin(w, r) {
		return
	}
	session, err := s.cfg.Sessions.Get(r)
	if err != nil || !s.cfg.Sessions.RequireCSRF(r, session) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if s.cfg.Push == nil {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]bool{"enabled": false})
		return
	}
	var in struct {
		SourceEndpoints []string `json:"source_endpoints"`
		Subscription    struct {
			Endpoint string `json:"endpoint"`
			Keys     struct {
				P256DH string `json:"p256dh"`
				Auth   string `json:"auth"`
			} `json:"keys"`
		} `json:"subscription"`
	}
	if strictBody(r, &in) != nil {
		http.Error(w, "invalid", http.StatusBadRequest)
		return
	}
	validSources := len(in.SourceEndpoints) >= 1 && len(in.SourceEndpoints) <= store.MaxPushReplacementSources
	seenSources := make(map[string]struct{}, len(in.SourceEndpoints))
	for _, endpoint := range in.SourceEndpoints {
		if !validPushEndpoint(endpoint) || len(endpoint) > 2048 {
			validSources = false
		}
		if _, exists := seenSources[endpoint]; exists {
			validSources = false
		}
		seenSources[endpoint] = struct{}{}
	}
	if !validSources || !validPushEndpoint(in.Subscription.Endpoint) || len(in.Subscription.Endpoint) > 2048 || len(in.Subscription.Keys.P256DH) < 1 || len(in.Subscription.Keys.P256DH) > 512 || len(in.Subscription.Keys.Auth) < 1 || len(in.Subscription.Keys.Auth) > 256 || len(r.UserAgent()) > 256 {
		http.Error(w, "invalid", http.StatusBadRequest)
		return
	}
	replacement := store.PushSubscription{Subject: session.Identity.Subject, Endpoint: in.Subscription.Endpoint, P256DH: in.Subscription.Keys.P256DH, Auth: in.Subscription.Keys.Auth, UserAgent: r.UserAgent()}
	if err := s.cfg.Store.ReplacePush(r.Context(), session.Identity.Subject, in.SourceEndpoints, replacement); err != nil {
		switch {
		case errors.Is(err, store.ErrInvalidPushSubscription):
			http.Error(w, "invalid", http.StatusBadRequest)
		case errors.Is(err, store.ErrPushOwnership):
			http.Error(w, "forbidden", http.StatusForbidden)
		case errors.Is(err, store.ErrPushMissing):
			http.Error(w, "missing", http.StatusNotFound)
		case errors.Is(err, store.ErrPushConflict):
			http.Error(w, "conflict", http.StatusConflict)
		case errors.Is(err, store.ErrPushLimit):
			http.Error(w, "push subscription limit reached", http.StatusConflict)
		default:
			http.Error(w, "internal", http.StatusInternalServerError)
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (s *Server) testPush(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	if !s.requireOrigin(w, r) {
		return
	}
	session, err := s.cfg.Sessions.Get(r)
	if err != nil || !s.cfg.Sessions.RequireCSRF(r, session) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var in struct{}
	if strictBody(r, &in) != nil {
		http.Error(w, "invalid", http.StatusBadRequest)
		return
	}
	if s.cfg.Push == nil {
		jsonResponse(w, http.StatusOK, map[string]bool{"enabled": false})
		return
	}
	select {
	case s.pushWorkers <- struct{}{}:
		defer func() { <-s.pushWorkers }()
	default:
		http.Error(w, "busy", http.StatusTooManyRequests)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.Push.FanoutTimeout(store.MaxPushSubscriptionsPerOperator))
	defer cancel()
	if err := s.cfg.Push.Notify(ctx, session.Identity.Subject, "test"); err != nil {
		s.cfg.Logger.Warn("test web push delivery failed", "error", "delivery failed")
		http.Error(w, "delivery failed", http.StatusBadGateway)
		return
	}
	jsonResponse(w, http.StatusAccepted, map[string]bool{"enabled": true})
}
func (s *Server) requireOrigin(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("Origin") != s.cfg.Origin {
		http.Error(w, "forbidden", http.StatusForbidden)
		return false
	}
	return true
}
func (s *Server) actionStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "method", 405)
		return
	}
	if _, err := s.cfg.Sessions.Get(r); err != nil {
		http.Error(w, "unauthorized", 401)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/v1/actions/")
	if !protocol.IsUUIDv7(id) {
		http.Error(w, "invalid", 400)
		return
	}
	a, err := s.cfg.Store.Action(r.Context(), id)
	if err != nil {
		http.Error(w, "not found", 404)
		return
	}
	jsonResponse(w, 200, map[string]any{"action_id": a.ActionID, "operation_type": a.OperationType, "status": a.Status, "code": a.Code, "requested_at": a.RequestedAt.UTC().Format(time.RFC3339Nano), "completed_at": a.CompletedAt.UTC().Format(time.RFC3339Nano)})
}

func (s *Server) static() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(filepath.Clean("/"+r.URL.Path), "/")
		if path == "" || path == "." {
			path = "index.html"
		}
		read := func(name string) ([]byte, error) {
			file, err := s.staticRoot.Open(name)
			if err != nil {
				return nil, err
			}
			defer file.Close()
			return io.ReadAll(io.LimitReader(file, 8*1024*1024))
		}
		data, err := read(path)
		if err != nil {
			data, err = read("index.html")
			path = "index.html"
		}
		if err != nil {
			http.NotFound(w, r)
			return
		}
		if path == "index.html" || strings.HasSuffix(path, "service-worker.js") {
			w.Header().Set("Cache-Control", "no-store")
		} else if hashedAsset.MatchString(filepath.Base(path)) {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		if typ := mime.TypeByExtension(filepath.Ext(path)); typ != "" {
			w.Header().Set("Content-Type", typ)
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Write(data)
	})
}

var hashedAsset = regexp.MustCompile(`(?i)[._-][0-9a-f]{8,}[._-]`)

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		w.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}
func strictBody(r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, protocol.MaxFrameBytes)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return err
	}
	if d.Decode(&struct{}{}) != io.EOF {
		return errors.New("trailing body")
	}
	return nil
}
func jsonResponse(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func validPushEndpoint(raw string) bool {
	_, err := pushendpoint.Parse(raw)
	return err == nil
}
func browserExcerpt(v string, already bool) (string, bool) {
	r := []rune(v)
	if len(r) <= protocol.MaxBrowserPromptRunes {
		return v, already
	}
	return string(r[:protocol.MaxBrowserPromptRunes]), true
}
func mustJSON(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
