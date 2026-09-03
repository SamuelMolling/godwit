package controlplane

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"

	"github.com/SamuelMolling/godwit/internal/engine"
)

const (
	planA = "aaaaaaaa-0000-0000-0000-000000000001"
	planB = "aaaaaaaa-0000-0000-0000-000000000002"
	planC = "aaaaaaaa-0000-0000-0000-000000000003"
)

func storedPlan(id string) Plan {
	return Plan{
		ID: id, Target: "app", Key: "k1", Rollout: RolloutDirect, HistoryHash: "h1",
		Applied:           []engine.Applied{{Version: 1, Name: "a", Checksum: "c1", AppliedAt: time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)}},
		SchemaFingerprint: "f1", SchemaDefinition: "table a\n", Drift: "+ x",
		Migrations: []PlanMigration{
			{Version: 1, Name: "a", Checksum: "c1", Applied: true, Phase: PhaseExpand, Statements: []PlanStatement{{SQL: "SELECT 1"}}},
			{Version: 2, Name: "b", Checksum: "c2", Phase: PhaseExpand, Statements: []PlanStatement{
				{SQL: "DROP TABLE t", NoTx: true, Hazards: []PlanHazard{{Code: "H002", Detail: "drop"}}},
			}},
		},
		Validated: true, Acked: []string{"H002"}, AllowOutOfOrder: true, CreatedBy: "ci", Source: "repo@sha",
	}
}

