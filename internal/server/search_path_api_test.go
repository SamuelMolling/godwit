package server

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/gen/godwit/v1/godwitv1connect"
)

func TestAPISearchPathValidation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client := newClient(startService(t, newDatabase(t, "st"), "r1", nil), "")
	cases := []struct{ name, path, want string }{
		{"journal schema", "app,godwit", "holds godwit's journal"},
		{"not an identifier", "my schema", "is not a schema name"},
		{"role dependent", "$user,public", "is not a schema name"},
	}
	for _, tc := range cases {
		_, err := client.RegisterTarget(ctx, connect.NewRequest(&godwitv1.RegisterTargetRequest{
			Name: "a", Provider: "kubernetes", SecretPath: "/s", SearchPath: tc.path,
		}))
		if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: err = %v", tc.name, err)
		}
	}
}

func targetExec(t *testing.T, dsn string, sql ...string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	for _, s := range sql {
		if _, err := conn.Exec(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
}

func targetScan(t *testing.T, dsn, sql string, into any) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	if err := conn.QueryRow(ctx, sql).Scan(into); err != nil {
		t.Fatal(err)
	}
}

func registerWithSearchPath(t *testing.T, client godwitv1connect.GodwitServiceClient, dsn, path string) {
	t.Helper()
	if _, err := client.RegisterTarget(context.Background(), connect.NewRequest(&godwitv1.RegisterTargetRequest{
		Name: "app", Provider: "static", Dsn: dsn, SearchPath: path,
	})); err != nil {
		t.Fatal(err)
	}
}

func shadowFiles() []*godwitv1.MigrationFile {
	return []*godwitv1.MigrationFile{
		{Name: "20260901120000_shadow.up.sql", Body: "CREATE TABLE migrations (id int, note text);"},
		{Name: "20260901120000_shadow.down.sql", Body: "DROP TABLE migrations;"},
	}
}

func TestAPISearchPathAppliedAndJournalUntouched(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client := newClient(startService(t, newDatabase(t, "st"), "r1", nil), "")
	targetDSN := newDatabase(t, "tg")
	targetExec(t, targetDSN, "CREATE SCHEMA app")
	registerWithSearchPath(t, client, targetDSN, "app,public")

	created, err := client.CreateRun(ctx, connect.NewRequest(&godwitv1.CreateRunRequest{Target: "app", Files: shadowFiles()}))
	if err != nil {
		t.Fatal(err)
	}
	waitRun(t, client, created.Msg.RunId)

	var schemas string
	targetScan(t, targetDSN,
		`SELECT coalesce(string_agg(table_schema, ',' ORDER BY table_schema), '') FROM information_schema.tables WHERE table_name = 'migrations'`,
		&schemas)
	if schemas != "app,godwit" {
		t.Fatalf("migrations tables = %q, want the migration in app and the journal in godwit", schemas)
	}
	var journalled string
	targetScan(t, targetDSN, `SELECT string_agg(name, ',') FROM godwit.migrations`, &journalled)
	if journalled != "shadow" {
		t.Fatalf("godwit.migrations = %q", journalled)
	}

	st, err := client.GetTargetStatus(ctx, connect.NewRequest(&godwitv1.GetTargetStatusRequest{Target: "app"}))
	if err != nil || st.Msg.SearchPath != "app,public" {
		t.Fatalf("status = %+v, err = %v", st.Msg, err)
	}
	plan := persistPlan(t, client, []*godwitv1.MigrationFile{
		{Name: "20260901130000_next.up.sql", Body: "CREATE TABLE widgets (id int);"},
		{Name: "20260901130000_next.down.sql", Body: "DROP TABLE widgets;"},
	}, nil)
	if plan.Observed.SearchPath != "app,public" {
		t.Fatalf("observation = %+v", plan.Observed)
	}

	reverted, err := client.RevertRun(ctx, connect.NewRequest(&godwitv1.RevertRunRequest{RunId: created.Msg.RunId, AcknowledgeHazards: []string{"H002"}}))
	if err != nil {
		t.Fatal(err)
	}
	waitRun(t, client, reverted.Msg.RunId)
	targetScan(t, targetDSN,
		`SELECT coalesce(string_agg(table_schema, ',' ORDER BY table_schema), '') FROM information_schema.tables WHERE table_name = 'migrations'`,
		&schemas)
	if schemas != "godwit" {
		t.Fatalf("after revert = %q, want the journal table alone", schemas)
	}
}

func TestAPIRevertRefusesUnreachableTarget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client := newClient(startService(t, newDatabase(t, "st"), "r1", nil), "")
	targetDSN := newDatabase(t, "tg")
	targetExec(t, targetDSN, "CREATE SCHEMA app")
	registerWithSearchPath(t, client, targetDSN, "app,public")

	created, err := client.CreateRun(ctx, connect.NewRequest(&godwitv1.CreateRunRequest{Target: "app", Files: shadowFiles()}))
	if err != nil {
		t.Fatal(err)
	}
	waitRun(t, client, created.Msg.RunId)

	registerWithSearchPath(t, client, "postgres://nobody@127.0.0.1:1/x", "app,public")
	_, err = client.RevertRun(ctx, connect.NewRequest(&godwitv1.RevertRunRequest{RunId: created.Msg.RunId, AcknowledgeHazards: []string{"H002"}}))
	if connect.CodeOf(err) != connect.CodeInternal || !strings.Contains(err.Error(), "connect target") {
		t.Fatalf("err = %v", err)
	}
}

func TestAPIPlanStaleWhenSearchPathMoves(t *testing.T) {
	t.Parallel()
	client := newClient(startService(t, newDatabase(t, "st"), "r1", nil), "")
	targetDSN := newDatabase(t, "tg")
	targetExec(t, targetDSN, "CREATE SCHEMA app")
	registerWithSearchPath(t, client, targetDSN, "app,public")

	persistPlan(t, client, shadowFiles(), nil)
	registerWithSearchPath(t, client, targetDSN, "public")

	_, err := createRun(t, client, shadowFiles(), false)
	stale, _ := planDetail(t, err)
	if stale.Reason != "schema" || !strings.Contains(stale.SchemaDiff, "- search_path app,public") ||
		!strings.Contains(stale.SchemaDiff, "+ search_path public") {
		t.Fatalf("stale = %+v", stale)
	}
}
