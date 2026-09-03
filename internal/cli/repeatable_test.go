package cli

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
)

func TestApplyAndStatusRepeatable(t *testing.T) {
	t.Parallel()
	dsn := newTestDSN(t)
	dir := writeMigs(t, map[string]string{
		"20260901120000_users.up.sql":   "CREATE TABLE users (id int);",
		"20260901120000_users.down.sql": "DROP TABLE users;",
		"R__stats.up.sql":               "CREATE OR REPLACE VIEW stats AS SELECT 1 AS n;",
		"R__stats.down.sql":             "DROP VIEW IF EXISTS stats;",
	})

	code, out, errOut := runCLI("apply", "--dsn", dsn, "--dir", dir)
	if code != 0 {
		t.Fatal(errOut)
	}
	if !strings.Contains(out, "R__stats: applied") {
		t.Fatalf("apply = %s", out)
	}

	code, out, errOut = runCLI("status", "--dsn", dsn, "--dir", dir)
	if code != 0 {
		t.Fatal(errOut)
	}
	if !strings.Contains(out, "R__stats: unchanged since ") {
		t.Fatalf("status = %s", out)
	}

	if code, out, _ = runCLI("apply", "--dsn", dsn, "--dir", dir); code != 0 || !strings.Contains(out, "R__stats: skipped") {
		t.Fatalf("second apply = %s", out)
	}
}

func TestPlanTextMarksRepeatableUnchanged(t *testing.T) {
	t.Parallel()

	r := planReportFromProto(&godwitv1.PlanRunResponse{
		Target: "app", Rollout: "direct",
		Migrations: []*godwitv1.PlannedMigration{{
			Name: "stats", Repeatable: true, Applied: true, Phase: "expand",
			Statements: []*godwitv1.PlannedStatement{{Sql: "CREATE OR REPLACE VIEW stats AS SELECT 1;"}},
		}},
	})
	var b strings.Builder
	writePlanText(&b, r)
	if !strings.Contains(b.String(), "R__stats (up): 1 statement(s) [expand, unchanged]") {
		t.Fatalf("text = %s", b.String())
	}
}

func TestTargetStatusTextRepeatable(t *testing.T) {
	t.Parallel()

	at := timestamppb.New(time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC))
	out := statusText(&godwitv1.GetTargetStatusResponse{
		Target: "app", Provider: "static",
		Applied: []*godwitv1.AppliedMigration{{Name: "stats", Repeatable: true, AppliedAt: at}},
		Pending: []*godwitv1.PendingMigration{{Name: "audit", Repeatable: true}},
	})
	if !strings.Contains(out, "R__stats") || !strings.Contains(out, "unchanged") || !strings.Contains(out, "R__audit") {
		t.Fatalf("out = %s", out)
	}
}
