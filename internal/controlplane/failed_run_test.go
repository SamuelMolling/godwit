package controlplane

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/SamuelMolling/godwit/internal/creds"
)

func halfFailingFiles() map[string]string {
	files := twoTableFiles()
	files["20260101000002_c.up.sql"] = "SELECT 1/0;"
	files["20260101000002_c.down.sql"] = "SELECT 1;"

	return files
}

func twoTableFiles() map[string]string {
	return map[string]string{
		"20260101000000_a.up.sql":   "CREATE TABLE public.a (id int);",
		"20260101000000_a.down.sql": "DROP TABLE public.a;",
		"20260101000001_b.up.sql":   "CREATE TABLE public.b (id int);",
		"20260101000001_b.down.sql": "DROP TABLE public.b;",
	}
}

// failedRun records a run that applied some of its migrations and then failed, the way the scheduler
// leaves the store when a statement errors part way through.
func failedRun(t *testing.T, s *Store, target string, files map[string]string, exps map[string]Expansion, applied ...string) string {
	t.Helper()
	ctx := context.Background()
	id := uuid.NewString()
	if err := s.CreateRun(ctx, id, target, RolloutExpandContract, files, Timeouts{}, Provenance{}, "", exps); err != nil {
		t.Fatal(err)
	}
	for _, m := range applied {
		var exp *Expansion
		if e, ok := exps[m]; ok {
			exp = &e
		}
		if err := s.RecordApplied(ctx, id, m, false, exp); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Finish(ctx, id, StateFailed, "statement 0 of "+applied[0]+" (up): boom"); err != nil {
		t.Fatal(err)
	}

	return id
}

// A run that applies two migrations and then fails on a third leaves the two standing on the target;
// the control plane must account for them, not act as if the run had done nothing.
func TestFailedRunKeepsWhatItApplied(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, pool := newStore(t)
	sched, targetDSN := newScheduler(t, s, Config{Holder: "h1", MaxAttempts: 1})

	id := "6b6b6b6b-0000-0000-0000-000000000001"
	queueRun(t, s, id, halfFailingFiles())
	sched.Tick(ctx)
	waitState(t, s, id, StateFailed)

	if !tableExists(t, targetDSN, "public.a") || !tableExists(t, targetDSN, "public.b") {
		t.Fatal("the failed run's first two migrations stand on the target")
	}
	if got := targetValue(t, targetDSN, "SELECT count(*)::text FROM godwit.migrations"); got != "2" {
		t.Fatalf("godwit.migrations = %s, want the two the run applied", got)
	}

	applied, err := s.Applied(ctx, "app")
	if err != nil || !slices.Equal(applied.Versions, []int64{20260101000000, 20260101000001}) {
		t.Fatalf("applied = %+v, err = %v", applied, err)
	}
	targets, err := s.ListTargets(ctx, time.Time{})
	if err != nil || len(targets) != 1 || targets[0].AppliedCount != 2 {
		t.Fatalf("targets = %+v, err = %v", targets, err)
	}

	v := NewValidator(pool, s, uuid.NewString)
	val, err := v.Validate(ctx, "app", upPlans(t, twoTableFiles()), "")
	if err != nil {
		t.Fatalf("validating over a failed run's history: %v", err)
	}
	if !strings.Contains(val.Base, "public.a.id") || !strings.Contains(val.Base, "public.b.id") {
		t.Fatalf("the replay must rebuild what the failed run applied: %s", val.Base)
	}
	for i, effects := range val.Effects {
		if len(effects) != 0 {
			t.Fatalf("migration %d is already applied and must leave the scratch alone: %v", i, effects)
		}
	}

	rp, err := s.PlanRevert(ctx, id)
	if err != nil || len(rp.Plans) != 2 {
		t.Fatalf("a failed run's applied migrations stay revertable: %+v, err = %v", rp, err)
	}
}

// #59 expands a directive once. A failed run's directive is applied, so a later validation must replay
// it from the ledger instead of generating a second expansion against a catalog that already has it.
func TestFailedRunDoesNotReexpandItsDirective(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, v := replayFixture(t, "app")
	files := withShops(directiveFiles("change-type public.people.age bigint batch=500", revertDown))
	plans := upPlans(t, files)

	val, err := v.Validate(ctx, "app", plans, "")
	if err != nil || len(val.Expansions) != 1 {
		t.Fatalf("expansions = %+v, err = %v", val.Expansions, err)
	}
	failedRun(t, s, "app", files, val.Expansions, "20260102000000_d")

	again, err := v.Validate(ctx, "app", plans, "")
	if err != nil {
		t.Fatalf("validating after the failed run: %v", err)
	}
	if len(again.Expansions) != 0 {
		t.Fatalf("a directive a failed run applied must not be expanded again: %+v", again.Expansions)
	}
	if len(again.Plans[1].Statements) != 0 {
		t.Fatalf("statements = %+v", again.Plans[1].Statements)
	}
	if len(again.Effects[2]) == 0 {
		t.Fatal("the migration the failed run never reached is still pending")
	}
}

// A held row is the other half of the rule: the run applied the migration's expand phase, but the target
// records nothing until the contract phase lands, so neither the applied set nor the replay may claim it.
// The contract phase failing leaves such a row on a run that is not awaiting_contract.
func TestHeldMigrationIsNotAppliedUntilItsContractPhase(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, pool := newStore(t)
	targetDSN := newDatabase(t, "tg")
	if err := s.RegisterTarget(ctx, "app", "plain", map[string]string{"dsn": targetDSN}); err != nil {
		t.Fatal(err)
	}
	policies := Policies()
	policies["phased"] = phaseFrom{at: 2}
	sched := NewScheduler(s, plainProviders(), PGEngine{}, policies, Config{Holder: "h1", MaxAttempts: 1}, testLog)

	id := "6b6b6b6b-0000-0000-0000-000000000002"
	if err := s.CreateRun(ctx, id, "app", "phased", map[string]string{
		"20260101000000_t.up.sql":   "CREATE TABLE public.t (id int);\nCREATE TABLE public.u (id int);\nSELECT 1/0;",
		"20260101000000_t.down.sql": "DROP TABLE public.u, public.t;",
	}, Timeouts{}, Provenance{}, "", nil); err != nil {
		t.Fatal(err)
	}

	sched.Tick(ctx)
	waitState(t, s, id, StateAwaitingContract)
	assertHeldIsNotApplied(t, s, pool, id)

	if _, err := s.Confirm(ctx, id); err != nil {
		t.Fatal(err)
	}
	sched.Tick(ctx)
	waitState(t, s, id, StateFailed)
	assertHeldIsNotApplied(t, s, pool, id)
}

func plainProviders() map[string]creds.Provider {
	return map[string]creds.Provider{"plain": plainProvider{}}
}

func assertHeldIsNotApplied(t *testing.T, s *Store, pool *pgxpool.Pool, id string) {
	t.Helper()
	ctx := context.Background()
	rows, err := s.AppliedMigrations(ctx, id)
	if err != nil || len(rows) != 1 || !rows[0].Held {
		t.Fatalf("ledger = %+v, err = %v", rows, err)
	}
	applied, err := s.Applied(ctx, "app")
	if err != nil || len(applied.Versions) != 0 {
		t.Fatalf("a held migration is not in the target's history: %+v, err = %v", applied, err)
	}
	history, err := s.History(ctx, "app")
	if err != nil || len(history) != 0 {
		t.Fatalf("a held migration must never reach the replay: %+v, err = %v", history, err)
	}
	targets, err := s.ListTargets(ctx, time.Time{})
	if err != nil || targets[0].AppliedCount != 0 {
		t.Fatalf("targets = %+v, err = %v", targets, err)
	}
	val, err := NewValidator(pool, s, uuid.NewString).Validate(ctx, "app", nil, "")
	if err != nil || strings.Contains(val.Base, "public.u") {
		t.Fatalf("base = %q, err = %v", val.Base, err)
	}
	rp, err := s.PlanRevert(ctx, id)
	if err != nil || len(rp.Plans) != 1 {
		t.Fatalf("a held migration is still what the run applied, so a revert undoes it: %+v, err = %v", rp, err)
	}
}
