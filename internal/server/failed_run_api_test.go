package server

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
)

func failingC() []*godwitv1.MigrationFile {
	return []*godwitv1.MigrationFile{
		{Name: "20260101000002_c.up.sql", Body: "SELECT 1/0;"},
		{Name: "20260101000002_c.down.sql", Body: "SELECT 1;"},
	}
}

// A run that applies two migrations and then fails on a third leaves the two on the target. The control
// plane must say so: the applied count, the plan's per-migration applied flag and the ledger all agree
// with godwit.migrations, and the plan is not asked to run them a second time.
func TestFailedRunIsAccountedForOnTheTarget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client := newClient(startService(t, newDatabase(t, "st"), "r1", nil), "")
	targetDSN := newDatabase(t, "tg")
	registerTarget(t, client, targetDSN)

	files := append(append(tableA(), tableB()...), failingC()...)
	created, err := client.CreateRun(ctx, connect.NewRequest(&godwitv1.CreateRunRequest{
		Target: "app", Files: files, SkipValidation: true,
	}))
	if err != nil {
		t.Fatal(err)
	}
	run := waitState(t, client, created.Msg.RunId, godwitv1.RunState_RUN_STATE_FAILED)
	if run.Progress != nil {
		t.Fatalf("a settled run carries no progress: %+v", run.Progress)
	}
	if v := appliedVersions(t, targetDSN); len(v) != 2 {
		t.Fatalf("godwit.migrations = %v, want the two the run applied", v)
	}

	got, err := client.GetRun(ctx, connect.NewRequest(&godwitv1.GetRunRequest{RunId: created.Msg.RunId}))
	if err != nil || len(got.Msg.Applied) != 2 {
		t.Fatalf("ledger = %+v, err = %v", got.Msg.Applied, err)
	}
	targets, err := client.ListTargets(ctx, connect.NewRequest(&godwitv1.ListTargetsRequest{}))
	if err != nil || targets.Msg.Targets[0].AppliedCount != 2 {
		t.Fatalf("targets = %+v, err = %v", targets.Msg.Targets, err)
	}

	plan, err := client.PlanRun(ctx, connect.NewRequest(&godwitv1.PlanRunRequest{
		Target: "app", Files: append(tableA(), tableB()...),
	}))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range plan.Msg.Migrations {
		if !m.Applied {
			t.Fatalf("%s was applied by the failed run and must plan as applied: %+v", m.Name, plan.Msg.Migrations)
		}
	}
	if plan.Msg.Drift != "" {
		t.Fatalf("the replay rebuilt what the failed run applied, so there is no drift: %q", plan.Msg.Drift)
	}
}