func TestPlanStoreLifecycle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newStore(t)
	if err := s.RegisterTarget(ctx, "app", "static", map[string]string{}); err != nil {
		t.Fatal(err)
	}

	if _, err := s.ReadyPlan(ctx, "app", "k1", time.Time{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing plan err = %v", err)
	}
	if _, err := s.Plan(ctx, planA); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing plan err = %v", err)
	}
	if err := s.SavePlan(ctx, storedPlan(planA), goodFiles()); err != nil {
		t.Fatal(err)
	}
	got, err := s.ReadyPlan(ctx, "app", "k1", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	want := storedPlan(planA)
	if got.ID != planA || got.State != PlanReady || got.HistoryHash != "h1" || len(got.Applied) != 1 || got.Applied[0].Checksum != "c1" ||
		!got.Applied[0].AppliedAt.Equal(want.Applied[0].AppliedAt) || got.SchemaFingerprint != "f1" || got.SchemaDefinition != "table a\n" ||
		got.Drift != "+ x" || len(got.Migrations) != 2 || !got.Migrations[0].Applied || got.Migrations[1].Statements[0].Hazards[0].Code != "H002" ||
		!got.Migrations[1].Statements[0].NoTx || !got.Validated || got.Acked[0] != "H002" || !got.AllowOutOfOrder || got.CreatedBy != "ci" ||
		got.Source != "repo@sha" || got.CreatedAt.IsZero() || got.RunID != "" || got.SupersededBy != "" {
		t.Fatalf("plan = %+v", got)
	}
	if _, err := s.ReadyPlan(ctx, "app", "k1", time.Now().Add(time.Hour)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired plan err = %v", err)
	}

	if err := s.SavePlan(ctx, storedPlan(planB), goodFiles()); err != nil {
		t.Fatal(err)
	}
	if got, err = s.ReadyPlan(ctx, "app", "k1", time.Time{}); err != nil || got.ID != planB {
		t.Fatalf("refreshed plan = %+v, err = %v", got, err)
	}
	if _, err := s.Plan(ctx, planA); !errors.Is(err, ErrNotFound) {
		t.Fatalf("replaced plan err = %v", err)
	}
	var files int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM cp_plan_files`).Scan(&files); err != nil || files != 2 {
		t.Fatalf("files = %d, err = %v", files, err)
	}

	const runID = "bbbbbbbb-0000-0000-0000-000000000001"
	if err := s.CreateRun(ctx, runID, "app", RolloutDirect, goodFiles(), Timeouts{}, Provenance{}, planB); err != nil {
		t.Fatal(err)
	}
	if err := s.BindPlan(ctx, planB, runID); err != nil {
		t.Fatal(err)
	}
	if err := s.BindPlan(ctx, planB, runID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second bind err = %v", err)
	}
	if got, err = s.Plan(ctx, planB); err != nil || got.State != PlanBound || got.RunID != runID {
		t.Fatalf("bound plan = %+v, err = %v", got, err)
	}
	if r, err := s.Run(ctx, runID); err != nil || r.PlanID != planB {
		t.Fatalf("run = %+v, err = %v", r, err)
	}
	if _, err := s.ReadyPlan(ctx, "app", "k1", time.Time{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bound plan must not be ready: %v", err)
	}

	if err := s.SavePlan(ctx, storedPlan(planA), goodFiles()); err != nil {
		t.Fatal(err)
	}
	next := storedPlan(planC)
	if err := s.SupersedePlan(ctx, planA, next, goodFiles()); err != nil {
		t.Fatal(err)
	}
	if err := s.SupersedePlan(ctx, planA, next, goodFiles()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second supersede err = %v", err)
	}
	old, err := s.Plan(ctx, planA)
	if err != nil || old.State != PlanSuperseded || old.SupersededBy != planC {
		t.Fatalf("old plan = %+v, err = %v", old, err)
	}
	if got, err = s.ReadyPlan(ctx, "app", "k1", time.Time{}); err != nil || got.ID != planC {
		t.Fatalf("next plan = %+v, err = %v", got, err)
	}

	plans, err := s.ListPlans(ctx, "app", 0)
	if err != nil || len(plans) != 3 || plans[0].ID != planC || plans[2].ID != planB {
		t.Fatalf("plans = %+v, err = %v", plans, err)
	}
	if plans, err = s.ListPlans(ctx, "app", 1); err != nil || len(plans) != 1 {
		t.Fatalf("plans = %+v, err = %v", plans, err)
	}
	if plans, err = s.ListPlans(ctx, "other", 0); err != nil || len(plans) != 0 {
		t.Fatalf("plans = %+v, err = %v", plans, err)
	}

	if files, err := s.PlanFiles(ctx, planC); err != nil || len(files) != 2 || files["20260901120000_t.down.sql"] != "DROP TABLE t;" {
		t.Fatalf("files = %v, err = %v", files, err)
	}
	if n, err := s.ReadyPlanCount(ctx, "app", time.Time{}); err != nil || n != 1 {
		t.Fatalf("ready = %d, err = %v", n, err)
	}
	if n, err := s.ReadyPlanCount(ctx, "app", time.Now().Add(time.Hour)); err != nil || n != 0 {
		t.Fatalf("ready = %d, err = %v", n, err)
	}
	if n, err := s.SweepPlans(ctx, time.Now().Add(-time.Hour)); err != nil || n != 0 {
		t.Fatalf("swept = %d, err = %v", n, err)
	}
	if n, err := s.SweepPlans(ctx, time.Now().Add(time.Hour)); err != nil || n != 1 {
		t.Fatalf("swept = %d, err = %v", n, err)
	}
	if err := s.Finish(ctx, runID, StateSucceeded, ""); err != nil {
		t.Fatal(err)
	}
	if n, err := s.SweepPlans(ctx, time.Now().Add(time.Hour)); err != nil || n != 1 {
		t.Fatalf("swept = %d, err = %v", n, err)
	}
	if plans, err = s.ListPlans(ctx, "app", 0); err != nil || len(plans) != 1 || plans[0].ID != planC {
		t.Fatalf("plans = %+v, err = %v", plans, err)
	}
	if r, err := s.Run(ctx, runID); err != nil || r.PlanID != "" {
		t.Fatalf("run = %+v, err = %v", r, err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM cp_plan_files`).Scan(&files); err != nil || files != 2 {
		t.Fatalf("files = %d, err = %v", files, err)
	}
}

