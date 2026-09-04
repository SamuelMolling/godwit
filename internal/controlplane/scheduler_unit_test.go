package controlplane

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SamuelMolling/godwit/internal/engine"
)

// beat runs the heartbeat to completion and reports whether it gave the run up.
func beat(t *testing.T, sched *Scheduler, ctx context.Context, runID string) bool {
	t.Helper()
	var lost atomic.Bool
	done := make(chan struct{})
	go func() {
		sched.heartbeat(ctx, runID, func() { lost.Store(true) })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("heartbeat did not stop")
	}

	return lost.Load()
}

func TestHeartbeatStopsOnLostLease(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)

	// No lease exists, so the first beat reports ErrLeaseLost and gives the run up at once.
	sched := NewScheduler(s, nil, PGEngine{}, Policies(), Config{Holder: "h", TTL: 60 * time.Millisecond}, testLog)
	if !beat(t, sched, context.Background(), "99999999-9999-9999-9999-999999999999") {
		t.Fatal("a lease taken by another holder must stop the run")
	}
}

func TestHeartbeatStopsOnContext(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)

	sched := NewScheduler(s, nil, PGEngine{}, Policies(), Config{Holder: "h", TTL: time.Hour}, testLog)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if beat(t, sched, ctx, "id") {
		t.Fatal("a cancelled context is the run ending, not the lease")
	}
}

func TestHeartbeatRidesOutABlipAndGivesUpOnAnOutage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, pool := newStore(t)
	const id = "44444444-0000-0000-0000-00000000000b"
	if err := s.RegisterTarget(ctx, "app", "plain", map[string]string{"dsn": "x"}); err != nil {
		t.Fatal(err)
	}
	queueRun(t, s, id, goodFiles())
	if _, ok, err := s.Claim(ctx, "h", time.Hour); err != nil || !ok {
		t.Fatalf("claim = %v, %v", ok, err)
	}

	sched := NewScheduler(s, nil, PGEngine{}, Policies(), Config{Holder: "h", TTL: 300 * time.Millisecond}, testLog)
	landing, stop := context.WithTimeout(ctx, time.Second)
	defer stop()
	if beat(t, sched, landing, id) {
		t.Fatal("beats that land must not stop the run")
	}

	// The store never comes back: the beats retry, and give the run up before its lease expires.
	pool.Close()
	if !beat(t, sched, ctx, id) {
		t.Fatal("a store outage past the lease must stop the run")
	}
}

func TestExecuteStopsWhenTheLeaseIsGone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newStore(t)
	sched, _ := newScheduler(t, s, Config{Holder: "h1", TTL: 200 * time.Millisecond})
	const id = "44444444-0000-0000-0000-00000000000c"
	queueRun(t, s, id, map[string]string{
		"20260901120000_slow.up.sql":   "SELECT pg_sleep(5);",
		"20260901120000_slow.down.sql": "SELECT 1;",
	})
	run, err := s.Run(ctx, id)
	if err != nil {
		t.Fatal(err)
	}

	// Nothing holds a lease for this run, so the first beat reports it lost while the run is still going.
	sched.execute(ctx, run)

	after, err := s.Run(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != StateQueued || after.Error != "" {
		t.Fatalf("run = %+v: a replica that lost the lease must not write the run", after)
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
