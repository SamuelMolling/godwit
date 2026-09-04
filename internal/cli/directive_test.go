package cli

import (
	"strings"
	"testing"
	"time"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/internal/engine"
)

func expandedReport() planReport {
	batch := &engine.BatchSpec{Key: `"id"`, KeyKind: engine.BatchKeyInt, Size: 5000, Pause: 100 * time.Millisecond}

	return planReport{
		live: true, target: "app", rollout: "expand-contract", validated: true,
		items: []planItem{{
			Plan: engine.Plan{
				Migration: engine.Migration{Version: 20260901130000, Name: "age", Checksum: "c"},
				Direction: engine.DirectionUp,
				Statements: []engine.Statement{
					{SQL: "ALTER TABLE public.users ADD COLUMN age_new bigint", Phase: engine.PhaseExpand},
					{SQL: "UPDATE public.users SET age_new = age", Phase: engine.PhaseExpand, Batch: batch},
					{SQL: "ALTER TABLE public.users RENAME COLUMN age TO age_old", Phase: engine.PhaseContract},
					{SQL: "SELECT 1", NoTx: true},
				},
			},
			phase:      "expand",
			directives: []string{"-- godwit: change-type users.age bigint"},
			expanded:   true,
			notes:      []string{"leaves public.users.age_old for rollback"},
		}},
	}
}

func TestPlanTextRendersTheExpansion(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	writePlanText(&b, expandedReport())
	out := b.String()
	for _, want := range []string{
		"directive, expand 3 / contract 1",
		"  -- godwit: change-type users.age bigint",
		`[1] batch UPDATE public.users SET age_new = age   [expand]`,
		`batch over "id" (int), 5000 rows per transaction, pausing 100ms`,
		"[2] tx    ALTER TABLE public.users RENAME COLUMN age TO age_old   [contract]",
		"[3] no-tx SELECT 1\n",
		"note: leaves public.users.age_old for rollback",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}

	r := expandedReport()
	r.items[0].Statements[1].Batch = &engine.BatchSpec{Key: `"id"`, KeyKind: engine.BatchKeyInt, Size: 100}
	b.Reset()
	writePlanText(&b, r)
	if !strings.Contains(b.String(), `batch over "id" (int), 100 rows per transaction
`) &&
		!strings.Contains(b.String(), `batch over "id" (int), 100 rows per transaction`+"\n") {
		t.Fatalf("no pause:\n%s", b.String())
	}
	b.Reset()
	writePlanJSON(&b, r)
	if strings.Contains(b.String(), `"pause"`) {
		t.Fatalf("a zero pause must be omitted:\n%s", b.String())
	}

	r = expandedReport()
	r.items[0].Statements = r.items[0].Statements[:2]
	b.Reset()
	writePlanText(&b, r)
	if !strings.Contains(b.String(), "directive, expanded\n") {
		t.Fatalf("one-phase expansion:\n%s", b.String())
	}
}