func TestRunsApplying(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newStore(t)
	sched, _ := newScheduler(t, s, Config{Holder: "h"})

	start := time.Now().Add(-time.Minute)
	if runs, err := s.RunsApplying(ctx, "app", start); err != nil || len(runs) != 0 {
		t.Fatalf("runs = %v, err = %v", runs, err)
	}
	const first, second, failed = "cccccccc-0000-0000-0000-000000000021", "cccccccc-0000-0000-0000-000000000022", "cccccccc-0000-0000-0000-000000000023"
	queueRun(t, s, first, goodFiles())
	sched.Tick(ctx)
	waitState(t, s, first, StateSucceeded)
	queueRun(t, s, second, map[string]string{
		"20260901120001_u.up.sql": "CREATE TABLE u (id int);", "20260901120001_u.down.sql": "DROP TABLE u;",
	})
	sched.Tick(ctx)
	waitState(t, s, second, StateSucceeded)
	queueRun(t, s, failed, map[string]string{
		"20260901120002_v.up.sql": "CREATE TABLE u (id int);", "20260901120002_v.down.sql": "SELECT 1;",
	})
	sched.Tick(ctx)
	waitState(t, s, failed, StateFailed)

	runs, err := s.RunsApplying(ctx, "app", start)
	if err != nil || len(runs) != 2 || runs["20260901120000_t"] != first || runs["20260901120001_u"] != second {
		t.Fatalf("runs = %v, err = %v", runs, err)
	}
	if runs, err = s.RunsApplying(ctx, "app", time.Now()); err != nil || len(runs) != 0 {
		t.Fatalf("future runs = %v, err = %v", runs, err)
	}
}

func TestPlanStoreErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mock, s := newMockStore(t)

	mock.ExpectExec("DELETE FROM cp_plan_files").WithArgs("app", "k1").WillReturnError(errBoom)
	if err := s.SavePlan(ctx, storedPlan(planA), nil); err == nil || !strings.Contains(err.Error(), "replace plan files") {
		t.Fatalf("err = %v", err)
	}
	mock.ExpectExec("DELETE FROM cp_plan_files").WithArgs("app", "k1").WillReturnResult(pgxmock.NewResult("DELETE", 0))
	mock.ExpectExec("WITH p AS").WithArgs(anyArgs(20)...).WillReturnError(errBoom)
	if err := s.SavePlan(ctx, storedPlan(planA), nil); err == nil || !strings.Contains(err.Error(), "save plan") {
		t.Fatalf("err = %v", err)
	}

	mock.ExpectQuery("SELECT id, target, key").WithArgs("app", "k1", time.Time{}).WillReturnError(errBoom)
	if _, err := s.ReadyPlan(ctx, "app", "k1", time.Time{}); err == nil || !strings.Contains(err.Error(), "load plan") {
		t.Fatalf("err = %v", err)
	}
	mock.ExpectQuery("SELECT id, target, key").WithArgs(planA).WillReturnError(errBoom)
	if _, err := s.Plan(ctx, planA); err == nil || !strings.Contains(err.Error(), "load plan") {
		t.Fatalf("err = %v", err)
	}

	mock.ExpectQuery("SELECT id, target, key").WithArgs("app", 100).WillReturnError(errBoom)
	if _, err := s.ListPlans(ctx, "app", 0); err == nil || !strings.Contains(err.Error(), "list plans") {
		t.Fatalf("err = %v", err)
	}
	mock.ExpectQuery("SELECT id, target, key").WithArgs("app", 100).WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("x"))
	if _, err := s.ListPlans(ctx, "app", 0); err == nil || !strings.Contains(err.Error(), "list plans") {
		t.Fatalf("scan err = %v", err)
	}
	mock.ExpectQuery("SELECT id, target, key").WithArgs("app", 100).WillReturnRows(planRows().RowError(0, errBoom))
	if _, err := s.ListPlans(ctx, "app", 0); err == nil || !strings.Contains(err.Error(), "list plans") {
		t.Fatalf("row err = %v", err)
	}

	mock.ExpectExec("UPDATE cp_plans SET state = 'bound'").WithArgs(planA, "r1").WillReturnError(errBoom)
	if err := s.BindPlan(ctx, planA, "r1"); err == nil || !strings.Contains(err.Error(), "bind plan") {
		t.Fatalf("err = %v", err)
	}

	mock.ExpectExec("UPDATE cp_plans SET state = 'superseded'").WithArgs(planA).WillReturnError(errBoom)
	if err := s.SupersedePlan(ctx, planA, storedPlan(planB), nil); err == nil || !strings.Contains(err.Error(), "supersede plan") {
		t.Fatalf("err = %v", err)
	}
	mock.ExpectExec("UPDATE cp_plans SET state = 'superseded'").WithArgs(planA).WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec("DELETE FROM cp_plan_files").WithArgs("app", "k1").WillReturnError(errBoom)
	if err := s.SupersedePlan(ctx, planA, storedPlan(planB), nil); err == nil || !strings.Contains(err.Error(), "replace plan files") {
		t.Fatalf("err = %v", err)
	}
	mock.ExpectExec("UPDATE cp_plans SET state = 'superseded'").WithArgs(planA).WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec("DELETE FROM cp_plan_files").WithArgs("app", "k1").WillReturnResult(pgxmock.NewResult("DELETE", 0))
	mock.ExpectExec("WITH p AS").WithArgs(anyArgs(20)...).WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec("UPDATE cp_plans SET superseded_by").WithArgs(planA, planB).WillReturnError(errBoom)
	if err := s.SupersedePlan(ctx, planA, storedPlan(planB), nil); err == nil || !strings.Contains(err.Error(), "link superseded plan") {
		t.Fatalf("err = %v", err)
	}

	mock.ExpectQuery("SELECT name, body FROM cp_plan_files").WithArgs(planA).WillReturnError(errBoom)
	if _, err := s.PlanFiles(ctx, planA); err == nil || !strings.Contains(err.Error(), "list plan files") {
		t.Fatalf("err = %v", err)
	}
	mock.ExpectQuery("SELECT name, body FROM cp_plan_files").WithArgs(planA).
		WillReturnRows(pgxmock.NewRows([]string{"name", "body"}).AddRow("a", "b").RowError(0, errBoom))
	if _, err := s.PlanFiles(ctx, planA); err == nil || !strings.Contains(err.Error(), "read plan files") {
		t.Fatalf("row err = %v", err)
	}
	mock.ExpectQuery("SELECT count\\(\\*\\) FROM cp_plans").WithArgs("app", time.Time{}).WillReturnError(errBoom)
	if _, err := s.ReadyPlanCount(ctx, "app", time.Time{}); err == nil || !strings.Contains(err.Error(), "count ready plans") {
		t.Fatalf("err = %v", err)
	}
	mock.ExpectExec("DELETE FROM cp_plans").WithArgs(time.Time{}).WillReturnError(errBoom)
	if _, err := s.SweepPlans(ctx, time.Time{}); err == nil || !strings.Contains(err.Error(), "sweep plans") {
		t.Fatalf("err = %v", err)
	}

	mock.ExpectQuery("SELECT DISTINCT ON").WithArgs("app", time.Time{}).WillReturnError(errBoom)
	if _, err := s.RunsApplying(ctx, "app", time.Time{}); err == nil || !strings.Contains(err.Error(), "list applying runs") {
		t.Fatalf("err = %v", err)
	}
	mock.ExpectQuery("SELECT DISTINCT ON").WithArgs("app", time.Time{}).
		WillReturnRows(pgxmock.NewRows([]string{"version", "id"}).AddRow(int64(1), "r1").RowError(0, errBoom))
	if _, err := s.RunsApplying(ctx, "app", time.Time{}); err == nil || !strings.Contains(err.Error(), "list applying runs") {
		t.Fatalf("row err = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func planRows() *pgxmock.Rows {
	return pgxmock.NewRows([]string{
		"id", "target", "key", "rollout", "state", "history_hash", "applied", "schema_fingerprint", "schema_definition", "search_path", "drift", "plan",
		"validated", "acked", "allow_out_of_order", "created_by", "source", "created_at", "coalesce", "coalesce",
	}).AddRow(planA, "app", "k1", RolloutDirect, PlanReady, "h", []byte("[]"), "f", "d", "public", "", []byte("[]"),
		true, []string{}, false, "ci", "", now(), "", "")
}

func anyArgs(n int) []any {
	out := make([]any, n)
	for i := range out {
		out[i] = pgxmock.AnyArg()
	}

	return out
}
