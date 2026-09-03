package server

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/gen/godwit/v1/godwitv1connect"
)

func smallUsersFiles() []*godwitv1.MigrationFile {
	return []*godwitv1.MigrationFile{
		{Name: "20260901120000_users.up.sql", Body: "CREATE TABLE public.users (id bigserial PRIMARY KEY, age integer NOT NULL);\n" +
			"INSERT INTO public.users (age) SELECT g FROM generate_series(1, 100) g;"},
		{Name: "20260901120000_users.down.sql", Body: "DROP TABLE public.users;"},
	}
}

func notesFiles() []*godwitv1.MigrationFile {
	return []*godwitv1.MigrationFile{
		{Name: "20260901140000_notes.up.sql", Body: "CREATE TABLE public.notes (id bigint PRIMARY KEY);"},
		{Name: "20260901140000_notes.down.sql", Body: "DROP TABLE public.notes;"},
	}
}

func plannedMigration(t *testing.T, msg *godwitv1.PlanRunResponse, name string) *godwitv1.PlannedMigration {
	t.Helper()
	for _, m := range msg.Migrations {
		if m.Name == name {
			return m
		}
	}
	t.Fatalf("plan carries no migration %q: %+v", name, msg.Migrations)

	return nil
}

func planFiles(t *testing.T, client godwitv1connect.GodwitServiceClient, files []*godwitv1.MigrationFile, skip bool) *godwitv1.PlanRunResponse {
	t.Helper()
	res, err := client.PlanRun(context.Background(), connect.NewRequest(&godwitv1.PlanRunRequest{
		Target: "app", Files: files, SkipValidation: skip,
	}))
	if err != nil {
		t.Fatalf("plan after an applied change-type (skip_validation=%t): %v", skip, err)
	}

	return res.Msg
}

// The whole reported failure: once a change-type has run, every later plan, verify and migrate was refused.
func TestPlanAndMigrateAfterAppliedChangeType(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client := newClient(startService(t, newDatabase(t, "st"), "r1", nil), "")
	targetDSN := newDatabase(t, "tg")
	registerTarget(t, client, targetDSN)
	runToSuccess(t, client, smallUsersFiles(), nil)

	files := append(smallUsersFiles(), changeTypeFiles("change-type public.users.age bigint batch=50", "-- godwit: revert\n")...)
	runID := startRun(t, client, files)
	waitState(t, client, runID, godwitv1.RunState_RUN_STATE_AWAITING_CONTRACT)
	if _, err := client.ConfirmRollout(ctx, connect.NewRequest(&godwitv1.ConfirmRolloutRequest{RunId: runID})); err != nil {
		t.Fatal(err)
	}
	waitState(t, client, runID, godwitv1.RunState_RUN_STATE_SUCCEEDED)

	age := plannedMigration(t, planFiles(t, client, files, false), "age")
	if !age.Applied || age.Expanded || len(age.Statements) != 0 {
		t.Fatalf("an applied change-type must plan as history, not as an expansion: %+v", age)
	}
	if plannedMigration(t, planFiles(t, client, files, true), "age").Applied != true {
		t.Fatal("--skip-validation must not refuse a directive the target already applied")
	}

	next := append(files, notesFiles()...)
	runToSuccess(t, client, next, nil)
	if columnType(t, targetDSN, "age") != "bigint" || columnType(t, targetDSN, "age_old") != "integer" {
		t.Fatal("a later migrate must leave the swapped columns alone")
	}
	var notes int
	if err := targetConn(t, targetDSN).QueryRow(ctx,
		`SELECT count(*) FROM pg_class WHERE oid = 'public.notes'::regclass`).Scan(&notes); err != nil {
		t.Fatal(err)
	}
	if notes != 1 {
		t.Fatal("the migration after the change-type must have run")
	}
}

// A baseline records a directive migration with no expansion, so the replay records it the same way.
func TestBaselinedDirectiveStillPlans(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client := newClient(startService(t, newDatabase(t, "st"), "r1", nil), "")
	targetDSN := newDatabase(t, "tg")
	registerTarget(t, client, targetDSN)
	execTarget(t, targetConn(t, targetDSN), "CREATE TABLE public.users (id bigserial PRIMARY KEY, age bigint NOT NULL)")

	files := append(smallUsersFiles(), changeTypeFiles("change-type public.users.age bigint", "-- godwit: revert\n")...)
	if _, err := client.BaselineTarget(ctx, connect.NewRequest(&godwitv1.BaselineTargetRequest{
		Target: "app", Version: 20260901130000, Files: files,
	})); err != nil {
		t.Fatal(err)
	}
	msg := planFiles(t, client, append(files, notesFiles()...), false)
	if !plannedMigration(t, msg, "age").Applied {
		t.Fatalf("the baselined directive must plan as history: %+v", msg.Migrations)
	}
	if plannedMigration(t, msg, "notes").Applied {
		t.Fatal("the migration after the baseline is still pending")
	}
}
