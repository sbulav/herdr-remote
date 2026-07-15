package connector

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/dcolinmorgan/herdr-remote/internal/protocol"
)

func TestConnectorUsesEphemeralFallbackForActualPriorStrictEOF(t *testing.T) {
	var mu sync.Mutex
	var attempts int
	var offered [][]string
	var applicationTypes []string
	var priorRejections int
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
		if err != nil {
			return
		}
		defer conn.CloseNow()
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		_, frame, err := conn.Read(ctx)
		if err != nil {
			return
		}
		hello, err := decodePriorStrictHello(frame)
		mu.Lock()
		attempts++
		offered = append(offered, append([]string(nil), hello.Capabilities...))
		if err != nil {
			priorRejections++
		}
		mu.Unlock()
		if err != nil {
			return
		}
		welcome := protocol.Welcome{SelectedProtocol: 1, ServerMinProtocol: 1, ServerMaxProtocol: 1, AcceptedCapabilities: hello.Capabilities, ConnectionID: mustID(), HostID: testHost, HeartbeatIntervalMS: 20000, MaxMessageBytes: protocol.MaxFrameBytes, ServerTime: time.Now().UTC().Format(time.RFC3339Nano)}
		response, err := protocol.MarshalEnvelope(0, "server.welcome", welcome)
		if err != nil || conn.Write(ctx, websocket.MessageText, response) != nil {
			return
		}
		_, frame, err = conn.Read(ctx)
		if err != nil {
			return
		}
		envelope, _, err := protocol.DecodeStrict(frame, "connector")
		if err != nil {
			return
		}
		mu.Lock()
		applicationTypes = append(applicationTypes, envelope.Type)
		mu.Unlock()
	})
	server := httptest.NewTLSServer(handler)
	defer server.Close()
	var logs bytes.Buffer
	daemon := integrationDaemon(t, server, slog.New(slog.NewTextHandler(&logs, nil)))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = daemon.runConnection(ctx)
	_ = daemon.runConnection(ctx)

	mu.Lock()
	defer mu.Unlock()
	if attempts != 4 || priorRejections != 2 {
		t.Fatalf("attempts=%d prior strict rejections=%d", attempts, priorRejections)
	}
	for i, capabilities := range offered {
		if i%2 == 0 && !protocol.HasCapability(capabilities, protocol.StateInventoryCapability) {
			t.Fatalf("extended attempt %d capabilities = %v", i+1, capabilities)
		}
		if i%2 == 1 && !slices.Equal(capabilities, baselineV1Capabilities) {
			t.Fatalf("ephemeral baseline attempt %d capabilities = %v", i+1, capabilities)
		}
	}
	if !slices.Equal(applicationTypes, []string{"state.snapshot", "state.snapshot"}) {
		t.Fatalf("baseline application messages = %v", applicationTypes)
	}
	endpoint := compatibilityEndpoint(daemon.cfg.URL)
	daemon.compatibilityMu.RLock()
	_, cached := daemon.compatibilityEndpoints[endpoint]
	daemon.compatibilityMu.RUnlock()
	if cached {
		t.Fatal("ambiguous EOF cached compatibility mode")
	}
	if strings.Count(logs.String(), "ephemeral baseline v1 compatibility") != 2 || strings.Contains(logs.String(), "requires baseline v1 compatibility") {
		t.Fatalf("ephemeral compatibility logging = %s", logs.String())
	}
}

