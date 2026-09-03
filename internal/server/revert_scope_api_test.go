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

func tableA() []*godwitv1.MigrationFile {
	return []*godwitv1.MigrationFile{
		{Name: "20260101000000_a.up.sql", Body: "CREATE TABLE a (id int);"},
		{Name: "20260101000000_a.down.sql", Body: "DROP TABLE a;"},
	}
}

func tableB() []*godwitv1.MigrationFile {
	return []*godwitv1.MigrationFile{
		{Name: "20260101000001_b.up.sql", Body: "CREATE TABLE b (id int);"},
		{Name: "20260101000001_b.down.sql", Body: "DROP TABLE b;"},
	}
}

func relationExists(t *testing.T, dsn, name string) bool {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	var rel *string
	if err := conn.QueryRow(ctx, `SELECT to_regclass($1)::text`, name).Scan(&rel); err != nil {
		t.Fatal(err)
	}

	return rel != nil
}

func appliedVersions(t *testing.T, dsn string) []int64 {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	rows, err := conn.Query(ctx, `SELECT version FROM godwit.migrations ORDER BY version`)
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		t.Fatal(err)
	}

	return out
}

func revertAndWait(t *testing.T, client godwitv1connect.GodwitServiceClient, req *godwitv1.RevertRunRequest) *godwitv1.RevertRunResponse {
	t.Helper()
	res, err := client.RevertRun(context.Background(), connect.NewRequest(req))
	if err != nil {
		t.Fatal(err)
	}
	if r := waitState(t, client, res.Msg.RunId, godwitv1.RunState_RUN_STATE_SUCCEEDED); r.Error != "" {
		t.Fatalf("revert run = %+v", r)
	}

	return res.Msg
}

// TestRevertActsOnWhatTheRunApplied is the incident PR #67 reported: `migrate` sends the whole
// directory every time, so reverting the second run used to drop the first run's table too.
func TestRevertActsOnWhatTheRunApplied(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client := newClient(startService(t, newDatabase(t, "st"), "r1", nil), "")
	targetDSN := newDatabase(t, "tg")
	registerTarget(t, client, targetDSN)

	runToSuccess(t, client, tableA(), nil)
	both := append(tableA(), tableB()...)
	created, err := client.CreateRun(ctx, connect.NewRequest(&godwitv1.CreateRunRequest{Target: "app", Files: both}))
	if err != nil {
		t.Fatal(err)
	}
	runB := created.Msg.RunId
	waitState(t, client, runB, godwitv1.RunState_RUN_STATE_SUCCEEDED)

	got, err := client.GetRun(ctx, connect.NewRequest(&godwitv1.GetRunRequest{RunId: runB}))
	if err != nil || len(got.Msg.Applied) != 1 || got.Msg.Applied[0].Migration != "20260101000001_b" {
		t.Fatalf("ledger = %+v, err = %v", got.Msg.Applied, err)
	}

	plan := revertAndWait(t, client, &godwitv1.RevertRunRequest{RunId: runB, AcknowledgeHazards: []string{"H002"}})
	if len(plan.Migrations) != 1 || plan.Migrations[0].Name != "b" || plan.Reverts != runB || plan.Target != "app" {
		t.Fatalf("revert plan = %+v", plan)
	}
	if !relationExists(t, targetDSN, "a") {
		t.Fatal("reverting run B must leave run A's table alone")
	}
	if relationExists(t, targetDSN, "b") {
		t.Fatal("reverting run B must drop its own table")
	}
	if v := appliedVersions(t, targetDSN); len(v) != 1 || v[0] != 20260101000000 {
		t.Fatalf("godwit.migrations = %v; only run B's version may go", v)
	}

	// The revert is a new ledger entry, not a hole in the history: both runs are still listed, the
	// original marked reverted, and its ledger row points at the run that undid it.
	list, err := client.ListRuns(ctx, connect.NewRequest(&godwitv1.ListRunsRequest{Target: "app"}))
	if err != nil || len(list.Msg.Runs) != 3 {
		t.Fatalf("runs = %+v, err = %v", list, err)
	}
	if got, err = client.GetRun(ctx, connect.NewRequest(&godwitv1.GetRunRequest{RunId: runB})); err != nil ||
		got.Msg.Run.State != godwitv1.RunState_RUN_STATE_REVERTED || got.Msg.Applied[0].RevertedBy == "" {
		t.Fatalf("original after revert = %+v %+v, err = %v", got.Msg.Run, got.Msg.Applied, err)
	}
}

