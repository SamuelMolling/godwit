package controlplane

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/SamuelMolling/godwit/internal/engine"
)

func TestRetiredColumnsRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newStore(t)
	if err := s.RegisterTarget(ctx, "app", "plain", map[string]string{"dsn": "x"}); err != nil {
		t.Fatal(err)
	}
	runID := uuid.NewString()
	if err := s.CreateRun(ctx, runID, "app", RolloutDirect, map[string]string{"a.up.sql": "SELECT 1;"}, Timeouts{}, Provenance{}, "", nil); err != nil {
		t.Fatal(err)
	}
	cols := []RetiredColumn{{Schema: "public", Table: "users", Column: "age_old", Retires: "age"}}
	if err := s.RetireColumns(ctx, "app", runID, "20260101000000_x", cols); err != nil {
		t.Fatal(err)
	}
	if err := s.RetireColumns(ctx, "app", runID, "20260101000000_x", cols); err != nil {
		t.Fatalf("retiring twice must be idempotent: %v", err)
	}
	got, err := s.RetiredColumns(ctx, "app")
	if err != nil || len(got) != 1 || got[0] != cols[0] {
		t.Fatalf("retired = %+v, err = %v", got, err)
	}
	if err := s.UnretireColumns(ctx, "app", cols); err != nil {
		t.Fatal(err)
	}
	if got, err := s.RetiredColumns(ctx, "app"); err != nil || len(got) != 0 {
		t.Fatalf("retired after revert = %+v, err = %v", got, err)
	}
}

func TestRunExpansionsAndProgress(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newStore(t)
	if err := s.RegisterTarget(ctx, "app", "plain", map[string]string{"dsn": "x"}); err != nil {
		t.Fatal(err)
	}
	runID := uuid.NewString()
	exps := map[string]Expansion{"20260101000000_x": {ID: "20260101000000_x", UpSQL: "SELECT 1;", Hash: "h"}}
	if err := s.CreateRun(ctx, runID, "app", RolloutDirect, map[string]string{"a.up.sql": "SELECT 1;"}, Timeouts{}, Provenance{}, "", exps); err != nil {
		t.Fatal(err)
	}
	stored, err := s.Run(ctx, runID)
	if err != nil || stored.Expansions["20260101000000_x"].Hash != "h" {
		t.Fatalf("expansions = %+v, err = %v", stored.Expansions, err)
	}
	if err := s.SaveProgress(ctx, runID, RunProgress{Migration: "m", Statement: 3, Batches: 2, RowsDone: 10}); err != nil {
		t.Fatal(err)
	}
	run, err := s.Run(ctx, runID)
	if err != nil || run.Progress == nil || run.Progress.RowsDone != 10 {
		t.Fatalf("progress = %+v, err = %v", run.Progress, err)
	}
	if _, ok, err := s.AwaitingContract(ctx, "app"); err != nil || ok {
		t.Fatalf("no held run yet: ok = %t, err = %v", ok, err)
	}
	if err := s.Finish(ctx, runID, StateAwaitingContract, ""); err != nil {
		t.Fatal(err)
	}
	held, ok, err := s.AwaitingContract(ctx, "app")
	if err != nil || !ok || held.ID != runID {
		t.Fatalf("held = %+v %t %v", held, ok, err)
	}
}

func TestExpansionStoreErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mock, s := newMockStore(t)

	mock.ExpectExec("UPDATE cp_runs SET progress").WithArgs("r1", pgxmock.AnyArg()).WillReturnError(errBoom)
	if err := s.SaveProgress(ctx, "r1", RunProgress{}); err == nil || !strings.Contains(err.Error(), "save run progress") {
		t.Fatalf("err = %v", err)
	}
	cols := []RetiredColumn{{Schema: "public", Table: "users", Column: "age_old"}}
	mock.ExpectExec("INSERT INTO cp_retired_columns").WithArgs(anyArgs(7)...).WillReturnError(errBoom)
	if err := s.RetireColumns(ctx, "app", "r1", "m", cols); err == nil ||
		!strings.Contains(err.Error(), "retire column public.users.age_old") {
		t.Fatalf("err = %v", err)
	}
	mock.ExpectExec("DELETE FROM cp_retired_columns").WithArgs(anyArgs(4)...).WillReturnError(errBoom)
	if err := s.UnretireColumns(ctx, "app", cols); err == nil ||
		!strings.Contains(err.Error(), "unretire column public.users.age_old") {
		t.Fatalf("err = %v", err)
	}
	mock.ExpectQuery("FROM cp_retired_columns").WithArgs("app").WillReturnError(errBoom)
	if _, err := s.RetiredColumns(ctx, "app"); err == nil || !strings.Contains(err.Error(), "list retired columns") {
		t.Fatalf("err = %v", err)
	}
	mock.ExpectQuery("FROM cp_retired_columns").WithArgs("app").WillReturnRows(
		pgxmock.NewRows([]string{"schema", "rel", "col", "retires"}).AddRow("public", "users", "age_old", "age").RowError(0, errBoom))
	if _, err := s.RetiredColumns(ctx, "app"); err == nil || !strings.Contains(err.Error(), "read retired columns") {
		t.Fatalf("err = %v", err)
	}
	mock.ExpectQuery("state = 'awaiting_contract'").WithArgs("app").WillReturnError(errBoom)
	if _, _, err := s.AwaitingContract(ctx, "app"); err == nil || !strings.Contains(err.Error(), "load awaiting run") {
		t.Fatalf("err = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSchedulerExpandedErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newStore(t)
	sched, _ := newScheduler(t, s, Config{Holder: "h"})
	runID := uuid.NewString()
	files := map[string]string{
		"20260101000000_x.up.sql":   "-- godwit: change-type public.t.a bigint\n",
		"20260101000000_x.down.sql": "-- godwit: revert\n",
	}
	broken := map[string]Expansion{"20260101000000_x": {ID: "20260101000000_x", UpSQL: "NOT SQL", DownSQL: "NOT SQL"}}
	if err := s.CreateRun(ctx, runID, "app", RolloutDirect, files, Timeouts{}, Provenance{}, "", broken); err != nil {
		t.Fatal(err)
	}
	plans, err := PlansFromFiles(files, engine.DirectionUp)
	if err != nil {
		t.Fatal(err)
	}
	run := Run{ID: runID, Target: "app", Expansions: broken}
	if _, err := sched.expanded(ctx, run, plans, engine.DirectionUp); err == nil {
		t.Fatal("a broken up expansion must fail the run")
	}
	down, err := PlansFromFiles(files, engine.DirectionDown)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sched.expanded(ctx, Run{ID: "r2", Target: "app", Reverts: runID}, down, engine.DirectionDown); err == nil {
		t.Fatal("a broken down expansion must fail the revert")
	}
	if _, err := sched.expanded(ctx, Run{ID: "r2", Reverts: uuid.NewString()}, down, engine.DirectionDown); err == nil {
		t.Fatal("a missing original run must fail")
	}
}

func TestSchedulerRetireAndProgressWarn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newStore(t)
	sched, _ := newScheduler(t, s, Config{Holder: "h"})
	broken := NewScheduler(NewStore(failPool{}), nil, PGEngine{}, Policies(), Config{}, testLog)
	broken.progress(ctx, "r1")(engine.StatementEvent{Migration: "m"})
	broken.retire(ctx, Run{ID: "r1", Target: "app", Reverts: "r0"}, testLog)
	broken.retire(ctx, Run{
		ID: "r1", Target: "app",
		Expansions: map[string]Expansion{"m": {Retired: []RetiredColumn{{Schema: "public", Table: "t", Column: "a_old"}}}},
	}, testLog)

	runID := uuid.NewString()
	exps := map[string]Expansion{"20260101000000_x": {
		ID: "20260101000000_x", Retired: []RetiredColumn{{Schema: "public", Table: "t", Column: "a_old", Retires: "a"}},
	}}
	if err := s.CreateRun(ctx, runID, "app", RolloutDirect, map[string]string{"a.up.sql": "SELECT 1;"}, Timeouts{}, Provenance{}, "", exps); err != nil {
		t.Fatal(err)
	}
	stored, err := s.Run(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	sched.retire(ctx, stored, testLog)
	got, err := s.RetiredColumns(ctx, "app")
	if err != nil || len(got) != 1 {
		t.Fatalf("retired = %+v, err = %v", got, err)
	}
	sched.retire(ctx, Run{ID: uuid.NewString(), Target: "app", Reverts: runID}, testLog)
	if got, err := s.RetiredColumns(ctx, "app"); err != nil || len(got) != 0 {
		t.Fatalf("after revert = %+v, err = %v", got, err)
	}
	stored.Target = "ghost"
	sched.retire(ctx, stored, testLog)
}

// failPool refuses every statement, so the scheduler's best-effort bookkeeping takes its warning path.
type failPool struct{ Pool }

func (failPool) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errBoom
}

func (failPool) QueryRow(context.Context, string, ...any) pgx.Row { return errRow{} }

type errRow struct{}

func (errRow) Scan(...any) error { return errBoom }

