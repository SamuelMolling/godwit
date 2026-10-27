package report

import (
	"strings"
	"testing"

	"github.com/SamuelMolling/godwit/internal/comment"
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

func TestPlanMarkdownTellsThePullRequestHowToApply(t *testing.T) {
	t.Parallel()

	pending := hazardItem("orders", false)
	pending.Statements[0].Hazards = nil
	var b strings.Builder
	writePlanMarkdown(&b, Plan{
		live: true, target: "app", rollout: "direct", validated: true, planID: "p1", items: []item{pending},
	})
	got := b.String()
	want := "Before you merge, approve this pull request and comment `godwit apply` as the whole comment." +
		" godwit applies that comment, and stays quiet when anything else shares it."
	if !strings.Contains(got, want) {
		t.Fatalf("markdown missing the apply step:\n%s", got)
	}
	if cmd, err := comment.Parse(got, "apply"); err != nil || cmd != nil {
		t.Fatalf("the plan comment must not itself be an apply: %+v %v", cmd, err)
	}

	b.Reset()
	writePlanMarkdown(&b, Plan{
		live: true, target: "app", rollout: "direct", validated: true, planID: "p1",
		items: []item{hazardItem("orders", false)},
	})
	if !strings.Contains(b.String(), "comment `godwit apply --ack H001` as the whole comment.") {
		t.Fatalf("a hazard must ride on the command:\n%s", b.String())
	}

	for _, silent := range []Plan{
		{live: true, target: "app", rollout: "direct", validated: true, planID: "p1"},
		{live: true, target: "app", rollout: "direct", validated: true, items: []item{pending}},
	} {
		b.Reset()
		writePlanMarkdown(&b, silent)
		if strings.Contains(b.String(), "Before you merge") {
			t.Fatalf("nothing stored and pending must not ask for an apply:\n%s", b.String())
		}
	}
}
