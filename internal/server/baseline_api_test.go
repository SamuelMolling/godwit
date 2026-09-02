package server

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/internal/controlplane"
)

func baselineFiles() []*godwitv1.MigrationFile {
	return []*godwitv1.MigrationFile{
		{Name: "00000000000001_baseline.up.sql", Body: "CREATE TABLE t (id int);"},
		{Name: "00000000000001_baseline.down.sql", Body: "DROP TABLE t;"},
		{Name: "20260901120000_name.up.sql", Body: "ALTER TABLE t ADD COLUMN name text;"},
		{Name: "20260901120000_name.down.sql", Body: "ALTER TABLE t DROP COLUMN name;"},
	}
}

func TestBaselineTargetEndToEnd(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client := newClient(startService(t, newDatabase(t, "st"), "r1", nil), "")
	targetDSN := newDatabase(t, "tg")
	registerTarget(t, client, targetDSN)
	conn, err := pgx.Connect(ctx, targetDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, "CREATE TABLE t (id int)"); err != nil {
		t.Fatal(err)
	}

	invalid := []struct {
		name string
		req  *godwitv1.BaselineTargetRequest
		want string
	}{
		{"no target", &godwitv1.BaselineTargetRequest{Files: baselineFiles(), Version: 1}, "target is required"},
		{"no version", &godwitv1.BaselineTargetRequest{Target: "app", Files: baselineFiles()}, "version must be positive"},
		{"bad files", &godwitv1.BaselineTargetRequest{Target: "app", Version: 1, Files: baselineFiles()[:1]}, "down"},
		{"version too low", &godwitv1.BaselineTargetRequest{Target: "app", Version: 1, Files: baselineFiles()[2:]}, "no migration at or below version 1"},
	}
	for _, tc := range invalid {
		_, err := client.BaselineTarget(ctx, connect.NewRequest(tc.req))
		if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: err = %v", tc.name, err)
		}
	}
	if _, err := client.BaselineTarget(ctx, connect.NewRequest(&godwitv1.BaselineTargetRequest{
		Target: "ghost", Files: baselineFiles(), Version: 1,
	})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("unknown target: %v", err)
	}

	res, err := client.BaselineTarget(ctx, connect.NewRequest(&godwitv1.BaselineTargetRequest{
		Target: "app", Files: baselineFiles(), Version: 1,
	}))
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.GetRun(ctx, connect.NewRequest(&godwitv1.GetRunRequest{RunId: res.Msg.RunId}))
	if err != nil {
		t.Fatal(err)
	}
	run := got.Msg.Run
	if run.Kind != controlplane.KindBaseline || run.State != godwitv1.RunState_RUN_STATE_SUCCEEDED || run.FinishedAt == nil {
		t.Fatalf("run = %+v", run)
	}
	var versions []int64
	rows, err := conn.Query(ctx, "SELECT version FROM godwit.migrations ORDER BY version")
	if err != nil {
		t.Fatal(err)
	}
	var v int64
	if _, err := pgx.ForEachRow(rows, []any{&v}, func() error {
		versions = append(versions, v)

		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(versions) != 1 || versions[0] != 1 {
		t.Fatalf("applied versions = %v", versions)
	}
	drift, err := client.CheckDrift(ctx, connect.NewRequest(&godwitv1.CheckDriftRequest{Target: "app"}))
	if err != nil || drift.Msg.Drifted {
		t.Fatalf("drift after baseline = %+v, err = %v", drift, err)
	}

	runToSuccess(t, client, baselineFiles()[2:], nil)
	list, err := client.ListRuns(ctx, connect.NewRequest(&godwitv1.ListRunsRequest{Target: "app"}))
	if err != nil || len(list.Msg.Runs) != 2 || list.Msg.Runs[0].Kind != controlplane.KindMigrate || list.Msg.Runs[1].Kind != controlplane.KindBaseline {
		t.Fatalf("runs = %+v, err = %v", list, err)
	}

	if _, err := client.BaselineTarget(ctx, connect.NewRequest(&godwitv1.BaselineTargetRequest{
		Target: "app", Files: baselineFiles(), Version: 1,
	})); connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "already has applied migrations") {
		t.Fatalf("second baseline: %v", err)
	}
	if _, err := client.RevertRun(ctx, connect.NewRequest(&godwitv1.RevertRunRequest{
		RunId: res.Msg.RunId,
	})); connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "baseline runs cannot be reverted") {
		t.Fatalf("revert baseline: %v", err)
	}
}