// TestRevertOlderRunNeedsForce keeps the default at "the newest un-reverted run and nothing wider".
func TestRevertOlderRunNeedsForce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client := newClient(startService(t, newDatabase(t, "st"), "r1", nil), "")
	targetDSN := newDatabase(t, "tg")
	registerTarget(t, client, targetDSN)

	createdA, err := client.CreateRun(ctx, connect.NewRequest(&godwitv1.CreateRunRequest{Target: "app", Files: tableA()}))
	if err != nil {
		t.Fatal(err)
	}
	runA := createdA.Msg.RunId
	waitState(t, client, runA, godwitv1.RunState_RUN_STATE_SUCCEEDED)
	createdB, err := client.CreateRun(ctx, connect.NewRequest(&godwitv1.CreateRunRequest{
		Target: "app", Files: append(tableA(), tableB()...),
	}))
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, client, createdB.Msg.RunId, godwitv1.RunState_RUN_STATE_SUCCEEDED)

	_, err = client.RevertRun(ctx, connect.NewRequest(&godwitv1.RevertRunRequest{RunId: runA, AcknowledgeHazards: []string{"H002"}}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "is newer and still stands") {
		t.Fatalf("older run without force: %v", err)
	}
	dry, err := client.RevertRun(ctx, connect.NewRequest(&godwitv1.RevertRunRequest{
		RunId: runA, AcknowledgeHazards: []string{"H002"}, Force: true, DryRun: true,
	}))
	if err != nil || dry.Msg.RunId != "" || !dry.Msg.Forced || len(dry.Msg.Migrations) != 1 {
		t.Fatalf("forced dry run = %+v, err = %v", dry, err)
	}
	if !relationExists(t, targetDSN, "a") {
		t.Fatal("a dry run must not touch the target")
	}
	revertAndWait(t, client, &godwitv1.RevertRunRequest{RunId: runA, AcknowledgeHazards: []string{"H002"}, Force: true})
	if relationExists(t, targetDSN, "a") || !relationExists(t, targetDSN, "b") {
		t.Fatal("a forced revert undoes only the run it names")
	}

	// With no run id the newest un-reverted run is the target, and there is one left.
	plan := revertAndWait(t, client, &godwitv1.RevertRunRequest{Target: "app", AcknowledgeHazards: []string{"H002"}})
	if len(plan.Migrations) != 1 || plan.Migrations[0].Name != "b" {
		t.Fatalf("default target = %+v", plan)
	}
	_, err = client.RevertRun(ctx, connect.NewRequest(&godwitv1.RevertRunRequest{Target: "app"}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("nothing left to revert: %v", err)
	}
}

// TestRevertRefusesDataLoss holds Atlas's line: refuse, do not warn, when the plan destroys data.
func TestRevertRefusesDataLoss(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client := newClient(startService(t, newDatabase(t, "st"), "r1", nil), "")
	targetDSN := newDatabase(t, "tg")
	registerTarget(t, client, targetDSN)

	files := []*godwitv1.MigrationFile{
		{Name: "20260101000000_rows.up.sql", Body: "CREATE TABLE rows_t (id int, note text);\nINSERT INTO rows_t SELECT g, 'x' FROM generate_series(1, 5) g;"},
		{Name: "20260101000000_rows.down.sql", Body: "DROP TABLE rows_t;"},
		{Name: "20260101000001_empty.up.sql", Body: "CREATE TABLE empty_t (id int);"},
		{Name: "20260101000001_empty.down.sql", Body: "DROP TABLE empty_t;"},
	}
	runToSuccess(t, client, files, nil)

	_, err := client.RevertRun(ctx, connect.NewRequest(&godwitv1.RevertRunRequest{
		Target: "app", AcknowledgeHazards: []string{"H002"},
	}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition ||
		!strings.Contains(err.Error(), "rows_t holds 5 row(s)") || !strings.Contains(err.Error(), "--allow-data-loss") {
		t.Fatalf("data-loss gate: %v", err)
	}
	if !relationExists(t, targetDSN, "rows_t") {
		t.Fatal("the refusal must leave the target alone")
	}
	plan := revertAndWait(t, client, &godwitv1.RevertRunRequest{
		Target: "app", AcknowledgeHazards: []string{"H002"}, AllowDataLoss: true,
	})
	if len(plan.DataLoss) != 1 || plan.DataLoss[0].Kind != "table" || plan.DataLoss[0].Object != "rows_t" || plan.DataLoss[0].Rows != 5 {
		t.Fatalf("data loss = %+v", plan.DataLoss)
	}
	if relationExists(t, targetDSN, "rows_t") || relationExists(t, targetDSN, "empty_t") {
		t.Fatal("the allowed revert undoes both migrations")
	}
}

// TestRevertRefusesDataLossOnColumn measures a dropped column by the values it still holds.
func TestRevertRefusesDataLossOnColumn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client := newClient(startService(t, newDatabase(t, "st"), "r1", nil), "")
	targetDSN := newDatabase(t, "tg")
	registerTarget(t, client, targetDSN)

	runToSuccess(t, client, []*godwitv1.MigrationFile{
		{Name: "20260101000000_t.up.sql", Body: "CREATE TABLE t (id int);\nINSERT INTO t SELECT generate_series(1, 3);"},
		{Name: "20260101000000_t.down.sql", Body: "DROP TABLE t;"},
	}, nil)
	addNote := []*godwitv1.MigrationFile{
		{Name: "20260101000001_note.up.sql", Body: "ALTER TABLE t ADD COLUMN note text;"},
		{Name: "20260101000001_note.down.sql", Body: "ALTER TABLE t DROP COLUMN note;"},
	}
	runToSuccess(t, client, addNote, []string{"H003"})

	// Nothing has been written to the column yet, so the revert is not destructive.
	dry, err := client.RevertRun(ctx, connect.NewRequest(&godwitv1.RevertRunRequest{
		Target: "app", AcknowledgeHazards: []string{"H003"}, DryRun: true,
	}))
	if err != nil || len(dry.Msg.DataLoss) != 0 {
		t.Fatalf("empty column = %+v, err = %v", dry, err)
	}
	execStore(t, targetDSN, "UPDATE t SET note = 'kept' WHERE id = 1")
	_, err = client.RevertRun(ctx, connect.NewRequest(&godwitv1.RevertRunRequest{
		Target: "app", AcknowledgeHazards: []string{"H003"},
	}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "column t.note holds 1 row(s)") {
		t.Fatalf("written column: %v", err)
	}
}

// TestRevertAfterDirectiveRun is the second bug PR #67 reported: a migration carrying a directive used
// to make every later run non-revertable, because the revert was expanded with the wrong run's expansions.
func TestRevertAfterDirectiveRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client, targetDSN, _ := directiveServiceStore(t)
	directive := changeTypeFiles("change-type public.users.age bigint", "-- godwit: revert\n")
	planWith(t, client, append(usersFiles(), directive...))
	directiveRun := startRun(t, client, append(usersFiles(), directive...))
	waitState(t, client, directiveRun, godwitv1.RunState_RUN_STATE_AWAITING_CONTRACT)
	if _, err := client.ConfirmRollout(ctx, connect.NewRequest(&godwitv1.ConfirmRolloutRequest{RunId: directiveRun})); err != nil {
		t.Fatal(err)
	}
	waitState(t, client, directiveRun, godwitv1.RunState_RUN_STATE_SUCCEEDED)

	// A later plain run whose directory still carries the directive migration.
	plain := []*godwitv1.MigrationFile{
		{Name: "20260901140000_b.up.sql", Body: "CREATE TABLE b (id int);"},
		{Name: "20260901140000_b.down.sql", Body: "DROP TABLE b;"},
	}
	later := append(append(usersFiles(), directive...), plain...)
	created, err := client.CreateRun(ctx, connect.NewRequest(&godwitv1.CreateRunRequest{Target: "app", Files: later}))
	if err != nil {
		t.Fatal(err)
	}
	plainRun := created.Msg.RunId
	waitState(t, client, plainRun, godwitv1.RunState_RUN_STATE_SUCCEEDED)

	plan := revertAndWait(t, client, &godwitv1.RevertRunRequest{RunId: plainRun, AcknowledgeHazards: []string{"H002"}})
	if len(plan.Migrations) != 1 || plan.Migrations[0].Name != "b" {
		t.Fatalf("a directive in an earlier run must not reach this plan: %+v", plan.Migrations)
	}
	if relationExists(t, targetDSN, "b") {
		t.Fatal("the plain run's table must be gone")
	}
	if columnType(t, targetDSN, "age") != "bigint" {
		t.Fatal("the directive run must be untouched")
	}

	// And the directive run itself still reverts, from the inverse frozen on its own ledger row.
	revertAndWait(t, client, &godwitv1.RevertRunRequest{RunId: directiveRun, AllowDataLoss: true})
	if columnType(t, targetDSN, "age") != "integer" {
		t.Fatal("reverting the directive run must put age back")
	}
	got, err := client.GetRun(ctx, connect.NewRequest(&godwitv1.GetRunRequest{RunId: directiveRun}))
	if err != nil || got.Msg.Run.State != godwitv1.RunState_RUN_STATE_REVERTED {
		t.Fatalf("directive run = %+v, err = %v", got.Msg.Run, err)
	}
}

// TestRevertTargetUnreachable covers the gate failing on the target rather than on the plan.
func TestRevertTargetUnreachable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storeDSN := newDatabase(t, "st")
	client := newClient(startService(t, storeDSN, "r1", nil), "")
	registerTarget(t, client, newDatabase(t, "tg"))
	runToSuccess(t, client, tableA(), nil)
	list, err := client.ListRuns(ctx, connect.NewRequest(&godwitv1.ListRunsRequest{Target: "app"}))
	if err != nil {
		t.Fatal(err)
	}
	firstRun := list.Msg.Runs[0].Id

	// A down side that drops nothing skips the probe and stops at the search-path observation instead.
	runToSuccess(t, client, []*godwitv1.MigrationFile{
		{Name: "20260101000002_c.up.sql", Body: "CREATE TABLE c (id int);"},
		{Name: "20260101000002_c.down.sql", Body: "TRUNCATE c;"},
	}, nil)
	execStore(t, storeDSN, "UPDATE cp_targets SET provider = 'nope'")
	if _, err := client.RevertRun(ctx, connect.NewRequest(&godwitv1.RevertRunRequest{Target: "app"})); connect.CodeOf(err) != connect.CodeInternal ||
		!strings.Contains(err.Error(), "credential provider") {
		t.Fatalf("search path on a broken target: %v", err)
	}
	execStore(t, storeDSN, "UPDATE cp_run_applied SET reverted_by = NULL")
	execStore(t, storeDSN, "UPDATE cp_runs SET state = 'succeeded' WHERE state = 'reverted'")
	if _, err := client.RevertRun(ctx, connect.NewRequest(&godwitv1.RevertRunRequest{
		RunId: firstRun, AcknowledgeHazards: []string{"H002"}, Force: true,
	})); connect.CodeOf(err) != connect.CodeInternal || !strings.Contains(err.Error(), "credential provider") {
		t.Fatalf("data-loss probe on a broken target: %v", err)
	}
}

// TestRevertRefusals covers the shapes the API turns away before it plans anything.
func TestRevertRefusals(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client := newClient(startService(t, newDatabase(t, "st"), "r1", nil), "")
	targetDSN := newDatabase(t, "tg")
	registerTarget(t, client, targetDSN)
	runToSuccess(t, client, tableA(), nil)

	if _, err := client.RevertRun(ctx, connect.NewRequest(&godwitv1.RevertRunRequest{})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("no run and no target: %v", err)
	}
	if _, err := client.RevertRun(ctx, connect.NewRequest(&godwitv1.RevertRunRequest{Target: "ghost"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("unknown target: %v", err)
	}
	if _, err := client.RevertRun(ctx, connect.NewRequest(&godwitv1.RevertRunRequest{
		LockTimeout: "nope", Target: "app",
	})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("bad timeout: %v", err)
	}
	list, err := client.ListRuns(ctx, connect.NewRequest(&godwitv1.ListRunsRequest{Target: "app"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.RevertRun(ctx, connect.NewRequest(&godwitv1.RevertRunRequest{
		RunId: list.Msg.Runs[0].Id, Target: "other",
	})); connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "is on target app") {
		t.Fatalf("target mismatch: %v", err)
	}

	if _, err := client.RegisterTarget(ctx, connect.NewRequest(&godwitv1.RegisterTargetRequest{
		Name: "legacy", Provider: "static", Dsn: newDatabase(t, "lg"),
	})); err != nil {
		t.Fatal(err)
	}
	based, err := client.BaselineTarget(ctx, connect.NewRequest(&godwitv1.BaselineTargetRequest{
		Target: "legacy", Files: baselineFiles(), Version: 1,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.RevertRun(ctx, connect.NewRequest(&godwitv1.RevertRunRequest{RunId: based.Msg.RunId})); !strings.Contains(err.Error(), controlplane.ErrBaselineRun.Error()) {
		t.Fatalf("baseline run: %v", err)
	}
}
