package server

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/gen/godwit/v1/godwitv1connect"
	"github.com/SamuelMolling/godwit/internal/controlplane"
)

func ordersFiles() []*godwitv1.MigrationFile {
	return []*godwitv1.MigrationFile{
		{Name: "20260901120000_orders.up.sql", Body: "CREATE TABLE public.orders (id bigserial PRIMARY KEY, total numeric);\n" +
			"INSERT INTO public.orders (total) VALUES (10), (20), (30);"},
		{Name: "20260901120000_orders.down.sql", Body: "DROP TABLE public.orders;"},
	}
}

func assertFiles(body string) []*godwitv1.MigrationFile {
	return append(ordersFiles(),
		&godwitv1.MigrationFile{Name: "20260901130000_check.up.sql", Body: body},
		&godwitv1.MigrationFile{Name: "20260901130000_check.down.sql", Body: "SELECT 1;"})
}

// assertService boots a service whose target already holds three orders.
func assertService(t *testing.T) (godwitv1connect.GodwitServiceClient, string) {
	t.Helper()
	client := newClient(startService(t, newDatabase(t, "st"), "r1", nil), "")
	targetDSN := newDatabase(t, "tg")
	registerTarget(t, client, targetDSN)
	runToSuccess(t, client, ordersFiles(), nil)

	return client, targetDSN
}

func ordersColumnType(t *testing.T, dsn, column string) string {
	t.Helper()
	var typ string
	err := targetConn(t, dsn).QueryRow(context.Background(),
		`SELECT coalesce(max(format_type(atttypid, atttypmod)), '')
		 FROM pg_attribute WHERE attrelid = 'public.orders'::regclass AND attname = $1 AND NOT attisdropped`,
		column).Scan(&typ)
	if err != nil {
		t.Fatal(err)
	}

	return typ
}

func execOnTarget(t *testing.T, dsn, sql string) {
	t.Helper()
	if _, err := targetConn(t, dsn).Exec(context.Background(), sql); err != nil {
		t.Fatal(err)
	}
}

const auditTable = "CREATE TABLE public.audit (id int);\n"

const nullTotals = "-- godwit: assert 'SELECT count(*) FROM orders WHERE total IS NULL' = 0\n"

// The plan carries the assertion as a statement with its condition beside it, and the run checks it
// before the statement it guards.
func TestAssertionIsPlannedAndChecked(t *testing.T) {
	client, targetDSN := assertService(t)
	files := assertFiles(nullTotals + auditTable)
	msg := planWith(t, client, files)

	var pm *godwitv1.PlannedMigration
	for _, m := range msg.Migrations {
		if m.Name == "check" {
			pm = m
		}
	}
	if pm == nil || !pm.Expanded || len(pm.Statements) != 2 {
		t.Fatalf("migration = %+v", pm)
	}
	st := pm.Statements[0]
	if st.Assert == nil || st.Assert.Op != "=" || st.Assert.Value != "0" || st.Assert.Kind != "int" {
		t.Fatalf("statement = %+v", st)
	}
	if !strings.HasSuffix(st.Sql, "SELECT count(*) FROM orders WHERE total IS NULL") {
		t.Fatalf("sql = %q", st.Sql)
	}
	if st.Phase != controlplane.PhaseExpand {
		t.Fatalf("phase = %q", st.Phase)
	}
	if pm.Statements[1].Assert != nil {
		t.Fatal("only the assertion carries a condition")
	}

	waitState(t, client, startRun(t, client, files), godwitv1.RunState_RUN_STATE_SUCCEEDED)
	if got := ordersColumnType(t, targetDSN, "total"); got != "numeric" {
		t.Fatalf("total = %q", got)
	}
	var audit int
	if err := targetConn(t, targetDSN).QueryRow(context.Background(),
		`SELECT count(*) FROM pg_class WHERE relname = 'audit'`).Scan(&audit); err != nil {
		t.Fatal(err)
	}
	if audit != 1 {
		t.Fatal("the statement the assertion guards must have run")
	}
}

func TestCreateRunAssertionFailsTheRun(t *testing.T) {
	client, targetDSN := assertService(t)
	files := assertFiles("-- godwit: assert 'SELECT count(*) FROM orders' = 0\n" + auditTable)
	planWith(t, client, files)
	runID := startRun(t, client, files)
	run := waitRunState(t, client, runID, godwitv1.RunState_RUN_STATE_FAILED)
	for _, want := range []string{"assertion failed", "returned 3", "want = 0"} {
		if !strings.Contains(run.Error, want) {
			t.Fatalf("error = %q, want containing %q", run.Error, want)
		}
	}
	if run.Attempts > 1 {
		t.Fatalf("attempts = %d; an assertion that does not hold is not transient", run.Attempts)
	}
	var audit int
	if err := targetConn(t, targetDSN).QueryRow(context.Background(),
		`SELECT count(*) FROM pg_class WHERE relname = 'audit'`).Scan(&audit); err != nil {
		t.Fatal(err)
	}
	if audit != 0 {
		t.Fatal("the statement after the assertion must not have run")
	}
}

