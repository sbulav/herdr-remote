package push

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dcolinmorgan/herdr-remote/internal/store"
)

func TestVAPIDPublicKeyValidation(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	valid := base64.RawURLEncoding.EncodeToString(elliptic.Marshal(elliptic.P256(), key.X, key.Y))
	privateBytes := make([]byte, 32)
	key.D.FillBytes(privateBytes)
	private := base64.RawURLEncoding.EncodeToString(privateBytes)
	if err := ValidatePublicKey(valid); err != nil {
		t.Fatalf("valid key rejected: %v", err)
	}
	for _, invalid := range []string{"", "not-base64", base64.RawURLEncoding.EncodeToString([]byte{4, 1, 2})} {
		if ValidatePublicKey(invalid) == nil {
			t.Fatalf("invalid key accepted: %q", invalid)
		}
	}
	if err := ValidateVAPIDConfiguration(valid, private, "mailto:operator@example.com"); err != nil {
		t.Fatalf("valid VAPID configuration rejected: %v", err)
	}
	service := Service{Store: &store.Store{}, Sender: &VAPIDSender{PublicKey: valid, PrivateKey: private, Subscriber: "mailto:operator@example.com"}}
	if err := service.ValidateConfiguration(valid); err != nil {
		t.Fatalf("valid service configuration rejected: %v", err)
	}
	if service.ValidateConfiguration("different") == nil {
		t.Fatal("advertised public key mismatch accepted")
	}
	other, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	otherPrivate := make([]byte, 32)
	other.D.FillBytes(otherPrivate)
	for _, test := range []struct {
		name, public, private, subscriber string
	}{
		{"mismatch", valid, base64.RawURLEncoding.EncodeToString(otherPrivate), "mailto:operator@example.com"},
		{"bad-private", valid, "bad", "mailto:operator@example.com"},
		{"bad-subscriber", valid, private, "http://example.com"},
	} {
		if ValidateVAPIDConfiguration(test.public, test.private, test.subscriber) == nil {
			t.Fatalf("%s accepted", test.name)
		}
	}
}

type fakeSender struct{ outcomes map[string]Outcome }

func (f fakeSender) Send(_ context.Context, s store.PushSubscription, e Event) (Outcome, error) {
	if err := e.Validate(); err != nil {
		return PermanentFailure, err
	}
	out := f.outcomes[s.Endpoint]
	if out == Retry {
		return out, errors.New("retry")
	}
	return out, nil
}
func TestGoneSubscriptionsAreCleanedAndRetryIsReported(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	gone404 := "https://push.example/gone-404"
	gone410 := "https://push.example/gone-410"
	retry := "https://push.example/retry"
	for _, endpoint := range []string{gone404, gone410, retry} {
		if err := st.UpsertPush(ctx, store.PushSubscription{Subject: "operator", Endpoint: endpoint, P256DH: "key", Auth: "auth"}); err != nil {
			t.Fatal(err)
		}
	}
	svc := Service{Store: st, Sender: fakeSender{map[string]Outcome{gone404: Gone, gone410: Gone, retry: Retry}}, MaxAttempts: 1}
	if err := svc.Notify(ctx, "operator", "agent_state_changed"); err == nil {
		t.Fatal("retry failure hidden")
	}
	subs, err := st.PushSubscriptions(ctx, "operator")
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 1 || subs[0].Endpoint != retry {
		t.Fatalf("gone subscription not removed: %#v", subs)
	}
	for _, endpoint := range []string{gone404, gone410} {
		err := st.ReplacePush(ctx, "operator", []string{endpoint}, store.PushSubscription{Subject: "operator", Endpoint: endpoint + "-replacement", P256DH: "key", Auth: "auth"})
		if !errors.Is(err, store.ErrPushMissing) {
			t.Fatalf("cleanup of %s did not produce missing replacement source: %v", endpoint, err)
		}
	}
}
func TestHTTPSemanticsNeverFakeSuccess(t *testing.T) {
	for status, want := range map[int]Outcome{201: Sent, 404: Gone, 410: Gone, 429: Retry, 503: Retry, 400: PermanentFailure} {
		got, err := classifyStatus(status)
		if got != want {
			t.Fatalf("status %d: got %v", status, got)
		}
		if got != Sent && got != Gone && err == nil {
			t.Fatalf("status %d hid failure", status)
		}
	}
}

