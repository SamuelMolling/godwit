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
	"github.com/SamuelMolling/godwit/internal/engine"
)

func validatedTarget(t *testing.T) (godwitv1connect.GodwitServiceClient, *pgx.Conn) {
	t.Helper()
	ctx := context.Background()
	client := newClient(startService(t, newDatabase(t, "st"), "r1", []string{"ops:admin:a"}), "a")
	targetDSN := newDatabase(t, "tg")
	registerTarget(t, client, targetDSN)
	runToSuccess(t, client, orderedFiles()[:2], nil)
	conn, err := pgx.Connect(ctx, targetDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })

	return client, conn
}

func validatedPlan(t *testing.T, client godwitv1connect.GodwitServiceClient, files []*godwitv1.MigrationFile) *godwitv1.PlanRunResponse {
	t.Helper()
	res, err := client.PlanRun(context.Background(), connect.NewRequest(&godwitv1.PlanRunRequest{Target: "app", Files: files, Persist: true}))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Msg.Validated || res.Msg.PlanId == "" {
		t.Fatalf("plan = %+v", res.Msg)
	}

	return res.Msg
}

func execTarget(t *testing.T, conn *pgx.Conn, sql string) {
	t.Helper()
	if _, err := conn.Exec(context.Background(), sql); err != nil {
		t.Fatal(err)
	}
}

