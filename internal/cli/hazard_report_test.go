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

// On a target that holds every migration, nothing runs and `--ack` changes nothing: the footer must not
// ask for it, and it must still say the hazard is there.
func TestPlanMarkdownCountsOnlyWhatTheRunWouldExecute(t *testing.T) {
	t.Parallel()

	var b strings.Builder
	writePlanMarkdown(&b, planReport{live: true, target: "app", rollout: "direct", items: []planItem{hazardItem("users", true)}})
	got := b.String()
	if strings.Contains(got, "acknowledge them with") {
		t.Fatalf("an applied migration's hazard must not be presented as acknowledgeable:\n%s", got)
	}
	if !strings.Contains(got, "✅ no hazards") {
		t.Fatalf("footer must agree with the gate:\n%s", got)
	}
	if !strings.Contains(got, "H001: CREATE INDEX without CONCURRENTLY blocks writes on t") {
		t.Fatalf("the hazard is still true of that statement and must stay in its row:\n%s", got)
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
	if !strings.Contains(got, "⚠️ 1 hazard(s); acknowledge them with `--ack`") {
		t.Fatalf("the pending hazard must still be gated:\n%s", got)
	}
	if !strings.Contains(got, "1 hazard(s) on statements this run would not execute") {
		t.Fatalf("mixed plan must separate the two:\n%s", got)
	}
}

// A drift signal godwit stops emitting has to be visible as a decision, with the knob that undoes it.
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
