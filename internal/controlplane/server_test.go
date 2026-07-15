package controlplane

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/dcolinmorgan/herdr-remote/internal/auth"
	"github.com/dcolinmorgan/herdr-remote/internal/connector"
	"github.com/dcolinmorgan/herdr-remote/internal/enrollment"
	"github.com/dcolinmorgan/herdr-remote/internal/protocol"
	"github.com/dcolinmorgan/herdr-remote/internal/push"
	"github.com/dcolinmorgan/herdr-remote/internal/store"
)

func testServer(t *testing.T) (*Server, *Hub, *store.Store) {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	ca, key := makeCA(t)
	enroll := enrollment.New(st, ca, key)
	proxy, err := auth.NewProxy(auth.ProxyConfig{CIDRs: []string{"127.0.0.0/8"}, Headers: auth.HeaderConfig{Issuer: "X-Issuer", Audience: "X-Audience", Subject: "X-Subject", Assurance: "X-MFA"}, Expected: auth.Identity{Issuer: "issuer", Audience: "audience", Subject: "operator", Assurance: "mfa"}})
	if err != nil {
		t.Fatal(err)
	}
	sessions := auth.NewTestSessions()
	metrics := &Metrics{}
	hub, err := NewHub(st, nil, metrics)
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(ServerConfig{Origin: "https://app.example", Proxy: proxy, Sessions: sessions, Store: st, Enrollment: enroll, Metrics: metrics}, hub)
	if err != nil {
		t.Fatal(err)
	}
	return server, hub, st
}
func headers(r *http.Request, subject string) {
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("X-Issuer", "issuer")
	r.Header.Set("X-Audience", "audience")
	r.Header.Set("X-Subject", subject)
	r.Header.Set("X-MFA", "mfa")
}
func TestHealthReadinessIdentityAndOrigin(t *testing.T) {
	s, _, st := testServer(t)
	defer st.Close()
	handler := s.BrowserHandler()
	for _, path := range []string{"/healthz", "/readyz", "/metrics"} {
		r := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("%s = %d", path, w.Code)
		}
	}
	bad := httptest.NewRequest("GET", "/api/v1/session", nil)
	headers(bad, "attacker")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, bad)
	if w.Code != 401 {
		t.Fatalf("wrong identity accepted: %d", w.Code)
	}
	login := httptest.NewRequest("GET", "/api/v1/session", nil)
	headers(login, "operator")
	loginW := httptest.NewRecorder()
	handler.ServeHTTP(loginW, login)
	if loginW.Code != 200 {
		t.Fatalf("login failed: %d", loginW.Code)
	}
	cookie := loginW.Result().Cookies()[0]
	ws := httptest.NewRequest("GET", "/v1/browser/ws", nil)
	headers(ws, "operator")
	ws.AddCookie(cookie)
	ws.Header.Set("Origin", "https://evil.example")
	wsW := httptest.NewRecorder()
	handler.ServeHTTP(wsW, ws)
	if wsW.Code != 403 {
		t.Fatalf("bad origin accepted: %d", wsW.Code)
	}
}
func TestStateChangingHTTPRequiresExactOrigin(t *testing.T) {
	s, _, st := testServer(t)
	defer st.Close()
	handler := s.BrowserHandler()
	login := httptest.NewRequest("GET", "/api/v1/session", nil)
	headers(login, "operator")
	loginW := httptest.NewRecorder()
	handler.ServeHTTP(loginW, login)
	var sessionBody map[string]string
	if err := json.Unmarshal(loginW.Body.Bytes(), &sessionBody); err != nil {
		t.Fatal(err)
	}
	request := func(origin string) int {
		r := httptest.NewRequest("POST", "/api/v1/enrollments", strings.NewReader(`{"display_name":"host"}`))
		headers(r, "operator")
		r.AddCookie(loginW.Result().Cookies()[0])
		r.Header.Set("X-CSRF-Token", sessionBody["csrf_token"])
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Code
	}
	if got := request(""); got != 403 {
		t.Fatalf("missing origin = %d", got)
	}
	if got := request("https://evil.example"); got != 403 {
		t.Fatalf("wrong origin = %d", got)
	}
	if got := request("https://app.example"); got != 201 {
		t.Fatalf("exact origin = %d", got)
	}
	badName := httptest.NewRequest("POST", "/api/v1/enrollments", strings.NewReader("{\"display_name\":\"bad\\nname\"}"))
	headers(badName, "operator")
	badName.AddCookie(loginW.Result().Cookies()[0])
	badName.Header.Set("X-CSRF-Token", sessionBody["csrf_token"])
	badName.Header.Set("Origin", "https://app.example")
	badNameResponse := httptest.NewRecorder()
	handler.ServeHTTP(badNameResponse, badName)
	if badNameResponse.Code != http.StatusBadRequest {
		t.Fatalf("control-character display name status = %d", badNameResponse.Code)
	}
}

