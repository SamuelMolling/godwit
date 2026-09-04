package controlplane

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/SamuelMolling/godwit/internal/engine"
)

func TestHeartbeatStopsOnLostLease(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)

	// No lease exists, so the first beat reports ErrLeaseLost and returns.
	sched := NewScheduler(s, nil, PGEngine{}, Policies(), Config{Holder: "h", TTL: 60 * time.Millisecond}, testLog)
	done := make(chan struct{})
	go func() {
		sched.heartbeat(context.Background(), "99999999-9999-9999-9999-999999999999")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("heartbeat did not stop after lease loss")
	}
}

func TestHeartbeatStopsOnContext(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)

	sched := NewScheduler(s, nil, PGEngine{}, Policies(), Config{Holder: "h", TTL: time.Hour}, testLog)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		sched.heartbeat(ctx, "id")
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("heartbeat did not stop on ctx cancel")
	}
}

func TestApplyRunFilesError(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)
	sched := NewScheduler(s, nil, PGEngine{}, Policies(), Config{Holder: "h"}, testLog)

	s.pool.(interface{ Close() }).Close()
	if _, err := sched.applyRun(context.Background(), Run{ID: "id", Target: "app"}); err == nil {
		t.Fatal("want error")
	}
}

func TestApplyRunUnknownTarget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newStore(t)
	if err := s.RegisterTarget(ctx, "app", "plain", map[string]string{"dsn": "x"}); err != nil {
		t.Fatal(err)
	}
	queueRun(t, s, "aaaaaaaa-0000-0000-0000-000000000001", goodFiles())

	sched := NewScheduler(s, nil, PGEngine{}, Policies(), Config{Holder: "h"}, testLog)
	_, err := sched.applyRun(ctx, Run{ID: "aaaaaaaa-0000-0000-0000-000000000001", Target: "ghost", Rollout: RolloutDirect})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err = %v", err)
	}
}

func TestApplyMigrationsBadSQL(t *testing.T) {
	t.Parallel()

	_, err := applyMigrations(context.Background(), nil,
		[]engine.Migration{{Version: 1, Name: "bad", UpSQL: "NOT SQL", DownSQL: "SELECT 1;"}})
	if err == nil || !strings.Contains(err.Error(), "parse") {
		t.Fatalf("err = %v", err)
	}
}

func TestProgressThrottlesPartialReports(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newStore(t)
	const id = "44444444-0000-0000-0000-000000000001"
	if err := s.RegisterTarget(ctx, "app", "plain", map[string]string{"dsn": "postgres://x"}); err != nil {
		t.Fatal(err)
	}
	queueRun(t, s, id, goodFiles())

	report := NewScheduler(s, nil, PGEngine{}, Policies(), Config{Holder: "h"}, testLog).progress(ctx, id)
	batch := engine.Statement{Batch: &engine.BatchSpec{}}
	rows := func() int64 {
		run, err := s.Run(ctx, id)
		if err != nil {
			t.Fatal(err)
		}

		return run.Progress.RowsDone
	}

	report(engine.StatementEvent{Migration: "m", Index: 4, Statement: batch, RowsDone: 100, Batches: 1, Partial: true})
	if got := rows(); got != 100 {
		t.Fatalf("first report = %d rows", got)
	}
	report(engine.StatementEvent{Migration: "m", Index: 4, Statement: batch, RowsDone: 200, Batches: 2, Partial: true})
	if got := rows(); got != 100 {
		t.Fatalf("second report inside the interval = %d rows, want the first still", got)
	}
	report(engine.StatementEvent{Migration: "m", Index: 4, Statement: batch, RowsDone: 300, Batches: 3})
	if got := rows(); got != 300 {
		t.Fatalf("end of statement = %d rows", got)
	}
}
