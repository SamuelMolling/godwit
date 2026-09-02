package server

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/gen/godwit/v1/godwitv1connect"
	"github.com/SamuelMolling/godwit/internal/controlplane"
)

func columnExists(t *testing.T, dsn, column string) bool {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	var exists bool
	if err := conn.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 't' AND column_name = $1)`,
		column).Scan(&exists); err != nil {
		t.Fatal(err)
	}

	return exists
}

func TestExpandContractEndToEnd(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client := newClient(startService(t, newDatabase(t, "st"), "r1", nil), "")
	targetDSN := newDatabase(t, "tg")
	registerTarget(t, client, targetDSN)
	runToSuccess(t, client, migrationFiles(), nil)

	_, err := client.CreateRun(ctx, connect.NewRequest(&godwitv1.CreateRunRequest{
		Target: "app", Files: migrationFiles(), Rollout: "canary",
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("err = %v", err)
	}

	files := []*godwitv1.MigrationFile{
		{Name: "20260901130000_add.up.sql", Body: "ALTER TABLE t ADD COLUMN new_id int;"},
		{Name: "20260901130000_add.down.sql", Body: "ALTER TABLE t DROP COLUMN new_id;"},
		{Name: "20260901130001_drop.up.sql", Body: "ALTER TABLE t DROP COLUMN id;"},
		{Name: "20260901130001_drop.down.sql", Body: "ALTER TABLE t ADD COLUMN id int;"},
	}
	created, err := client.CreateRun(ctx, connect.NewRequest(&godwitv1.CreateRunRequest{
		Target: "app", Files: files, Rollout: controlplane.RolloutExpandContract, AcknowledgeHazards: []string{"H003"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	runID := created.Msg.RunId

	stream, err := client.WatchRun(ctx, connect.NewRequest(&godwitv1.WatchRunRequest{RunId: runID}))
	if err != nil {
		t.Fatal(err)
	}
	var last *godwitv1.Run
	for stream.Receive() {
		last = stream.Msg().Run
	}
	if stream.Err() != nil || last.State != godwitv1.RunState_RUN_STATE_AWAITING_CONTRACT ||
		last.Rollout != controlplane.RolloutExpandContract || last.Phase != controlplane.PhaseExpand {
		t.Fatalf("run = %+v, err = %v", last, stream.Err())
	}
	if !columnExists(t, targetDSN, "new_id") || !columnExists(t, targetDSN, "id") {
		t.Fatal("expand phase must add new_id and keep id")
	}

	if _, err := client.ConfirmRollout(ctx, connect.NewRequest(&godwitv1.ConfirmRolloutRequest{RunId: runID})); err != nil {
		t.Fatal(err)
	}
	stream, err = client.WatchRun(ctx, connect.NewRequest(&godwitv1.WatchRunRequest{RunId: runID}))
	if err != nil {
		t.Fatal(err)
	}
	for stream.Receive() {
		last = stream.Msg().Run
	}
	if stream.Err() != nil || last.State != godwitv1.RunState_RUN_STATE_SUCCEEDED || last.Phase != controlplane.PhaseContract {
		t.Fatalf("run = %+v, err = %v", last, stream.Err())
	}
	if columnExists(t, targetDSN, "id") {
		t.Fatal("contract phase must drop id")
	}

	_, err = client.ConfirmRollout(ctx, connect.NewRequest(&godwitv1.ConfirmRolloutRequest{RunId: runID}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("err = %v", err)
	}
}

func watchToEnd(t *testing.T, client godwitv1connect.GodwitServiceClient, runID string) *godwitv1.Run {
	t.Helper()
	stream, err := client.WatchRun(context.Background(), connect.NewRequest(&godwitv1.WatchRunRequest{RunId: runID}))
	if err != nil {
		t.Fatal(err)
	}
	var last *godwitv1.Run
	for stream.Receive() {
		last = stream.Msg().Run
	}
	if stream.Err() != nil || last == nil {
		t.Fatalf("watch %s: %v", runID, stream.Err())
	}

	return last
}

func TestRevertRunEndToEnd(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storeDSN := newDatabase(t, "st")
	client := newClient(startService(t, storeDSN, "r1", nil), "")
	targetDSN := newDatabase(t, "tg")
	registerTarget(t, client, targetDSN)

	created, err := client.CreateRun(ctx, connect.NewRequest(&godwitv1.CreateRunRequest{Target: "app", Files: migrationFiles()}))
	if err != nil {
		t.Fatal(err)
	}
	runID := created.Msg.RunId
	watchToEnd(t, client, runID)

	// The down side drops a table, so H002 must be acknowledged.
	_, err = client.RevertRun(ctx, connect.NewRequest(&godwitv1.RevertRunRequest{RunId: runID}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "H002") {
		t.Fatalf("err = %v", err)
	}
	reverted, err := client.RevertRun(ctx, connect.NewRequest(&godwitv1.RevertRunRequest{RunId: runID, AcknowledgeHazards: []string{"H002"}}))
	if err != nil {
		t.Fatal(err)
	}
	last := watchToEnd(t, client, reverted.Msg.RunId)
	if last.State != godwitv1.RunState_RUN_STATE_SUCCEEDED || last.Reverts != runID {
		t.Fatalf("revert run = %+v", last)
	}
	if columnExists(t, targetDSN, "id") {
		t.Fatal("revert must drop t")
	}
	orig, err := client.GetRun(ctx, connect.NewRequest(&godwitv1.GetRunRequest{RunId: runID}))
	if err != nil || orig.Msg.Run.State != godwitv1.RunState_RUN_STATE_REVERTED {
		t.Fatalf("original = %+v, err = %v", orig.Msg.Run, err)
	}

	_, err = client.RevertRun(ctx, connect.NewRequest(&godwitv1.RevertRunRequest{RunId: runID, AcknowledgeHazards: []string{"H002"}}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("second revert: %v", err)
	}

	// A down file that fails on the scratch database is refused.
	bad, err := client.CreateRun(ctx, connect.NewRequest(&godwitv1.CreateRunRequest{
		Target: "app", Files: []*godwitv1.MigrationFile{
			{Name: "20260901140000_v.up.sql", Body: "CREATE TABLE v (id int);"},
			{Name: "20260901140000_v.down.sql", Body: "SELECT 1/0;"},
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	watchToEnd(t, client, bad.Msg.RunId)
	_, err = client.RevertRun(ctx, connect.NewRequest(&godwitv1.RevertRunRequest{RunId: bad.Msg.RunId, AcknowledgeHazards: []string{"H002"}}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("invalid down: %v", err)
	}
	execStore(t, storeDSN, "UPDATE cp_run_files SET body = 'NOT SQL' WHERE name = '20260901140000_v.down.sql'")
	_, err = client.RevertRun(ctx, connect.NewRequest(&godwitv1.RevertRunRequest{RunId: bad.Msg.RunId}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "syntax") {
		t.Fatalf("unparsable down: %v", err)
	}

	execStore(t, storeDSN, "DROP TABLE cp_run_files")
	_, err = client.RevertRun(ctx, connect.NewRequest(&godwitv1.RevertRunRequest{RunId: bad.Msg.RunId}))
	if connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("missing files table: %v", err)
	}
}
