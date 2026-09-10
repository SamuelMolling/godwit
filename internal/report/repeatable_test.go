package report

import (
	"strings"
	"testing"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
)

func TestPlanTextMarksRepeatableUnchanged(t *testing.T) {
	t.Parallel()

	r := PlanFromProto(&godwitv1.PlanRunResponse{
		Target: "app", Rollout: "direct",
		Migrations: []*godwitv1.PlannedMigration{{
			Name: "stats", Repeatable: true, Applied: true, Skipped: true, Phase: "expand",
			Statements: []*godwitv1.PlannedStatement{{Sql: "CREATE OR REPLACE VIEW stats AS SELECT 1;"}},
		}},
	})
	var b strings.Builder
	writePlanText(&b, r)
	if !strings.Contains(b.String(), "  R__stats  unchanged since it was last applied\n") {
		t.Fatalf("text = %s", b.String())
	}
}
