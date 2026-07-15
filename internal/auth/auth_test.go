package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProxyIdentityAndSourceAreExact(t *testing.T) {
	cfg := ProxyConfig{CIDRs: []string{"10.0.0.0/8"}, Headers: HeaderConfig{Issuer: "X-Issuer", Audience: "X-Audience", Subject: "X-Subject", Assurance: "X-MFA"}, Expected: Identity{"issuer", "audience", "operator", "mfa"}}
	p, err := NewProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	handler := p.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	request := func(remote, subject, origin string) int {
		r := httptest.NewRequest("GET", "https://app.example/", nil)
		r.RemoteAddr = remote
		r.Header.Set("X-Issuer", "issuer")
		r.Header.Set("X-Audience", "audience")
		r.Header.Set("X-Subject", subject)
		r.Header.Set("X-MFA", "mfa")
		r.Header.Set("Origin", origin)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Code
	}
	if got := request("10.1.2.3:1234", "operator", "https://app.example"); got != 204 {
		t.Fatalf("trusted identity rejected: %d", got)
	}
	if got := request("192.0.2.1:1234", "operator", ""); got != 401 {
		t.Fatalf("untrusted source accepted: %d", got)
	}
	if got := request("10.1.2.3:1234", "other", ""); got != 401 {
		t.Fatalf("wrong subject accepted: %d", got)
	}
}
