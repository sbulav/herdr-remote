// Package push sends generic wake-up-only Web Push notifications. Payloads are
// deliberately incapable of carrying host, terminal, prompt, or action data.
package push

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
	"github.com/dcolinmorgan/herdr-remote/internal/protocol"
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
	if e.Kind != "agent_state_changed" && e.Kind != "connector_state_changed" {
		return errors.New("invalid push event kind")
	}
	return nil
}

type VAPIDSender struct {
	PublicKey, PrivateKey, Subscriber string
	TTL                               int
}

func (s *VAPIDSender) Send(ctx context.Context, sub store.PushSubscription, event Event) (Outcome, error) {
	if err := event.Validate(); err != nil {
		return PermanentFailure, err
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return PermanentFailure, err
	}
	resp, err := webpush.SendNotificationWithContext(ctx, payload, &webpush.Subscription{Endpoint: sub.Endpoint, Keys: webpush.Keys{P256dh: sub.P256DH, Auth: sub.Auth}}, &webpush.Options{Subscriber: s.Subscriber, VAPIDPublicKey: s.PublicKey, VAPIDPrivateKey: s.PrivateKey, TTL: s.TTL})
	if err != nil {
		return Retry, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return classifyStatus(resp.StatusCode)
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
	Store       *store.Store
	Sender      Sender
	MaxAttempts int
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
	var joined error
	for _, sub := range subs {
		attempts := s.MaxAttempts
		if attempts < 1 {
			attempts = 3
		}
		var out Outcome
		var sendErr error
		for attempt := 0; attempt < attempts; attempt++ {
			out, sendErr = s.Sender.Send(ctx, sub, Event{EventID: id, Kind: kind})
			if out != Retry {
				break
			}
			if attempt+1 < attempts {
				select {
				case <-ctx.Done():
					sendErr = ctx.Err()
					attempt = attempts
				case <-time.After(RetryDelay(attempt)):
				}
			}
		}
		if out == Gone {
			if err := s.Store.DeletePush(ctx, sub.Endpoint); err != nil {
				joined = errors.Join(joined, err)
			}
		}
		if sendErr != nil {
			joined = errors.Join(joined, sendErr)
		}
	}
	return joined
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
