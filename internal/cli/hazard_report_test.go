package cli

import (
	"strings"
	"testing"

	"github.com/SamuelMolling/godwit/internal/engine"
)

func hazardItem(id string, skipped bool) planItem {
	return planItem{
		Plan: engine.Plan{
			Migration: engine.Migration{Version: 20260901120000, Name: id},
			Direction: engine.DirectionUp,
			Statements: []engine.Statement{{
				SQL:     "CREATE INDEX idx ON t (v)",
				Hazards: []engine.Hazard{{Code: "H001", Detail: "CREATE INDEX without CONCURRENTLY blocks writes on t"}},
			}},
		},
		applied: skipped, skipped: skipped, phase: "expand",
	}
}

func TestPlanMarkdownCountsOnlyWhatTheRunWouldExecute(t *testing.T) {
	t.Parallel()

	var b strings.Builder
	writePlanMarkdown(&b, planReport{live: true, target: "app", rollout: "direct", items: []planItem{hazardItem("users", true)}})
	got := b.String()
	if strings.Contains(got, "must be acknowledged before this runs") {
		t.Fatalf("an applied migration's hazard must not be presented as acknowledgeable:\n%s", got)
	}
	if !strings.HasSuffix(got, "\nPlan: 0 to apply, 0 to revert, 0 hazard(s) to acknowledge\n") {
		t.Fatalf("footer must agree with the gate:\n%s", got)
	}
	if !strings.Contains(got, "| H001: CREATE INDEX without CONCURRENTLY blocks writes on t |") {
		t.Fatalf("the hazard is still true of that statement and must stay beside it:\n%s", got)
	}
	if !strings.Contains(got, "1 hazard(s) on statements this run would not execute") {
		t.Fatalf("the uncounted hazards must be named:\n%s", got)
	}

	b.Reset()
	writePlanMarkdown(&b, planReport{
		live: true, target: "app", rollout: "direct",
		items: []planItem{hazardItem("users", true), hazardItem("orders", false)},
	})
	got = b.String()
	if !strings.Contains(got, "1 hazard must be acknowledged before this runs; use `--ack H001`.\n") ||
		!strings.HasSuffix(got, "\nPlan: 1 to apply, 0 to revert, 1 hazard(s) to acknowledge\n") {
		t.Fatalf("the pending hazard must still be gated:\n%s", got)
	}
	if !strings.Contains(got, "1 hazard(s) on statements this run would not execute") {
		t.Fatalf("mixed plan must separate the two:\n%s", got)
	}
}

func TestPlanTextKeepsTheHazardsItWillNotExecute(t *testing.T) {
	t.Parallel()

	var b strings.Builder
	writePlanText(&b, planReport{live: true, target: "app", rollout: "direct", items: []planItem{hazardItem("users", true)}})
	for _, want := range []string{
		"  20260901120000_users  already in the target's history\n",
		"    hazard H001: CREATE INDEX without CONCURRENTLY blocks writes on t\n",
		"  1 hazard(s) on statements this run would not execute",
		"\nPlan: 0 to apply, 0 to revert, 0 hazard(s) to acknowledge\n",
	} {
		if !strings.Contains(b.String(), want) {
			t.Fatalf("text missing %q:\n%s", want, b.String())
		}
	}
}

func TestSkipReasonNamesWhyTheBodyStaysUnrun(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		item planItem
		want string
	}{
		{planItem{note: "collapsed by a checkpoint"}, "collapsed by a checkpoint"},
		{planItem{}, "the run would not execute its body"},
	} {
		if got := tc.item.skipReason(); got != tc.want {
			t.Fatalf("skipReason = %q, want %q", got, tc.want)
		}
	}
}

func TestPlanReportNamesTheIgnoredBookkeepingTables(t *testing.T) {
	t.Parallel()

	r := planReport{
		live: true, target: "app", rollout: "direct", planID: "p1", planKey: "k1",
		observed: &planObservation{
			HistoryHash: "h1", SchemaFingerprint: "f1", At: "2026-09-01T10:00:00Z",
			IgnoredTables: []string{"public.schema_migrations (golang-migrate)"},
		},
	}
	var b strings.Builder
	writePlanMarkdown(&b, r)
	for _, want := range []string{
		"ignored: public.schema_migrations (golang-migrate) left behind by a previous migration tool",
		"set ignore_adopted_tables=false on the target to count them",
	} {
		if !strings.Contains(b.String(), want) {
			t.Fatalf("markdown missing %q:\n%s", want, b.String())
		}
	}

	b.Reset()
	r.observed.IgnoredTables = nil
	writePlanText(&b, r)
	if strings.Contains(b.String(), "ignored:") {
		t.Fatalf("nothing ignored must say nothing:\n%s", b.String())
	}
}
