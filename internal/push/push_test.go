package push

import (
	"context"
	"errors"
	"testing"

	"github.com/dcolinmorgan/herdr-remote/internal/store"
)

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
	gone := "https://push.example/gone"
	retry := "https://push.example/retry"
	for _, endpoint := range []string{gone, retry} {
		if err := st.UpsertPush(ctx, store.PushSubscription{Subject: "operator", Endpoint: endpoint, P256DH: "key", Auth: "auth"}); err != nil {
			t.Fatal(err)
		}
	}
	svc := Service{Store: st, Sender: fakeSender{map[string]Outcome{gone: Gone, retry: Retry}}, MaxAttempts: 1}
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
