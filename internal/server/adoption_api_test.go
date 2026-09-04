package server

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/internal/controlplane"
	"github.com/SamuelMolling/godwit/internal/engine"
)

func adoptedFiles() []*godwitv1.MigrationFile {
	return []*godwitv1.MigrationFile{
		{Name: "20260101000000_orders.up.sql", Body: "CREATE TABLE orders (id int);"},
		{Name: "20260101000000_orders.down.sql", Body: "DROP TABLE orders;"},
		{Name: "20260101000001_total.up.sql", Body: "ALTER TABLE orders ADD COLUMN total numeric;"},
		{Name: "20260101000001_total.down.sql", Body: "ALTER TABLE orders DROP COLUMN total;"},
	}
}

// journalledElsewhere builds the database the way another godwit instance left it: the schema and a
// full godwit journal, with no run in this control plane's store.
func journalledElsewhere(t *testing.T, dsn string, files []*godwitv1.MigrationFile) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	byName := map[string]string{}
	for _, f := range files {
		byName[f.Name] = f.Body
	}
	plans, err := controlplane.PlansFromFiles(byName, engine.DirectionUp)
	if err != nil {
		t.Fatal(err)
	}
	exec := engine.New(conn, engine.Options{})
	for _, p := range plans {
		if _, err := exec.Up(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
}

// The whole adoption story: a database with history godwit never ran, taken over, then migrated.
func TestAdoptADatabaseThatAlreadyHasHistory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client := newClient(startService(t, newDatabase(t, "st"), "r1", nil), "")
	targetDSN := newDatabase(t, "tg")
	registerTarget(t, client, targetDSN)
	files := adoptedFiles()
	journalledElsewhere(t, targetDSN, files)

	next := append(files, //nolint:gocritic // the run submits the whole directory, adopted files included
		&godwitv1.MigrationFile{Name: "20260101000002_note.up.sql", Body: "ALTER TABLE orders ADD COLUMN note text;"},
		&godwitv1.MigrationFile{Name: "20260101000002_note.down.sql", Body: "ALTER TABLE orders DROP COLUMN note;"})

	_, err := client.PlanRun(ctx, connect.NewRequest(&godwitv1.PlanRunRequest{Target: "app", Files: next, Persist: true}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition ||
		!strings.Contains(err.Error(), "app records 20260101000000_orders, 20260101000001_total") ||
		!strings.Contains(err.Error(), "godwit target reconcile app") {
		t.Fatalf("plan before adoption: %v", err)
	}

	rec, err := client.ReconcileTarget(ctx, connect.NewRequest(&godwitv1.ReconcileTargetRequest{Target: "app", Files: files}))
	if err != nil || len(rec.Msg.Adopted) != 2 || rec.Msg.RunId == "" {
		t.Fatalf("reconcile = %+v, err = %v", rec, err)
	}
	run, err := client.GetRun(ctx, connect.NewRequest(&godwitv1.GetRunRequest{RunId: rec.Msg.RunId}))
	if err != nil || run.Msg.Run.Kind != controlplane.KindReconcile || len(run.Msg.Applied) != 2 || !run.Msg.Applied[0].Adopted {
		t.Fatalf("reconcile run = %+v, err = %v", run, err)
	}
	if _, err := client.RevertRun(ctx, connect.NewRequest(&godwitv1.RevertRunRequest{RunId: rec.Msg.RunId})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("revert of a reconcile: %v", err)
	}

	again, err := client.ReconcileTarget(ctx, connect.NewRequest(&godwitv1.ReconcileTargetRequest{Target: "app", Files: files}))
	if err != nil || len(again.Msg.Adopted) != 0 || again.Msg.RunId != "" {
		t.Fatalf("a reconciled target adopts nothing: %+v, err = %v", again, err)
	}

	plan, err := client.PlanRun(ctx, connect.NewRequest(&godwitv1.PlanRunRequest{Target: "app", Files: next, Persist: true}))
	if err != nil {
		t.Fatalf("plan after adoption: %v", err)
	}
	if plan.Msg.Drift != "" {
		t.Fatalf("an adopted target is not drifted: %q", plan.Msg.Drift)
	}
	pending := 0
	for _, m := range plan.Msg.Migrations {
		if !m.Applied {
			pending++
		}
	}
	if pending != 1 {
		t.Fatalf("only the new migration is pending: %+v", plan.Msg.Migrations)
	}

	runToSuccess(t, client, next, nil)
	targets, err := client.ListTargets(ctx, connect.NewRequest(&godwitv1.ListTargetsRequest{}))
	if err != nil || len(targets.Msg.Targets) != 1 || targets.Msg.Targets[0].AppliedCount != 3 {
		t.Fatalf("targets = %+v, err = %v", targets, err)
	}
	status, err := client.GetTargetStatus(ctx, connect.NewRequest(&godwitv1.GetTargetStatusRequest{Target: "app", Files: next}))
	if err != nil || len(status.Msg.Applied) != 3 {
		t.Fatalf("the target's own journal and the ledger now agree: %+v, err = %v", status, err)
	}
}

// A reconcile the control plane cannot decide on its own refuses, and says which migrations it means.
func TestReconcileTargetRefusals(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client := newClient(startService(t, newDatabase(t, "st"), "r1", nil), "")
	targetDSN := newDatabase(t, "tg")
	registerTarget(t, client, targetDSN)
	files := adoptedFiles()
	journalledElsewhere(t, targetDSN, files)

	if _, err := client.ReconcileTarget(ctx, connect.NewRequest(&godwitv1.ReconcileTargetRequest{})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("no target: %v", err)
	}
	if _, err := client.ReconcileTarget(ctx, connect.NewRequest(&godwitv1.ReconcileTargetRequest{
		Target: "app", Files: files[:1],
	})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("half a migration: %v", err)
	}
	if _, err := client.ReconcileTarget(ctx, connect.NewRequest(&godwitv1.ReconcileTargetRequest{
		Target: "ghost", Files: files,
	})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("unknown target: %v", err)
	}
	if _, err := client.ReconcileTarget(ctx, connect.NewRequest(&godwitv1.ReconcileTargetRequest{
		Target: "app", Files: files[:2],
	})); connect.CodeOf(err) != connect.CodeFailedPrecondition ||
		!strings.Contains(err.Error(), "absent from the directory: 20260101000001_total") {
		t.Fatalf("directory short of the target: %v", err)
	}

	edited := adoptedFiles()
	edited[0].Body = "CREATE TABLE orders (id bigint);"
	if _, err := client.ReconcileTarget(ctx, connect.NewRequest(&godwitv1.ReconcileTargetRequest{
		Target: "app", Files: edited,
	})); connect.CodeOf(err) != connect.CodeFailedPrecondition ||
		!strings.Contains(err.Error(), "different content than the directory carries: 20260101000000_orders") {
		t.Fatalf("edited body: %v", err)
	}
}

// A database with the schema but no godwit journal is adopted by baseline, and the migration above the
// baseline still runs.
func TestBaselineAdoptsAnUnjournalledDatabase(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client := newClient(startService(t, newDatabase(t, "st"), "r1", nil), "")
	targetDSN := newDatabase(t, "tg")
	registerTarget(t, client, targetDSN)
	execStore(t, targetDSN, "CREATE TABLE orders (id int); ALTER TABLE orders ADD COLUMN total numeric")

	files := adoptedFiles()
	if _, err := client.BaselineTarget(ctx, connect.NewRequest(&godwitv1.BaselineTargetRequest{
		Target: "app", Files: files, Version: 20260101000001,
	})); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	next := append(files, //nolint:gocritic // the run submits the whole directory, baselined files included
		&godwitv1.MigrationFile{Name: "20260101000002_note.up.sql", Body: "ALTER TABLE orders ADD COLUMN note text;"},
		&godwitv1.MigrationFile{Name: "20260101000002_note.down.sql", Body: "ALTER TABLE orders DROP COLUMN note;"})
	runToSuccess(t, client, next, nil)

	status, err := client.GetTargetStatus(ctx, connect.NewRequest(&godwitv1.GetTargetStatusRequest{Target: "app", Files: next}))
	if err != nil || len(status.Msg.Applied) != 3 {
		t.Fatalf("status = %+v, err = %v", status, err)
	}
}
