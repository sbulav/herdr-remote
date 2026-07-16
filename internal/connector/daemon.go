package connector

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/dcolinmorgan/herdr-remote/internal/protocol"
)

type DaemonConfig struct {
	URL, HostID, DisplayName, Version, CertFile, KeyFile, CAFile string
	Logger                                                       *slog.Logger
}
type Daemon struct {
	cfg                    DaemonConfig
	engines                map[string]*Engine
	instanceID             string
	compatibilityMu        sync.RWMutex
	compatibilityEndpoints map[string]struct{}
}

var baselineV1Capabilities = []string{"output.subscribe.v1", "prompt.respond.v1"}

const (
	priorStrictCapabilityErrorCode    = "INVALID_MESSAGE"
	priorStrictCapabilityErrorMessage = "invalid or duplicate capability"
	priorStrictCapabilityCloseCode    = websocket.StatusPolicyViolation
	priorStrictCapabilityCloseReason  = "invalid or duplicate capability"
)

type optionalCapabilityHandshakeRejection struct {
	err       error
	cacheable bool
}

func (e *optionalCapabilityHandshakeRejection) Error() string { return e.err.Error() }
func (e *optionalCapabilityHandshakeRejection) Unwrap() error { return e.err }

func NewDaemon(cfg DaemonConfig, e *Engine) (*Daemon, error) {
	return NewMultiDaemon(cfg, map[string]*Engine{e.cfg.InstanceID: e})
}
func NewMultiDaemon(cfg DaemonConfig, engines map[string]*Engine) (*Daemon, error) {
	if cfg.URL == "" || cfg.CertFile == "" || cfg.KeyFile == "" || cfg.CAFile == "" {
		return nil, errors.New("connector TLS configuration is required")
	}
	id, err := protocol.NewUUIDv7()
	if err != nil {
		return nil, err
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if len(engines) < 1 || len(engines) > protocol.MaxInstances {
		return nil, errors.New("connector instance count out of bounds")
	}
	for instance, engine := range engines {
		if engine == nil || engine.cfg.InstanceID != instance || engine.cfg.HostID != cfg.HostID {
			return nil, errors.New("connector instance configuration mismatch")
		}
	}
	return &Daemon{cfg: cfg, engines: engines, instanceID: id, compatibilityEndpoints: map[string]struct{}{}}, nil
}

func (d *Daemon) Run(ctx context.Context) error {
	attempt := 0
	for {
		err := d.runConnection(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		d.cfg.Logger.Warn("connector disconnected", "error", safeError(err))
		attempt++
		max := time.Second << min(attempt, 6)
		if max > 60*time.Second {
			max = 60 * time.Second
		}
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(max)))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(n.Int64())):
		}
	}
}

func (d *Daemon) runConnection(ctx context.Context) error {
	endpoint := compatibilityEndpoint(d.cfg.URL)
	d.compatibilityMu.RLock()
	_, baseline := d.compatibilityEndpoints[endpoint]
	d.compatibilityMu.RUnlock()
	err := d.runOnce(ctx, !baseline, nil)
	var rejection *optionalCapabilityHandshakeRejection
	if baseline || !errors.As(err, &rejection) || ctx.Err() != nil {
		return err
	}
	if !rejection.cacheable {
		d.cfg.Logger.Warn("retrying connector hello with ephemeral baseline v1 compatibility", "endpoint", endpoint, "disabled_optional_capability", protocol.StateInventoryCapability)
		return d.runOnce(ctx, false, nil)
	}
	return d.runOnce(ctx, false, func() {
		d.compatibilityMu.Lock()
		_, alreadyCached := d.compatibilityEndpoints[endpoint]
		if !alreadyCached {
			d.compatibilityEndpoints[endpoint] = struct{}{}
		}
		d.compatibilityMu.Unlock()
		if !alreadyCached {
			d.cfg.Logger.Warn("connector endpoint requires baseline v1 compatibility", "endpoint", endpoint, "disabled_optional_capability", protocol.StateInventoryCapability)
		}
	})
}