func TestExplicitStrictRejectionCachesAfterBaselineWelcome(t *testing.T) {
	var attempts atomic.Int32
	var applicationMessages atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		_, frame, err := conn.Read(ctx)
		if err != nil {
			return
		}
		_, decoded, err := protocol.DecodeStrict(frame, "connector")
		if err != nil {
			return
		}
		hello := decoded.(*protocol.Hello)
		attempts.Add(1)
		if protocol.HasCapability(hello.Capabilities, protocol.StateInventoryCapability) {
			body := protocol.ProtocolError{Code: priorStrictCapabilityErrorCode, Message: priorStrictCapabilityErrorMessage}
			response, _ := protocol.MarshalEnvelope(0, "protocol.error", body)
			_ = conn.Write(ctx, websocket.MessageText, response)
			return
		}
		welcome := protocol.Welcome{SelectedProtocol: 1, ServerMinProtocol: 1, ServerMaxProtocol: 1, AcceptedCapabilities: hello.Capabilities, ConnectionID: mustID(), HostID: testHost, HeartbeatIntervalMS: 20000, MaxMessageBytes: protocol.MaxFrameBytes, ServerTime: time.Now().UTC().Format(time.RFC3339Nano)}
		response, _ := protocol.MarshalEnvelope(0, "server.welcome", welcome)
		if conn.Write(ctx, websocket.MessageText, response) != nil {
			return
		}
		if _, _, err := conn.Read(ctx); err == nil {
			applicationMessages.Add(1)
		}
	})
	server := httptest.NewTLSServer(handler)
	defer server.Close()
	var logs bytes.Buffer
	daemon := integrationDaemon(t, server, slog.New(slog.NewTextHandler(&logs, nil)))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = daemon.runConnection(ctx)
	_ = daemon.runConnection(ctx)
	if attempts.Load() != 3 || applicationMessages.Load() != 2 || !daemon.compatibilityMode() {
		t.Fatalf("attempts=%d messages=%d compatibility=%t", attempts.Load(), applicationMessages.Load(), daemon.compatibilityMode())
	}
	if strings.Count(logs.String(), "requires baseline v1 compatibility") != 1 {
		t.Fatalf("explicit compatibility logging = %s", logs.String())
	}
}

func TestPriorStrictCapabilityEvidenceMustMatchExactly(t *testing.T) {
	if !priorStrictCapabilityProtocolError(protocol.ProtocolError{Code: priorStrictCapabilityErrorCode, Message: priorStrictCapabilityErrorMessage}) {
		t.Fatal("exact prior protocol error was not recognized")
	}
	if priorStrictCapabilityProtocolError(protocol.ProtocolError{Code: priorStrictCapabilityErrorCode, Message: "invalid hello"}) {
		t.Fatal("non-specific protocol error was accepted")
	}
	if !priorStrictCapabilityClose(websocket.CloseError{Code: priorStrictCapabilityCloseCode, Reason: priorStrictCapabilityCloseReason}) {
		t.Fatal("exact prior close was not recognized")
	}
	if priorStrictCapabilityClose(websocket.CloseError{Code: priorStrictCapabilityCloseCode, Reason: "server restarting"}) {
		t.Fatal("non-specific close was accepted")
	}
}

