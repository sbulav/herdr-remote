package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/dcolinmorgan/herdr-remote/internal/auth"
	"github.com/dcolinmorgan/herdr-remote/internal/protocol"
)

func TestLogoutImmediatelyRevokesBoundBrowserSocket(t *testing.T) {
	server, _, st := testServer(t)
	defer st.Close()
	server.cfg.SessionCheckInterval = time.Hour
	httpServer := httptest.NewServer(server.BrowserHandler())
	defer httpServer.Close()
	conn, cookie, csrf, _ := openBrowserSocket(t, httpServer)
	defer conn.CloseNow()
	second, _ := dialBrowserSocket(t, httpServer, cookie)
	defer second.CloseNow()
	request, err := http.NewRequest(http.MethodPost, httpServer.URL+"/auth/logout", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	setProxyHeaders(request.Header)
	request.AddCookie(cookie)
	request.Header.Set("Origin", "https://app.example")
	request.Header.Set("X-CSRF-Token", csrf)
	response, err := httpServer.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("logout status = %d", response.StatusCode)
	}
	readCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, _, err = conn.Read(readCtx)
	if err == nil || readCtx.Err() != nil || websocket.CloseStatus(err) != browserUnauthorizedCloseCode {
		t.Fatalf("socket not revoked by logout: %v", err)
	}
	_, _, err = second.Read(readCtx)
	if err == nil || readCtx.Err() != nil || websocket.CloseStatus(err) != browserUnauthorizedCloseCode {
		t.Fatalf("second socket not revoked by logout: %v", err)
	}
}

func TestBrowserSocketExpiresWithoutInboundMessages(t *testing.T) {
	for _, test := range []struct {
		name      string
		advance   time.Duration
		broadcast bool
	}{{"idle", 31 * time.Minute, false}, {"idle-with-state-broadcast", 31 * time.Minute, true}, {"absolute", 8*time.Hour + time.Second, false}} {
		t.Run(test.name, func(t *testing.T) {
			var clock atomic.Int64
			clock.Store(time.Now().UTC().UnixNano())
			server, hub, st := testServer(t)
			defer st.Close()
			server.cfg.Sessions = auth.NewTestSessionsWithClock(func() time.Time { return time.Unix(0, clock.Load()).UTC() })
			server.cfg.SessionCheckInterval = 5 * time.Millisecond
			httpServer := httptest.NewServer(server.BrowserHandler())
			defer httpServer.Close()
			conn, _, _, _ := openBrowserSocket(t, httpServer)
			defer conn.CloseNow()
			clock.Add(int64(test.advance))
			if test.broadcast {
				hub.notify("state.delta", StateEvent{Changes: []protocol.StateChange{{Operation: "host.upsert", HostID: "019f64ca-1000-7000-8000-000000000002", Host: &protocol.HostState{DisplayName: "host", Status: "connected"}}}})
			}
			readCtx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_, _, err := conn.Read(readCtx)
			if err == nil || readCtx.Err() != nil || websocket.CloseStatus(err) != browserUnauthorizedCloseCode {
				t.Fatalf("socket did not expire without messages: %v", err)
			}
		})
	}
}

func TestEveryInboundBrowserMessageRequiresCurrentSession(t *testing.T) {
	for _, messageType := range []string{"output.subscribe", "output.unsubscribe", "state.resync", "action.request"} {
		t.Run(messageType, func(t *testing.T) {
			var clock atomic.Int64
			clock.Store(time.Now().UTC().UnixNano())
			server, _, st := testServer(t)
			defer st.Close()
			server.cfg.Sessions = auth.NewTestSessionsWithClock(func() time.Time { return time.Unix(0, clock.Load()).UTC() })
			server.cfg.SessionCheckInterval = time.Hour
			httpServer := httptest.NewServer(server.BrowserHandler())
			defer httpServer.Close()
			conn, _, _, snapshot := openBrowserSocket(t, httpServer)
			defer conn.CloseNow()
			clock.Add(int64(31 * time.Minute))
			body := inboundBrowserBody(messageType, snapshot)
			frame, err := protocol.MarshalEnvelope(1, messageType, body)
			if err != nil {
				t.Fatal(err)
			}
			writeCtx, cancelWrite := context.WithTimeout(context.Background(), time.Second)
			err = conn.Write(writeCtx, websocket.MessageText, frame)
			cancelWrite()
			if err != nil {
				t.Fatal(err)
			}
			readCtx, cancelRead := context.WithTimeout(context.Background(), time.Second)
			defer cancelRead()
			_, _, err = conn.Read(readCtx)
			if err == nil || readCtx.Err() != nil || websocket.CloseStatus(err) != browserUnauthorizedCloseCode {
				t.Fatalf("expired session remained open for %s: %v", messageType, err)
			}
		})
	}
}