type concurrentSender struct {
	mu      sync.Mutex
	seen    map[string]bool
	current atomic.Int32
	maximum atomic.Int32
	stall   string
	stalls  map[string]bool
	barrier chan struct{}
	arrived atomic.Int32
	once    sync.Once
}

func (s *concurrentSender) Send(ctx context.Context, subscription store.PushSubscription, _ Event) (Outcome, error) {
	current := s.current.Add(1)
	defer s.current.Add(-1)
	for {
		maximum := s.maximum.Load()
		if current <= maximum || s.maximum.CompareAndSwap(maximum, current) {
			break
		}
	}
	s.mu.Lock()
	s.seen[subscription.Endpoint] = true
	s.mu.Unlock()
	if s.barrier != nil {
		position := s.arrived.Add(1)
		if position == 2 {
			s.once.Do(func() { close(s.barrier) })
		}
		if position <= 2 {
			select {
			case <-s.barrier:
			case <-ctx.Done():
				return Retry, ctx.Err()
			}
		}
	}
	if subscription.Endpoint == s.stall || s.stalls[subscription.Endpoint] {
		<-ctx.Done()
		return Retry, ctx.Err()
	}
	return Sent, nil
}

func TestNotifyFansOutConcurrentlyWithIndependentTimeouts(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	stall := "https://push.example/device-7"
	for i := 0; i < 8; i++ {
		if err := st.UpsertPush(ctx, store.PushSubscription{Subject: "operator", Endpoint: fmt.Sprintf("https://push.example/device-%d", i), P256DH: "key", Auth: "auth"}); err != nil {
			t.Fatal(err)
		}
	}
	sender := &concurrentSender{seen: map[string]bool{}, stall: stall, barrier: make(chan struct{})}
	service := Service{Store: st, Sender: sender, MaxAttempts: 1, MaxConcurrency: 2, PerDeviceTimeout: 30 * time.Millisecond}
	started := time.Now()
	if err := service.Notify(ctx, "operator", "test"); err == nil {
		t.Fatal("stalled endpoint failure hidden")
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("stalled endpoint starved fanout: %v", elapsed)
	}
	sender.mu.Lock()
	seen := len(sender.seen)
	sender.mu.Unlock()
	if seen != 8 {
		t.Fatalf("fanout reached %d devices", seen)
	}
	if maximum := sender.maximum.Load(); maximum < 2 || maximum > 2 {
		t.Fatalf("maximum concurrency = %d", maximum)
	}
	if current := sender.current.Load(); current != 0 {
		t.Fatalf("send goroutines still active: %d", current)
	}
}

func TestFanoutDeadlineAllowsAllSupportedDevicesAfterStalledFirstBatch(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	sender := &concurrentSender{seen: map[string]bool{}}
	for i := 0; i < store.MaxPushSubscriptionsPerOperator; i++ {
		endpoint := fmt.Sprintf("https://push.example/device-%02d", i)
		if err := st.UpsertPush(ctx, store.PushSubscription{Subject: "operator", Endpoint: endpoint, P256DH: "key", Auth: "auth"}); err != nil {
			t.Fatal(err)
		}
	}
	service := Service{Store: st, Sender: sender, MaxAttempts: 1, MaxConcurrency: 4, PerDeviceTimeout: 20 * time.Millisecond}
	// PushSubscriptions returns newest first, so stall the first complete worker batch.
	stalled := map[string]bool{}
	for i := store.MaxPushSubscriptionsPerOperator - 4; i < store.MaxPushSubscriptionsPerOperator; i++ {
		stalled[fmt.Sprintf("https://push.example/device-%02d", i)] = true
	}
	sender.stalls = stalled
	overall, cancel := context.WithTimeout(ctx, service.FanoutTimeout(store.MaxPushSubscriptionsPerOperator))
	defer cancel()
	if err := service.Notify(overall, "operator", "test"); err == nil {
		t.Fatal("stalled first batch failure hidden")
	}
	sender.mu.Lock()
	seen := len(sender.seen)
	sender.mu.Unlock()
	if seen != store.MaxPushSubscriptionsPerOperator {
		t.Fatalf("fanout reached %d of %d devices", seen, store.MaxPushSubscriptionsPerOperator)
	}
	if overall.Err() != nil {
		t.Fatalf("derived overall deadline expired before full fanout: %v", overall.Err())
	}
	if current := sender.current.Load(); current != 0 {
		t.Fatalf("send goroutines still active: %d", current)
	}
}
