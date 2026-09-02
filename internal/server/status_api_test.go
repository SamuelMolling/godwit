package server

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/internal/controlplane"
)

func TestGetTargetStatusEndToEnd(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client := newClient(startService(t, newDatabase(t, "st"), "r1", nil), "")
	targetDSN := newDatabase(t, "tg")
	registerTarget(t, client, targetDSN)
	files := baselineFiles()

	res, err := client.GetTargetStatus(ctx, connect.NewRequest(&godwitv1.GetTargetStatusRequest{Target: "app", Files: files}))
	if err != nil {
		t.Fatal(err)
	}
	st := res.Msg
	if st.Provider != "static" || len(st.Applied) != 0 || len(st.Pending) != 2 || st.LastRun != nil || st.DriftBaseline != nil {
		t.Fatalf("fresh target = %+v", st)
	}

	runToSuccess(t, client, files[:2], nil)
	execStore(t, targetDSN, "UPDATE godwit.migrations SET checksum = 'stale'")
	execStore(t, targetDSN, "CREATE TABLE stray (id int)")
	if _, err := client.CheckDrift(ctx, connect.NewRequest(&godwitv1.CheckDriftRequest{Target: "app"})); err != nil {
		t.Fatal(err)
	}

	res, err = client.GetTargetStatus(ctx, connect.NewRequest(&godwitv1.GetTargetStatusRequest{Target: "app", Files: files}))
	if err != nil {
		t.Fatal(err)
	}
	st = res.Msg
	if len(st.Applied) != 1 || st.Applied[0].Version != 1 || st.Applied[0].Name != "baseline" || !st.Applied[0].ChecksumMismatch ||
		st.Applied[0].AppliedAt == nil {
		t.Fatalf("applied = %+v", st.Applied)
	}
	if len(st.Pending) != 1 || st.Pending[0].Version != 20260901120000 || st.Pending[0].Name != "name" {
		t.Fatalf("pending = %+v", st.Pending)
	}
	if st.LastRun == nil || st.LastRun.Kind != controlplane.KindMigrate || st.LastRun.State != godwitv1.RunState_RUN_STATE_SUCCEEDED ||
		st.LastRun.FinishedAt == nil {
		t.Fatalf("last run = %+v", st.LastRun)
	}
	if st.DriftBaseline == nil || st.DriftBaseline.RunId != st.LastRun.Id || st.DriftBaseline.TakenAt == nil || !st.DriftBaseline.UnresolvedDrift {
		t.Fatalf("baseline = %+v", st.DriftBaseline)
	}

	if _, err := client.GetTargetStatus(ctx, connect.NewRequest(&godwitv1.GetTargetStatusRequest{Target: "ghost"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("unknown target: %v", err)
	}
}
