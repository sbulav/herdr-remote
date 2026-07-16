// Package push sends generic wake-up-only Web Push notifications. Payloads are
// deliberately incapable of carrying host, terminal, prompt, or action data.
package push

import (
	"context"
	"crypto/elliptic"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
	"github.com/dcolinmorgan/herdr-remote/internal/protocol"
	"github.com/dcolinmorgan/herdr-remote/internal/pushendpoint"
	"github.com/dcolinmorgan/herdr-remote/internal/store"
)

type Outcome int

const (
	Sent Outcome = iota
	Gone
	Retry
	PermanentFailure
)

type Sender interface {
	Send(context.Context, store.PushSubscription, Event) (Outcome, error)
}
type Event struct {
	EventID string `json:"event_id"`
	Kind    string `json:"kind"`
}

func (e Event) Validate() error {
	if !protocol.IsUUIDv7(e.EventID) {
		return errors.New("invalid push event ID")
	}
	if e.Kind != "agent_state_changed" && e.Kind != "connector_state_changed" && e.Kind != "test" {
		return errors.New("invalid push event kind")
	}
	return nil
}

func ValidatePublicKey(value string) error {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(decoded) != 65 || decoded[0] != 4 {
		return errors.New("invalid VAPID public key")
	}
	x, y := elliptic.Unmarshal(elliptic.P256(), decoded)
	if x == nil || y == nil {
		return errors.New("invalid VAPID public key")
	}
	return nil
}

func ValidateVAPIDConfiguration(publicKey, privateKey, subscriber string) error {
	if err := ValidatePublicKey(publicKey); err != nil {
		return err
	}
	privateBytes, err := base64.RawURLEncoding.DecodeString(privateKey)
	if err != nil || len(privateBytes) != 32 {
		return errors.New("invalid VAPID private key")
	}
	d := new(big.Int).SetBytes(privateBytes)
	if d.Sign() <= 0 || d.Cmp(elliptic.P256().Params().N) >= 0 {
		return errors.New("invalid VAPID private key")
	}
	x, y := elliptic.P256().ScalarBaseMult(privateBytes)
	derived := base64.RawURLEncoding.EncodeToString(elliptic.Marshal(elliptic.P256(), x, y))
	if len(derived) != len(publicKey) || subtle.ConstantTimeCompare([]byte(derived), []byte(publicKey)) != 1 {
		return errors.New("VAPID key pair mismatch")
	}
	u, err := url.Parse(subscriber)
	if err != nil || u.Fragment != "" {
		return errors.New("invalid VAPID subscriber URI")
	}
	validMailto := u.Scheme == "mailto" && u.Opaque != "" && !strings.ContainsAny(u.Opaque, "\r\n")
	validHTTPS := u.Scheme == "https" && u.Host != "" && u.User == nil
	if !validMailto && !validHTTPS {
		return errors.New("invalid VAPID subscriber URI")
	}
	return nil
}

type VAPIDSender struct {
	PublicKey, PrivateKey, Subscriber string
	TTL                               int
	resolver                          netIPResolver
	dialer                            contextDialer
	tlsConfig                         *tls.Config
}

type netIPResolver interface {
	LookupNetIP(context.Context, string, string) ([]netip.Addr, error)
}

type contextDialer interface {
	DialContext(context.Context, string, string) (net.Conn, error)
}

var ErrUnsafePushEndpoint = errors.New("unsafe push endpoint")

func (s *VAPIDSender) Send(ctx context.Context, sub store.PushSubscription, event Event) (Outcome, error) {
	if err := event.Validate(); err != nil {
		return PermanentFailure, err
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return PermanentFailure, err
	}
	client, closeClient, err := s.httpClient(ctx, sub.Endpoint)
	if err != nil {
		if errors.Is(err, ErrUnsafePushEndpoint) {
			return PermanentFailure, err
		}
		return Retry, err
	}
	defer closeClient()
	resp, err := webpush.SendNotificationWithContext(ctx, payload, &webpush.Subscription{Endpoint: sub.Endpoint, Keys: webpush.Keys{P256dh: sub.P256DH, Auth: sub.Auth}}, &webpush.Options{HTTPClient: client, Subscriber: s.Subscriber, VAPIDPublicKey: s.PublicKey, VAPIDPrivateKey: s.PrivateKey, TTL: s.TTL})
	if err != nil {
		if errors.Is(err, ErrUnsafePushEndpoint) {
			return PermanentFailure, err
		}
		return Retry, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return classifyStatus(resp.StatusCode)
}

func (s *VAPIDSender) httpClient(ctx context.Context, rawEndpoint string) (*http.Client, func(), error) {
	endpoint, err := pushendpoint.Parse(rawEndpoint)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrUnsafePushEndpoint, err)
	}
	hostname := endpoint.Hostname()
	resolver := s.resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	dialer := s.dialer
	if dialer == nil {
		dialer = &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	}
	if _, err := resolveSafePushIPs(ctx, resolver, hostname); err != nil {
		return nil, nil, err
	}
	port := endpoint.Port()
	if port == "" {
		port = "443"
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if s.tlsConfig != nil {
		tlsConfig = s.tlsConfig.Clone()
		if tlsConfig.MinVersion < tls.VersionTLS12 {
			tlsConfig.MinVersion = tls.VersionTLS12
		}
	}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           safePushDialContext(resolver, dialer, hostname, port),
		ForceAttemptHTTP2:     true,
		DisableKeepAlives:     true,
		TLSClientConfig:       tlsConfig,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return client, transport.CloseIdleConnections, nil
}