func TestConnectorListenerRejectsNoClientCertificate(t *testing.T) {
	s, _, st := testServer(t)
	defer st.Close()
	ts := httptest.NewUnstartedServer(s.ConnectorHandler())
	ts.TLS = &tls.Config{ClientAuth: tls.RequireAnyClientCert}
	ts.StartTLS()
	defer ts.Close()
	_, err := ts.Client().Get(ts.URL + "/v1/connectors/ws")
	if err == nil {
		t.Fatal("TLS listener accepted a client without certificate")
	}
}
func TestMTLSFingerprintMappingAndRevocation(t *testing.T) {
	server, _, st := testServer(t)
	defer st.Close()
	ca, key := makeCA(t)
	host := "019f64ca-1000-7000-8000-000000000002"
	clientCert, parsed := issueClientCertificate(t, ca, key)
	if err := st.AddCertificate(context.Background(), store.Certificate{Serial: parsed.SerialNumber.Text(16), HostID: host, Fingerprint: enrollment.Fingerprint(parsed), NotBefore: parsed.NotBefore, NotAfter: parsed.NotAfter}); err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	ts := httptest.NewUnstartedServer(server.ConnectorHandler())
	ts.TLS = &tls.Config{ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool}
	ts.StartTLS()
	defer ts.Close()
	transport := ts.Client().Transport.(*http.Transport).Clone()
	transport.TLSClientConfig = transport.TLSClientConfig.Clone()
	transport.TLSClientConfig.Certificates = []tls.Certificate{clientCert}
	client := &http.Client{Transport: transport}
	response, err := client.Get(ts.URL + "/v1/connectors/ws")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized {
		t.Fatal("mapped production certificate rejected")
	}
	if err := st.RevokeHost(context.Background(), host); err != nil {
		t.Fatal(err)
	}
	response, err = client.Get(ts.URL + "/v1/connectors/ws")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked certificate status = %d", response.StatusCode)
	}
}
func TestUnsupportedConnectorRangeGetsProtocolErrorAnd4406(t *testing.T) {
	server, _, st := testServer(t)
	defer st.Close()
	ca, key := makeCA(t)
	host := "019f64ca-1000-7000-8000-000000000002"
	clientCert, parsed := issueClientCertificate(t, ca, key)
	if err := st.AddCertificate(context.Background(), store.Certificate{Serial: parsed.SerialNumber.Text(16), HostID: host, Fingerprint: enrollment.Fingerprint(parsed), NotBefore: parsed.NotBefore, NotAfter: parsed.NotAfter}); err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	ts := httptest.NewUnstartedServer(server.ConnectorHandler())
	ts.TLS = &tls.Config{ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool}
	ts.StartTLS()
	defer ts.Close()
	transport := ts.Client().Transport.(*http.Transport).Clone()
	transport.TLSClientConfig = transport.TLSClientConfig.Clone()
	transport.TLSClientConfig.Certificates = []tls.Certificate{clientCert}
	client := &http.Client{Transport: transport}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, strings.Replace(ts.URL, "https://", "wss://", 1)+"/v1/connectors/ws", &websocket.DialOptions{HTTPClient: client, CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	hello := protocol.Hello{MinProtocol: 2, MaxProtocol: 2, ConnectorVersion: "0.1.0", ConnectorInstanceID: mustTestID(), DisplayName: "host", Platform: "linux", Architecture: "amd64"}
	frame, _ := protocol.MarshalEnvelope(0, "connector.hello", hello)
	if err := conn.Write(ctx, websocket.MessageText, frame); err != nil {
		t.Fatal(err)
	}
	_, response, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	env, _, err := protocol.DecodeStrict(response, "connector")
	if err != nil || env.Type != "protocol.error" {
		t.Fatalf("response = %s, %v", env.Type, err)
	}
	_, _, err = conn.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusCode(4406) {
		t.Fatalf("close status = %d, %v", websocket.CloseStatus(err), err)
	}
}
func issueClientCertificate(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey) (tls.Certificate, *x509.Certificate) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(2), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalPKCS8PrivateKey(key)
	pair, err := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := x509.ParseCertificate(der)
	return pair, parsed
}

func TestDisconnectClassifiesUnknownWriteAndFailedRead(t *testing.T) {
	_, hub, st := testServer(t)
	defer st.Close()
	ctx := context.Background()
	writeID := "019f64ca-3000-7000-8000-000000000105"
	readID := "019f64ca-3000-7000-8000-000000000106"
	for _, v := range []struct{ id, op string }{{writeID, "agent.send_text"}, {readID, "agent.read"}} {
		if err := st.BeginAction(ctx, store.ActionIntent{ActionID: v.id, OperationType: v.op, Issuer: "i", Subject: "s", HostID: "h", InstanceID: "default", TerminalID: "term"}); err != nil {
			t.Fatal(err)
		}
	}
	l := &lease{hostID: "host", pending: map[string]*pending{writeID: {operation: "agent.send_text", received: make(chan struct{}), result: make(chan protocol.ActionResult, 1)}, readID: {operation: "agent.read", received: make(chan struct{}), result: make(chan protocol.ActionResult, 1)}}}
	hub.mu.Lock()
	hub.leases[l.hostID] = l
	hub.mu.Unlock()
	hub.release(l)
	write, _ := st.Action(ctx, writeID)
	read, _ := st.Action(ctx, readID)
	if write.Status != "unknown" || write.Code == nil || *write.Code != "OUTCOME_UNKNOWN" {
		t.Fatalf("write = %#v", write)
	}
	if read.Status != "failed" || read.Code == nil || *read.Code != "CONNECTION_LOST" {
		t.Fatalf("read = %#v", read)
	}
}

