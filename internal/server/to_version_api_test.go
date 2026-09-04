package server

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
)

func toVersionFiles() []*godwitv1.MigrationFile {
	return []*godwitv1.MigrationFile{
		{Name: "20260901120001_t.up.sql", Body: "CREATE TABLE t (id int);"},
		{Name: "20260901120001_t.down.sql", Body: "DROP TABLE t;"},
		{Name: "20260901120002_a.up.sql", Body: "ALTER TABLE t ADD COLUMN a int;"},
		{Name: "20260901120002_a.down.sql", Body: "ALTER TABLE t DROP COLUMN a;"},
		{Name: "20260901120003_b.up.sql", Body: "ALTER TABLE t ADD COLUMN b int;"},
		{Name: "20260901120003_b.down.sql", Body: "ALTER TABLE t DROP COLUMN b;"},
		{Name: "R__v.up.sql", Body: "CREATE OR REPLACE VIEW v AS SELECT id, b FROM t;"},
		{Name: "R__v.down.sql", Body: "DROP VIEW IF EXISTS v;"},
	}
}

func columns(t *testing.T, dsn, table string) string {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	rows, err := conn.Query(ctx, `SELECT column_name FROM information_schema.columns
		WHERE table_name = $1 AND table_schema NOT IN ('pg_catalog', 'information_schema') ORDER BY column_name`, table)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	var name string
	if _, err := pgx.ForEachRow(rows, []any{&name}, func() error {
		out = append(out, name)

		return nil
	}); err != nil {
		t.Fatal(err)
	}

	return strings.Join(out, ",")
}

func TestVersionTargetStopsAtTheChosenMigration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client := newClient(startService(t, newDatabase(t, "st"), "r1", nil), "")
	targetDSN := newDatabase(t, "tg")
	registerTarget(t, client, targetDSN)
	files := toVersionFiles()

	planned, err := client.PlanRun(ctx, connect.NewRequest(&godwitv1.PlanRunRequest{
		Target: "app", Files: files, ToVersion: 20260901120002, Persist: true,
	}))
	if err != nil {
		t.Fatal(err)
	}
	withheld := map[string]bool{}
	for _, m := range planned.Msg.Migrations {
		if m.Withheld {
			withheld[m.Name] = true
		}
	}
	if len(withheld) != 2 || !withheld["b"] || !withheld["v"] {
		t.Fatalf("withheld = %v; the plan must name what it does not cover", withheld)
	}

	created, err := client.CreateRun(ctx, connect.NewRequest(&godwitv1.CreateRunRequest{
		Target: "app", Files: files, ToVersion: 20260901120002,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if created.Msg.PlanId != planned.Msg.PlanId {
		t.Fatalf("plan = %q, want the stored plan %q: the same target produces the same key", created.Msg.PlanId, planned.Msg.PlanId)
	}
	if run := watchToEnd(t, client, created.Msg.RunId); run.State != godwitv1.RunState_RUN_STATE_SUCCEEDED {
		t.Fatalf("run = %+v", run)
	}
	if got := columns(t, targetDSN, "t"); got != "a,id" {
		t.Fatalf("columns = %q, want a,id", got)
	}
	if got := columns(t, targetDSN, "v"); got != "" {
		t.Fatalf("view v = %q; a repeatable is not built while a version it ships with is held back", got)
	}

	if _, err := client.CreateRun(ctx, connect.NewRequest(&godwitv1.CreateRunRequest{
		Target: "app", Files: files, ToVersion: 20260901120001,
	})); connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "it never reverts") {
		t.Fatalf("behind history: %v", err)
	}

	rest, err := client.CreateRun(ctx, connect.NewRequest(&godwitv1.CreateRunRequest{Target: "app", Files: files}))
	if err != nil {
		t.Fatal(err)
	}
	if run := watchToEnd(t, client, rest.Msg.RunId); run.State != godwitv1.RunState_RUN_STATE_SUCCEEDED {
		t.Fatalf("rest = %+v", run)
	}
	if got := columns(t, targetDSN, "t"); got != "a,b,id" {
		t.Fatalf("columns = %q, want a,b,id", got)
	}
	if got := columns(t, targetDSN, "v"); got != "b,id" {
		t.Fatalf("view v = %q; the repeatable runs once nothing is held back", got)
	}
}
