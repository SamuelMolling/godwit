package server

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/gen/godwit/v1/godwitv1connect"
	"github.com/SamuelMolling/godwit/internal/controlplane"
)

const usersRows = 20000

func usersFiles() []*godwitv1.MigrationFile {
	return []*godwitv1.MigrationFile{
		{Name: "20260901120000_users.up.sql", Body: fmt.Sprintf(
			"CREATE TABLE public.users (id bigserial PRIMARY KEY, age integer NOT NULL);\n"+
				"INSERT INTO public.users (age) SELECT g FROM generate_series(1, %d) g;", usersRows)},
		{Name: "20260901120000_users.down.sql", Body: "DROP TABLE public.users;"},
	}
}

func changeTypeFiles(directive, down string) []*godwitv1.MigrationFile {
	return []*godwitv1.MigrationFile{
		{Name: "20260901130000_age.up.sql", Body: "-- godwit: " + directive + "\n"},
		{Name: "20260901130000_age.down.sql", Body: down},
	}
}

func directiveSet(directive, down string) []*godwitv1.MigrationFile {
	return append(usersFiles(), changeTypeFiles(directive, down)...)
}

func targetConn(t *testing.T, dsn string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })

	return conn
}

func columnType(t *testing.T, dsn, column string) string {
	t.Helper()
	var typ string
	err := targetConn(t, dsn).QueryRow(context.Background(),
		`SELECT coalesce(max(format_type(atttypid, atttypmod)), '')
		 FROM pg_attribute WHERE attrelid = 'public.users'::regclass AND attname = $1 AND NOT attisdropped`,
		column).Scan(&typ)
	if err != nil {
		t.Fatal(err)
	}

	return typ
}

func planWith(t *testing.T, client godwitv1connect.GodwitServiceClient, files []*godwitv1.MigrationFile) *godwitv1.PlanRunResponse {
	t.Helper()
	res, err := client.PlanRun(context.Background(), connect.NewRequest(&godwitv1.PlanRunRequest{
		Target: "app", Files: files, Rollout: controlplane.RolloutExpandContract, Persist: true,
	}))
	if err != nil {
		t.Fatal(err)
	}

	return res.Msg
}