func TestBrowserActionTranslatesEpochsAcrossConnector(t *testing.T) {
	server, hub, st := testServer(t)
	defer st.Close()
	host := "019f64ca-1000-7000-8000-000000000002"
	connectorEpoch := "019f64ca-3000-7000-8000-000000000110"
	browserEpoch := "019f64ca-3000-7000-8000-000000000103"
	l := &lease{hostID: host, connectionID: "019f64ca-3000-7000-8000-000000000111", version: "0.1.0", queue: connector.NewQueue(8), instances: map[string]protocol.InstanceSnapshot{"default": {InstanceID: "default", Epoch: connectorEpoch, HerdrVersion: "0.7.3", HerdrProtocol: 16, Status: "online", Capabilities: []string{"read.v1"}, Agents: []protocol.Agent{{TerminalID: "term", Agent: "opencode", Status: "blocked", Generation: 1}}}}, pending: map[string]*pending{}, outputs: map[string]func(protocol.OutputSnapshot){}, rateTokens: 10, lastRate: time.Now()}
	hub.mu.Lock()
	hub.leases[host] = l
	hub.mu.Unlock()
	lines := 1
	actionID := "019f64ca-3000-7000-8000-000000000105"
	request := protocol.BrowserActionRequest{SessionID: "019f64ca-3000-7000-8000-000000000101", ActionRequest: protocol.ActionRequest{ActionID: actionID, Target: protocol.Target{HostID: host, InstanceID: "default", TerminalID: "term"}, TimeoutMS: 5000, Expected: protocol.Expected{StateEpoch: browserEpoch, ConnectorEpoch: connectorEpoch, AgentGeneration: 1, Agent: "opencode", Statuses: []string{"blocked"}}, Operation: protocol.Operation{Type: "agent.read", Source: "recent", Lines: &lines}}}
	go func() {
		frame, _ := l.queue.Next(context.Background())
		_, body, err := protocol.DecodeStrict(frame, "connector")
		if err != nil {
			t.Error(err)
			return
		}
		sent := body.(*protocol.ActionRequest)
		if sent.Expected.StateEpoch != connectorEpoch || sent.Expected.ConnectorEpoch != "" {
			t.Errorf("connector epochs = %#v", sent.Expected)
		}
		hub.actionReceived(l, actionID)
		read := protocol.ReadResult{StateEpoch: connectorEpoch, AgentGeneration: 1, ContentRevision: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
		hub.actionResult(l, protocol.ActionResult{ActionID: actionID, OperationType: "agent.read", Status: "succeeded", Message: json.RawMessage("null"), Result: mustJSON(read)})
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var final protocol.ActionResult
	server.browserAction(ctx, auth.Identity{Issuer: "issuer", Subject: "operator"}, request.SessionID, browserEpoch, request, func(typ string, body any) bool {
		if typ == "action.result" {
			final = body.(protocol.ActionResult)
		}
		return true
	})
	var read protocol.ReadResult
	if err := json.Unmarshal(final.Result, &read); err != nil {
		t.Fatal(err)
	}
	if read.StateEpoch != browserEpoch || read.ConnectorEpoch != connectorEpoch {
		t.Fatalf("browser result epochs = %#v", read)
	}
}
func TestDurableConnectorCompletionSurvivesBrowserFrameDisconnect(t *testing.T) {
	server, hub, st := testServer(t)
	defer st.Close()
	host := "019f64ca-1000-7000-8000-000000000002"
	connectorEpoch := "019f64ca-3000-7000-8000-000000000110"
	browserEpoch := "019f64ca-3000-7000-8000-000000000103"
	l := &lease{hostID: host, connectionID: mustTestID(), version: "0.1.0", queue: connector.NewQueue(8), instances: map[string]protocol.InstanceSnapshot{"default": {InstanceID: "default", Epoch: connectorEpoch, HerdrVersion: "0.8.0", HerdrProtocol: 17, Status: "online", Capabilities: []string{"checked_input.v1"}, Agents: []protocol.Agent{{TerminalID: "term", Agent: "opencode", Status: "blocked", Generation: 1, HerdrInputRevision: 42}}}}, pending: map[string]*pending{}, outputs: map[string]func(protocol.OutputSnapshot){}, rateTokens: 10, lastRate: time.Now()}
	hub.leases[host] = l
	text := "continue"
	id := "019f64ca-3000-7000-8000-000000000190"
	request := protocol.BrowserActionRequest{SessionID: mustTestID(), ActionRequest: protocol.ActionRequest{ActionID: id, Target: protocol.Target{HostID: host, InstanceID: "default", TerminalID: "term"}, TimeoutMS: 1000, Expected: protocol.Expected{StateEpoch: browserEpoch, ConnectorEpoch: connectorEpoch, AgentGeneration: 1, HerdrInputRevision: 42, Agent: "opencode", Statuses: []string{"blocked"}}, Operation: protocol.Operation{Type: "agent.send_text", Text: &text}}}
	go func() {
		_, _ = l.queue.Next(context.Background())
		hub.actionReceived(l, id)
		hub.actionResult(l, protocol.ActionResult{ActionID: id, OperationType: "agent.send_text", Status: "succeeded", Message: json.RawMessage("null"), Result: mustJSON(protocol.WriteResult{HerdrAcknowledged: true})})
	}()
	ctx, cancel := context.WithCancel(context.Background())
	server.browserAction(ctx, auth.Identity{Issuer: "issuer", Subject: "operator"}, request.SessionID, browserEpoch, request, func(messageType string, _ any) bool {
		if messageType == "action.result" {
			status, err := st.Action(context.Background(), id)
			if err != nil || status.Status != "succeeded" {
				t.Errorf("completion was not durable before delivery: %#v %v", status, err)
			}
			cancel()
			return false
		}
		return true
	})
	status, err := st.Action(context.Background(), id)
	if err != nil || status.Status != "succeeded" || status.Code != nil {
		t.Fatalf("browser disconnect rewrote durable completion: %#v %v", status, err)
	}
	handler := server.BrowserHandler()
	login := httptest.NewRequest("GET", "/api/v1/session", nil)
	headers(login, "operator")
	loginResponse := httptest.NewRecorder()
	handler.ServeHTTP(loginResponse, login)
	query := httptest.NewRequest("GET", "/api/v1/actions/"+id, nil)
	headers(query, "operator")
	query.AddCookie(loginResponse.Result().Cookies()[0])
	queryResponse := httptest.NewRecorder()
	handler.ServeHTTP(queryResponse, query)
	if queryResponse.Code != http.StatusOK || !strings.Contains(queryResponse.Body.String(), `"status":"succeeded"`) {
		t.Fatalf("action status response %d: %s", queryResponse.Code, queryResponse.Body.String())
	}
}
func TestDeltaAgentLimitRejectsWithoutPartialMutation(t *testing.T) {
	_, hub, st := testServer(t)
	defer st.Close()
	epoch := "019f64ca-3000-7000-8000-000000000110"
	agents := make([]protocol.Agent, protocol.MaxAgents)
	for i := range agents {
		agents[i] = protocol.Agent{TerminalID: fmt.Sprintf("term-%d", i), PaneID: "p", WorkspaceID: "w", TabID: "t", Agent: "opencode", Status: "working", Generation: 1}
	}
	l := &lease{hostID: "019f64ca-1000-7000-8000-000000000002", instances: map[string]protocol.InstanceSnapshot{"default": {InstanceID: "default", Epoch: epoch, HerdrVersion: "0.8.0", HerdrProtocol: 17, Status: "online", Capabilities: []string{"read.v1"}, Agents: agents}}}
	hub.mu.Lock()
	hub.leases[l.hostID] = l
	hub.mu.Unlock()
	extra := protocol.Agent{TerminalID: "overflow", PaneID: "p", WorkspaceID: "w", TabID: "t", Agent: "opencode", Status: "working", Generation: 1}
	err := hub.updateDelta(l, protocol.StateDelta{InstanceID: "default", Epoch: epoch, Sequence: 1, Changes: []protocol.StateChange{{Operation: "upsert", Agent: &extra}}})
	if err == nil {
		t.Fatal("agent overflow accepted")
	}
	if got := len(l.instances["default"].Agents); got != protocol.MaxAgents {
		t.Fatalf("partial mutation left %d agents", got)
	}
}
func TestAuditFailureStaysClosedUntilRecordRepaired(t *testing.T) {
	_, hub, st := testServer(t)
	defer st.Close()
	id := "019f64ca-3000-7000-8000-000000000155"
	if err := st.BeginAction(context.Background(), store.ActionIntent{ActionID: id, OperationType: "agent.send_text", Issuer: "i", Subject: "s", HostID: "h", InstanceID: "default", TerminalID: "term"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	code := "OUTCOME_UNKNOWN"
	hub.completeAction(ctx, id, "unknown", &code, time.Now())
	if !hub.AuditBlocked() {
		t.Fatal("audit failure did not fail closed")
	}
	if err := hub.RepairAudits(context.Background()); err != nil {
		t.Fatal(err)
	}
	if hub.AuditBlocked() {
		t.Fatal("audit gate remained closed after durable repair")
	}
	status, err := st.Action(context.Background(), id)
	if err != nil || status.Status != "unknown" {
		t.Fatalf("repair not durable: %#v %v", status, err)
	}
}
func TestLifecycleAuditRepairsKeepUniqueEventsAndOriginalTimes(t *testing.T) {
	_, hub, st := testServer(t)
	defer st.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := hub.auditEvent(ctx, "connector.disconnect", "host", nil); err == nil {
		t.Fatal("canceled lifecycle audit unexpectedly succeeded")
	}
	if err := hub.auditEvent(ctx, "connector.disconnect", "host", nil); err == nil {
		t.Fatal("second canceled lifecycle audit unexpectedly succeeded")
	}
	hub.auditMu.Lock()
	queued := len(hub.auditRepairs)
	hub.auditMu.Unlock()
	if queued != 2 {
		t.Fatalf("queued lifecycle repairs = %d", queued)
	}
	repairStarted := time.Now().UTC()
	if err := hub.RepairAudits(context.Background()); err != nil {
		t.Fatal(err)
	}
	occurrences, err := st.AuditEventOccurrences(context.Background(), "connector.disconnect")
	if err != nil {
		t.Fatal(err)
	}
	if len(occurrences) != 2 {
		t.Fatalf("repaired events = %d", len(occurrences))
	}
	for _, occurred := range occurrences {
		if !occurred.Before(repairStarted) {
			t.Fatalf("occurrence time %s was replaced by repair time %s", occurred, repairStarted)
		}
	}
}
func TestActionQueueFailureFinalizesConservatively(t *testing.T) {
	_, hub, st := testServer(t)
	defer st.Close()
	id := "019f64ca-3000-7000-8000-000000000166"
	if err := st.BeginAction(context.Background(), store.ActionIntent{ActionID: id, OperationType: "agent.send_text", Issuer: "i", Subject: "s", HostID: "019f64ca-1000-7000-8000-000000000002", InstanceID: "default", TerminalID: "term"}); err != nil {
		t.Fatal(err)
	}
	q := connector.NewQueue(1)
	_ = q.Put(context.Background(), []byte("occupied"))
	l := &lease{hostID: "019f64ca-1000-7000-8000-000000000002", queue: q, pending: map[string]*pending{}, rateTokens: 10, lastRate: time.Now()}
	hub.mu.Lock()
	hub.leases[l.hostID] = l
	hub.mu.Unlock()
	text := "write"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	handle, err := hub.Dispatch(ctx, protocol.ActionRequest{ActionID: id, Target: protocol.Target{HostID: l.hostID, InstanceID: "default", TerminalID: "term"}, TimeoutMS: 1000, Expected: protocol.Expected{StateEpoch: "019f64ca-3000-7000-8000-000000000110", AgentGeneration: 1, HerdrInputRevision: 1, Agent: "opencode", Statuses: []string{"blocked"}}, Operation: protocol.Operation{Type: "agent.send_text", Text: &text}})
	if err != nil {
		t.Fatal(err)
	}
	result := <-handle.Result
	if result.Status != "unknown" || *result.Code != "OUTCOME_UNKNOWN" {
		t.Fatalf("queue failure result = %#v", result)
	}
	status, _ := st.Action(context.Background(), id)
	if status.Status != "unknown" {
		t.Fatalf("audit status = %#v", status)
	}
}
func TestSilentConnectedConnectorExpiresActionAndFreesCapacity(t *testing.T) {
	_, hub, st := testServer(t)
	defer st.Close()
	host := "019f64ca-1000-7000-8000-000000000002"
	l := &lease{hostID: host, queue: connector.NewQueue(4), pending: map[string]*pending{}, rateTokens: 10, lastRate: time.Now()}
	hub.leases[host] = l
	run := func(id, operation string) protocol.ActionResult {
		if err := st.BeginAction(context.Background(), store.ActionIntent{ActionID: id, OperationType: operation, Issuer: "i", Subject: "s", HostID: host, InstanceID: "default", TerminalID: "term"}); err != nil {
			t.Fatal(err)
		}
		op := protocol.Operation{Type: operation}
		if operation == "agent.read" {
			lines := 1
			op.Source = "recent"
			op.Lines = &lines
		}
		handle, err := hub.Dispatch(context.Background(), protocol.ActionRequest{ActionID: id, Target: protocol.Target{HostID: host, InstanceID: "default", TerminalID: "term"}, TimeoutMS: 10, Expected: protocol.Expected{StateEpoch: mustTestID(), AgentGeneration: 1, HerdrInputRevision: 1, Agent: "opencode", Statuses: []string{"blocked"}}, Operation: op})
		if err != nil {
			t.Fatal(err)
		}
		select {
		case result := <-handle.Result:
			return result
		case <-time.After(time.Second):
			t.Fatal("control-plane action deadline did not fire")
		}
		return protocol.ActionResult{}
	}
	write := run("019f64ca-3000-7000-8000-000000000188", "agent.interrupt")
	if write.Status != "unknown" || *write.Code != "OUTCOME_UNKNOWN" {
		t.Fatalf("write deadline = %#v", write)
	}
	read := run("019f64ca-3000-7000-8000-000000000189", "agent.read")
	if read.Status != "failed" || *read.Code != "DEADLINE_EXCEEDED" {
		t.Fatalf("read deadline = %#v", read)
	}
	hub.mu.RLock()
	pendingCount := len(l.pending)
	hub.mu.RUnlock()
	if pendingCount != 0 {
		t.Fatalf("deadline left %d in-flight actions", pendingCount)
	}
}
func TestBrowserDisconnectCleansOutputSubscriptions(t *testing.T) {
	_, hub, st := testServer(t)
	defer st.Close()
	q := connector.NewQueue(2)
	subscriptionID := "019f64ca-3000-7000-8000-000000000177"
	l := &lease{hostID: "019f64ca-1000-7000-8000-000000000002", queue: q, outputs: map[string]func(protocol.OutputSnapshot){subscriptionID: func(protocol.OutputSnapshot) {}}}
	hub.mu.Lock()
	hub.leases[l.hostID] = l
	hub.mu.Unlock()
	cleanupBrowserOutputs(hub, map[string]string{subscriptionID: l.hostID})
	if len(l.outputs) != 0 {
		t.Fatal("browser output subscription leaked")
	}
	frame, err := q.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	env, _, err := protocol.DecodeStrict(frame, "connector")
	if err != nil || env.Type != "output.unsubscribe" {
		t.Fatalf("unsubscribe delivery = %s, %v", env.Type, err)
	}
}
func TestDisconnectedHostIsRetainedAndStateUsesDelta(t *testing.T) {
	_, hub, st := testServer(t)
	defer st.Close()
	hostID := "019f64ca-1000-7000-8000-000000000002"
	l := &lease{hostID: hostID, displayName: "host", connectionID: mustTestID(), queue: connector.NewQueue(8), instances: map[string]protocol.InstanceSnapshot{}, pending: map[string]*pending{}, outputs: map[string]func(protocol.OutputSnapshot){}, rateTokens: 10, lastRate: time.Now()}
	if err := hub.acquire(l); err != nil {
		t.Fatal(err)
	}
	events := make(chan StateEvent, 4)
	unsubscribe := hub.Subscribe(func(kind string, value any) {
		if kind == "state.delta" {
			events <- value.(StateEvent)
		}
	})
	defer unsubscribe()
	epoch := "019f64ca-3000-7000-8000-000000000110"
	snapshot := protocol.InstanceSnapshot{InstanceID: "default", Epoch: epoch, HerdrVersion: "0.7.3", HerdrProtocol: 16, Status: "online", Capabilities: []string{"read.v1"}, Agents: []protocol.Agent{{TerminalID: "term", PaneID: "p", WorkspaceID: "w", TabID: "t", Agent: "opencode", Status: "working", Generation: 1}}}
	if err := hub.updateSnapshot(l, snapshot); err != nil {
		t.Fatal(err)
	}
	event := <-events
	body := protocol.StateDelta{SessionID: mustTestID(), StateEpoch: mustTestID(), Sequence: 1, Changes: event.Changes}
	frame, _ := protocol.MarshalEnvelope(1, "state.delta", body)
	if _, _, err := protocol.DecodeStrict(frame, "browser"); err != nil {
		t.Fatalf("projected delta invalid: %v", err)
	}
	hub.release(l)
	state := hub.Snapshot(mustTestID(), mustTestID())
	if len(state.Hosts) != 1 || state.Hosts[0].Status != "disconnected" || len(state.Hosts[0].Instances) != 1 {
		t.Fatalf("disconnected projection = %#v", state.Hosts)
	}
	for _, kind := range []string{"connector.connect", "connector.disconnect"} {
		count, err := st.CountAuditEvents(context.Background(), kind)
		if err != nil || count != 1 {
			t.Fatalf("audit %s = %d, %v", kind, count, err)
		}
	}
}
func TestKnownDisconnectedHostSurvivesControlPlaneRestart(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	host := "019f64ca-1000-7000-8000-000000000002"
	if err := st.CreateEnrollment(context.Background(), store.HashToken("token"), host, "persisted", time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := st.CommitEnrollmentCertificate(context.Background(), store.HashToken("token"), store.Certificate{Serial: "1", HostID: host, Fingerprint: "known-host", NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	hub, err := NewHub(st, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := hub.Snapshot(mustTestID(), mustTestID())
	if len(snapshot.Hosts) != 1 || snapshot.Hosts[0].HostID != host || snapshot.Hosts[0].Status != "disconnected" {
		t.Fatalf("recovered hosts = %#v", snapshot.Hosts)
	}
}
func TestHostInstanceAndSequenceLimitsAreAtomic(t *testing.T) {
	_, hub, st := testServer(t)
	defer st.Close()
	for i := 0; i < 10; i++ {
		id := fmt.Sprintf("019f64ca-1000-7000-8000-%012x", i+1)
		hub.hosts[id] = protocol.HostSnapshot{HostID: id, DisplayName: "host", Status: "disconnected"}
	}
	extra := &lease{hostID: "019f64ca-1000-7000-8000-000000000099", displayName: "extra"}
	if err := hub.acquire(extra); err == nil {
		t.Fatal("eleventh retained host accepted")
	}
	host := "019f64ca-1000-7000-8000-000000000001"
	l := &lease{hostID: host, instances: map[string]protocol.InstanceSnapshot{}}
	hub.leases[host] = l
	for i := 0; i < protocol.MaxInstances; i++ {
		id := fmt.Sprintf("instance-%d", i)
		l.instances[id] = protocol.InstanceSnapshot{InstanceID: id, Epoch: mustTestID(), HerdrVersion: "0.7.3", HerdrProtocol: 16, Status: "online", Capabilities: []string{"read.v1"}}
	}
	overflow := protocol.InstanceSnapshot{InstanceID: "overflow", Epoch: mustTestID(), HerdrVersion: "0.7.3", HerdrProtocol: 16, Status: "online", Capabilities: []string{"read.v1"}}
	if err := hub.updateSnapshot(l, overflow); err == nil {
		t.Fatal("seventeenth instance accepted")
	}
	base := l.instances["instance-0"]
	base.Agents = []protocol.Agent{{TerminalID: "term", PaneID: "p", WorkspaceID: "w", TabID: "t", Agent: "opencode", Status: "working", Generation: 1}}
	base.Sequence = 0
	l.instances["instance-0"] = base
	agent := base.Agents[0]
	agent.Generation = 2
	if err := hub.updateDelta(l, protocol.StateDelta{InstanceID: "instance-0", Epoch: base.EffectiveEpoch(), Sequence: 2, Changes: []protocol.StateChange{{Operation: "upsert", Agent: &agent}}}); err == nil {
		t.Fatal("state sequence gap accepted")
	}
	if got := l.instances["instance-0"].Agents[0].Generation; got != 1 {
		t.Fatalf("sequence failure mutated state: %d", got)
	}
}
func TestConnectorEpochChangeEmitsInvalidatingDelta(t *testing.T) {
	_, hub, st := testServer(t)
	defer st.Close()
	host := "019f64ca-1000-7000-8000-000000000002"
	first := protocol.InstanceSnapshot{InstanceID: "default", Epoch: "019f64ca-3000-7000-8000-000000000110", HerdrVersion: "0.7.3", HerdrProtocol: 16, Status: "online", Capabilities: []string{"read.v1"}}
	l := &lease{hostID: host, instances: map[string]protocol.InstanceSnapshot{"default": first}}
	hub.leases[host] = l
	hub.hosts[host] = protocol.HostSnapshot{HostID: host, DisplayName: "host", Status: "connected", Instances: []protocol.InstanceSnapshot{projectInstance(first)}}
	events := make(chan StateEvent, 1)
	unsub := hub.Subscribe(func(kind string, value any) {
		if kind == "state.delta" {
			events <- value.(StateEvent)
		}
	})
	defer unsub()
	second := first
	second.Epoch = "019f64ca-3000-7000-8000-000000000111"
	if err := hub.updateSnapshot(l, second); err != nil {
		t.Fatal(err)
	}
	event := <-events
	if len(event.Changes) != 1 || event.Changes[0].Operation != "instance.epoch_changed" || event.Changes[0].PreviousConnectorEpoch != first.Epoch {
		t.Fatalf("epoch event = %#v", event)
	}
}
func TestReconnectSnapshotEmitsEpochChangedInsteadOfInstanceUpsert(t *testing.T) {
	_, hub, st := testServer(t)
	defer st.Close()
	host := "019f64ca-1000-7000-8000-000000000002"
	oldEpoch := "019f64ca-3000-7000-8000-000000000110"
	old := protocol.InstanceSnapshot{InstanceID: "default", ConnectorEpoch: oldEpoch, HerdrVersion: "0.7.3", HerdrProtocol: 16, Status: "online", Capabilities: []string{"read.v1"}}
	hub.hosts[host] = protocol.HostSnapshot{HostID: host, DisplayName: "host", Status: "disconnected", Instances: []protocol.InstanceSnapshot{old}}
	lease := &lease{hostID: host, displayName: "host", connectionID: mustTestID(), queue: connector.NewQueue(8), instances: map[string]protocol.InstanceSnapshot{}, pending: map[string]*pending{}, outputs: map[string]func(protocol.OutputSnapshot){}, rateTokens: 10, lastRate: time.Now()}
	if err := hub.acquire(lease); err != nil {
		t.Fatal(err)
	}
	events := make(chan StateEvent, 4)
	unsubscribe := hub.Subscribe(func(kind string, value any) {
		if kind == "state.delta" {
			events <- value.(StateEvent)
		}
	})
	defer unsubscribe()
	snapshot := protocol.InstanceSnapshot{InstanceID: "default", Epoch: "019f64ca-3000-7000-8000-000000000111", HerdrVersion: "0.7.3", HerdrProtocol: 16, Status: "online", Capabilities: []string{"read.v1"}}
	if err := hub.updateSnapshot(lease, snapshot); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(time.Second)
	for {
		select {
		case event := <-events:
			for _, change := range event.Changes {
				if change.Operation == "instance.upsert" {
					t.Fatal("reconnect emitted instance.upsert")
				}
				if change.Operation == "instance.epoch_changed" {
					if change.PreviousConnectorEpoch != oldEpoch {
						t.Fatalf("previous epoch = %s", change.PreviousConnectorEpoch)
					}
					return
				}
			}
		case <-deadline:
			t.Fatal("reconnect did not emit instance.epoch_changed")
		}
	}
}
func TestConnectorReconnectRemovesUnconfiguredInstance(t *testing.T) {
	_, hub, st := testServer(t)
	defer st.Close()
	host := "019f64ca-1000-7000-8000-000000000002"
	defaultOld := protocol.InstanceSnapshot{InstanceID: "default", ConnectorEpoch: "019f64ca-3000-7000-8000-000000000110", HerdrVersion: "0.7.3", HerdrProtocol: 16, Status: "online", Capabilities: []string{"read.v1"}, Agents: []protocol.Agent{{TerminalID: "default-term", Agent: "opencode", Status: "working", AgentGeneration: 1, ConnectorEpoch: "019f64ca-3000-7000-8000-000000000110"}}}
	removed := protocol.InstanceSnapshot{InstanceID: "removed", ConnectorEpoch: "019f64ca-3000-7000-8000-000000000120", HerdrVersion: "0.7.3", HerdrProtocol: 16, Status: "online", Capabilities: []string{"read.v1"}, Agents: []protocol.Agent{{TerminalID: "removed-term", Agent: "opencode", Status: "blocked", AgentGeneration: 1, ConnectorEpoch: "019f64ca-3000-7000-8000-000000000120"}}}
	hub.hosts[host] = protocol.HostSnapshot{HostID: host, DisplayName: "host", Status: "connected", Instances: []protocol.InstanceSnapshot{defaultOld, removed}}
	oldLease := &lease{hostID: host, displayName: "host", instances: map[string]protocol.InstanceSnapshot{"default": defaultOld, "removed": removed}, pending: map[string]*pending{}, outputs: map[string]func(protocol.OutputSnapshot){mustTestID(): func(protocol.OutputSnapshot) {}}}
	hub.leases[host] = oldLease
	hub.release(oldLease)
	if len(oldLease.outputs) != 0 {
		t.Fatal("connector disconnect retained output subscriptions")
	}

	events := make(chan StateEvent, 4)
	unsubscribe := hub.Subscribe(func(kind string, value any) {
		if kind == "state.delta" {
			events <- value.(StateEvent)
		}
	})
	defer unsubscribe()
	lease := &lease{hostID: host, displayName: "host", connectionID: mustTestID(), queue: connector.NewQueue(8), instances: map[string]protocol.InstanceSnapshot{}, pending: map[string]*pending{}, outputs: map[string]func(protocol.OutputSnapshot){}, rateTokens: 10, lastRate: time.Now(), inventoryExpected: true}
	if err := hub.acquire(lease); err != nil {
		t.Fatal(err)
	}
	if err := hub.updateInventory(lease, protocol.InstanceInventory{InstanceIDs: []string{"default"}}); err != nil {
		t.Fatal(err)
	}

	state := hub.Snapshot(mustTestID(), mustTestID())
	if len(state.Hosts) != 1 || len(state.Hosts[0].Instances) != 1 || state.Hosts[0].Instances[0].InstanceID != "default" {
		t.Fatalf("reconciled snapshot retained removed instance or agent: %#v", state.Hosts)
	}
	defaultNew := protocol.InstanceSnapshot{InstanceID: "default", Epoch: "019f64ca-3000-7000-8000-000000000111", HerdrVersion: "0.7.3", HerdrProtocol: 16, Status: "online", Capabilities: []string{"read.v1"}}
	if err := hub.updateSnapshot(lease, defaultNew); err != nil {
		t.Fatal(err)
	}
	removedNew := removed
	removedNew.ConnectorEpoch = ""
	removedNew.Epoch = "019f64ca-3000-7000-8000-000000000121"
	if err := hub.updateSnapshot(lease, removedNew); err == nil {
		t.Fatal("connector restored an instance absent from its authoritative inventory")
	}

	var sawRemove, sawEpochChange bool
	deadline := time.After(time.Second)
	for !sawRemove || !sawEpochChange {
		select {
		case event := <-events:
			for _, change := range event.Changes {
				switch change.Operation {
				case "instance.remove":
					if change.InstanceID == "default" {
						t.Fatal("returning instance was removed before epoch change")
					}
					if change.InstanceID == "removed" && change.Reason == "unconfigured" {
						sawRemove = true
					}
				case "instance.epoch_changed":
					if change.InstanceID == "default" && change.PreviousConnectorEpoch == defaultOld.EffectiveEpoch() && change.ConnectorEpoch == defaultNew.EffectiveEpoch() {
						sawEpochChange = true
					}
				}
			}
		case <-deadline:
			t.Fatalf("reconnect events remove=%t epoch_changed=%t", sawRemove, sawEpochChange)
		}
	}
	state = hub.Snapshot(mustTestID(), mustTestID())
	if len(state.Hosts[0].Instances) != 1 || state.Hosts[0].Instances[0].InstanceID != "default" || state.Hosts[0].Instances[0].EffectiveEpoch() != defaultNew.EffectiveEpoch() {
		t.Fatalf("post-epoch snapshot = %#v", state.Hosts[0].Instances)
	}
}
func TestNewServerOldConnectorConservativelyRetainsPriorInstances(t *testing.T) {
	_, hub, st := testServer(t)
	defer st.Close()
	accepted := acceptConnectorCapabilities([]string{"output.subscribe.v1"})
	if protocol.HasCapability(accepted, protocol.StateInventoryCapability) {
		t.Fatal("server accepted inventory capability that old connector did not offer")
	}
	host := "019f64ca-1000-7000-8000-000000000002"
	oldEpoch := "019f64ca-3000-7000-8000-000000000110"
	prior := []protocol.InstanceSnapshot{
		{InstanceID: "default", ConnectorEpoch: oldEpoch, HerdrVersion: "0.7.3", HerdrProtocol: 16, Status: "online", Capabilities: []string{"read.v1"}},
		{InstanceID: "possibly-removed", ConnectorEpoch: "019f64ca-3000-7000-8000-000000000120", HerdrVersion: "0.7.3", HerdrProtocol: 16, Status: "online", Capabilities: []string{"read.v1"}},
	}
	hub.hosts[host] = protocol.HostSnapshot{HostID: host, DisplayName: "host", Status: "disconnected", Instances: prior}
	lease := &lease{hostID: host, displayName: "host", connectionID: mustTestID(), queue: connector.NewQueue(8), instances: map[string]protocol.InstanceSnapshot{}, pending: map[string]*pending{}, outputs: map[string]func(protocol.OutputSnapshot){}, rateTokens: 10, lastRate: time.Now()}
	if err := hub.acquire(lease); err != nil {
		t.Fatal(err)
	}
	returning := prior[0]
	returning.ConnectorEpoch = ""
	returning.Epoch = "019f64ca-3000-7000-8000-000000000111"
	if err := hub.updateSnapshot(lease, returning); err != nil {
		t.Fatal(err)
	}
	state := hub.Snapshot(mustTestID(), mustTestID())
	if len(state.Hosts) != 1 || len(state.Hosts[0].Instances) != 2 {
		t.Fatalf("old connector caused destructive reconciliation: %#v", state.Hosts)
	}
}

func TestNewServerNegotiatesInventoryOnlyWhenOffered(t *testing.T) {
	oldAccepted := acceptConnectorCapabilities([]string{"output.subscribe.v1", "unknown.future.v1"})
	if protocol.HasCapability(oldAccepted, protocol.StateInventoryCapability) {
		t.Fatal("inventory accepted for old connector")
	}
	newAccepted := acceptConnectorCapabilities([]string{"output.subscribe.v1", protocol.StateInventoryCapability})
	if !protocol.HasCapability(newAccepted, protocol.StateInventoryCapability) {
		t.Fatal("inventory capability was not negotiated")
	}
}
func TestStaticServerRejectsSymlinkEscape(t *testing.T) {
	server, _, st := testServer(t)
	defer st.Close()
	rootDir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret transcript"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootDir, "index.html"), []byte("safe index"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(rootDir, "leak.txt")); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(rootDir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	server.staticRoot = root
	server.cfg.StaticDir = rootDir
	request := httptest.NewRequest("GET", "/leak.txt", nil)
	response := httptest.NewRecorder()
	server.static().ServeHTTP(response, request)
	if strings.Contains(response.Body.String(), "secret transcript") {
		t.Fatal("static symlink escaped configured root")
	}
}

type recordingPushSender struct{ sent chan push.Event }

func (s recordingPushSender) Send(_ context.Context, _ store.PushSubscription, event push.Event) (push.Outcome, error) {
	s.sent <- event
	return push.Sent, nil
}
func TestPushOnlyFiresForSemanticAttentionTransition(t *testing.T) {
	base, hub, st := testServer(t)
	defer st.Close()
	sender := recordingPushSender{sent: make(chan push.Event, 2)}
	if err := st.UpsertPush(context.Background(), store.PushSubscription{Subject: "operator", Endpoint: "https://push.example/sub", P256DH: "key", Auth: "auth"}); err != nil {
		t.Fatal(err)
	}
	cfg := base.cfg
	cfg.Push = &push.Service{Store: st, Sender: sender, MaxAttempts: 1}
	cfg.OperatorSubject = "operator"
	if _, err := NewServer(cfg, hub); err != nil {
		t.Fatal(err)
	}
	host := "019f64ca-1000-7000-8000-000000000002"
	epoch := "019f64ca-3000-7000-8000-000000000110"
	l := &lease{hostID: host, instances: map[string]protocol.InstanceSnapshot{"default": {InstanceID: "default", Epoch: epoch, Sequence: 0, HerdrVersion: "0.7.3", HerdrProtocol: 16, Status: "online", Capabilities: []string{"read.v1"}, Agents: []protocol.Agent{{TerminalID: "term", PaneID: "p", WorkspaceID: "w", TabID: "t", Agent: "opencode", Status: "working", Generation: 1}}}}}
	hub.mu.Lock()
	hub.leases[host] = l
	hub.hosts[host] = protocol.HostSnapshot{HostID: host, DisplayName: "host", Status: "connected", Instances: []protocol.InstanceSnapshot{projectInstance(l.instances["default"])}}
	hub.mu.Unlock()
	idle := l.instances["default"].Agents[0]
	idle.Status = "idle"
	idle.Generation = 2
	if err := hub.updateDelta(l, protocol.StateDelta{InstanceID: "default", Epoch: epoch, Sequence: 1, Changes: []protocol.StateChange{{Operation: "upsert", Agent: &idle}}}); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-sender.sent:
		t.Fatalf("non-attention push sent: %#v", event)
	case <-time.After(20 * time.Millisecond):
	}
	blocked := idle
	blocked.Status = "blocked"
	blocked.Generation = 3
	if err := hub.updateDelta(l, protocol.StateDelta{InstanceID: "default", Epoch: epoch, Sequence: 2, Changes: []protocol.StateChange{{Operation: "upsert", Agent: &blocked}}}); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-sender.sent:
		if event.Kind != "agent_state_changed" {
			t.Fatalf("push = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("attention transition did not send push")
	}
}

type blockingPushSender struct {
	started chan struct{}
	release chan struct{}
}

func (s blockingPushSender) Send(_ context.Context, _ store.PushSubscription, _ push.Event) (push.Outcome, error) {
	s.started <- struct{}{}
	<-s.release
	return push.Sent, nil
}
func TestPushWorkersAreBounded(t *testing.T) {
	base, hub, st := testServer(t)
	defer st.Close()
	if err := st.UpsertPush(context.Background(), store.PushSubscription{Subject: "operator", Endpoint: "https://push.example/bounded", P256DH: "key", Auth: "auth"}); err != nil {
		t.Fatal(err)
	}
	sender := blockingPushSender{started: make(chan struct{}, 10), release: make(chan struct{})}
	cfg := base.cfg
	cfg.Push = &push.Service{Store: st, Sender: sender, MaxAttempts: 1}
	cfg.OperatorSubject = "operator"
	server, err := NewServer(cfg, hub)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		hub.notify("attention", nil)
	}
	deadline := time.After(time.Second)
	for len(sender.started) < cap(server.pushWorkers) {
		select {
		case <-deadline:
			t.Fatal("push workers did not start")
		case <-time.After(time.Millisecond):
		}
	}
	time.Sleep(10 * time.Millisecond)
	if got := len(sender.started); got != cap(server.pushWorkers) {
		t.Fatalf("active push sends = %d, capacity = %d", got, cap(server.pushWorkers))
	}
	close(sender.release)
}
func mustTestID() string { id, _ := protocol.NewUUIDv7(); return id }

func makeCA(t *testing.T) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(der)
	return ca, key
}

var _ = json.NewEncoder