func TestConnectorDoesNotFallbackOnNonEOFPreWelcomeFailures(t *testing.T) {
	tests := []struct {
		name  string
		close func(*websocket.Conn, context.Context)
	}{
		{name: "server-restart", close: func(conn *websocket.Conn, ctx context.Context) {
			_ = conn.Close(websocket.StatusGoingAway, "server restarting")
		}},
		{name: "internal-failure", close: func(conn *websocket.Conn, ctx context.Context) {
			_ = conn.Close(websocket.StatusInternalError, "internal failure")
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var attempts atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					return
				}
				ctx, cancel := context.WithTimeout(r.Context(), time.Second)
				defer cancel()
				if _, _, err := conn.Read(ctx); err != nil {
					_ = conn.CloseNow()
					return
				}
				test.close(conn, ctx)
			}))
			defer server.Close()
			daemon := integrationDaemon(t, server, slog.New(slog.NewTextHandler(io.Discard, nil)))
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := daemon.runConnection(ctx); err == nil {
				t.Fatal("pre-welcome disconnect unexpectedly succeeded")
			}
			if attempts.Load() != 1 || daemon.compatibilityMode() {
				t.Fatalf("ambiguous disconnect triggered fallback: attempts=%d compatibility=%t", attempts.Load(), daemon.compatibilityMode())
			}
		})
	}
	for _, name := range []string{"network-timeout", "cancellation"} {
		t.Run(name, func(t *testing.T) {
			var attempts atomic.Int32
			helloRead := make(chan struct{})
			release := make(chan struct{})
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer conn.CloseNow()
				if _, _, err := conn.Read(context.Background()); err != nil {
					return
				}
				close(helloRead)
				<-release
			}))
			defer server.Close()
			daemon := integrationDaemon(t, server, slog.New(slog.NewTextHandler(io.Discard, nil)))
			var ctx context.Context
			var cancel context.CancelFunc
			if name == "network-timeout" {
				ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
			} else {
				ctx, cancel = context.WithCancel(context.Background())
				go func() {
					<-helloRead
					cancel()
				}()
			}
			err := daemon.runConnection(ctx)
			cancel()
			close(release)
			if err == nil {
				t.Fatal("blocked pre-welcome connection unexpectedly succeeded")
			}
			if attempts.Load() != 1 || daemon.compatibilityMode() {
				t.Fatalf("%s triggered fallback: attempts=%d compatibility=%t", name, attempts.Load(), daemon.compatibilityMode())
			}
		})
	}
}

func TestFailedBaselineRetryIsNotCached(t *testing.T) {
	var mu sync.Mutex
	var offered [][]string
	var attempts int
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		_, frame, err := conn.Read(ctx)
		if err != nil {
			return
		}
		_, decoded, err := protocol.DecodeStrict(frame, "connector")
		if err != nil {
			return
		}
		hello := decoded.(*protocol.Hello)
		mu.Lock()
		attempts++
		attempt := attempts
		offered = append(offered, append([]string(nil), hello.Capabilities...))
		mu.Unlock()
		if attempt == 1 {
			body := protocol.ProtocolError{Code: priorStrictCapabilityErrorCode, Message: priorStrictCapabilityErrorMessage}
			response, _ := protocol.MarshalEnvelope(0, "protocol.error", body)
			_ = conn.Write(ctx, websocket.MessageText, response)
		}
	})
	server := httptest.NewTLSServer(handler)
	defer server.Close()
	var logs bytes.Buffer
	daemon := integrationDaemon(t, server, slog.New(slog.NewTextHandler(&logs, nil)))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = daemon.runConnection(ctx)
	if daemon.compatibilityMode() {
		t.Fatal("failed baseline retry was cached")
	}
	_ = daemon.runConnection(ctx)
	if daemon.compatibilityMode() {
		t.Fatal("repeated failed ephemeral baseline retry was cached")
	}
	mu.Lock()
	defer mu.Unlock()
	if attempts != 4 {
		t.Fatalf("attempts after failed baseline retry = %d", attempts)
	}
	if !protocol.HasCapability(offered[0], protocol.StateInventoryCapability) || protocol.HasCapability(offered[1], protocol.StateInventoryCapability) || !protocol.HasCapability(offered[2], protocol.StateInventoryCapability) || protocol.HasCapability(offered[3], protocol.StateInventoryCapability) {
		t.Fatalf("extended/baseline/extended/baseline offers = %v", offered)
	}
	if strings.Contains(logs.String(), "requires baseline v1 compatibility") {
		t.Fatalf("failed baseline mode was logged as enabled: %s", logs.String())
	}
}

