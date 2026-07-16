package push

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"testing"

	"github.com/dcolinmorgan/herdr-remote/internal/store"
)

type sequenceResolver struct {
	mu      sync.Mutex
	answers [][]netip.Addr
	calls   []string
}

func (r *sequenceResolver) LookupNetIP(_ context.Context, network, host string) ([]netip.Addr, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, network+":"+host)
	if len(r.answers) == 0 {
		return nil, errors.New("unexpected DNS lookup")
	}
	answer := r.answers[0]
	r.answers = r.answers[1:]
	return answer, nil
}

type recordingDialer struct {
	mu        sync.Mutex
	addresses []string
	peers     []net.Conn
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func (d *recordingDialer) DialContext(_ context.Context, _, address string) (net.Conn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.addresses = append(d.addresses, address)
	client, peer := net.Pipe()
	d.peers = append(d.peers, peer)
	return client, nil
}

func (d *recordingDialer) close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, peer := range d.peers {
		peer.Close()
	}
}

func TestSafePushDialResolvesAndDialsValidatedPublicIP(t *testing.T) {
	public := netip.MustParseAddr("93.184.216.34")
	resolver := &sequenceResolver{answers: [][]netip.Addr{{public}, {public}}}
	dialer := &recordingDialer{}
	defer dialer.close()
	sender := &VAPIDSender{resolver: resolver, dialer: dialer}
	client, closeClient, err := sender.httpClient(context.Background(), "https://push.example/send")
	if err != nil {
		t.Fatal(err)
	}
	defer closeClient()
	transport := client.Transport.(*http.Transport)
	if transport.TLSClientConfig.InsecureSkipVerify || transport.TLSClientConfig.ServerName != "" {
		t.Fatalf("TLS hostname verification was overridden: %#v", transport.TLSClientConfig)
	}
	connection, err := transport.DialContext(context.Background(), "tcp", "push.example:443")
	if err != nil {
		t.Fatal(err)
	}
	connection.Close()
	if len(dialer.addresses) != 1 || dialer.addresses[0] != "93.184.216.34:443" {
		t.Fatalf("dial targets = %#v", dialer.addresses)
	}
}

func TestSafePushClientRejectsPrivateDNSAndRebinding(t *testing.T) {
	public := netip.MustParseAddr("93.184.216.34")
	private := netip.MustParseAddr("10.0.0.8")
	for _, test := range []struct {
		name    string
		answers [][]netip.Addr
		dial    bool
	}{
		{name: "private DNS", answers: [][]netip.Addr{{private}}},
		{name: "mixed public and private DNS", answers: [][]netip.Addr{{public, private}}},
		{name: "DNS rebinding", answers: [][]netip.Addr{{public}, {private}}, dial: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			resolver := &sequenceResolver{answers: test.answers}
			dialer := &recordingDialer{}
			defer dialer.close()
			sender := &VAPIDSender{resolver: resolver, dialer: dialer}
			client, closeClient, err := sender.httpClient(context.Background(), "https://push.example/send")
			if !test.dial {
				if !errors.Is(err, ErrUnsafePushEndpoint) {
					t.Fatalf("private DNS error = %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				defer closeClient()
				transport := client.Transport.(*http.Transport)
				_, err = transport.DialContext(context.Background(), "tcp", "push.example:443")
				if !errors.Is(err, ErrUnsafePushEndpoint) {
					t.Fatalf("rebinding error = %v", err)
				}
			}
			if len(dialer.addresses) != 0 {
				t.Fatalf("unsafe DNS reached dialer: %#v", dialer.addresses)
			}
		})
	}
}

func TestSafePushClientNeverFollowsRedirects(t *testing.T) {
	resolver := &sequenceResolver{answers: [][]netip.Addr{{netip.MustParseAddr("93.184.216.34")}}}
	dialer := &recordingDialer{}
	sender := &VAPIDSender{resolver: resolver, dialer: dialer}
	client, closeClient, err := sender.httpClient(context.Background(), "https://push.example/send")
	if err != nil {
		t.Fatal(err)
	}
	defer closeClient()
	requests := 0
	client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": []string{"https://private.example/internal"}}, Body: http.NoBody, Request: request}, nil
	})
	original, _ := http.NewRequest(http.MethodPost, "https://push.example/send", nil)
	response, err := client.Do(original)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusFound || requests != 1 {
		t.Fatalf("redirect response=%d requests=%d", response.StatusCode, requests)
	}
	if len(resolver.calls) != 1 || len(dialer.addresses) != 0 {
		t.Fatalf("redirect target reached network: DNS=%#v dial=%#v", resolver.calls, dialer.addresses)
	}
}

