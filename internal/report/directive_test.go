package report

import (
	"strings"
	"testing"
	"time"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/internal/engine"
)

func expandedReport() Plan {
	batch := &engine.BatchSpec{Key: `"id"`, KeyKind: engine.BatchKeyInt, Size: 5000, Pause: 100 * time.Millisecond}

	return Plan{
		live: true, target: "app", rollout: "expand-contract", validated: true,
		items: []item{{
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
		"20260901130000_age  4 statements, expand then contract phases, written by a directive\n",
		"  -- godwit: change-type users.age bigint",
		"  statement 1, runs in batches over \"id\" (int), 5000 rows per transaction, pausing 100ms\n      UPDATE public.users SET age_new = age;\n",
		"  statement 2, runs inside a transaction, contract phase\n      ALTER TABLE public.users RENAME COLUMN age TO age_old;\n",
		"  statement 3, runs outside a transaction\n      SELECT 1;\n",
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
	if !strings.Contains(b.String(), `runs in batches over "id" (int), 100 rows per transaction`+"\n") {
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
	if !strings.Contains(b.String(), "2 statements, written by a directive\n") {
		t.Fatalf("one-phase expansion:\n%s", b.String())
	}
}

func TestPlanMarkdownRendersTheExpansion(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	writePlanMarkdown(&b, expandedReport())
	out := b.String()
	for _, want := range []string{
		"```diff\n+ 20260901130000_age  4 statements, expand then contract phases, written by a directive\n```\n",
		"### `20260901130000_age`\n\n```sql\n-- godwit: change-type users.age bigint\n```\n",
		"`statement 0`, runs inside a transaction\n\n```sql\nALTER TABLE public.users ADD COLUMN age_new bigint;\n```\n",
		"`statement 2`, runs inside a transaction, contract phase\n\n```sql\nALTER TABLE public.users RENAME COLUMN age TO age_old;\n```\n",
		"\nnote: leaves public.users.age_old for rollback\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

func TestRenderersShowTheStatementUnderTheExpandedMarker(t *testing.T) {
	t.Parallel()
	marked := engine.ExpandedMarker + "change-type users.age bigint"
	r := expandedReport()
	r.items[0].Statements[0].SQL = marked + "\nALTER TABLE public.users ADD COLUMN age_new bigint"
	r.items[0].Statements[3].Assert = &engine.AssertSpec{Op: "=", Kind: "num", Value: "0"}
	r.items[0].Statements[3].SQL = engine.ExpandedMarker + "assert 'SELECT 1' = 0\nSELECT 1"

	var b strings.Builder
	writePlanText(&b, r)
	out := b.String()
	for _, want := range []string{
		"  statement 0, runs inside a transaction\n      " + marked + "\n      ALTER TABLE public.users ADD COLUMN age_new bigint;\n",
		"  statement 3, runs as a check, the result must be = 0\n      " + engine.ExpandedMarker + "assert 'SELECT 1' = 0\n      SELECT 1;\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}

	b.Reset()
	writePlanMarkdown(&b, r)
	if md := b.String(); !strings.Contains(md,
		"```sql\n"+marked+"\nALTER TABLE public.users ADD COLUMN age_new bigint;\n```\n") {
		t.Fatalf("the block must carry the marker and the statement:\n%s", md)
	}

	b.Reset()
	writePlanJSON(&b, r)
	if !strings.Contains(b.String(), `-- godwit expanded: change-type users.age bigint\nALTER TABLE`) {
		t.Fatalf("json must keep both lines:\n%s", b.String())
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
	r := PlanFromProto(&godwitv1.PlanRunResponse{
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

func assertReport() Plan {
	r := expandedReport()
	r.items[0].Statements = []engine.Statement{
		{SQL: "SELECT Count(*) FROM orders WHERE total IS NULL", Phase: engine.PhaseExpand, Assert: &engine.AssertSpec{
			Op: "=", Kind: engine.AssertInt, Value: "0",
		}},
	}
	r.items[0].directives = []string{"-- godwit: assert 'SELECT Count(*) FROM orders WHERE total IS NULL' = 0"}

	return r
}

func TestPlanRendersTheAssertion(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	writePlanText(&b, assertReport())
	if want := "  statement 0, runs as a check, the result must be = 0\n      SELECT Count(*) FROM orders WHERE total IS NULL;\n"; !strings.Contains(b.String(), want) {
		t.Fatalf("missing %q in:\n%s", want, b.String())
	}
	b.Reset()
	writePlanMarkdown(&b, assertReport())
	if !strings.Contains(b.String(),
		"`statement 0`, runs as a check, the result must be = 0\n\n```sql\nSELECT Count(*) FROM orders WHERE total IS NULL;\n```\n") {
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
	r := PlanFromProto(&godwitv1.PlanRunResponse{
		Target: "app", Migrations: []*godwitv1.PlannedMigration{{
			Version: 1, Name: "m", Expanded: true,
			Statements: []*godwitv1.PlannedStatement{{
				Sql: "SELECT Count(*) FROM t", Phase: "expand",
				Assert: &godwitv1.PlannedAssert{Op: ">", Kind: "int", Value: "0"},
			}},
		}},
	})
	st := r.items[0].Statements[0]
	if st.Assert == nil || st.Assert.Op != ">" || st.Assert.Value != "0" || st.Assert.Kind != "int" {
		t.Fatalf("statement = %+v", st)
	}
}