func waitState(t *testing.T, client godwitv1connect.GodwitServiceClient, id string, want godwitv1.RunState) *godwitv1.Run {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	var last *godwitv1.Run
	for time.Now().Before(deadline) {
		r, err := client.GetRun(context.Background(), connect.NewRequest(&godwitv1.GetRunRequest{RunId: id}))
		if err != nil {
			t.Fatal(err)
		}
		last = r.Msg.Run
		if last.State == want {
			return last
		}
		if last.State == godwitv1.RunState_RUN_STATE_FAILED || last.State == godwitv1.RunState_RUN_STATE_NEEDS_ATTENTION {
			t.Fatalf("run %s: %s", last.State, last.Error)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("run never reached %v, last %+v", want, last)

	return nil
}

func startRun(t *testing.T, client godwitv1connect.GodwitServiceClient, files []*godwitv1.MigrationFile) string {
	t.Helper()
	created, err := client.CreateRun(context.Background(), connect.NewRequest(&godwitv1.CreateRunRequest{
		Target: "app", Files: files, Rollout: controlplane.RolloutExpandContract,
	}))
	if err != nil {
		t.Fatal(err)
	}

	return created.Msg.RunId
}

// directiveService boots a replica with the users table already applied on a fresh target.
func directiveService(t *testing.T) (godwitv1connect.GodwitServiceClient, string) {
	t.Helper()
	client, targetDSN, _ := directiveServiceStore(t)

	return client, targetDSN
}

func directiveServiceStore(t *testing.T) (godwitv1connect.GodwitServiceClient, string, string) {
	t.Helper()
	storeDSN := newDatabase(t, "st")
	client := newClient(startService(t, storeDSN, "r1", nil), "")
	targetDSN := newDatabase(t, "tg")
	registerTarget(t, client, targetDSN)
	runToSuccess(t, client, usersFiles(), nil)

	return client, targetDSN, storeDSN
}

func TestPlanRunExpandsChangeType(t *testing.T) {
	t.Parallel()
	client, _ := directiveService(t)
	msg := planWith(t, client, directiveSet("change-type public.users.age bigint", "-- godwit: revert\n"))

	var pm *godwitv1.PlannedMigration
	for _, m := range msg.Migrations {
		if m.Name == "age" {
			pm = m
		}
	}
	if pm == nil || !pm.Expanded || len(pm.Directives) != 1 {
		t.Fatalf("migration = %+v", pm)
	}
	if pm.Directives[0] != "-- godwit: change-type public.users.age bigint" {
		t.Fatalf("directive = %q", pm.Directives[0])
	}
	expand, contract, batches := 0, 0, 0
	for _, st := range pm.Statements {
		switch st.Phase {
		case controlplane.PhaseContract:
			contract++
		default:
			expand++
		}
		if st.Batch != nil {
			batches++
			if st.Batch.Key != "id" || st.Batch.Kind != "int" || st.Batch.Size != 5000 {
				t.Fatalf("batch = %+v", st.Batch)
			}
		}
	}
	if expand != 6 || contract != 6 || batches != 1 {
		t.Fatalf("expand %d contract %d batches %d: %+v", expand, contract, batches, pm.Statements)
	}
	if !strings.Contains(pm.Statements[3].Sql, "$1::bigint") {
		t.Fatalf("backfill does not cast the cursor: %s", pm.Statements[3].Sql)
	}
	if !strings.Contains(strings.Join(pm.Notes, "\n"), "leaves public.users.age_old for rollback") {
		t.Fatalf("notes = %v", pm.Notes)
	}
	if msg.PlanId == "" {
		t.Fatal("expansion must be stored with the plan")
	}
}

func TestCreateRunAppliesTheStoredExpansion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client, targetDSN := directiveService(t)
	files := directiveSet("change-type public.users.age bigint batch=5000 pause=150ms", "-- godwit: revert\n")
	planWith(t, client, files)
	runID := startRun(t, client, files)

	insertDuringBackfill(t, targetDSN)
	held := waitState(t, client, runID, godwitv1.RunState_RUN_STATE_AWAITING_CONTRACT)
	if held.Phase != controlplane.PhaseExpand {
		t.Fatalf("held run = %+v", held)
	}
	if columnType(t, targetDSN, "age") != "integer" || columnType(t, targetDSN, "age_new") != "bigint" {
		t.Fatal("expand phase must add age_new and keep age")
	}
	assertSynced(t, targetDSN)

	var recorded int
	if err := targetConn(t, targetDSN).QueryRow(ctx,
		`SELECT count(*) FROM godwit.migrations WHERE version = 20260901130000`).Scan(&recorded); err != nil {
		t.Fatal(err)
	}
	if recorded != 0 {
		t.Fatal("a held migration must not be recorded before the contract phase")
	}

	if _, err := client.ConfirmRollout(ctx, connect.NewRequest(&godwitv1.ConfirmRolloutRequest{RunId: runID})); err != nil {
		t.Fatal(err)
	}
	waitState(t, client, runID, godwitv1.RunState_RUN_STATE_SUCCEEDED)
	if got := columnType(t, targetDSN, "age"); got != "bigint" {
		t.Fatalf("age = %s", got)
	}
	if got := columnType(t, targetDSN, "age_old"); got != "integer" {
		t.Fatalf("age_old = %q, want the pre-swap column kept for rollback", got)
	}
	if columnType(t, targetDSN, "age_new") != "" {
		t.Fatal("age_new must be gone after the swap")
	}
	assertChecksum(t, targetDSN, files)
	assertNoSync(t, targetDSN)
}

// insertDuringBackfill waits for the sync trigger and writes a row while the backfill is still running;
// the trigger, not the backfill, is what must make that row consistent.
func insertDuringBackfill(t *testing.T, dsn string) {
	t.Helper()
	ctx := context.Background()
	conn := targetConn(t, dsn)
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		var ok bool
		if err := conn.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'users_age_sync')`).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		if ok {
			if _, err := conn.Exec(ctx, `INSERT INTO public.users (age) VALUES (-1)`); err != nil {
				t.Fatal(err)
			}

			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("sync trigger never appeared")
}

func assertSynced(t *testing.T, dsn string) {
	t.Helper()
	var stale, rows int
	if err := targetConn(t, dsn).QueryRow(context.Background(),
		`SELECT count(*) FILTER (WHERE age_new IS DISTINCT FROM age::bigint), count(*) FROM public.users`).Scan(&stale, &rows); err != nil {
		t.Fatal(err)
	}
	if stale != 0 || rows != usersRows+1 {
		t.Fatalf("%d of %d rows out of sync", stale, rows)
	}
}

func assertNoSync(t *testing.T, dsn string) {
	t.Helper()
	var triggers, functions int
	if err := targetConn(t, dsn).QueryRow(context.Background(),
		`SELECT (SELECT count(*) FROM pg_trigger WHERE tgname = 'users_age_sync'),
		        (SELECT count(*) FROM pg_proc WHERE proname = 'users_age_sync')`).Scan(&triggers, &functions); err != nil {
		t.Fatal(err)
	}
	if triggers != 0 || functions != 0 {
		t.Fatalf("%d triggers and %d functions left behind", triggers, functions)
	}
}

// assertChecksum proves the target records the checksum of the committed file, not of the expansion.
func assertChecksum(t *testing.T, dsn string, files []*godwitv1.MigrationFile) {
	t.Helper()
	migs, err := controlplane.MigrationsFromFiles(fileMap(files))
	if err != nil {
		t.Fatal(err)
	}
	var want string
	for _, m := range migs {
		if m.Name == "age" {
			want = m.Checksum
		}
	}
	var got string
	if err := targetConn(t, dsn).QueryRow(context.Background(),
		`SELECT checksum FROM godwit.migrations WHERE version = 20260901130000`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("recorded checksum %s, want the file's %s", got, want)
	}
}

func fileMap(files []*godwitv1.MigrationFile) map[string]string {
	out := map[string]string{}
	for _, f := range files {
		out[f.Name] = f.Body
	}

	return out
}

func TestCreateRunDirectiveUnderDirectRolloutRefuses(t *testing.T) {
	t.Parallel()
	client, _ := directiveService(t)
	files := changeTypeFiles("change-type public.users.age bigint", "-- godwit: revert\n")
	_, err := client.CreateRun(context.Background(), connect.NewRequest(&godwitv1.CreateRunRequest{
		Target: "app", Files: files, Rollout: controlplane.RolloutDirect,
	}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition ||
		!strings.Contains(err.Error(), "use rollout: expand-contract") {
		t.Fatalf("err = %v", err)
	}
}

func TestPlanRunDirectiveUnderSkipValidation(t *testing.T) {
	t.Parallel()
	client, _ := directiveService(t)
	_, err := client.PlanRun(context.Background(), connect.NewRequest(&godwitv1.PlanRunRequest{
		Target: "app", Files: changeTypeFiles("change-type public.users.age bigint", "-- godwit: revert\n"),
		Rollout: controlplane.RolloutExpandContract, Persist: true, SkipValidation: true,
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument ||
		!strings.Contains(err.Error(), "drop --skip-validation") {
		t.Fatalf("err = %v", err)
	}
}

func TestPlanRunRefusesWhileAwaitingContract(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client, _ := directiveService(t)
	files := directiveSet("change-type public.users.age bigint", "-- godwit: revert\n")
	planWith(t, client, files)
	runID := startRun(t, client, files)
	waitState(t, client, runID, godwitv1.RunState_RUN_STATE_AWAITING_CONTRACT)

	_, err := client.PlanRun(ctx, connect.NewRequest(&godwitv1.PlanRunRequest{
		Target: "app", Files: files, Rollout: controlplane.RolloutExpandContract, Persist: true,
	}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "awaiting contract") {
		t.Fatalf("plan err = %v", err)
	}
	_, err = client.CreateRun(ctx, connect.NewRequest(&godwitv1.CreateRunRequest{
		Target: "app", Files: files, Rollout: controlplane.RolloutExpandContract,
	}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "awaiting contract") {
		t.Fatalf("create err = %v", err)
	}
}

func TestPlanRunReplanChangesExpansionIsStale(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client, _ := directiveService(t)
	files := changeTypeFiles("change-type public.users.age bigint", "-- godwit: revert\n")
	planWith(t, client, append(usersFiles(), files...))

	// A godwit run of its own: the history moves in an attributable way, so the bind gets as far as
	// comparing the statements instead of stopping at the schema check.
	runToSuccess(t, client, append(usersFiles(), []*godwitv1.MigrationFile{
		{Name: "20260901125000_nullable.up.sql", Body: "ALTER TABLE public.users ALTER COLUMN age DROP NOT NULL;"},
		{Name: "20260901125000_nullable.down.sql", Body: "ALTER TABLE public.users ALTER COLUMN age SET NOT NULL;"},
	}...), nil)

	_, err := client.CreateRun(ctx, connect.NewRequest(&godwitv1.CreateRunRequest{
		Target: "app", Files: append(usersFiles(), files...), Rollout: controlplane.RolloutExpandContract,
	}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition ||
		!strings.Contains(err.Error(), "statements changed after re-plan") {
		t.Fatalf("err = %v", err)
	}
}

func TestRevertChangeTypeAtAwaitingContract(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client, targetDSN := directiveService(t)
	files := changeTypeFiles("change-type public.users.age bigint", "-- godwit: revert\n")
	planWith(t, client, files)
	runID := startRun(t, client, files)
	waitState(t, client, runID, godwitv1.RunState_RUN_STATE_AWAITING_CONTRACT)

	rev, err := client.RevertRun(ctx, connect.NewRequest(&godwitv1.RevertRunRequest{RunId: runID}))
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, client, rev.Msg.RunId, godwitv1.RunState_RUN_STATE_SUCCEEDED)
	if columnType(t, targetDSN, "age") != "integer" || columnType(t, targetDSN, "age_new") != "" {
		t.Fatal("revert must drop age_new and leave age untouched")
	}
	assertNoSync(t, targetDSN)
	assertRowsIntact(t, targetDSN)
}

func TestRevertChangeTypeAfterSwap(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client, targetDSN := directiveService(t)
	files := changeTypeFiles("change-type public.users.age bigint", "-- godwit: revert\n")
	planWith(t, client, files)
	runID := startRun(t, client, files)
	waitState(t, client, runID, godwitv1.RunState_RUN_STATE_AWAITING_CONTRACT)
	if _, err := client.ConfirmRollout(ctx, connect.NewRequest(&godwitv1.ConfirmRolloutRequest{RunId: runID})); err != nil {
		t.Fatal(err)
	}
	waitState(t, client, runID, godwitv1.RunState_RUN_STATE_SUCCEEDED)

	rev, err := client.RevertRun(ctx, connect.NewRequest(&godwitv1.RevertRunRequest{RunId: runID}))
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, client, rev.Msg.RunId, godwitv1.RunState_RUN_STATE_SUCCEEDED)
	if got := columnType(t, targetDSN, "age"); got != "integer" {
		t.Fatalf("age = %s after revert", got)
	}
	if columnType(t, targetDSN, "age_new") != "" || columnType(t, targetDSN, "age_old") != "" {
		t.Fatal("revert must leave neither age_new nor age_old")
	}
	assertRowsIntact(t, targetDSN)
}

func assertRowsIntact(t *testing.T, dsn string) {
	t.Helper()
	var rows, sum int64
	if err := targetConn(t, dsn).QueryRow(context.Background(),
		`SELECT count(*), coalesce(sum(age), 0) FROM public.users`).Scan(&rows, &sum); err != nil {
		t.Fatal(err)
	}
	if rows != usersRows || sum != int64(usersRows)*(usersRows+1)/2 {
		t.Fatalf("%d rows summing to %d", rows, sum)
	}
}

func TestPlanRunKeepOldFalseRefusesGeneratedDown(t *testing.T) {
	t.Parallel()
	client, _ := directiveService(t)
	_, err := client.PlanRun(context.Background(), connect.NewRequest(&godwitv1.PlanRunRequest{
		Target: "app", Files: changeTypeFiles("change-type public.users.age bigint keep-old=false", "-- godwit: revert\n"),
		Rollout: controlplane.RolloutExpandContract, Persist: true,
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument ||
		!strings.Contains(err.Error(), "keep-old=false cannot be generated") {
		t.Fatalf("err = %v", err)
	}
}

func TestCreateRunImplicitDirectiveExpandsAtAdmission(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client, targetDSN := directiveService(t)
	files := changeTypeFiles("change-type public.users.age bigint keep-old=false",
		"ALTER TABLE public.users DROP COLUMN age;\nALTER TABLE public.users ADD COLUMN age integer;")
	runID := startRun(t, client, files)
	waitState(t, client, runID, godwitv1.RunState_RUN_STATE_AWAITING_CONTRACT)
	if _, err := client.ConfirmRollout(ctx, connect.NewRequest(&godwitv1.ConfirmRolloutRequest{RunId: runID})); err != nil {
		t.Fatal(err)
	}
	waitState(t, client, runID, godwitv1.RunState_RUN_STATE_SUCCEEDED)
	if got := columnType(t, targetDSN, "age"); got != "bigint" {
		t.Fatalf("age = %s", got)
	}
	if columnType(t, targetDSN, "age_old") != "" {
		t.Fatal("keep-old=false must drop the pre-swap column")
	}

	audit, err := client.ListAudit(ctx, connect.NewRequest(&godwitv1.ListAuditRequest{RunId: runID}))
	if err != nil {
		t.Fatal(err)
	}
	var detail string
	for _, e := range audit.Msg.Entries {
		if e.Action == controlplane.AuditRunCreate {
			detail = e.Detail
		}
	}
	if !strings.Contains(detail, "expands 20260901130000_age") {
		t.Fatalf("audit detail = %q", detail)
	}
}

func TestPlanRunDirectiveUnderDirectRolloutRefuses(t *testing.T) {
	t.Parallel()
	client, _ := directiveService(t)
	_, err := client.PlanRun(context.Background(), connect.NewRequest(&godwitv1.PlanRunRequest{
		Target: "app", Files: changeTypeFiles("change-type public.users.age bigint", "-- godwit: revert\n"),
		Rollout: controlplane.RolloutDirect, Persist: true,
	}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition ||
		!strings.Contains(err.Error(), "use rollout: expand-contract") {
		t.Fatalf("err = %v", err)
	}
}

func TestRevertRefusesUnexpandableDown(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client, _, storeDSN := directiveServiceStore(t)
	files := changeTypeFiles("change-type public.users.age bigint", "-- godwit: revert\n")
	planWith(t, client, files)
	runID := startRun(t, client, files)
	waitState(t, client, runID, godwitv1.RunState_RUN_STATE_AWAITING_CONTRACT)

	execStore(t, storeDSN, `UPDATE cp_runs SET expansions = '{"20260901130000_age": {"down_held_sql": "NOT SQL"}}'::jsonb WHERE state = 'awaiting_contract'`)
	if _, err := client.RevertRun(ctx, connect.NewRequest(&godwitv1.RevertRunRequest{RunId: runID})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("err = %v", err)
	}
}

func TestTargetKeepOldDefault(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client, targetDSN, _ := directiveServiceStore(t)
	keepOld := false
	if _, err := client.RegisterTarget(ctx, connect.NewRequest(&godwitv1.RegisterTargetRequest{
		Name: "app", Provider: "static", Dsn: targetDSN, KeepOld: &keepOld,
	})); err != nil {
		t.Fatal(err)
	}
	files := changeTypeFiles("change-type public.users.age bigint",
		"ALTER TABLE public.users DROP COLUMN age;\nALTER TABLE public.users ADD COLUMN age integer;")
	msg := planWith(t, client, files)
	var notes []string
	for _, m := range msg.Migrations {
		notes = append(notes, m.Notes...)
	}
	if !strings.Contains(strings.Join(notes, "\n"), "becomes irreversible") {
		t.Fatalf("the target default must drop the old column: %v", notes)
	}

	keepOld = true
	if _, err := client.RegisterTarget(ctx, connect.NewRequest(&godwitv1.RegisterTargetRequest{
		Name: "app", Provider: "static", Dsn: targetDSN, KeepOld: &keepOld,
	})); err != nil {
		t.Fatal(err)
	}
	msg = planWith(t, client, changeTypeFiles("change-type public.users.age bigint", "-- godwit: revert\n"))
	notes = nil
	for _, m := range msg.Migrations {
		notes = append(notes, m.Notes...)
	}
	if !strings.Contains(strings.Join(notes, "\n"), "for rollback") {
		t.Fatalf("keep_old=true must keep the old column: %v", notes)
	}
}
