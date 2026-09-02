package server

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/internal/controlplane"
)

func TestPlanRunEndToEnd(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client := newClient(startService(t, newDatabase(t, "st"), "r1", nil), "")
	registerTarget(t, client, newDatabase(t, "tg"))
	all := orderedFiles()
	all[5].Body = "ALTER TABLE t DROP COLUMN b;"
	all[4].Body = "ALTER TABLE t DROP COLUMN a;"

	res, err := client.PlanRun(ctx, connect.NewRequest(&godwitv1.PlanRunRequest{
		Target: "app", Files: all, Rollout: controlplane.RolloutExpandContract, AcknowledgeHazards: []string{"H003"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	plan := res.Msg
	if plan.Target != "app" || plan.Rollout != controlplane.RolloutExpandContract || !plan.Validated || len(plan.Migrations) != 3 {
		t.Fatalf("fresh plan = %+v", plan)
	}
	for i, want := range []struct {
		phase   string
		hazards int
	}{{controlplane.PhaseExpand, 0}, {controlplane.PhaseExpand, 0}, {controlplane.PhaseContract, 1}} {
		m := plan.Migrations[i]
		if m.Applied || m.Phase != want.phase || len(m.Statements) != 1 || len(m.Statements[0].Hazards) != want.hazards || m.Checksum == "" {
			t.Fatalf("migration %d = %+v", i, m)
		}
	}
	if h := plan.Migrations[2].Statements[0].Hazards[0]; h.Code != "H003" || !strings.HasPrefix(h.Recipe, "-- expand then contract: ship the application version that no longer reads or writes t.a") {
		t.Fatalf("hazard = %+v", h)
	}
	list, err := client.ListRuns(ctx, connect.NewRequest(&godwitv1.ListRunsRequest{Target: "app"}))
	if err != nil || len(list.Msg.Runs) != 0 {
		t.Fatalf("runs after plan = %+v, err = %v", list, err)
	}

	runToSuccess(t, client, all[:2], nil)
	res, err = client.PlanRun(ctx, connect.NewRequest(&godwitv1.PlanRunRequest{
		Target: "app", Files: all[:4], SkipValidation: true,
	}))
	if err != nil {
		t.Fatal(err)
	}
	plan = res.Msg
	if plan.Validated || !plan.Migrations[0].Applied || plan.Migrations[1].Applied || plan.Migrations[1].Phase != controlplane.PhaseExpand {
		t.Fatalf("plan after run = %+v", plan)
	}

	_, err = client.PlanRun(ctx, connect.NewRequest(&godwitv1.PlanRunRequest{Target: "app", Files: all}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "H003") {
		t.Fatalf("hazard gate: %v", err)
	}
	broken := []*godwitv1.MigrationFile{
		{Name: "20260901120009_broken.up.sql", Body: "SELECT 1/0;"},
		{Name: "20260901120009_broken.down.sql", Body: "SELECT 1;"},
	}
	_, err = client.PlanRun(ctx, connect.NewRequest(&godwitv1.PlanRunRequest{Target: "app", Files: broken}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "failed validation") {
		t.Fatalf("validation: %v", err)
	}
	if _, err := client.PlanRun(ctx, connect.NewRequest(&godwitv1.PlanRunRequest{Target: "ghost", Files: all[:2]})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("unknown target: %v", err)
	}
	list, err = client.ListRuns(ctx, connect.NewRequest(&godwitv1.ListRunsRequest{Target: "app"}))
	if err != nil || len(list.Msg.Runs) != 1 {
		t.Fatalf("runs after refusals = %+v, err = %v", list, err)
	}
}
