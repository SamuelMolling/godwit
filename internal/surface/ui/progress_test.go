package ui

import (
	"net/http"
	"testing"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
)

func backfilling(p *godwitv1.RunProgress) *stub {
	s := fixture()
	for _, r := range s.runs {
		if r.Id == "r-run-00001" {
			r.Progress = p
		}
	}

	return s
}

func TestRunPageBackfill(t *testing.T) {
	t.Parallel()
	s := backfilling(&godwitv1.RunProgress{
		Migration: "20260901190000_quantity", Statement: 4, Phase: "expand",
		RowsDone: 12000, RowsTotal: 200000, Batches: 60,
	})
	h := newUI(s, Config{User: "sam", Password: "pw"})

	rec := do(h, http.MethodGet, "/ui/runs/r-run-00001", nil, "Authorization", basic("sam", "pw"))
	want(t, rec, http.StatusOK,
		"<h3>Backfill</h3>",
		`<i style="width:6%"></i>`,
		"<b>12,000</b> rows written of ~200,000 estimated",
		"≈6%",
		`<span class="chip">60 batches</span>`,
		"20260901190000_quantity</span> statement 4 · expand",
		"the total is the planner's estimate for the table",
		"Running, attempt 1: statement 4 of 20260901190000_quantity (expand)",
	)
}

func TestRunPageBackfillWithoutEstimate(t *testing.T) {
	t.Parallel()
	s := backfilling(&godwitv1.RunProgress{Migration: "m", Statement: 4, RowsDone: 900, Batches: 1})
	h := newUI(s, Config{User: "sam", Password: "pw"})

	rec := do(h, http.MethodGet, "/ui/runs/r-run-00001", nil, "Authorization", basic("sam", "pw"))
	want(t, rec, http.StatusOK, "<b>900</b> rows written", `<span class="chip">1 batch</span>`, "m</span> statement 4.")
	absent(t, rec, "estimated", "class=\"bar\"", "≈")
}

func TestRunPageBackfillCapsAtHundred(t *testing.T) {
	t.Parallel()
	s := backfilling(&godwitv1.RunProgress{Migration: "m", RowsDone: 300, RowsTotal: 100, Batches: 3})
	h := newUI(s, Config{User: "sam", Password: "pw"})

	rec := do(h, http.MethodGet, "/ui/runs/r-run-00001", nil, "Authorization", basic("sam", "pw"))
	want(t, rec, http.StatusOK, `<i style="width:100%"></i>`, "≈100%")
}

func TestRunPageNoBackfillBlock(t *testing.T) {
	t.Parallel()
	for name, p := range map[string]*godwitv1.RunProgress{
		"none":      nil,
		"not batch": {Migration: "m", Statement: 2, Phase: "expand"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newUI(backfilling(p), Config{User: "sam", Password: "pw"})

			rec := do(h, http.MethodGet, "/ui/runs/r-run-00001", nil, "Authorization", basic("sam", "pw"))
			want(t, rec, http.StatusOK, "Running, attempt 1")
			absent(t, rec, "<h3>Backfill</h3>")
		})
	}
}

// A finished run keeps the progress of its last statement; it is history, not something still moving.
func TestSettledRunShowsNoBackfill(t *testing.T) {
	t.Parallel()
	s := fixture()
	for _, r := range s.runs {
		if r.Id == "r-ok-000001" {
			r.Progress = &godwitv1.RunProgress{Migration: "m", RowsDone: 12000, RowsTotal: 200000, Batches: 60}
		}
	}
	h := newUI(s, Config{User: "sam", Password: "pw"})

	rec := do(h, http.MethodGet, "/ui/runs/r-ok-000001", nil, "Authorization", basic("sam", "pw"))
	want(t, rec, http.StatusOK, "Succeeded")
	absent(t, rec, "<h3>Backfill</h3>", "12,000")
}

func TestRunsListShowsBackfill(t *testing.T) {
	t.Parallel()
	s := backfilling(&godwitv1.RunProgress{Migration: "m", RowsDone: 12000, RowsTotal: 200000, Batches: 60})
	h := newUI(s, Config{User: "sam", Password: "pw"})

	rec := do(h, http.MethodGet, "/ui/", nil, "Authorization", basic("sam", "pw"))
	want(t, rec, http.StatusOK, `<span class="sub">≈6% · 12,000/~200,000 rows · 60 batches</span>`)
}

func TestRunsListBackfillWithoutEstimate(t *testing.T) {
	t.Parallel()
	s := backfilling(&godwitv1.RunProgress{Migration: "m", RowsDone: 12000, Batches: 60})
	h := newUI(s, Config{User: "sam", Password: "pw"})

	rec := do(h, http.MethodGet, "/ui/", nil, "Authorization", basic("sam", "pw"))
	want(t, rec, http.StatusOK, `<span class="sub">12,000 rows · 60 batches</span>`)
}

func TestThousands(t *testing.T) {
	t.Parallel()
	for in, out := range map[int64]string{0: "0", 12: "12", 999: "999", 1000: "1,000", 12000: "12,000", 200000: "200,000", 1234567: "1,234,567"} {
		if got := thousands(in); got != out {
			t.Fatalf("thousands(%d) = %q, want %q", in, got, out)
		}
	}
}

func TestStatementLine(t *testing.T) {
	t.Parallel()
	if got := statementLine(nil); got != "" {
		t.Fatalf("nil progress = %q", got)
	}
	if got := statementLine(&godwitv1.RunProgress{Migration: "m", Statement: 3}); got != "statement 3 of m" {
		t.Fatalf("got %q", got)
	}
}
