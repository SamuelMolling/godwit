package notify

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Delivery results reported to the metrics recorder.
const (
	ResultDelivered = "delivered"
	ResultFailed    = "failed"
	ResultDropped   = "dropped"
)

const (
	asyncQueueSize       = 256
	asyncDeliveryTimeout = 10 * time.Second
)

// Async queues events for one provider and delivers them from one worker, off the run's path.
type Async struct {
	provider string
	next     Notifier
	log      *slog.Logger
	record   func(provider, result string)
	timeout  time.Duration

	mu     sync.RWMutex
	closed bool
	queue  chan Event
	done   chan struct{}
}

// NewAsync starts the worker; record may be nil.
func NewAsync(provider string, next Notifier, log *slog.Logger, record func(provider, result string)) *Async {
	if record == nil {
		record = func(string, string) {}
	}
	a := &Async{
		provider: provider,
		next:     next,
		log:      log,
		record:   record,
		timeout:  asyncDeliveryTimeout,
		queue:    make(chan Event, asyncQueueSize),
		done:     make(chan struct{}),
	}
	go a.run()

	return a
}

// Notify implements Notifier; it never blocks and never fails.
func (a *Async) Notify(_ context.Context, e Event) error {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.closed {
		select {
		case a.queue <- e:
			return nil
		default:
		}
	}
	a.log.Warn("notification dropped", "provider", a.provider, "kind", e.Kind, "type", e.Type, "target", e.Target, "run", e.RunID)
	a.record(a.provider, ResultDropped)

	return nil
}

// Close stops accepting events and waits for the queued ones to be delivered.
func (a *Async) Close() {
	a.mu.Lock()
	if !a.closed {
		a.closed = true
		close(a.queue)
	}
	a.mu.Unlock()
	<-a.done
}

func (a *Async) run() {
	defer close(a.done)
	for e := range a.queue {
		ctx, cancel := context.WithTimeout(context.Background(), a.timeout)
		err := a.next.Notify(ctx, e)
		cancel()
		if err != nil {
			a.log.Warn("notification failed", "provider", a.provider, "kind", e.Kind, "type", e.Type, "target", e.Target, "run", e.RunID, "error", err)
			a.record(a.provider, ResultFailed)

			continue
		}
		a.record(a.provider, ResultDelivered)
	}
}