func waitRunState(t *testing.T, client godwitv1connect.GodwitServiceClient, id string, states ...godwitv1.RunState) *godwitv1.Run {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		r, err := client.GetRun(context.Background(), connect.NewRequest(&godwitv1.GetRunRequest{RunId: id}))
		if err != nil {
			t.Fatal(err)
		}
		for _, st := range states {
			if r.Msg.Run.State == st {
				return r.Msg.Run
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("run %s never reached %v", id, states)

	return nil
}

func TestPlanRun_PrefixAlreadyApplied(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client, conn := validatedTarget(t)
	execTarget(t, conn, "ALTER TABLE t ADD COLUMN a int")

	plan := validatedPlan(t, client, orderedFiles())
	m := plan.Migrations
	if len(m) != 3 || !m[0].Applied || m[0].AlreadyApplied || !m[1].AlreadyApplied || m[2].AlreadyApplied || plan.Drift != "" {
		t.Fatalf("plan = %+v", plan)
	}
	if !strings.HasPrefix(m[1].Effect, "+ column") || !strings.Contains(m[1].Effect, ".t.a integer") || m[1].Note != "" || m[2].Note != "" {
		t.Fatalf("migration 2 = %+v, 3 = %+v", m[1], m[2])
	}

	created, err := client.CreateRun(ctx, connect.NewRequest(&godwitv1.CreateRunRequest{Target: "app", Files: orderedFiles()}))
	if err != nil {
		t.Fatal(err)
	}
	if created.Msg.PlanId != plan.PlanId {
		t.Fatalf("bound to %q, want %q", created.Msg.PlanId, plan.PlanId)
	}
	waitRun(t, client, created.Msg.RunId)

	applied, err := engine.ListApplied(ctx, conn)
	if err != nil || len(applied) != 3 {
		t.Fatalf("applied = %+v, err = %v", applied, err)
	}
	var stmts int
	if err := conn.QueryRow(ctx, `SELECT stmt_count FROM godwit.runs WHERE version = 20260901120002 AND state = 'succeeded'`).Scan(&stmts); err != nil || stmts != 0 {
		t.Fatalf("stmt_count = %d, err = %v", stmts, err)
	}
	if err := conn.QueryRow(ctx, `SELECT stmt_count FROM godwit.runs WHERE version = 20260901120003 AND state = 'succeeded'`).Scan(&stmts); err != nil || stmts != 1 {
		t.Fatalf("stmt_count = %d, err = %v", stmts, err)
	}
	if actions := auditActions(t, client); !strings.Contains(strings.Join(actions, " "), "plan.create") {
		t.Fatalf("audit = %v", actions)
	}
}

func TestPlanRun_NonPrefixIsDrift(t *testing.T) {
	t.Parallel()
	client, conn := validatedTarget(t)
	execTarget(t, conn, "ALTER TABLE t ADD COLUMN b int")

	plan := validatedPlan(t, client, orderedFiles())
	m := plan.Migrations
	if m[1].AlreadyApplied || m[2].AlreadyApplied || !strings.Contains(plan.Drift, "+ column") || !strings.Contains(plan.Drift, ".t.b integer") {
		t.Fatalf("plan = %+v", plan)
	}
	if m[1].Note != "" || m[2].Note != "effect is present but not as a prefix" {
		t.Fatalf("notes = %q %q", m[1].Note, m[2].Note)
	}
}

func TestPlanRun_DMLNeverMarked(t *testing.T) {
	t.Parallel()
	client, conn := validatedTarget(t)
	execTarget(t, conn, "ALTER TABLE t ADD COLUMN a int")
	files := orderedFiles()[:4]
	files[2] = &godwitv1.MigrationFile{Name: files[2].Name, Body: "ALTER TABLE t ADD COLUMN a int; INSERT INTO t (id, a) VALUES (1, 1);"}

	plan := validatedPlan(t, client, files)
	if m := plan.Migrations[1]; m.AlreadyApplied || m.Note != engine.OpaqueDML || plan.Drift != "" {
		t.Fatalf("plan = %+v", plan)
	}
}

func TestPlanRun_EmptyEffectNeverMarked(t *testing.T) {
	t.Parallel()
	client, _ := validatedTarget(t)
	files := append(orderedFiles()[:2],
		&godwitv1.MigrationFile{Name: "20260901120002_fn.up.sql", Body: "CREATE FUNCTION f() RETURNS int AS 'SELECT 1' LANGUAGE sql;"},
		&godwitv1.MigrationFile{Name: "20260901120002_fn.down.sql", Body: "DROP FUNCTION f;"},
	)

	plan := validatedPlan(t, client, files)
	if m := plan.Migrations[1]; m.AlreadyApplied || m.Note != engine.OpaqueUnknown || plan.Drift != "" {
		t.Fatalf("plan = %+v", plan)
	}
}

func TestScheduler_MarkOnlyRefusesInvalidIndex(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client, conn := validatedTarget(t)
	execTarget(t, conn, "INSERT INTO t VALUES (1), (1)")
	if _, err := conn.Exec(ctx, "CREATE UNIQUE INDEX CONCURRENTLY t_id_idx ON t (id)"); err == nil {
		t.Fatal("unique index over duplicates must fail")
	}
	files := append(orderedFiles()[:2],
		&godwitv1.MigrationFile{Name: "20260901120002_idx.up.sql", Body: "CREATE UNIQUE INDEX CONCURRENTLY t_id_idx ON t (id);"},
		&godwitv1.MigrationFile{Name: "20260901120002_idx.down.sql", Body: "DROP INDEX CONCURRENTLY t_id_idx;"},
	)

	plan := validatedPlan(t, client, files)
	if !plan.Migrations[1].AlreadyApplied {
		t.Fatalf("plan = %+v", plan)
	}
	created, err := client.CreateRun(ctx, connect.NewRequest(&godwitv1.CreateRunRequest{Target: "app", Files: files}))
	if err != nil {
		t.Fatal(err)
	}
	run := waitRunState(t, client, created.Msg.RunId, godwitv1.RunState_RUN_STATE_FAILED, godwitv1.RunState_RUN_STATE_NEEDS_ATTENTION)
	if !strings.Contains(run.Error, "t_id_idx exists but is INVALID") {
		t.Fatalf("error = %q", run.Error)
	}
	applied, err := engine.ListApplied(ctx, conn)
	if err != nil || len(applied) != 1 {
		t.Fatalf("applied = %+v, err = %v", applied, err)
	}
}

func TestPlanRun_SkipValidationFallsBackToBaselineDrift(t *testing.T) {
	t.Parallel()
	client, conn := validatedTarget(t)
	execTarget(t, conn, "ALTER TABLE t ADD COLUMN a int")

	res, err := client.PlanRun(context.Background(), connect.NewRequest(&godwitv1.PlanRunRequest{
		Target: "app", Files: orderedFiles(), Persist: true, SkipValidation: true,
	}))
	if err != nil {
		t.Fatal(err)
	}
	m := res.Msg.Migrations
	if res.Msg.Validated || m[1].AlreadyApplied || m[1].Note != "" || !strings.Contains(res.Msg.Drift, ".t.a integer") {
		t.Fatalf("plan = %+v", res.Msg)
	}
}

func TestPlanRun_AlreadyAppliedAcrossUsers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client := newClient(startService(t, newDatabase(t, "st"), "r1", []string{"ops:admin:a"}), "a")
	targetDSN := newDatabase(t, "tg")
	admin, err := pgx.Connect(ctx, targetDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admin.Close(ctx) }()
	role := "app_" + admin.Config().Database
	for _, sql := range []string{
		"CREATE ROLE " + role + " LOGIN PASSWORD 'app'",
		"GRANT CREATE ON DATABASE " + admin.Config().Database + " TO " + role,
		"GRANT ALL ON SCHEMA public TO " + role,
	} {
		execTarget(t, admin, sql)
	}
	registerTarget(t, client, strings.Replace(targetDSN, "godwit:godwit@", role+":app@", 1))
	runToSuccess(t, client, orderedFiles()[:2], nil)
	execTarget(t, admin, "ALTER TABLE public.t ADD COLUMN a int")

	plan := validatedPlan(t, client, orderedFiles())
	if m := plan.Migrations[1]; !m.AlreadyApplied || !strings.HasPrefix(m.Effect, "+ column public.t.a integer") || plan.Drift != "" {
		t.Fatalf("plan = %+v", plan)
	}
}