const totalToBigint = "-- godwit: change-type public.orders.total bigint using='total::bigint'\n"

// The assertion sits at the end of the expand phase, so the confirm re-checks it against the data as it
// is now: a swap that was safe an hour ago does not become safe again.
func TestConfirmRolloutRecheckstheAssertion(t *testing.T) {
	ctx := context.Background()
	client, targetDSN := assertService(t)
	files := assertFiles(totalToBigint + nullTotals)
	planWith(t, client, files)
	runID := startRun(t, client, files)
	waitState(t, client, runID, godwitv1.RunState_RUN_STATE_AWAITING_CONTRACT)
	if got := ordersColumnType(t, targetDSN, "total_new"); got != "bigint" {
		t.Fatalf("total_new = %q", got)
	}

	execOnTarget(t, targetDSN, "INSERT INTO public.orders (total) VALUES (NULL)")
	if _, err := client.ConfirmRollout(ctx, connect.NewRequest(&godwitv1.ConfirmRolloutRequest{RunId: runID})); err != nil {
		t.Fatal(err)
	}
	run := waitRunState(t, client, runID, godwitv1.RunState_RUN_STATE_FAILED)
	if !strings.Contains(run.Error, "assertion failed") || !strings.Contains(run.Error, "returned 1") {
		t.Fatalf("error = %q", run.Error)
	}
	if got := ordersColumnType(t, targetDSN, "total"); got != "numeric" {
		t.Fatalf("total = %q, want the swap held back", got)
	}
	var recorded int
	if err := targetConn(t, targetDSN).QueryRow(ctx,
		`SELECT count(*) FROM godwit.migrations WHERE version = 20260901130000`).Scan(&recorded); err != nil {
		t.Fatal(err)
	}
	if recorded != 0 {
		t.Fatal("a migration whose assertion failed is not recorded")
	}
}

func TestConfirmRolloutSwapsWhenTheAssertionStillHolds(t *testing.T) {
	ctx := context.Background()
	client, targetDSN := assertService(t)
	files := assertFiles(totalToBigint + nullTotals)
	planWith(t, client, files)
	runID := startRun(t, client, files)
	waitState(t, client, runID, godwitv1.RunState_RUN_STATE_AWAITING_CONTRACT)
	if _, err := client.ConfirmRollout(ctx, connect.NewRequest(&godwitv1.ConfirmRolloutRequest{RunId: runID})); err != nil {
		t.Fatal(err)
	}
	waitState(t, client, runID, godwitv1.RunState_RUN_STATE_SUCCEEDED)
	if got := ordersColumnType(t, targetDSN, "total"); got != "bigint" {
		t.Fatalf("total = %q", got)
	}
}

// The comparison is part of the file, so editing it is editing the migration.
func TestEditedAssertionMakesThePlanStale(t *testing.T) {
	ctx := context.Background()
	client, _ := assertService(t)
	files := assertFiles(nullTotals)
	first := planWith(t, client, files)
	edited := assertFiles(strings.Replace(nullTotals, "= 0", "> 0", 1))
	second := planWith(t, client, edited)
	if first.PlanKey == second.PlanKey {
		t.Fatalf("both plans key to %s", first.PlanKey)
	}
	_, err := client.CreateRun(ctx, connect.NewRequest(&godwitv1.CreateRunRequest{
		Target: "app", Files: edited, Rollout: controlplane.RolloutExpandContract, PlanId: first.PlanId,
	}))
	if err == nil || !strings.Contains(err.Error(), "do not match plan") {
		t.Fatalf("err = %v", err)
	}
}

func TestAssertionRefusedAtPlanTime(t *testing.T) {
	ctx := context.Background()
	client, _ := assertService(t)
	cases := []struct{ name, body, want string }{
		{"unknown column", "-- godwit: assert 'SELECT count(*) FROM orders WHERE nope IS NULL' = 0\n", "nope"},
		{"wrong type", "-- godwit: assert 'SELECT max(total::text) FROM orders' = 0\n", "needs a int column"},
		{"no inverse", nullTotals, "no inverse"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := assertFiles(tc.body)
			if tc.name == "no inverse" {
				files[3].Body = "-- godwit: revert\n"
			}
			_, err := client.PlanRun(ctx, connect.NewRequest(&godwitv1.PlanRunRequest{
				Target: "app", Files: files, Rollout: controlplane.RolloutExpandContract, Persist: true,
			}))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
		})
	}
}
