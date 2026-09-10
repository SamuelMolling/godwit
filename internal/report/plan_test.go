package report

import (
	"strings"
	"testing"
)

func TestPlanMarkdown_WithObservationAndDrift(t *testing.T) {
	t.Parallel()

	var b strings.Builder
	writePlanMarkdown(&b, Plan{
		live: true, target: "app", rollout: "direct", validated: true, planID: "p1", planKey: "k1", drift: "+ column public.rogue.id integer null=YES default=<none>",
		observed: &planObservation{HistoryHash: "h1", SchemaFingerprint: "f1", AppliedCount: 2, NewestApplied: 20260901120000, At: "2026-09-01T10:00:00Z"},
	})
	want := "## godwit plan\n\n" +
		"✅ **Nothing to apply.** `app` is at 20260901120000 (2 migrations).\n\n" +
		"<details><summary>1 change on this database was not made by a migration</summary>\n\n" +
		"```diff\n+ column public.rogue.id integer null=YES default=<none>\n```\n\n</details>\n\n" +
		"Plan: 0 to apply, 0 to revert, 0 hazard(s) to acknowledge\n\n" +
		"<!-- godwit-plan-id: p1 -->\n<!-- godwit-plan-key: k1 -->\n" +
		"<!-- godwit-plan-verdict: nothing to apply -->\n<!-- godwit-plan-hazards: 0 -->\n"
	if b.String() != want {
		t.Fatalf("markdown = %q, want %q", b.String(), want)
	}

	b.Reset()
	writePlanMarkdown(&b, Plan{live: true, target: "app", rollout: "direct"})
	want = "## godwit dry run\n\n✅ **Nothing to apply.** `app` already has every migration this plan covers.\n\n" +
		"⚠️ **Not validated.** These statements were never replayed on a scratch database, so nothing has proved they apply. Nothing read the schema they leave behind either, so what follows is the SQL the run would execute rather than what it does to the database.\n\n" +
		"Plan: 0 to apply, 0 to revert, 0 hazard(s) to acknowledge\n\n" +
		"<!-- godwit-plan-verdict: nothing to apply -->\n<!-- godwit-plan-hazards: 0 -->\n"
	if got := b.String(); got != want {
		t.Fatalf("markdown = %q, want %q", got, want)
	}
}
