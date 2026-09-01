package server

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/internal/controlplane"
)

func columnExists(t *testing.T, dsn, table, column string) bool {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	var exists bool
	if err := conn.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = $1 AND column_name = $2)`,
		table, column).Scan(&exists); err != nil {
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
	if !columnExists(t, targetDSN, "t", "new_id") || !columnExists(t, targetDSN, "t", "id") {
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
	if columnExists(t, targetDSN, "t", "id") {
		t.Fatal("contract phase must drop id")
	}

	_, err = client.ConfirmRollout(ctx, connect.NewRequest(&godwitv1.ConfirmRolloutRequest{RunId: runID}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("err = %v", err)
	}
}
