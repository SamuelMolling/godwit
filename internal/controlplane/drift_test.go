package controlplane

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/SamuelMolling/godwit/internal/creds"
	"github.com/SamuelMolling/godwit/internal/engine"
	"github.com/SamuelMolling/godwit/internal/notify"
)

type recordingNotifier struct {
	mu     sync.Mutex
	events []notify.Event
	fail   bool
}

func (n *recordingNotifier) Notify(_ context.Context, e notify.Event) error {
	n.mu.Lock()
	n.events = append(n.events, e)
	n.mu.Unlock()
	if n.fail {
		return errors.New("notify boom")
	}

	return nil
}

func (n *recordingNotifier) types() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []string
	for _, e := range n.events {
		out = append(out, e.Kind+":"+e.Type)
	}

	return strings.Join(out, " ")
}

func newMonitor(t *testing.T, s *Store, n notify.Notifier) (*DriftMonitor, string) {
	t.Helper()
	sched, targetDSN := newScheduler(t, s, Config{Holder: "h"})

	return NewDriftMonitor(s, sched, PGEngine{}, n, time.Minute, testLog), targetDSN
}

func execTarget(t *testing.T, dsn, sql string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, sql); err != nil {
		t.Fatal(err)
	}
}

func TestDriftLifecycle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newStore(t)
	notifier := &recordingNotifier{}
	mon, targetDSN := newMonitor(t, s, notifier)
	sched, _ := NewScheduler(s, map[string]creds.Provider{"plain": plainProvider{}},
		PGEngine{}, Policies(), Config{Holder: "h"}, testLog), targetDSN

	queueRun(t, s, "d1111111-0000-0000-0000-000000000001", goodFiles())
	sched.Tick(ctx)
	waitState(t, s, "d1111111-0000-0000-0000-000000000001", StateSucceeded)

	snap, err := s.SnapshotFor(ctx, "app")
	if err != nil || snap.Fingerprint == "" || !strings.Contains(snap.Definition, ".t.id") {
		t.Fatalf("snapshot = %+v, err = %v", snap, err)
	}

	d, err := mon.Check(ctx, "app")
	if err != nil || d.Drifted {
		t.Fatalf("drift = %+v, err = %v", d, err)
	}

	// Manual out-of-band change → drift detected + notified once.
	execTarget(t, targetDSN, "ALTER TABLE t ADD COLUMN sneaky text")
	mon.Tick(ctx)
	mon.Tick(ctx) // identical open drift must not re-notify
	if got := notifier.types(); got != "drift:detected" {
		t.Fatalf("notifications = %q", got)
	}
	events, err := s.ListDriftEvents(ctx, "app")
	if err != nil || len(events) != 1 || events[0].ResolvedAt != nil ||
		!strings.Contains(events[0].Diff, ".t.sneaky") {
		t.Fatalf("events = %+v, err = %v", events, err)
	}

	// Reverting the change resolves the drift on the next check.
	execTarget(t, targetDSN, "ALTER TABLE t DROP COLUMN sneaky")
	d, err = mon.Check(ctx, "app")
	if err != nil || d.Drifted {
		t.Fatalf("drift = %+v, err = %v", d, err)
	}
	events, _ = s.ListDriftEvents(ctx, "")
	if len(events) != 1 || events[0].ResolvedAt == nil {
		t.Fatalf("events = %+v", events)
	}
	if _, err := mon.Check(ctx, "app"); err != nil {
		t.Fatal(err)
	}
	if got := notifier.types(); got != "drift:detected drift:resolved" {
		t.Fatalf("notifications = %q", got)
	}
	if e := notifier.events[0]; e.Target != "app" || !strings.Contains(e.Detail, ".t.sneaky") || e.At.IsZero() {
		t.Fatalf("detected event = %+v", e)
	}
}

func TestAcceptBaseline(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newStore(t)
	notifier := &recordingNotifier{fail: true} // notification failure must not break detection
	mon, targetDSN := newMonitor(t, s, notifier)

	execTarget(t, targetDSN, "CREATE TABLE manual (id int)")
	if err := mon.AcceptBaseline(ctx, "app"); err != nil {
		t.Fatal(err)
	}
	d, err := mon.Check(ctx, "app")
	if err != nil || d.Drifted {
		t.Fatalf("drift after accept = %+v, err = %v", d, err)
	}

	// New manual change drifts again; failing notifier doesn't block the event.
	execTarget(t, targetDSN, "CREATE TABLE manual2 (id int)")
	d, err = mon.Check(ctx, "app")
	if err != nil || !d.Drifted {
		t.Fatalf("drift = %+v, err = %v", d, err)
	}
	if got := notifier.types(); got != "drift:accepted drift:detected" {
		t.Fatalf("notifications = %q", got)
	}
}

