package controlplane

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/SamuelMolling/godwit/internal/creds"
)

func registerTargetDB(t *testing.T, s *Store, name string) {
	t.Helper()
	if err := s.RegisterTarget(context.Background(), name, "plain",
		map[string]string{"dsn": newDatabase(t, "tg")}); err != nil {
		t.Fatal(err)
	}
}

func queueRunOn(t *testing.T, s *Store, id, target string, files map[string]string) {
	t.Helper()
	if err := s.CreateRun(context.Background(), id, target, RolloutDirect, files, Timeouts{}, Provenance{}, "", nil); err != nil {
		t.Fatal(err)
	}
}

func sleepFiles(seconds string) map[string]string {
	return map[string]string{
		"20260901120000_t.up.sql":   "CREATE TABLE t (id int);\nSELECT pg_sleep(" + seconds + ");",
		"20260901120000_t.down.sql": "DROP TABLE t;",
	}
}

// A run that takes minutes used to hold the replica's only slot: Tick executed inline on the ticker
// goroutine, so nothing was claimed for any other target until it finished.
func TestSlowRunDoesNotStarveAnotherTarget(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)
	registerTargetDB(t, s, "slow")
	registerTargetDB(t, s, "quick")
	sched := NewScheduler(s, map[string]creds.Provider{"plain": plainProvider{}}, PGEngine{}, Policies(),
		Config{Holder: "h1", Interval: 20 * time.Millisecond, MaxConcurrentRuns: 2}, testLog)

	const slowID, quickID = "cccccccc-0000-0000-0000-000000000001", "cccccccc-0000-0000-0000-000000000002"
	queueRunOn(t, s, slowID, "slow", sleepFiles("30"))
	queueRunOn(t, s, quickID, "quick", goodFiles())

	ctx, cancel := context.WithCancel(context.Background())
	go sched.Run(ctx)

	waitState(t, s, quickID, StateSucceeded)
	slow, err := s.Run(context.Background(), slowID)
	if err != nil {
		t.Fatal(err)
	}
	if slow.State != StateRunning {
		t.Fatalf("the slow run should still be going while the other target finished: %+v", slow)
	}
	cancel()
}

// dispatch never starts more than MaxConcurrentRuns at once, and Run waits for what it started.
func TestDispatchRespectsTheSlotLimit(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)
	registerTargetDB(t, s, "app")
	sched := NewScheduler(s, map[string]creds.Provider{"plain": plainProvider{}}, PGEngine{}, Policies(),
		Config{Holder: "h1", Interval: time.Hour, MaxConcurrentRuns: 1}, testLog)

	sched.slots <- struct{}{}
	queueRunOn(t, s, "cccccccc-0000-0000-0000-000000000003", "app", goodFiles())
	sched.dispatch(context.Background())
	sched.inflight.Wait()

	r, err := s.Run(context.Background(), "cccccccc-0000-0000-0000-000000000003")
	if err != nil {
		t.Fatal(err)
	}
	if r.State != StateQueued {
		t.Fatalf("a full replica must claim nothing: %+v", r)
	}
	<-sched.slots

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sched.Run(ctx)
}

// Without a deadline a run holds its slot for as long as its statements take, which a submitted
// migration controls through batch=, pause= and statement_timeout: "0".
func TestRunTimeoutFinishesTheRun(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)
	registerTargetDB(t, s, "app")
	sched := NewScheduler(s, map[string]creds.Provider{"plain": plainProvider{}}, PGEngine{}, Policies(),
		Config{Holder: "h1", RunTimeout: 300 * time.Millisecond}, testLog)

	const id = "cccccccc-0000-0000-0000-000000000004"
	queueRunOn(t, s, id, "app", sleepFiles("30"))
	sched.Tick(context.Background())

	r, err := s.Run(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if r.State == StateSucceeded || !strings.Contains(r.Error, "context deadline exceeded") {
		t.Fatalf("run = %+v", r)
	}
}

func TestSchedulerConfigDefaults(t *testing.T) {
	t.Parallel()

	c := Config{}.withDefaults()
	if c.MaxConcurrentRuns != DefaultMaxConcurrentRuns || c.RunTimeout != DefaultRunTimeout {
		t.Fatalf("defaults = %+v", c)
	}
}