func openBrowserSocket(t *testing.T, server *httptest.Server) (*websocket.Conn, *http.Cookie, string, protocol.SessionSnapshot) {
	t.Helper()
	request, _ := http.NewRequest(http.MethodGet, server.URL+"/api/v1/session", nil)
	setProxyHeaders(request.Header)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	cookie := response.Cookies()[0]
	csrfRequest, _ := http.NewRequest(http.MethodGet, server.URL+"/api/v1/csrf", nil)
	setProxyHeaders(csrfRequest.Header)
	csrfRequest.AddCookie(cookie)
	csrfResponse, err := server.Client().Do(csrfRequest)
	if err != nil {
		t.Fatal(err)
	}
	var csrfBody struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(csrfResponse.Body).Decode(&csrfBody); err != nil {
		t.Fatal(err)
	}
	csrfResponse.Body.Close()
	conn, snapshot := dialBrowserSocket(t, server, cookie)
	return conn, cookie, csrfBody.Token, snapshot
}

func dialBrowserSocket(t *testing.T, server *httptest.Server, cookie *http.Cookie) (*websocket.Conn, protocol.SessionSnapshot) {
	t.Helper()
	header := http.Header{}
	setProxyHeaders(header)
	header.Set("Origin", "https://app.example")
	header.Set("Cookie", cookie.String())
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, strings.Replace(server.URL, "http://", "ws://", 1)+"/v1/browser/ws", &websocket.DialOptions{HTTPHeader: header, CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		t.Fatal(err)
	}
	_, frame, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, decoded, err := protocol.DecodeStrict(frame, "browser")
	if err != nil {
		t.Fatal(err)
	}
	return conn, *decoded.(*protocol.SessionSnapshot)
}

func setProxyHeaders(header http.Header) {
	header.Set("X-Issuer", "issuer")
	header.Set("X-Audience", "audience")
	header.Set("X-Subject", "operator")
	header.Set("X-MFA", "mfa")
}

func inboundBrowserBody(messageType string, snapshot protocol.SessionSnapshot) any {
	target := protocol.Target{HostID: "019f64ca-1000-7000-8000-000000000002", InstanceID: "default", TerminalID: "term"}
	switch messageType {
	case "output.subscribe":
		return protocol.OutputSubscribe{SessionID: snapshot.SessionID, SubscriptionID: mustTestID(), Target: target, Source: "recent", Lines: 10, PollIntervalMS: 1000}
	case "output.unsubscribe":
		return protocol.OutputUnsubscribe{SessionID: snapshot.SessionID, SubscriptionID: mustTestID()}
	case "state.resync":
		epoch := snapshot.StateEpoch
		sequence := uint64(1)
		return protocol.StateResync{SessionID: snapshot.SessionID, ExpectedEpoch: &epoch, ExpectedSequence: &sequence, Reason: "operator_refresh"}
	default:
		lines := 10
		return protocol.BrowserActionRequest{SessionID: snapshot.SessionID, ActionRequest: protocol.ActionRequest{ActionID: mustTestID(), Target: target, TimeoutMS: 1000, Expected: protocol.Expected{StateEpoch: snapshot.StateEpoch, ConnectorEpoch: mustTestID(), AgentGeneration: 1, Agent: "opencode", Statuses: []string{"blocked"}}, Operation: protocol.Operation{Type: "agent.read", Source: "recent", Lines: &lines}}}
	}
}
