package controlplane

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestHistoryReplaysRunsInCreationOrder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, pool := newStore(t)
	if err := s.RegisterTarget(ctx, "app", "static", map[string]string{}); err != nil {
		t.Fatal(err)
	}

	first, second := "ffffffff-0000-0000-0000-000000000001", "00000000-0000-0000-0000-000000000002"
	queueRun(t, s, first, map[string]string{
		"20260901120001_t.up.sql":   "CREATE TABLE public.t (id int);",
		"20260901120001_t.down.sql": "DROP TABLE public.t;",
	})
	ledger(t, s, first, "20260901120001_t")
	if err := s.Finish(ctx, first, StateSucceeded, ""); err != nil {
		t.Fatal(err)
	}
	queueRun(t, s, second, map[string]string{
		"20260901120002_a.up.sql":   "ALTER TABLE public.t ADD COLUMN a int;",
		"20260901120002_a.down.sql": "ALTER TABLE public.t DROP COLUMN a;",
	})
	ledger(t, s, second, "20260901120002_a")
	if err := s.Finish(ctx, second, StateSucceeded, ""); err != nil {
		t.Fatal(err)
	}
	// One statement stamps both runs with its own transaction timestamp: two runs queued in the same tick.
	if _, err := pool.Exec(ctx, `UPDATE cp_runs SET created_at = now() WHERE target = 'app'`); err != nil {
		t.Fatal(err)
	}

	history, err := s.History(ctx, "app")
	if err != nil || len(history) != 2 || history[0].Migrations[0].ID != "20260901120001_t" {
		t.Fatalf("history = %+v, err = %v", history, err)
	}
	if _, err := NewValidator(NewScratch(pool, ""), s, uuid.NewString).Validate(ctx, "app", nil, ""); err != nil {
		t.Fatalf("replay of runs sharing one created_at: %v", err)
	}

	last, ok, err := s.LastRun(ctx, "app")
	if err != nil || !ok || last.ID != second {
		t.Fatalf("last run = %+v, ok = %t, err = %v", last, ok, err)
	}
	runs, err := s.ListRuns(ctx, "app")
	if err != nil || len(runs) != 2 || runs[0].ID != second {
		t.Fatalf("runs = %+v, err = %v", runs, err)
	}
	targets, err := s.ListTargets(ctx, time.Time{})
	if err != nil || targets[0].LastRun == nil || targets[0].LastRun.ID != second {
		t.Fatalf("targets = %+v, err = %v", targets, err)
	}
	if err := s.CreateRevert(ctx, uuid.NewString(), runs[1], false, Timeouts{}, Provenance{}); err == nil {
		t.Fatal("a run another one stands on top of is not the target's newest")
	}
}

func TestAppliedCountKeepsRepeatablesApart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newStore(t)
	if err := s.RegisterTarget(ctx, "app", "static", map[string]string{}); err != nil {
		t.Fatal(err)
	}

	id := uuid.NewString()
	queueRun(t, s, id, map[string]string{
		"20260901120001_t.up.sql":       "CREATE TABLE public.t (id int);",
		"20260901120001_t.down.sql":     "DROP TABLE public.t;",
		"R__order_statistics.up.sql":    "CREATE OR REPLACE VIEW public.s AS SELECT count(*) FROM public.t;",
		"R__order_statistics.down.sql":  "DROP VIEW IF EXISTS public.s;",
		"R__order_statistical.up.sql":   "CREATE OR REPLACE VIEW public.l AS SELECT min(id) FROM public.t;",
		"R__order_statistical.down.sql": "DROP VIEW IF EXISTS public.l;",
	})
	ledger(t, s, id, "20260901120001_t", "R__order_statistics", "R__order_statistical")

	applied, err := s.Applied(ctx, "app")
	if err != nil || len(applied.Versions) != 1 || len(applied.Repeatables) != 2 {
		t.Fatalf("applied = %+v, err = %v", applied, err)
	}
	targets, err := s.ListTargets(ctx, time.Time{})
	if want := len(applied.Versions) + len(applied.Repeatables); err != nil || targets[0].AppliedCount != want {
		t.Fatalf("applied count = %d, want %d: repeatable names share no version prefix", targets[0].AppliedCount, want)
	}
}
