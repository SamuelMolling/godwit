package server

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
)

func hazardFiles(n int) []*godwitv1.MigrationFile {
	files := []*godwitv1.MigrationFile{
		{Name: "20260901120001_policy.up.sql", Body: "CREATE TABLE team_policy (team_id int);\nCREATE INDEX idx_team_policy_team ON team_policy (team_id);"},
		{Name: "20260901120001_policy.down.sql", Body: "DROP TABLE team_policy;"},
		{Name: "20260901120002_seats.up.sql", Body: "CREATE TABLE seats (team_id int);\nCREATE INDEX idx_seats_team ON seats (team_id);"},
		{Name: "20260901120002_seats.down.sql", Body: "DROP TABLE seats;"},
		{Name: "20260901120003_audit.up.sql", Body: "CREATE TABLE audit_log (team_id int);\nCREATE INDEX idx_audit_log_team ON audit_log (team_id);"},
		{Name: "20260901120003_audit.down.sql", Body: "DROP TABLE audit_log;"},
	}

	return files[:2*n]
}

func TestHazardGateReadsOnlyPendingMigrations(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client := newClient(startService(t, newDatabase(t, "st"), "r1", nil), "")
	registerTarget(t, client, newDatabase(t, "tg"))

	runToSuccess(t, client, hazardFiles(1), []string{"H001"})

	plan, err := client.PlanRun(ctx, connect.NewRequest(&godwitv1.PlanRunRequest{Target: "app", Files: hazardFiles(1)}))
	if err != nil {
		t.Fatalf("plan over an applied hazard: %v", err)
	}
	if len(plan.Msg.Migrations) != 1 || !plan.Msg.Migrations[0].Applied {
		t.Fatalf("plan = %+v", plan.Msg.Migrations)
	}

	_, err = client.PlanRun(ctx, connect.NewRequest(&godwitv1.PlanRunRequest{Target: "app", Files: hazardFiles(3)}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "seats") ||
		!strings.Contains(err.Error(), "audit_log") || strings.Contains(err.Error(), "team_policy") {
		t.Fatalf("plan over pending hazards: %v", err)
	}

	_, err = client.PlanRun(ctx, connect.NewRequest(&godwitv1.PlanRunRequest{
		Target: "app", Files: hazardFiles(3), ToVersion: 20260901120002,
	}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "seats") ||
		strings.Contains(err.Error(), "audit_log") {
		t.Fatalf("plan with the third migration withheld: %v", err)
	}

	if _, err := client.CreateRun(ctx, connect.NewRequest(&godwitv1.CreateRunRequest{
		Target: "app", Files: hazardFiles(3), AcknowledgeHazards: []string{"H001"},
	})); err != nil {
		t.Fatalf("run with the pending hazards acknowledged: %v", err)
	}
}

func TestHazardGateReadsOutOfOrderMigrations(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client := newClient(startService(t, newDatabase(t, "st"), "r1", nil), "")
	registerTarget(t, client, newDatabase(t, "tg"))

	all := hazardFiles(3)
	skipped := append(all[:2:2], all[4:]...)
	runToSuccess(t, client, all[:2], []string{"H001"})
	runToSuccess(t, client, skipped, []string{"H001"})

	_, err := client.PlanRun(ctx, connect.NewRequest(&godwitv1.PlanRunRequest{Target: "app", Files: all, AllowOutOfOrder: true}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "seats") {
		t.Fatalf("plan over an out-of-order hazard: %v", err)
	}
}

func TestHazardGateReadsWhatARevertWouldRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client := newClient(startService(t, newDatabase(t, "st"), "r1", nil), "")
	registerTarget(t, client, newDatabase(t, "tg"))

	runToSuccess(t, client, hazardFiles(1), []string{"H001"})

	_, err := client.RevertRun(ctx, connect.NewRequest(&godwitv1.RevertRunRequest{Target: "app", DryRun: true}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "H002: DROP TABLE is destructive") {
		t.Fatalf("revert of an applied migration: %v", err)
	}
	if _, err := client.RevertRun(ctx, connect.NewRequest(&godwitv1.RevertRunRequest{
		Target: "app", DryRun: true, AcknowledgeHazards: []string{"H002"},
	})); err != nil {
		t.Fatalf("revert with the hazard acknowledged: %v", err)
	}
}