func TestSafePushIPRanges(t *testing.T) {
	for _, value := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"} {
		if !isSafePushIP(netip.MustParseAddr(value)) {
			t.Fatalf("public address %s rejected", value)
		}
	}
	for _, value := range []string{
		"0.0.0.0", "10.0.0.1", "100.64.0.1", "127.0.0.1", "169.254.1.1", "172.16.0.1", "192.0.2.1", "192.168.0.1", "198.18.0.1", "198.51.100.1", "203.0.113.1", "224.0.0.1", "240.0.0.1",
		"::", "::1", "::ffff:127.0.0.1", "64:ff9b::a00:1", "100::1", "2001::1", "2001:db8::1", "2002::1", "3ffe::1", "3fff::1", "4000::1", "5f00::1", "fc00::1", "fe80::1", "fec0::1", "ff02::1",
	} {
		if isSafePushIP(netip.MustParseAddr(value)) {
			t.Fatalf("non-public address %s accepted", value)
		}
	}
}

func TestVAPIDSenderRejectsUnsafePersistedEndpointBeforeNetwork(t *testing.T) {
	resolver := &sequenceResolver{answers: [][]netip.Addr{{netip.MustParseAddr("93.184.216.34")}}}
	dialer := &recordingDialer{}
	sender := &VAPIDSender{resolver: resolver, dialer: dialer}
	event := Event{EventID: "019f64ca-3000-7000-8000-000000000001", Kind: "test"}
	for _, endpoint := range []string{"https://127.0.0.1/push", "https://user@push.example/push", "https://push.example:8443/push", "https://push.example/push#internal"} {
		outcome, err := sender.Send(context.Background(), store.PushSubscription{Endpoint: endpoint}, event)
		if outcome != PermanentFailure || !errors.Is(err, ErrUnsafePushEndpoint) {
			t.Fatalf("endpoint %q outcome=%v error=%v", endpoint, outcome, err)
		}
	}
	if len(resolver.calls) != 0 || len(dialer.addresses) != 0 {
		t.Fatalf("unsafe persisted endpoints reached network: DNS=%#v dial=%#v", resolver.calls, dialer.addresses)
	}
}

func TestVAPIDSenderRevalidatesDNSInsideWebpushTransport(t *testing.T) {
	public := netip.MustParseAddr("93.184.216.34")
	private := netip.MustParseAddr("10.0.0.9")
	resolver := &sequenceResolver{answers: [][]netip.Addr{{public}, {private}}}
	dialer := &recordingDialer{}
	vapidPublic, vapidPrivate := testEncodedP256Key(t)
	receiverPublic, _ := testEncodedP256Key(t)
	auth := base64.RawURLEncoding.EncodeToString([]byte("0123456789abcdef"))
	sender := &VAPIDSender{PublicKey: vapidPublic, PrivateKey: vapidPrivate, Subscriber: "mailto:operator@example.com", resolver: resolver, dialer: dialer}
	event := Event{EventID: "019f64ca-3000-7000-8000-000000000001", Kind: "test"}
	outcome, err := sender.Send(context.Background(), store.PushSubscription{Endpoint: "https://push.example/send", P256DH: receiverPublic, Auth: auth}, event)
	if outcome != PermanentFailure || !errors.Is(err, ErrUnsafePushEndpoint) {
		t.Fatalf("rebinding outcome=%v error=%v", outcome, err)
	}
	if len(dialer.addresses) != 0 {
		t.Fatalf("rebinding reached dialer: %#v", dialer.addresses)
	}
}

func testEncodedP256Key(t *testing.T) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	private := make([]byte, 32)
	key.D.FillBytes(private)
	return base64.RawURLEncoding.EncodeToString(elliptic.Marshal(elliptic.P256(), key.X, key.Y)), base64.RawURLEncoding.EncodeToString(private)
}
