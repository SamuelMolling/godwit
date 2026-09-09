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
	writePlanMarkdown(&b, planReport{live: true, target: "app", rollout: "direct", validated: true, items: []planItem{hazardItem("users", true)}})
	got := b.String()
	if strings.Contains(got, "on what this run would execute") {
		t.Fatalf("an applied migration's hazard must not be presented as acknowledgeable:\n%s", got)
	}
	if !strings.HasSuffix(got, "\nPlan: 0 to apply, 0 to revert, 0 hazard(s) to acknowledge\n") {
		t.Fatalf("footer must agree with the gate:\n%s", got)
	}
	for _, gone := range []string{"H001", "CREATE INDEX without CONCURRENTLY", "this run will not execute", "already in the target"} {
		if strings.Contains(got, gone) {
			t.Fatalf("a hazard on a migration the target already has is archaeology, %q must not be rendered:\n%s", gone, got)
		}
	}

	b.Reset()
	writePlanMarkdown(&b, planReport{
		live: true, target: "app", rollout: "direct", validated: true,
		items: []planItem{hazardItem("users", true), hazardItem("orders", false)},
	})
	got = b.String()
	if !strings.Contains(got, "⚠️ 1 hazard on what this run would execute: take the recipe printed beside the statement,"+
		" or accept the risk with `--ack H001` (`/godwit apply --ack H001` on a pull request).\n") ||
		!strings.HasSuffix(got, "\nPlan: 1 to apply, 0 to revert, 1 hazard(s) to acknowledge\n") {
		t.Fatalf("the pending hazard must still be gated:\n%s", got)
	}
	if strings.Count(got, "H001") != 3 {
		t.Fatalf("only the pending migration's hazard may appear (statement, gate line, ack hint):\n%s", got)
	}
}

func TestPlanTextDropsWhatTheTargetAlreadyHas(t *testing.T) {
	t.Parallel()

	var b strings.Builder
	writePlanText(&b, planReport{live: true, target: "app", rollout: "direct", validated: true, items: []planItem{hazardItem("users", true)}})
	got := b.String()
	if !strings.Contains(got, "Nothing to apply. app is at 20260901120000 (1 migration).\n") {
		t.Fatalf("a clean plan must say where the target stands:\n%s", got)
	}
	for _, gone := range []string{"H001", "not executed by this run"} {
		if strings.Contains(got, gone) {
			t.Fatalf("text must not carry %q for a migration already applied:\n%s", gone, got)
		}
	}
	if !strings.HasSuffix(got, "\nPlan: 0 to apply, 0 to revert, 0 hazard(s) to acknowledge\n") {
		t.Fatalf("footer must agree with the gate:\n%s", got)
	}
}

func TestNotRunKeepsOnlyWhatTheReaderCouldActOn(t *testing.T) {
	t.Parallel()

	rep := hazardItem("stats", true)
	rep.Migration = engine.Migration{Name: "stats", Repeatable: true}
	held := hazardItem("later", true)
	held.applied, held.withheld = false, true
	r := planReport{
		live: true, target: "app", rollout: "direct", validated: true,
		items: []planItem{hazardItem("users", true), rep, held},
	}
	var b strings.Builder
	writePlanText(&b, r)
	got := b.String()
	if !strings.Contains(got, "\nnot executed by this run (2):\n  R__stats  unchanged since it was last applied\n"+
		"  20260901120000_later  held back by --to\n") {
		t.Fatalf("a repeatable and a withheld migration are still worth a row:\n%s", got)
	}
	if strings.Contains(got, "20260901120000_users") {
		t.Fatalf("the applied one must be gone:\n%s", got)
	}

	b.Reset()
	writePlanMarkdown(&b, r)
	if !strings.Contains(b.String(), "<details><summary>2 migrations this run will not execute</summary>\n\n"+
		"| Migration | Not executed because |\n|---|---|\n"+
		"| `R__stats` | unchanged since it was last applied |\n"+
		"| `20260901120000_later` | held back by --to |\n") {
		t.Fatalf("the markdown block loses its hazard column, not its rows:\n%s", b.String())
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
		live: true, target: "app", rollout: "direct", validated: true, planID: "p1", planKey: "k1",
		observed: &planObservation{
			HistoryHash: "h1", SchemaFingerprint: "f1", At: "2026-09-01T10:00:00Z",
			IgnoredTables: []string{"public.schema_migrations (golang-migrate)"},
		},
	}
	var b strings.Builder
	writePlanMarkdown(&b, r)
	want := "⚠️ **Left behind by another migration tool:** public.schema_migrations (golang-migrate). godwit keeps them" +
		" out of the schema and its drift; drop what nothing reads any more, or set `ignore_adopted_tables=false`" +
		" on the target to count them."
	if !strings.Contains(b.String(), want) {
		t.Fatalf("markdown missing %q:\n%s", want, b.String())
	}
	if strings.Contains(b.String(), "<details><summary>plan details</summary>\n\n```\ntarget: app\nrollout: direct\nplan: p1\n```") == false {
		t.Fatalf("the details block keeps only what a person reads:\n%s", b.String())
	}

	b.Reset()
	r.observed.IgnoredTables = nil
	writePlanText(&b, r)
	if strings.Contains(b.String(), "Left behind") {
		t.Fatalf("nothing ignored must say nothing:\n%s", b.String())
	}
}

func TestPlanDetailsDropTheMachineIdentity(t *testing.T) {
	t.Parallel()

	r := planReport{
		live: true, target: "app", rollout: "direct", validated: true, planID: "p1", planKey: "k1",
		observed: &planObservation{HistoryHash: "h1", SchemaFingerprint: "f1", AppliedCount: 3, NewestApplied: 20260902165420, At: "2026-09-01T10:00:00Z"},
	}
	var b strings.Builder
	writePlanText(&b, r)
	for _, gone := range []string{"key: ", "observed: ", "h1", "f1", "validation: "} {
		if strings.Contains(b.String(), gone) {
			t.Fatalf("%q is for machines and belongs to the JSON only:\n%s", gone, b.String())
		}
	}
	if !strings.Contains(b.String(), "Nothing to apply. app is at 20260902165420 (3 migrations).\n") {
		t.Fatalf("the observation survives as one line of state:\n%s", b.String())
	}

	b.Reset()
	writePlanMarkdown(&b, r)
	if !strings.HasSuffix(b.String(), "\n<!-- godwit-plan-key: k1 -->\n") {
		t.Fatalf("the key stays where the Action reads it, out of sight:\n%s", b.String())
	}
}
