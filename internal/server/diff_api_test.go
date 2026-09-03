package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/gen/godwit/v1/godwitv1connect"
	"github.com/SamuelMolling/godwit/internal/controlplane"
)

func diffTarget(t *testing.T, skipValidation bool) (godwitv1connect.GodwitServiceClient, *pgx.Conn, string) {
	t.Helper()
	ctx := context.Background()
	client := newClient(startServiceCfg(t, Config{
		Listen: "127.0.0.1:0", StoreDSN: newDatabase(t, "st") + "&search_path=public", MasterKey: testKey, Holder: "r1",
		Scheduler: controlplane.Config{Interval: 50 * time.Millisecond}, Log: testLog, SkipValidation: skipValidation,
	}), "")
	targetDSN := newDatabase(t, "tg") + "&search_path=public"
	registerTarget(t, client, targetDSN)
	runToSuccess(t, client, orderedFiles()[:4], nil)
	conn, err := pgx.Connect(ctx, targetDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })

	return client, conn, targetDSN
}

func diffSchema(t *testing.T, client godwitv1connect.GodwitServiceClient, ddl string) *godwitv1.DiffResponse {
	t.Helper()
	res, err := client.Diff(context.Background(), connect.NewRequest(&godwitv1.DiffRequest{Target: "app", Schema: ddl}))
	if err != nil {
		t.Fatal(err)
	}

	return res.Msg
}

func TestDiff_AddColumn(t *testing.T) {
	t.Parallel()
	client, _, targetDSN := diffTarget(t, false)

	d := diffSchema(t, client, "CREATE TABLE t (id int, a int, status text NOT NULL DEFAULT 'new');")
	if !strings.Contains(d.UpSql, `ALTER TABLE "public"."t" ADD COLUMN "status"`) || !strings.Contains(d.DownSql, `DROP COLUMN "status"`) {
		t.Fatalf("up:\n%s\ndown:\n%s", d.UpSql, d.DownSql)
	}
	if len(d.Statements) != 1 || len(d.Statements[0].Hazards) != 0 || d.Drift != "" || d.Observed.AppliedCount != 2 {
		t.Fatalf("diff = %+v", d)
	}

	runToSuccess(t, client, []*godwitv1.MigrationFile{
		{Name: "20260901120003_add_status.up.sql", Body: d.UpSql},
		{Name: "20260901120003_add_status.down.sql", Body: d.DownSql},
	}, nil)
	if !columnExists(t, targetDSN, "status") {
		t.Fatal("status column missing after applying the generated migration")
	}
	if same := diffSchema(t, client, "CREATE TABLE t (id int, a int, status text NOT NULL DEFAULT 'new');"); same.UpSql != "" || same.DownSql != "" || len(same.Statements) != 0 {
		t.Fatalf("second diff = %+v", same)
	}
}

func TestDiff_DropTableDownRecreates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client, conn, _ := diffTarget(t, false)
	runToSuccess(t, client, []*godwitv1.MigrationFile{
		{Name: "20260901120003_orders.up.sql", Body: "CREATE TABLE orders (id bigint PRIMARY KEY, note text);"},
		{Name: "20260901120003_orders.down.sql", Body: "DROP TABLE orders;"},
	}, nil)

	d := diffSchema(t, client, "CREATE TABLE t (id int, a int);")
	if !strings.Contains(d.UpSql, `DROP TABLE "public"."orders"`) || !strings.Contains(d.DownSql, `CREATE TABLE "public"."orders"`) ||
		!strings.Contains(d.DownSql, `PRIMARY KEY USING INDEX`) {
		t.Fatalf("up:\n%s\ndown:\n%s", d.UpSql, d.DownSql)
	}
	if len(d.Statements) != 1 || d.Statements[0].Hazards[0].Code != "H002" || d.Statements[0].Hazards[0].Recipe == "" {
		t.Fatalf("statements = %+v", d.Statements)
	}

	files := []*godwitv1.MigrationFile{
		{Name: "20260901120004_drop_orders.up.sql", Body: d.UpSql},
		{Name: "20260901120004_drop_orders.down.sql", Body: d.DownSql},
	}
	created, err := client.CreateRun(ctx, connect.NewRequest(&godwitv1.CreateRunRequest{Target: "app", Files: files, AcknowledgeHazards: []string{"H002"}}))
	if err != nil {
		t.Fatal(err)
	}
	waitRun(t, client, created.Msg.RunId)
	if tableExists(t, conn, "orders") {
		t.Fatal("orders survived the generated up")
	}
	reverted, err := client.RevertRun(ctx, connect.NewRequest(&godwitv1.RevertRunRequest{RunId: created.Msg.RunId}))
	if err != nil {
		t.Fatal(err)
	}
	waitRun(t, client, reverted.Msg.RunId)
	if !tableExists(t, conn, "orders") {
		t.Fatal("orders missing after the generated down")
	}
	if same := diffSchema(t, client, "CREATE TABLE t (id int, a int);\nCREATE TABLE orders (id bigint PRIMARY KEY, note text);"); same.UpSql != "" {
		t.Fatalf("after revert:\n%s", same.UpSql)
	}
}

