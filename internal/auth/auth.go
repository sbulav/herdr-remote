// Package auth defines the reverse-proxy trust and opaque browser session
// boundary. OIDC tokens are never exposed to browser JavaScript or stored here.
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

type Identity struct{ Issuer, Audience, Subject, Assurance string }
type HeaderConfig struct{ Issuer, Audience, Subject, Assurance string }
type ProxyConfig struct {
	CIDRs    []string
	Headers  HeaderConfig
	Expected Identity
}
type Proxy struct {
	nets []*net.IPNet
	cfg  ProxyConfig
}
type identityKey struct{}

func NewProxy(c ProxyConfig) (*Proxy, error) {
	if c.Headers.Issuer == "" || c.Headers.Audience == "" || c.Headers.Subject == "" || c.Headers.Assurance == "" {
		return nil, errors.New("all identity headers are required")
	}
	p := &Proxy{cfg: c}
	for _, raw := range c.CIDRs {
		_, n, err := net.ParseCIDR(raw)
		if err != nil {
			return nil, err
		}
		p.nets = append(p.nets, n)
	}
	if len(p.nets) == 0 {
		return nil, errors.New("trusted proxy CIDRs required")
	}
	return p, nil
}
func (p *Proxy) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		ip := net.ParseIP(host)
		trusted := ip != nil && (ip.IsLoopback())
		for _, n := range p.nets {
			if n.Contains(ip) {
				trusted = true
				break
			}
		}
		if !trusted {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		one := func(name string) (string, bool) {
			values := r.Header.Values(name)
			return first(values), len(values) == 1 && values[0] != ""
		}
		issuer, ok1 := one(p.cfg.Headers.Issuer)
		audience, ok2 := one(p.cfg.Headers.Audience)
		subject, ok3 := one(p.cfg.Headers.Subject)
		assurance, ok4 := one(p.cfg.Headers.Assurance)
		id := Identity{issuer, audience, subject, assurance}
		if !ok1 || !ok2 || !ok3 || !ok4 || id != p.cfg.Expected {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), identityKey{}, id)))
	})
}
func IdentityFrom(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(identityKey{}).(Identity)
	return id, ok
}
func first(v []string) string {
	if len(v) == 0 {
		return ""
	}
	return v[0]
}

type Session struct {
	ID, CSRF          string
	Identity          Identity
	Created, LastSeen time.Time
}
type Sessions struct {
	mu         sync.Mutex
	items      map[string]Session
	now        func() time.Time
	CookieName string
	secret     []byte
}

func NewSessions(secretFile string) (*Sessions, error) {
	b, err := os.ReadFile(secretFile)
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(b))) < 32 {
		return nil, errors.New("session secret file must contain at least 32 bytes")
	}
	return &Sessions{items: map[string]Session{}, now: time.Now, CookieName: "__Host-herdr_session", secret: append([]byte(nil), b...)}, nil
}

// NewTestSessions avoids filesystem secrets in unit tests.
func NewTestSessions() *Sessions {
	return &Sessions{items: map[string]Session{}, now: time.Now, CookieName: "__Host-herdr_session", secret: []byte("test-session-secret-not-for-production")}
}
func (s *Sessions) Issue(w http.ResponseWriter, id Identity) (Session, error) {
	nonce, err := random(32)
	if err != nil {
		return Session{}, err
	}
	mac := hmac.New(sha256.New, s.secret)
	_, _ = mac.Write([]byte(nonce))
	sid := nonce + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	csrf, err := random(32)
	if err != nil {
		return Session{}, err
	}
	now := s.now().UTC()
	session := Session{ID: sid, CSRF: csrf, Identity: id, Created: now, LastSeen: now}
	s.mu.Lock()
	s.items[sid] = session
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: s.CookieName, Value: sid, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 8 * 60 * 60})
	return session, nil
}
func (s *Sessions) Get(r *http.Request) (Session, error) {
	cookie, err := r.Cookie(s.CookieName)
	if err != nil {
		return Session{}, err
	}
	now := s.now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.items[cookie.Value]
	if !ok || now.Sub(session.LastSeen) > 30*time.Minute || now.Sub(session.Created) > 8*time.Hour {
		delete(s.items, cookie.Value)
		return Session{}, errors.New("session expired")
	}
	id, ok := IdentityFrom(r.Context())
	if !ok || id != session.Identity {
		return Session{}, errors.New("identity changed")
	}
	session.LastSeen = now
	s.items[cookie.Value] = session
	return session, nil
}
func (s *Sessions) Check(session Session) error {
	now := s.now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.items[session.ID]
	if !ok || current.Identity != session.Identity || now.Sub(current.LastSeen) > 30*time.Minute || now.Sub(current.Created) > 8*time.Hour {
		delete(s.items, session.ID)
		return errors.New("session expired")
	}
	current.LastSeen = now
	s.items[session.ID] = current
	return nil
}
func (s *Sessions) RequireCSRF(r *http.Request, session Session) bool {
	got := r.Header.Get("X-CSRF-Token")
	return len(got) == len(session.CSRF) && subtle.ConstantTimeCompare([]byte(got), []byte(session.CSRF)) == 1
}
func (s *Sessions) Handler(w http.ResponseWriter, r *http.Request) {
	id, ok := IdentityFrom(r.Context())
	if !ok {
		http.Error(w, "unauthorized", 401)
		return
	}
	session, err := s.Issue(w, id)
	if err != nil {
		http.Error(w, "internal", 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]string{"csrf_token": session.CSRF})
}
func random(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
