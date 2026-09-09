package server

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
)

const golangMigrateJournal = `
	CREATE TABLE public.schema_migrations (version bigint NOT NULL PRIMARY KEY, dirty boolean NOT NULL);
	INSERT INTO public.schema_migrations VALUES (20260902165420, false)`

func TestPlanOfAnAdoptedDatabase(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client := newClient(startService(t, newDatabase(t, "st"), "r1", nil), "")
	targetDSN := newDatabase(t, "tg")
	registerTarget(t, client, targetDSN)
	execOnTarget(t, targetDSN, golangMigrateJournal)

	runToSuccess(t, client, hazardFiles(1), []string{"H001"})

	res, err := client.PlanRun(ctx, connect.NewRequest(&godwitv1.PlanRunRequest{
		Target: "app", Files: hazardFiles(1), Persist: true,
	}))
	if err != nil {
		t.Fatal(err)
	}
	plan := res.Msg
	if plan.Drift != "" {
		t.Fatalf("the previous tool's bookkeeping is not drift:\n%s", plan.Drift)
	}
	if got := plan.Observed.IgnoredTables; len(got) != 1 || got[0] != "public.schema_migrations (golang-migrate)" {
		t.Fatalf("ignored tables = %v", got)
	}
	if len(plan.Migrations) != 1 || !plan.Migrations[0].Applied || !plan.Migrations[0].Skipped {
		t.Fatalf("migrations = %+v", plan.Migrations)
	}
	if len(plan.Migrations[0].Statements[1].Hazards) != 1 {
		t.Fatalf("the hazard stays on the statement it is true of: %+v", plan.Migrations[0].Statements)
	}
}

func TestAdoptedTablesAreKeptWhenTheTargetSaysSo(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client := newClient(startService(t, newDatabase(t, "st"), "r1", nil), "")
	targetDSN := newDatabase(t, "tg")
	keep := false
	if _, err := client.RegisterTarget(ctx, connect.NewRequest(&godwitv1.RegisterTargetRequest{
		Name: "app", Provider: "static", Dsn: targetDSN, IgnoreAdoptedTables: &keep,
	})); err != nil {
		t.Fatal(err)
	}
	execOnTarget(t, targetDSN, golangMigrateJournal)

	runToSuccess(t, client, hazardFiles(1), []string{"H001"})

	res, err := client.PlanRun(ctx, connect.NewRequest(&godwitv1.PlanRunRequest{
		Target: "app", Files: hazardFiles(1), Persist: true,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if res.Msg.Drift == "" || len(res.Msg.Observed.IgnoredTables) != 0 {
		t.Fatalf("drift = %q, ignored = %v", res.Msg.Drift, res.Msg.Observed.IgnoredTables)
	}
}

func TestPlanRunIgnoresABaselineFromAnOlderSchemaFormat(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storeDSN := newDatabase(t, "st")
	client := newClient(startService(t, storeDSN, "r1", []string{"ops:admin:a"}), "a")
	targetDSN := newDatabase(t, "tg")
	registerTarget(t, client, targetDSN)
	runToSuccess(t, client, orderedFiles()[:2], nil)
	execOnTarget(t, targetDSN, "ALTER TABLE t ADD COLUMN a int")

	store, err := pgx.Connect(ctx, storeDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	if _, err := store.Exec(ctx, `UPDATE cp_snapshots SET definition = 'column public.t.id integer'`); err != nil {
		t.Fatal(err)
	}

	res, err := client.PlanRun(ctx, connect.NewRequest(&godwitv1.PlanRunRequest{
		Target: "app", Files: orderedFiles(), Persist: true, SkipValidation: true,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if res.Msg.Drift != "" {
		t.Fatalf("a baseline godwit cannot read is not drift:\n%s", res.Msg.Drift)
	}

	if _, err := client.CheckDrift(ctx, connect.NewRequest(&godwitv1.CheckDriftRequest{Target: "app"})); err == nil ||
		!strings.Contains(err.Error(), "godwit drift accept app") {
		t.Fatalf("the monitor must say what to do: %v", err)
	}
}