func (d *Daemon) runOnce(ctx context.Context, offerInventory bool, welcomed func()) error {
	client, err := tlsClient(d.cfg)
	if err != nil {
		return err
	}
	conn, _, err := websocket.Dial(ctx, d.cfg.URL, &websocket.DialOptions{HTTPClient: client, CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return err
	}
	defer conn.CloseNow()
	conn.SetReadLimit(protocol.MaxFrameBytes)
	capabilities := append([]string(nil), baselineV1Capabilities...)
	if offerInventory {
		capabilities = append(capabilities, protocol.StateInventoryCapability)
	}
	hello := protocol.Hello{MinProtocol: 1, MaxProtocol: 1, ConnectorVersion: d.cfg.Version, ConnectorInstanceID: d.instanceID, DisplayName: d.cfg.DisplayName, Platform: runtime.GOOS, Architecture: runtime.GOARCH, Capabilities: capabilities}
	b, _ := protocol.MarshalEnvelope(0, "connector.hello", hello)
	if err := conn.Write(ctx, websocket.MessageText, b); err != nil {
		return err
	}
	_, b, err = conn.Read(ctx)
	if err != nil {
		if offerInventory && priorStrictCapabilityClose(err) {
			return &optionalCapabilityHandshakeRejection{err: fmt.Errorf("server rejected extended hello before welcome: %w", err), cacheable: true}
		}
		if offerInventory && ambiguousPreWelcomeEOF(ctx, err) {
			return &optionalCapabilityHandshakeRejection{err: fmt.Errorf("server closed extended hello before welcome: %w", err)}
		}
		return err
	}
	_, body, err := protocol.DecodeStrict(b, "connector")
	if err != nil {
		return err
	}
	welcome, ok := body.(*protocol.Welcome)
	if !ok {
		if protocolError, isProtocolError := body.(*protocol.ProtocolError); isProtocolError {
			if offerInventory && priorStrictCapabilityProtocolError(*protocolError) {
				return &optionalCapabilityHandshakeRejection{err: fmt.Errorf("server rejected extended hello: %s", protocolError.Code), cacheable: true}
			}
			return fmt.Errorf("server rejected connector hello: %s", protocolError.Code)
		}
		return errors.New("expected server welcome")
	}
	if protocol.ValidateDirection(protocol.ControlToConnector, "server.welcome") != nil || welcome.SelectedProtocol < hello.MinProtocol || welcome.SelectedProtocol > hello.MaxProtocol {
		return errors.New("server selected unsupported protocol")
	}
	if welcome.HostID != d.cfg.HostID {
		return errors.New("certificate host identity mismatch")
	}
	for _, accepted := range welcome.AcceptedCapabilities {
		if !protocol.HasCapability(hello.Capabilities, accepted) {
			return errors.New("server accepted an unoffered capability")
		}
	}
	if welcomed != nil {
		welcomed()
	}
	if inventory, ok := d.negotiatedInventory(welcome.AcceptedCapabilities); ok {
		b, marshalErr := protocol.MarshalEnvelope(1, "state.inventory", inventory)
		if marshalErr != nil {
			return marshalErr
		}
		wctx, writeCancel := context.WithTimeout(ctx, 10*time.Second)
		writeErr := conn.Write(wctx, websocket.MessageText, b)
		writeCancel()
		if writeErr != nil {
			return writeErr
		}
	}
	q := NewQueue(256)
	for _, engine := range d.engines {
		engine.BeginConnection()
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer func() {
		for _, engine := range d.engines {
			engine.Stop()
		}
	}()
	errc := make(chan error, len(d.engines)*3+3)
	go func() {
		for {
			b, err := q.Next(runCtx)
			if err != nil {
				errc <- err
				return
			}
			wctx, c := context.WithTimeout(runCtx, 10*time.Second)
			err = conn.Write(wctx, websocket.MessageText, b)
			c()
			if err != nil {
				errc <- err
				return
			}
		}
	}()
	for _, engine := range d.engines {
		engine := engine
		go func() {
			errc <- engine.RunReconciliation(
				runCtx,
				func(s protocol.InstanceSnapshot) error {
					b, err := protocol.MarshalEnvelope(1, "state.snapshot", s)
					if err != nil {
						return err
					}
					c, cancel := context.WithTimeout(runCtx, 2*time.Second)
					defer cancel()
					return q.Put(c, b)
				},
				func(delta protocol.StateDelta) error {
					b, err := protocol.MarshalEnvelope(1, "state.delta", delta)
					if err != nil {
						return err
					}
					c, cancel := context.WithTimeout(runCtx, 2*time.Second)
					defer cancel()
					return q.Put(c, b)
				},
			)
		}()
		go func() {
			t := time.NewTicker(time.Second)
			defer t.Stop()
			for {
				select {
				case <-runCtx.Done():
					return
				case <-t.C:
					prompts, err := engine.pollPromptsAndPublish(runCtx)
					if err != nil {
						var publishErr *statePublishError
						if errors.As(err, &publishErr) {
							errc <- err
							return
						}
						continue
					}
					for _, p := range prompts {
						b, marshalErr := protocol.MarshalEnvelope(1, "prompt.snapshot", p)
						if marshalErr != nil {
							errc <- marshalErr
							return
						}
						c, cancel := context.WithTimeout(runCtx, 2*time.Second)
						if q.Put(c, b) != nil {
							cancel()
							errc <- connectorQueueError{}
							return
						}
						cancel()
					}
				}
			}
		}()
	}
	go func() {
		t := time.NewTicker(time.Duration(welcome.HeartbeatIntervalMS) * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-runCtx.Done():
				errc <- runCtx.Err()
				return
			case <-t.C:
				pctx, c := context.WithTimeout(runCtx, 10*time.Second)
				err := conn.Ping(pctx)
				c()
				if err != nil {
					errc <- err
					return
				}
			}
		}
	}()
	var malformed protocol.MalformedTracker
	outputOwners := map[string]*Engine{}
	for {
		select {
		case err := <-errc:
			return err
		default:
		}
		_, frame, err := conn.Read(runCtx)
		if err != nil {
			return err
		}
		env, msg, err := protocol.DecodeStrict(frame, "connector")
		directionErr := protocol.ValidateDirection(protocol.ControlToConnector, env.Type)
		if err != nil || directionErr != nil {
			code := "INVALID_MESSAGE"
			if directionErr != nil || (env.Type != "" && err != nil) {
				code = "UNSUPPORTED_MESSAGE"
			}
			var reply *string
			if protocol.IsUUIDv7(env.MessageID) {
				value := env.MessageID
				reply = &value
			}
			body := protocol.ProtocolError{InReplyTo: reply, Code: code, Message: "server message rejected"}
			b, _ := protocol.MarshalEnvelope(1, "protocol.error", body)
			qctx, cancel := context.WithTimeout(runCtx, 2*time.Second)
			queueErr := q.Put(qctx, b)
			cancel()
			if queueErr != nil {
				return queueErr
			}
			if malformed.Add(time.Now()) {
				return errors.New("malformed message threshold reached")
			}
			continue
		}
		switch m := msg.(type) {
		case *protocol.ActionRequest:
			engine := d.engines[m.Target.InstanceID]
			if engine == nil {
				return errors.New("action names unknown instance")
			}
			go func() {
				if err := d.handleAction(runCtx, q, engine, *m); err != nil {
					select {
					case errc <- err:
					default:
					}
				}
			}()
		case *protocol.OutputSubscribe:
			if len(outputOwners) >= 4 {
				continue
			}
			if _, exists := outputOwners[m.SubscriptionID]; exists {
				continue
			}
			engine := d.engines[m.Target.InstanceID]
			if engine == nil {
				return errors.New("output names unknown instance")
			}
			if err := engine.StartOutput(runCtx, *m, func(o protocol.OutputSnapshot) error {
				b, err := protocol.MarshalEnvelope(1, "output.snapshot", o)
				if err == nil {
					q.ReplaceOutput(o.SubscriptionID, b)
				}
				return err
			}); err != nil {
				// Output is a transient view. A stale or duplicate view request
				// must not tear down host state or checked actions.
				continue
			}
			outputOwners[m.SubscriptionID] = engine
		case *protocol.OutputUnsubscribe:
			if engine := outputOwners[m.SubscriptionID]; engine != nil {
				engine.StopOutput(m.SubscriptionID)
				delete(outputOwners, m.SubscriptionID)
			}
		case *protocol.ConnectorStateResync:
			engine := d.engines[m.InstanceID]
			if engine == nil {
				return errors.New("resync names unknown instance")
			}
			if err := engine.forceReconcileAndPublish(runCtx); err != nil {
				return err
			}
		}
	}
}

func priorStrictCapabilityClose(err error) bool {
	var closeError websocket.CloseError
	return errors.As(err, &closeError) && closeError.Code == priorStrictCapabilityCloseCode && closeError.Reason == priorStrictCapabilityCloseReason
}

func ambiguousPreWelcomeEOF(ctx context.Context, err error) bool {
	if ctx.Err() != nil || websocket.CloseStatus(err) != -1 {
		return false
	}
	var timeout interface{ Timeout() bool }
	if errors.As(err, &timeout) && timeout.Timeout() {
		return false
	}
	return errors.Is(err, io.EOF) || strings.HasSuffix(err.Error(), ": EOF")
}

func priorStrictCapabilityProtocolError(protocolError protocol.ProtocolError) bool {
	return protocolError.Code == priorStrictCapabilityErrorCode && protocolError.Message == priorStrictCapabilityErrorMessage
}

func compatibilityEndpoint(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	return parsed.String()
}

func (d *Daemon) negotiatedInventory(accepted []string) (protocol.InstanceInventory, bool) {
	if !protocol.HasCapability(accepted, protocol.StateInventoryCapability) {
		return protocol.InstanceInventory{}, false
	}
	instanceIDs := make([]string, 0, len(d.engines))
	for instanceID := range d.engines {
		instanceIDs = append(instanceIDs, instanceID)
	}
	slices.Sort(instanceIDs)
	return protocol.InstanceInventory{InstanceIDs: instanceIDs}, true
}

func (d *Daemon) handleAction(ctx context.Context, q *Queue, engine *Engine, a protocol.ActionRequest) error {
	var enqueueErr error
	r := engine.HandleActionWithReceipt(ctx, a, func() bool {
		b, err := protocol.MarshalEnvelope(1, "action.received", protocol.ActionReceived{ActionID: a.ActionID})
		if err != nil {
			enqueueErr = err
			return false
		}
		c, cancel := context.WithTimeout(ctx, 2*time.Second)
		err = q.Put(c, b)
		cancel()
		enqueueErr = err
		return err == nil
	})
	if enqueueErr != nil {
		return enqueueErr
	}
	b, err := protocol.MarshalEnvelope(1, "action.result", r.Result)
	if err != nil {
		return err
	}
	c, cancel := context.WithTimeout(ctx, 2*time.Second)
	err = q.Put(c, b)
	cancel()
	return err
}

func tlsClient(c DaemonConfig) (*http.Client, error) {
	cert, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
	if err != nil {
		return nil, err
	}
	pem, err := os.ReadFile(c.CAFile)
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		return nil, errors.New("invalid server CA file")
	}
	tr := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}, RootCAs: roots}}
	return &http.Client{Transport: tr, Timeout: 0}, nil
}
func safeError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, io.EOF) {
		return "connection closed"
	}
	return fmt.Sprintf("%T", err)
}

type connectorQueueError struct{}

func (connectorQueueError) Error() string { return "connector priority queue unavailable" }