func TestDriftErrorBranches(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("check without baseline", func(t *testing.T) {
		t.Parallel()
		s, _ := newStore(t)
		mon, _ := newMonitor(t, s, notify.None{})
		if _, err := mon.Check(ctx, "app"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("unknown target", func(t *testing.T) {
		t.Parallel()
		s, _ := newStore(t)
		mon, _ := newMonitor(t, s, notify.None{})
		if err := s.SaveSnapshot(ctx, "app", "fp", "def", ""); err != nil {
			t.Fatal(err)
		}
		// Break the target's provider config so DSN resolution fails.
		if err := s.RegisterTarget(ctx, "app", "plain", map[string]string{}); err != nil {
			t.Fatal(err)
		}
		if _, err := mon.Check(ctx, "app"); err == nil {
			t.Fatal("want error")
		}
		if err := mon.AcceptBaseline(ctx, "app"); err == nil {
			t.Fatal("want error")
		}
		mon.Tick(ctx) // logs the failure, must not panic
	})

	t.Run("unreachable target", func(t *testing.T) {
		t.Parallel()
		s, _ := newStore(t)
		mon, _ := newMonitor(t, s, notify.None{})
		if err := s.RegisterTarget(ctx, "app", "plain", map[string]string{"dsn": "postgres://bad:bad@127.0.0.1:1/x"}); err != nil {
			t.Fatal(err)
		}
		if err := s.SaveSnapshot(ctx, "app", "fp", "def", ""); err != nil {
			t.Fatal(err)
		}
		if _, err := mon.Check(ctx, "app"); err == nil {
			t.Fatal("want error")
		}
		if err := mon.AcceptBaseline(ctx, "app"); err == nil {
			t.Fatal("want error")
		}
	})

	t.Run("store closed", func(t *testing.T) {
		t.Parallel()
		s, _ := newStore(t)
		mon, _ := newMonitor(t, s, notify.None{})
		s.pool.(interface{ Close() }).Close()
		mon.Tick(ctx) // SnapshotTargets fails, logged
	})
}

func TestTick_SweepsPlans(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newStore(t)
	mon, _ := newMonitor(t, s, notify.None{})
	mon.PlanRetention = 24 * time.Hour
	if err := s.SavePlan(ctx, storedPlan(planA), goodFiles()); err != nil {
		t.Fatal(err)
	}
	if err := s.SupersedePlan(ctx, planA, storedPlan(planB), goodFiles()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE cp_plans SET created_at = now() - interval '2 days'`); err != nil {
		t.Fatal(err)
	}

	mon.Tick(ctx)
	plans, err := s.ListPlans(ctx, "app", 0)
	if err != nil || len(plans) != 1 || plans[0].ID != planB || plans[0].State != PlanReady {
		t.Fatalf("plans = %+v, err = %v", plans, err)
	}

	s.pool.(interface{ Close() }).Close()
	mon.Tick(ctx)
}

func TestDriftMonitorRunLoop(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)
	mon, targetDSN := newMonitor(t, s, notify.None{})
	mon.interval = 50 * time.Millisecond

	execTarget(t, targetDSN, "CREATE TABLE looped (id int)")
	if err := mon.AcceptBaseline(context.Background(), "app"); err != nil {
		t.Fatal(err)
	}
	execTarget(t, targetDSN, "ALTER TABLE looped ADD COLUMN x int")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mon.Run(ctx)

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		events, err := s.ListDriftEvents(context.Background(), "app")
		if err == nil && len(events) == 1 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("drift never detected by the run loop")
}

func TestValidator(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s2, pool := newStore(t)
	if err := s2.RegisterTarget(ctx, "app", "plain", map[string]string{"dsn": "x"}); err != nil {
		t.Fatal(err)
	}
	v := NewValidator(pool, s2, func() string { return strings.ReplaceAll(uuid.NewString(), "-", "") })

	good, err := buildPlans([]engine.Migration{{
		Version: 1, Name: "ok",
		UpSQL: "CREATE TABLE v (id int);", DownSQL: "DROP TABLE v;",
	}}, engine.DirectionUp)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Validate(ctx, "app", good, ""); err != nil {
		t.Fatal(err)
	}

	bad, err := buildPlans([]engine.Migration{{
		Version: 1, Name: "bad",
		UpSQL: "SELECT 1/0;", DownSQL: "SELECT 1;",
	}}, engine.DirectionUp)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Validate(ctx, "app", bad, ""); err == nil || !strings.Contains(err.Error(), "failed validation") {
		t.Fatalf("err = %v", err)
	}

	// A pre-existing scratch database makes CREATE DATABASE fail.
	dup := NewValidator(pool, s2, func() string { return "dup" })
	if _, err := pool.Exec(ctx, "CREATE DATABASE godwit_validate_dup"); err != nil {
		t.Fatal(err)
	}
	if _, err := dup.Validate(ctx, "app", good, ""); err == nil || !strings.Contains(err.Error(), "create scratch database") {
		t.Fatalf("err = %v", err)
	}

	pool.Close()
	if _, err := v.Validate(ctx, "app", good, ""); err == nil || !strings.Contains(err.Error(), "history files") {
		t.Fatalf("err = %v", err)
	}
}

func TestValidatorReplay(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, pool := newStore(t)
	if err := s.RegisterTarget(ctx, "app", "plain", map[string]string{"dsn": "x"}); err != nil {
		t.Fatal(err)
	}
	v := NewValidator(pool, s, func() string { return "replay" })
	mock, err := pgxmock.NewConn()
	if err != nil {
		t.Fatal(err)
	}

	if err := v.Replay(ctx, mock, "app", "app,pg_catalog", nil); err == nil || !strings.Contains(err.Error(), "mirror search path") {
		t.Fatalf("search path err = %v", err)
	}

	plain, err := PlansFromFiles(map[string]string{
		"20260901120001_v.up.sql":   "CREATE TABLE v (id int);",
		"20260901120001_v.down.sql": "DROP TABLE v;",
	}, engine.DirectionUp)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Replay(ctx, mock, "app", "", plain); !errors.Is(err, ErrValidationFailed) {
		t.Fatalf("apply err = %v", err)
	}

	directive, err := PlansFromFiles(map[string]string{
		"20260901120002_d.up.sql":   "-- godwit: drop-column public.v.id\n",
		"20260901120002_d.down.sql": "-- godwit: revert\n",
	}, engine.DirectionUp)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Replay(ctx, mock, "app", "", directive); err == nil || errors.Is(err, ErrValidationFailed) {
		t.Fatalf("expand err = %v", err)
	}

	pool.Close()
	if err := v.Replay(ctx, mock, "app", "", nil); err == nil || !strings.Contains(err.Error(), "history files") {
		t.Fatalf("history err = %v", err)
	}
}