func TestPlanMarkdownRendersTheExpansion(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	writePlanMarkdown(&b, expandedReport())
	out := b.String()
	for _, want := range []string{
		"<details><summary>expansion of <code>-- godwit: change-type users.age bigint</code> (4 statements)</summary>",
		"-- expand\nALTER TABLE public.users ADD COLUMN age_new bigint;",
		"-- contract\nALTER TABLE public.users RENAME COLUMN age TO age_old;",
		"note: leaves public.users.age_old for rollback",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

func TestPlanJSONRendersTheExpansion(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	writePlanJSON(&b, expandedReport())
	out := b.String()
	for _, want := range []string{
		`"phase":"expand"`,
		`"batch":{"key":"\"id\"","kind":"int","size":5000,"pause":"100ms"}`,
		`"phase":"contract"`,
		`"expanded":true`,
		`"directives":["-- godwit: change-type users.age bigint"]`,
		`"notes":["leaves public.users.age_old for rollback"]`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

func TestPlanReportFromProtoCarriesBatches(t *testing.T) {
	t.Parallel()
	r := planReportFromProto(&godwitv1.PlanRunResponse{
		Target: "app", Migrations: []*godwitv1.PlannedMigration{{
			Version: 1, Name: "m", Expanded: true, Directives: []string{"-- godwit: backfill t set='a = 1'"},
			Notes: []string{"n"},
			Statements: []*godwitv1.PlannedStatement{{
				Sql: "UPDATE t SET a = 1", Phase: "expand",
				Batch: &godwitv1.PlannedBatch{Key: "id", Kind: "int", Size: 100, Pause: "50ms"},
			}},
		}},
	})
	st := r.items[0].Statements[0]
	if st.Batch == nil || st.Batch.Size != 100 || st.Batch.Pause != 50*time.Millisecond || st.Phase != "expand" {
		t.Fatalf("statement = %+v", st)
	}
	if !r.items[0].expanded || len(r.items[0].directives) != 1 || len(r.items[0].notes) != 1 {
		t.Fatalf("item = %+v", r.items[0])
	}
}

func TestRunLineShowsBackfillProgress(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		progress *godwitv1.RunProgress
		want     string
	}{
		{nil, "run r1: running"},
		{&godwitv1.RunProgress{Migration: "m", Statement: 2}, "[statement 2 of m]"},
		{&godwitv1.RunProgress{Batches: 3, RowsDone: 30}, "[backfill 30 rows (batch 3)]"},
		{&godwitv1.RunProgress{Batches: 64, RowsDone: 320000, RowsTotal: 1240000}, "[backfill 320000/~1240000 rows (batch 64)]"},
	} {
		got := runLine(&godwitv1.Run{Id: "r1", State: godwitv1.RunState_RUN_STATE_RUNNING, Progress: tc.progress})
		if !strings.Contains(got, tc.want) {
			t.Fatalf("line = %q, want %q", got, tc.want)
		}
	}
}

func TestTargetAddKeepOldFlag(t *testing.T) {
	t.Parallel()
	stub := &stubService{}
	url := startStub(t, stub)

	if code, _, errOut := runCLI("target", "add", "app", "--server", url, "--provider", "static", "--dsn", "x", "--keep-old=false"); code != 0 {
		t.Fatalf("code = %d, err = %s", code, errOut)
	}
	if got := stub.registered.KeepOld; got == nil || *got {
		t.Fatalf("keep_old = %v", got)
	}
	if code, _, errOut := runCLI("target", "add", "app", "--server", url, "--provider", "static", "--dsn", "x"); code != 0 {
		t.Fatalf("code = %d, err = %s", code, errOut)
	}
	if got := stub.registered.KeepOld; got != nil {
		t.Fatalf("an untouched flag must leave the target's default alone: %v", *got)
	}
}

func assertReport() planReport {
	r := expandedReport()
	r.items[0].Statements = []engine.Statement{
		{SQL: "SELECT count(*) FROM orders WHERE total IS NULL", Phase: engine.PhaseExpand, Assert: &engine.AssertSpec{
			Op: "=", Kind: engine.AssertInt, Value: "0",
		}},
	}
	r.items[0].directives = []string{"-- godwit: assert 'SELECT count(*) FROM orders WHERE total IS NULL' = 0"}

	return r
}

func TestPlanRendersTheAssertion(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	writePlanText(&b, assertReport())
	for _, want := range []string{
		"[0] assert SELECT count(*) FROM orders WHERE total IS NULL   [expand]",
		"        the result must be = 0",
	} {
		if !strings.Contains(b.String(), want) {
			t.Fatalf("missing %q in:\n%s", want, b.String())
		}
	}
	b.Reset()
	writePlanMarkdown(&b, assertReport())
	if !strings.Contains(b.String(), "| assert | `SELECT count(*) FROM orders WHERE total IS NULL` must be `= 0` |") {
		t.Fatalf("markdown:\n%s", b.String())
	}
	b.Reset()
	writePlanJSON(&b, assertReport())
	if !strings.Contains(b.String(), `"assert":{"op":"=","kind":"int","value":"0"}`) {
		t.Fatalf("json:\n%s", b.String())
	}
}

func TestPlanReportFromProtoCarriesAssertions(t *testing.T) {
	t.Parallel()
	r := planReportFromProto(&godwitv1.PlanRunResponse{
		Target: "app", Migrations: []*godwitv1.PlannedMigration{{
			Version: 1, Name: "m", Expanded: true,
			Statements: []*godwitv1.PlannedStatement{{
				Sql: "SELECT count(*) FROM t", Phase: "expand",
				Assert: &godwitv1.PlannedAssert{Op: ">", Kind: "int", Value: "0"},
			}},
		}},
	})
	st := r.items[0].Statements[0]
	if st.Assert == nil || st.Assert.Op != ">" || st.Assert.Value != "0" || st.Assert.Kind != "int" {
		t.Fatalf("statement = %+v", st)
	}
}