func TestConnectorDoesNotDowngradeOnAuthenticationOrProtocolMajorErrors(t *testing.T) {
	t.Run("authentication", func(t *testing.T) {
		var requests atomic.Int32
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			requests.Add(1)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		}))
		defer server.Close()
		daemon := integrationDaemon(t, server, slog.New(slog.NewTextHandler(io.Discard, nil)))
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := daemon.runConnection(ctx); err == nil {
			t.Fatal("authentication rejection unexpectedly succeeded")
		}
		if requests.Load() != 1 || daemon.compatibilityMode() {
			t.Fatalf("authentication rejection downgraded: requests=%d compatibility=%t", requests.Load(), daemon.compatibilityMode())
		}
	})

	t.Run("protocol-major", func(t *testing.T) {
		var attempts atomic.Int32
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			attempts.Add(1)
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer conn.CloseNow()
			ctx, cancel := context.WithTimeout(r.Context(), time.Second)
			defer cancel()
			if _, _, err := conn.Read(ctx); err != nil {
				return
			}
			body := protocol.ProtocolError{Code: "UNSUPPORTED_PROTOCOL", Message: "unsupported protocol"}
			frame, _ := protocol.MarshalEnvelope(0, "protocol.error", body)
			_ = conn.Write(ctx, websocket.MessageText, frame)
		}))
		defer server.Close()
		daemon := integrationDaemon(t, server, slog.New(slog.NewTextHandler(io.Discard, nil)))
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := daemon.runConnection(ctx); err == nil {
			t.Fatal("protocol-major rejection unexpectedly succeeded")
		}
		if attempts.Load() != 1 || daemon.compatibilityMode() {
			t.Fatalf("protocol-major rejection downgraded: attempts=%d compatibility=%t", attempts.Load(), daemon.compatibilityMode())
		}
	})
}

func (d *Daemon) compatibilityMode() bool {
	d.compatibilityMu.RLock()
	defer d.compatibilityMu.RUnlock()
	_, ok := d.compatibilityEndpoints[compatibilityEndpoint(d.cfg.URL)]
	return ok
}

func integrationDaemon(t *testing.T, server *httptest.Server, logger *slog.Logger) *Daemon {
	t.Helper()
	certificate := server.TLS.Certificates[0]
	keyDER, err := x509.MarshalPKCS8PrivateKey(certificate.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile := filepath.Join(dir, "client.pem")
	keyFile := filepath.Join(dir, "client-key.pem")
	caFile := filepath.Join(dir, "ca.pem")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Certificate[0]})
	if err := os.WriteFile(certFile, certPEM, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(caFile, certPEM, 0600); err != nil {
		t.Fatal(err)
	}
	engine, err := NewEngine(Config{HostID: testHost, InstanceID: "default", ReconcileInterval: time.Hour}, &fakeLocal{version: "0.7.3"})
	if err != nil {
		t.Fatal(err)
	}
	daemon, err := NewDaemon(DaemonConfig{URL: strings.Replace(server.URL, "https://", "wss://", 1), HostID: testHost, DisplayName: "host", Version: "0.1.0", CertFile: certFile, KeyFile: keyFile, CAFile: caFile, Logger: logger}, engine)
	if err != nil {
		t.Fatal(err)
	}
	return daemon
}

func decodePriorStrictHello(frame []byte) (protocol.Hello, error) {
	var envelope protocol.Envelope
	if err := priorStrictDecode(frame, &envelope); err != nil {
		return protocol.Hello{}, err
	}
	if envelope.Protocol != 0 || envelope.Type != "connector.hello" {
		return protocol.Hello{}, errors.New("invalid bootstrap envelope")
	}
	var hello protocol.Hello
	if err := priorStrictDecode(envelope.Body, &hello); err != nil {
		return protocol.Hello{}, err
	}
	priorCapabilities := map[string]bool{"read.v1": true, "output.subscribe.v1": true, "prompt.snapshot.v1": true, "checked_input.v1": true, "prompt.respond.v1": true}
	seen := map[string]bool{}
	for _, capability := range hello.Capabilities {
		if !priorCapabilities[capability] || seen[capability] {
			return hello, errors.New("invalid or duplicate capability")
		}
		seen[capability] = true
	}
	return hello, nil
}

func priorStrictDecode(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON")
	}
	return nil
}