func TestDiff_NoChanges(t *testing.T) {
	t.Parallel()
	client, _, _ := diffTarget(t, true)

	d := diffSchema(t, client, "CREATE TABLE t (id int, a int);")
	if d.UpSql != "" || d.DownSql != "" || len(d.Statements) != 0 || d.Drift != "" || d.Observed.AppliedCount != 2 {
		t.Fatalf("diff = %+v", d)
	}
}

func TestDiff_DriftReported(t *testing.T) {
	t.Parallel()
	client, conn, _ := diffTarget(t, false)
	execTarget(t, conn, "ALTER TABLE t ADD COLUMN extra int")

	d := diffSchema(t, client, "CREATE TABLE t (id int, a int, extra int, b int);")
	if d.Drift != "+ column public.t.extra integer null=YES default=<none>" {
		t.Fatalf("drift = %q", d.Drift)
	}
	if !strings.Contains(d.UpSql, `ADD COLUMN "b"`) || strings.Contains(d.UpSql, "extra") {
		t.Fatalf("up:\n%s", d.UpSql)
	}
}

func TestDiff_IndexConcurrently(t *testing.T) {
	t.Parallel()
	client, _, _ := diffTarget(t, false)

	d := diffSchema(t, client, "CREATE TABLE t (id int, a int);\nCREATE INDEX t_a_idx ON t (a);")
	if !strings.Contains(d.UpSql, "CREATE INDEX CONCURRENTLY t_a_idx ON public.t") || !strings.Contains(d.DownSql, "DROP INDEX CONCURRENTLY") {
		t.Fatalf("up:\n%s\ndown:\n%s", d.UpSql, d.DownSql)
	}
	if len(d.Statements) != 1 || !d.Statements[0].NoTx || len(d.Statements[0].Hazards) != 0 {
		t.Fatalf("statements = %+v", d.Statements)
	}
	runToSuccess(t, client, []*godwitv1.MigrationFile{
		{Name: "20260901120003_idx.up.sql", Body: d.UpSql},
		{Name: "20260901120003_idx.down.sql", Body: d.DownSql},
	}, nil)
}

func TestDiff_BaseFiles(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client, _, _ := diffTarget(t, false)

	files := orderedFiles()
	desired := "CREATE TABLE t (id int, a int, b int);"
	res, err := client.Diff(ctx, connect.NewRequest(&godwitv1.DiffRequest{
		Target: "app", Schema: desired, Base: godwitv1.DiffBase_DIFF_BASE_FILES, Files: files,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if res.Msg.UpSql != "" || res.Msg.DownSql != "" {
		t.Fatalf("committed files already express the schema: up = %q, down = %q", res.Msg.UpSql, res.Msg.DownSql)
	}

	live := diffSchema(t, client, desired)
	if !strings.Contains(live.UpSql, `ADD COLUMN "b"`) {
		t.Fatalf("the live base still owes the pending migration: %s", live.UpSql)
	}

	files[4].Body = "ALTER TABLE t ADD COLUMN c int;"
	res, err = client.Diff(ctx, connect.NewRequest(&godwitv1.DiffRequest{
		Target: "app", Schema: desired, Base: godwitv1.DiffBase_DIFF_BASE_FILES, Files: files,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Msg.UpSql, `ADD COLUMN "b"`) || !strings.Contains(res.Msg.UpSql, `DROP COLUMN "c"`) {
		t.Fatalf("a hand-edited generated file must show as residue: %s", res.Msg.UpSql)
	}

	res, err = client.Diff(ctx, connect.NewRequest(&godwitv1.DiffRequest{
		Target: "app", Schema: "CREATE TABLE t (id int, a int, b int, email text);",
		Base: godwitv1.DiffBase_DIFF_BASE_FILES, Files: orderedFiles(),
	}))
	if err != nil || !strings.Contains(res.Msg.UpSql, `ADD COLUMN "email"`) {
		t.Fatalf("a schema changed without regenerating: up = %q, err = %v", res.Msg.UpSql, err)
	}
}

func TestDiff_BaseFilesNeedsValidation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client, _, _ := diffTarget(t, true)

	_, err := client.Diff(ctx, connect.NewRequest(&godwitv1.DiffRequest{
		Target: "app", Schema: "CREATE TABLE t (id int);", Base: godwitv1.DiffBase_DIFF_BASE_FILES, Files: orderedFiles(),
	}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "needs validation") {
		t.Fatalf("err = %v", err)
	}
}

func TestDiff_Errors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client, _, _ := diffTarget(t, false)

	_, err := client.Diff(ctx, connect.NewRequest(&godwitv1.DiffRequest{Target: "app", Schema: "CREATE TABLE t (id nosuchtype);"}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "desired schema failed to apply") {
		t.Fatalf("bad ddl: %v", err)
	}
	_, err = client.Diff(ctx, connect.NewRequest(&godwitv1.DiffRequest{Target: "ghost", Schema: "CREATE TABLE t (id int);"}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("unknown target: %v", err)
	}
}

func tableExists(t *testing.T, conn *pgx.Conn, name string) bool {
	t.Helper()
	var present bool
	if err := conn.QueryRow(context.Background(), "SELECT to_regclass($1) IS NOT NULL", "public."+name).Scan(&present); err != nil {
		t.Fatal(err)
	}

	return present
}