func safePushDialContext(resolver netIPResolver, dialer contextDialer, expectedHost, expectedPort string) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil || !strings.EqualFold(host, expectedHost) || port != expectedPort {
			return nil, fmt.Errorf("%w: unexpected dial target", ErrUnsafePushEndpoint)
		}
		addresses, err := resolveSafePushIPs(ctx, resolver, expectedHost)
		if err != nil {
			return nil, err
		}
		var dialErrors error
		for _, address := range addresses {
			connection, err := dialer.DialContext(ctx, network, net.JoinHostPort(address.String(), expectedPort))
			if err == nil {
				return connection, nil
			}
			dialErrors = errors.Join(dialErrors, err)
		}
		return nil, dialErrors
	}
}

func resolveSafePushIPs(ctx context.Context, resolver netIPResolver, hostname string) ([]netip.Addr, error) {
	addresses, err := resolver.LookupNetIP(ctx, "ip", hostname)
	if err != nil {
		return nil, err
	}
	if len(addresses) == 0 {
		return nil, errors.New("push endpoint resolved to no addresses")
	}
	for _, address := range addresses {
		if !isSafePushIP(address) {
			return nil, fmt.Errorf("%w: hostname resolved to a non-public address", ErrUnsafePushEndpoint)
		}
	}
	return addresses, nil
}

var blockedPushNetworks = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("192.175.48.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("3fff::/20"),
	netip.MustParsePrefix("3ffe::/16"),
	netip.MustParsePrefix("5f00::/16"),
	netip.MustParsePrefix("fec0::/10"),
}

var globalIPv6UnicastNetwork = netip.MustParsePrefix("2000::/3")

func isSafePushIP(address netip.Addr) bool {
	if !address.IsValid() || address.Zone() != "" {
		return false
	}
	address = address.Unmap()
	if !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() || address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast() || address.IsMulticast() || address.IsUnspecified() {
		return false
	}
	if address.Is6() && !globalIPv6UnicastNetwork.Contains(address) {
		return false
	}
	for _, network := range blockedPushNetworks {
		if network.Contains(address) {
			return false
		}
	}
	return true
}
func classifyStatus(status int) (Outcome, error) {
	switch {
	case status >= 200 && status < 300:
		return Sent, nil
	case status == http.StatusNotFound || status == http.StatusGone:
		return Gone, nil
	case status == http.StatusTooManyRequests || status >= 500:
		return Retry, fmt.Errorf("push endpoint returned %d", status)
	default:
		return PermanentFailure, fmt.Errorf("push endpoint returned %d", status)
	}
}

type Service struct {
	Store            *store.Store
	Sender           Sender
	MaxAttempts      int
	MaxConcurrency   int
	PerDeviceTimeout time.Duration
}

func (s *Service) FanoutTimeout(maxSubscriptions int) time.Duration {
	if maxSubscriptions < 1 {
		maxSubscriptions = 1
	}
	workers := s.MaxConcurrency
	if workers < 1 {
		workers = 4
	}
	workers = min(workers, maxSubscriptions)
	perDevice := s.PerDeviceTimeout
	if perDevice <= 0 {
		perDevice = 10 * time.Second
	}
	batches := (maxSubscriptions + workers - 1) / workers
	margin := perDevice / 5
	if margin < time.Second {
		margin = time.Second
	}
	return time.Duration(batches)*perDevice + margin
}

func (s *Service) ValidateConfiguration(publicKey string) error {
	if s == nil || s.Store == nil || s.Sender == nil {
		return errors.New("incomplete push service")
	}
	if sender, ok := s.Sender.(*VAPIDSender); ok {
		if publicKey != sender.PublicKey {
			return errors.New("advertised VAPID public key mismatch")
		}
		return ValidateVAPIDConfiguration(sender.PublicKey, sender.PrivateKey, sender.Subscriber)
	}
	return ValidatePublicKey(publicKey)
}

func (s *Service) Notify(ctx context.Context, subject, kind string) error {
	id, err := protocol.NewUUIDv7()
	if err != nil {
		return err
	}
	subs, err := s.Store.PushSubscriptions(ctx, subject)
	if err != nil {
		return err
	}
	if len(subs) == 0 {
		return nil
	}
	workers := s.MaxConcurrency
	if workers < 1 {
		workers = 4
	}
	workers = min(workers, len(subs))
	timeout := s.PerDeviceTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	jobs := make(chan store.PushSubscription, len(subs))
	results := make(chan error, len(subs))
	for _, sub := range subs {
		jobs <- sub
	}
	close(jobs)
	var workersDone sync.WaitGroup
	workersDone.Add(workers)
	for range workers {
		go func() {
			defer workersDone.Done()
			for sub := range jobs {
				deviceCtx, cancel := context.WithTimeout(ctx, timeout)
				results <- s.deliver(deviceCtx, sub, Event{EventID: id, Kind: kind})
				cancel()
			}
		}()
	}
	var joined error
	for range subs {
		joined = errors.Join(joined, <-results)
	}
	workersDone.Wait()
	return joined
}

func (s *Service) deliver(ctx context.Context, sub store.PushSubscription, event Event) error {
	attempts := s.MaxAttempts
	if attempts < 1 {
		attempts = 3
	}
	var out Outcome
	var sendErr error
	for attempt := 0; attempt < attempts; attempt++ {
		out, sendErr = s.Sender.Send(ctx, sub, event)
		if out != Retry {
			break
		}
		if attempt+1 < attempts {
			timer := time.NewTimer(RetryDelay(attempt))
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
	if out == Retry && sendErr == nil {
		sendErr = errors.New("push delivery retry exhausted")
	}
	if out == Gone {
		if err := s.Store.DeletePushForSubject(ctx, sub.Subject, sub.Endpoint); err != nil {
			return errors.Join(sendErr, err)
		}
	}
	return sendErr
}

func RetryDelay(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	if attempt > 6 {
		attempt = 6
	}
	return time.Second * time.Duration(1<<attempt)
}