func TestDiffKeepsRetiredColumns(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d, s, targetDSN := newDiffer(t, nil)
	execDSN(t, targetDSN, `CREATE TABLE public.users (id bigint PRIMARY KEY, age bigint, age_old integer)`)
	runID := uuid.NewString()
	if err := s.CreateRun(ctx, runID, "app", RolloutDirect, map[string]string{"a.up.sql": "SELECT 1;"}, Timeouts{}, Provenance{}, "", nil); err != nil {
		t.Fatal(err)
	}
	cols := []RetiredColumn{{Schema: "public", Table: "users", Column: "age_old", Retires: "age"}}
	if err := s.RetireColumns(ctx, "app", runID, "m", cols); err != nil {
		t.Fatal(err)
	}
	out, err := d.Diff(ctx, "app", `CREATE TABLE public.users (id bigint PRIMARY KEY, age bigint, nick text)`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.UpSQL, "nick") {
		t.Fatalf("an unrelated change must survive the filter:\n%s", out.UpSQL)
	}
	if strings.Contains(out.UpSQL, "age_old") {
		t.Fatalf("retired column must not be proposed for dropping:\n%s", out.UpSQL)
	}
	if len(out.Retained) != 1 || out.Retained[0] != "public.users.age_old" {
		t.Fatalf("retained = %v", out.Retained)
	}
	if err := s.UnretireColumns(ctx, "app", cols); err != nil {
		t.Fatal(err)
	}
	out, err = d.Diff(ctx, "app", `CREATE TABLE public.users (id bigint PRIMARY KEY, age bigint)`)
	if err != nil || !strings.Contains(out.UpSQL, "age_old") || out.Retained != nil {
		t.Fatalf("out = %+v, err = %v", out, err)
	}
}

func TestDiffRetiredColumnsError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d, _, targetDSN := newDiffer(t, nil)
	execDSN(t, targetDSN, `CREATE TABLE public.users (id bigint PRIMARY KEY)`)
	if _, err := d.pool.Exec(ctx, `DROP TABLE cp_retired_columns`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Diff(ctx, "app", `CREATE TABLE public.users (id bigint PRIMARY KEY, age bigint)`); err == nil ||
		!strings.Contains(err.Error(), "list retired columns") {
		t.Fatalf("err = %v", err)
	}
}

func TestValidateReplaysExpandedHistory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, pool := newStore(t)
	if err := s.RegisterTarget(ctx, "app", "plain", map[string]string{"dsn": "x"}); err != nil {
		t.Fatal(err)
	}
	v := NewValidator(pool, s, uuid.NewString)

	runID := uuid.NewString()
	files := map[string]string{
		"20260101000000_x.up.sql":   "-- godwit: change-type public.t.a bigint\n",
		"20260101000000_x.down.sql": "-- godwit: revert\n",
	}
	exps := map[string]Expansion{"20260101000000_x": {ID: "20260101000000_x", UpSQL: "NOT SQL"}}
	if err := s.CreateRun(ctx, runID, "app", RolloutDirect, files, Timeouts{}, Provenance{}, "", exps); err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(ctx, runID, StateSucceeded, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Validate(ctx, "app", nil, ""); err == nil || !strings.Contains(err.Error(), "history run 0") {
		t.Fatalf("broken history expansion = %v", err)
	}

	if err := s.RegisterTarget(ctx, "other", "plain", map[string]string{"dsn": "x"}); err != nil {
		t.Fatal(err)
	}
	bad := uuid.NewString()
	if err := s.CreateRun(ctx, bad, "other", RolloutDirect, map[string]string{"nonsense.up.sql": "SELECT 1;"}, Timeouts{}, Provenance{}, "", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(ctx, bad, StateSucceeded, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Validate(ctx, "other", nil, ""); err == nil || !strings.Contains(err.Error(), "history run 0") {
		t.Fatalf("broken history files = %v", err)
	}
}

func TestSchedulerFailsRunWithBrokenExpansion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newStore(t)
	sched, _ := newScheduler(t, s, Config{Holder: "h"})
	runID := uuid.NewString()
	files := map[string]string{
		"20260101000000_x.up.sql":   "-- godwit: change-type public.t.a bigint\n",
		"20260101000000_x.down.sql": "-- godwit: revert\n",
	}
	exps := map[string]Expansion{"20260101000000_x": {ID: "20260101000000_x", UpSQL: "NOT SQL"}}
	if err := s.CreateRun(ctx, runID, "app", RolloutDirect, files, Timeouts{}, Provenance{}, "", exps); err != nil {
		t.Fatal(err)
	}
	sched.Tick(ctx)
	run := waitState(t, s, runID, StateFailed)
	if !strings.Contains(run.Error, "does not parse") {
		t.Fatalf("run error = %q", run.Error)
	}
}

func TestValidatorKeepOldTargetDefault(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, pool := newStore(t)
	v := NewValidator(pool, s, uuid.NewString)
	if _, err := v.expander(ctx, "ghost"); err == nil {
		t.Fatal("an unknown target must fail before the scratch is created")
	}
	if err := s.RegisterTarget(ctx, "app", "plain", map[string]string{"dsn": "x", ConfigKeepOld: "false"}); err != nil {
		t.Fatal(err)
	}
	x, err := v.expander(ctx, "app")
	if err != nil || x.Expander.KeepOld {
		t.Fatalf("expander = %+v, err = %v", x.Expander, err)
	}
	if _, err := v.Validate(ctx, "ghost", nil, ""); err == nil {
		t.Fatal("Validate must refuse an unknown target")
	}
}
