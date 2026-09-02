package notify

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

type blockingNotifier struct {
	release chan struct{}
	calls   int
	err     error
	mu      sync.Mutex
}

func (n *blockingNotifier) Notify(ctx context.Context, _ Event) error {
	n.mu.Lock()
	n.calls++
	n.mu.Unlock()
	select {
	case <-n.release:
		return n.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

type recorder struct {
	mu      sync.Mutex
	results map[string]int
}

func (r *recorder) record(provider, result string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.results == nil {
		r.results = map[string]int{}
	}
	r.results[provider+"/"+result]++
}

func (r *recorder) get(key string) int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.results[key]
}

func TestAsyncDeliversAndDrops(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	next := &blockingNotifier{release: make(chan struct{})}
	rec := &recorder{}
	a := NewAsync("p", next, log, rec.record)

	for i := 0; i < asyncQueueSize+1; i++ {
		if err := a.Notify(context.Background(), Event{Kind: KindRun, Type: RunCreated, Target: "app", RunID: "r"}); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for rec.get("p/dropped") == 0 && time.Now().Before(deadline) {
		_ = a.Notify(context.Background(), Event{Kind: KindRun})
		time.Sleep(10 * time.Millisecond)
	}
	if rec.get("p/dropped") == 0 || !strings.Contains(buf.String(), "notification dropped") {
		t.Fatalf("full queue must drop: %v / %q", rec.results, buf.String())
	}

	close(next.release)
	a.Close()
	a.Close()
	if rec.get("p/delivered") < asyncQueueSize {
		t.Fatalf("delivered = %d", rec.get("p/delivered"))
	}
	if err := a.Notify(context.Background(), Event{}); err != nil || rec.get("p/dropped") < 2 {
		t.Fatalf("closed queue must drop: %v", rec.results)
	}
}

func TestAsyncFailureAndTimeout(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	next := &stubNotifier{err: errors.New("boom")}
	rec := &recorder{}
	a := NewAsync("p", next, log, rec.record)
	_ = a.Notify(context.Background(), Event{Kind: KindRun, Type: RunFailed, Target: "app"})
	a.Close()
	if rec.get("p/failed") != 1 || !strings.Contains(buf.String(), "boom") {
		t.Fatalf("results = %v, log = %q", rec.results, buf.String())
	}

	slow := &blockingNotifier{release: make(chan struct{})}
	b := NewAsync("q", slow, log, nil)
	b.timeout = 10 * time.Millisecond
	_ = b.Notify(context.Background(), Event{})
	b.Close()
	if !strings.Contains(buf.String(), "context deadline exceeded") {
		t.Fatalf("log = %q", buf.String())
	}
}
