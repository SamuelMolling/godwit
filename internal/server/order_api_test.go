package server

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
)

func orderedFiles() []*godwitv1.MigrationFile {
	return []*godwitv1.MigrationFile{
		{Name: "20260901120001_t.up.sql", Body: "CREATE TABLE t (id int);"},
		{Name: "20260901120001_t.down.sql", Body: "DROP TABLE t;"},
		{Name: "20260901120002_a.up.sql", Body: "ALTER TABLE t ADD COLUMN a int;"},
		{Name: "20260901120002_a.down.sql", Body: "ALTER TABLE t DROP COLUMN a;"},
		{Name: "20260901120003_b.up.sql", Body: "ALTER TABLE t ADD COLUMN b int;"},
		{Name: "20260901120003_b.down.sql", Body: "ALTER TABLE t DROP COLUMN b;"},
	}
}

func TestOutOfOrderGuard(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client := newClient(startService(t, newDatabase(t, "st"), "r1", nil), "")
	targetDSN := newDatabase(t, "tg")
	registerTarget(t, client, targetDSN)

	all := orderedFiles()
	runToSuccess(t, client, append(all[:2:2], all[4:]...), nil)

	_, err := client.CreateRun(ctx, connect.NewRequest(&godwitv1.CreateRunRequest{Target: "app", Files: all}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition ||
		!strings.Contains(err.Error(), "out-of-order migrations 20260901120002: newest applied version on app is 20260901120003") {
		t.Fatalf("out of order: %v", err)
	}
	list, err := client.ListRuns(ctx, connect.NewRequest(&godwitv1.ListRunsRequest{Target: "app"}))
	if err != nil || len(list.Msg.Runs) != 1 {
		t.Fatalf("runs after refusal = %+v, err = %v", list, err)
	}

	created, err := client.CreateRun(ctx, connect.NewRequest(&godwitv1.CreateRunRequest{Target: "app", Files: all[2:4], AllowOutOfOrder: true}))
	if err != nil {
		t.Fatal(err)
	}
	if run := watchToEnd(t, client, created.Msg.RunId); run.State != godwitv1.RunState_RUN_STATE_SUCCEEDED {
		t.Fatalf("out-of-order run = %+v", run)
	}

	conn, err := pgx.Connect(ctx, targetDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	var n int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM godwit.migrations").Scan(&n); err != nil || n != 3 {
		t.Fatalf("applied versions = %d, err = %v", n, err)
	}

	revert, err := client.RevertRun(ctx, connect.NewRequest(&godwitv1.RevertRunRequest{RunId: created.Msg.RunId, AcknowledgeHazards: []string{"H003"}}))
	if err != nil {
		t.Fatal(err)
	}
	if run := watchToEnd(t, client, revert.Msg.RunId); run.State != godwitv1.RunState_RUN_STATE_SUCCEEDED {
		t.Fatalf("revert of the out-of-order run = %+v", run)
	}
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM godwit.migrations").Scan(&n); err != nil || n != 2 {
		t.Fatalf("applied versions after revert = %d, err = %v", n, err)
	}
}
