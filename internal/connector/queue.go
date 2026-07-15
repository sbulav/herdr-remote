package connector

import (
	"context"
	"errors"
	"sync"
)

var ErrQueueFull = errors.New("connector priority queue full")

// Queue reserves a bounded FIFO for state/actions and coalesces output by
// subscription. Priority messages are always selected first.
type Queue struct {
	high   chan []byte
	wake   chan struct{}
	mu     sync.Mutex
	output map[string][]byte
}

func NewQueue(n int) *Queue {
	if n < 1 {
		n = 1
	}
	return &Queue{high: make(chan []byte, n), wake: make(chan struct{}, 1), output: map[string][]byte{}}
}
func (q *Queue) Put(ctx context.Context, b []byte) error {
	select {
	case q.high <- append([]byte(nil), b...):
		q.signal()
		return nil
	case <-ctx.Done():
		return ErrQueueFull
	}
}
func (q *Queue) ReplaceOutput(id string, b []byte) {
	q.mu.Lock()
	q.output[id] = append([]byte(nil), b...)
	q.mu.Unlock()
	q.signal()
}
func (q *Queue) Next(ctx context.Context) ([]byte, error) {
	for {
		select {
		case b := <-q.high:
			return b, nil
		default:
		}
		q.mu.Lock()
		for id, b := range q.output {
			delete(q.output, id)
			q.mu.Unlock()
			return b, nil
		}
		q.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-q.wake:
		}
	}
}
func (q *Queue) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}
func (q *Queue) Len() int { return len(q.high) }
