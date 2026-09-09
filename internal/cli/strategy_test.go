package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/SamuelMolling/godwit/internal/config"
	"github.com/SamuelMolling/godwit/internal/engine"
)

func concurrentPlan() planReport {
	return planReport{
		live: true, target: "app", rollout: "direct", validated: true, format: config.PlanFormatSchema,
		items: []planItem{
			{skipped: true, Plan: engine.Plan{
				Migration:  engine.Migration{Version: 20260910100000, Name: "done"},
				Statements: []engine.Statement{{SQL: "SELECT 1", NoTx: true}},
			}},
			{Plan: engine.Plan{
				Migration: engine.Migration{Version: 20260910120000, Name: "reshape"},
				Direction: engine.DirectionUp,
				Statements: []engine.Statement{
					{SQL: "CREATE INDEX CONCURRENTLY a_idx ON public.a (v)", NoTx: true},
					{SQL: "REINDEX INDEX CONCURRENTLY b_idx", NoTx: true},
					{
						SQL:   "UPDATE public.a SET v = 1",
						Batch: &engine.BatchSpec{Key: "id", KeyKind: "bigint", Size: 2000, Pause: 250 * time.Millisecond},
					},
					{
						SQL:   "UPDATE public.b SET v = 1",
						Batch: &engine.BatchSpec{Key: "id", KeyKind: "bigint", Size: 2000},
					},
					{SQL: "SELECT count(*) FROM public.a", Assert: &engine.AssertSpec{Op: "eq", Kind: "int", Value: "0"}},
					{SQL: "SELECT count(*) FROM public.b", Assert: &engine.AssertSpec{Op: "eq", Kind: "int", Value: "0"}},
					{
						SQL: "ALTER TABLE public.a ADD CONSTRAINT a_chk CHECK (v > 0)",
						Hazards: []engine.Hazard{
							{Code: "H006", Detail: "ADD CONSTRAINT CHECK scans the whole table under lock; add it NOT VALID", Object: "public.a"},
							{Code: "H002", Detail: "DROP TABLE is destructive", Object: "public.gone"},
						},
					},
					{
						SQL:     "CREATE INDEX c_idx ON public.c (v)",
						Hazards: []engine.Hazard{{Code: "H001", Detail: "CREATE INDEX without CONCURRENTLY blocks writes on c", Object: "public.c"}},
					},
				},
			}},
		},
	}
}

func TestStrategyDescribesEveryShapeTheRunTakes(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	writePlanText(&b, concurrentPlan())
	for _, want := range []string{
		"8 statements run one at a time, in the order this report lists them, each committing with its own journal row",
		"2 of them cannot run inside a transaction, because PostgreSQL refuses them there",
		"2 statements do not run as one statement at all: godwit walks the table by id in batches of 2000 rows," +
			" pausing 250ms; by id in batches of 2000 rows and commits each batch",
		"2 statements change nothing: they are conditions the migration declared",
		"2 statements hold a lock the rest of the application queues behind while they run: public.a: ADD" +
			" CONSTRAINT CHECK scans the whole table under lock (H006); public.c: CREATE INDEX without" +
			" CONCURRENTLY blocks writes on c (H001).",
		"An index built CONCURRENTLY that failed leaves an INVALID index behind",
	} {
		if !strings.Contains(b.String(), want) {
			t.Fatalf("missing %q in:\n%s", want, b.String())
		}
	}
	if lock := concurrentPlan().strategy(terminal)[4]; strings.Contains(lock, "H002") {
		t.Fatalf("a destructive hazard is not a lock hazard: %s", lock)
	}
}

func TestStrategyIsSilentWithoutALiveTargetOrAStatement(t *testing.T) {
	t.Parallel()
	if got := (planReport{}).strategy(terminal); got != nil {
		t.Fatalf("an offline plan says nothing about how a run would go: %v", got)
	}
	empty := planReport{live: true, target: "app", items: []planItem{{skipped: true}}}
	if got := empty.strategy(terminal); got != nil {
		t.Fatalf("nothing to run is nothing to describe: %v", got)
	}
}
