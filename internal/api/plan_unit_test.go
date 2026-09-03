package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/internal/controlplane"
	"github.com/SamuelMolling/godwit/internal/engine"
)

const planID = "0d4a1d1e-4c4c-4f3e-9c2a-1d2e3f4a5b6c"

func planFiles() []*godwitv1.MigrationFile {
	return []*godwitv1.MigrationFile{
		{Name: "20260901120000_t.up.sql", Body: "SELECT 1;"},
		{Name: "20260901120000_t.down.sql", Body: "SELECT 2;"},
	}
}

func anyArgs(n int) []any {
	out := make([]any, n)
	for i := range out {
		out[i] = pgxmock.AnyArg()
	}

	return out
}

func planServer(t *testing.T, obs controlplane.Observation, validator Validator) (*Server, pgxmock.PgxPoolIface) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mock.Close)
	s := NewServer(controlplane.NewStore(mock), nil, validator, nil)
	s.Inspector = stubInspector{obs: obs}

	return s, mock
}

func expectApplied(mock pgxmock.PgxPoolIface, versions ...int64) {
	expectTarget(mock)
	rows := pgxmock.NewRows([]string{"version"})
	for _, v := range versions {
		rows.AddRow(v)
	}
	mock.ExpectQuery("SELECT DISTINCT left").WithArgs("app").WillReturnRows(rows)
	expectNoRepeatables(mock)
}

func expectIdle(mock pgxmock.PgxPoolIface) {
	mock.ExpectQuery("state = 'awaiting_contract'").WithArgs("app").WillReturnError(pgx.ErrNoRows)
}

func expectNoBound(mock pgxmock.PgxPoolIface) {
	expectIdle(mock)
	mock.ExpectQuery("AND files_hash = \\$3 AND state = 'bound'").WithArgs("app", controlplane.RolloutDirect, pgxmock.AnyArg()).WillReturnError(pgx.ErrNoRows)
}

func expectReadyPlan(mock pgxmock.PgxPoolIface, fingerprint string, applied []engine.Applied, migs []controlplane.PlanMigration) {
	expectNoBound(mock)
	mock.ExpectQuery("AND state = 'ready' AND created_at >= \\$3").WithArgs("app", pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "target", "key", "rollout", "state", "history_hash", "applied", "repeatables", "schema_fingerprint", "schema_definition", "search_path", "drift", "plan",
			"validated", "acked", "allow_out_of_order", "created_by", "source", "created_at", "coalesce", "coalesce", "expansions",
		}).AddRow(planID, "app", "k", controlplane.RolloutDirect, controlplane.PlanReady, controlplane.HistoryHash(applied, nil), applied, []engine.Repeatable{},
			fingerprint, "table a\n", "", "", migs, false, []string{}, false, "ci", "", time.Now(), "", "", map[string]controlplane.Expansion{}))
}

func expectSnapshot(mock pgxmock.PgxPoolIface, fingerprint string) {
	mock.ExpectQuery("FROM cp_snapshots").WithArgs("app").WillReturnRows(
		pgxmock.NewRows([]string{"target", "fingerprint", "definition", "coalesce", "taken_at"}).AddRow("app", fingerprint, "table a\n", "", time.Now()))
}

func plannedMigrations(t *testing.T) []controlplane.PlanMigration {
	t.Helper()
	spec, err := upSpec("app", "", planFiles())
	if err != nil {
		t.Fatal(err)
	}

	return controlplane.BuildPlanMigrations(spec.rollout, spec.plans, controlplane.AppliedSet{}, nil)
}

func createReq(acked ...string) *connect.Request[godwitv1.CreateRunRequest] {
	return connect.NewRequest(&godwitv1.CreateRunRequest{Target: "app", Files: planFiles(), AcknowledgeHazards: acked})
}

func stalePlanDetail(t *testing.T, err error) *godwitv1.PlanStale {
	t.Helper()
	var cerr *connect.Error
	if !errors.As(err, &cerr) || cerr.Code() != connect.CodeFailedPrecondition || len(cerr.Details()) != 1 {
		t.Fatalf("err = %v, want failed_precondition with one detail", err)
	}
	msg, err := cerr.Details()[0].Value()
	if err != nil {
		t.Fatal(err)
	}
	stale, ok := msg.(*godwitv1.PlanStale)
	if !ok {
		t.Fatalf("detail = %T", msg)
	}

	return stale
}

func TestPlanRunPersistErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	req := func() *connect.Request[godwitv1.PlanRunRequest] {
		return connect.NewRequest(&godwitv1.PlanRunRequest{Target: "app", Files: planFiles(), Persist: true})
	}

	s, mock := planServer(t, controlplane.Observation{}, nil)
	s.Inspector = nil
	if _, err := s.PlanRun(ctx, req()); connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("no inspector: %v", err)
	}

	s.Inspector = stubInspector{err: errors.New("target down")}
	mock.ExpectQuery("state = 'awaiting_contract'").WithArgs("app").WillReturnError(errors.New("runs down"))
	if _, err := s.PlanRun(ctx, req()); connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("awaiting run error: %v", err)
	}

	expectIdle(mock)
	if _, err := s.PlanRun(ctx, req()); connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("observe error: %v", err)
	}

	s.Inspector = stubInspector{obs: controlplane.Observation{Applied: []engine.Applied{{Version: 20260901120000, Name: "t", Checksum: "other"}}}}
	expectIdle(mock)
	expectApplied(mock, 20260901120000)
	if _, err := s.PlanRun(ctx, req()); connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "20260901120000_t applied with different content") {
		t.Fatalf("content mismatch: %v", err)
	}

	s.Inspector = stubInspector{obs: controlplane.Observation{Fingerprint: "f2", Definition: "table b\n"}}
	expectIdle(mock)
	expectApplied(mock)
	mock.ExpectQuery("FROM cp_snapshots").WithArgs("app").WillReturnError(errors.New("snapshot down"))
	if _, err := s.PlanRun(ctx, req()); connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("drift error: %v", err)
	}

	expectIdle(mock)
	expectApplied(mock)
	expectSnapshot(mock, "f1")
	mock.ExpectExec("DELETE FROM cp_plan_files").WithArgs("app", pgxmock.AnyArg()).WillReturnError(errors.New("save down"))
	if _, err := s.PlanRun(ctx, req()); connect.CodeOf(err) != connect.CodeInternal || !strings.Contains(err.Error(), "save down") {
		t.Fatalf("save error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestBindStoreErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	obs := controlplane.Observation{Fingerprint: "f2", Definition: "table b\n"}
	s, mock := planServer(t, obs, nil)

	expectNoBound(mock)
	mock.ExpectQuery("AND state = 'ready' AND created_at >= \\$3").WithArgs("app", pgxmock.AnyArg(), pgxmock.AnyArg()).WillReturnError(errors.New("plans down"))
	if _, err := s.CreateRun(ctx, createReq()); connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("ready plan error: %v", err)
	}

	expectNoBound(mock)
	mock.ExpectQuery("AND state = 'ready' AND created_at >= \\$3").WithArgs("app", pgxmock.AnyArg(), pgxmock.AnyArg()).WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery("SELECT provider, config FROM cp_targets").WithArgs("app").WillReturnError(errors.New("targets down"))
	if _, err := s.CreateRun(ctx, createReq()); connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("target error: %v", err)
	}

	s.RequirePlan = true
	expectNoBound(mock)
	mock.ExpectQuery("AND state = 'ready' AND created_at >= \\$3").WithArgs("app", pgxmock.AnyArg(), pgxmock.AnyArg()).WillReturnError(pgx.ErrNoRows)
	expectTarget(mock)
	mock.ExpectQuery("ORDER BY created_at DESC, id LIMIT").WithArgs("app", 3).WillReturnError(errors.New("list down"))
	if _, err := s.CreateRun(ctx, createReq()); connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("list plans error: %v", err)
	}
	s.RequirePlan = false

	expectReadyPlan(mock, "f2", nil, nil)
	expectApplied(mock)
	mock.ExpectBegin()
	mock.ExpectExec("WITH r AS \\(INSERT INTO cp_runs").WithArgs(anyArgs(11)...).WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec("SET state = 'bound'").WithArgs(planID, pgxmock.AnyArg()).WillReturnError(errors.New("bind down"))
	mock.ExpectRollback()
	if _, err := s.CreateRun(ctx, createReq("H009")); connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("bind error: %v", err)
	}

	obs.Applied = []engine.Applied{{Version: 5, Name: "x", Checksum: "c"}}
	s.Inspector = stubInspector{obs: obs}
	expectReadyPlan(mock, "f2", nil, nil)
	mock.ExpectQuery("SELECT DISTINCT ON \\(regexp_replace").WithArgs("app", pgxmock.AnyArg()).WillReturnError(errors.New("runs down"))
	if _, err := s.CreateRun(ctx, createReq()); connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("attribute error: %v", err)
	}

	expectReadyPlan(mock, "f1", obs.Applied, nil)
	mock.ExpectQuery("FROM cp_snapshots").WithArgs("app").WillReturnError(errors.New("snapshot down"))
	if _, err := s.CreateRun(ctx, createReq()); connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("baseline error: %v", err)
	}

	expectReadyPlan(mock, "f1", obs.Applied, plannedMigrations(t))
	expectSnapshot(mock, "f2")
	expectApplied(mock)
	mock.ExpectExec("SET state = 'superseded'").WithArgs(planID).WillReturnError(errors.New("supersede down"))
	if _, err := s.CreateRun(ctx, createReq()); connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("supersede error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestBindReplanRefusals(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	obs := controlplane.Observation{Fingerprint: "f2", Definition: "table b\n"}

	s, mock := planServer(t, obs, nil)
	expectReadyPlan(mock, "f1", nil, nil)
	expectSnapshot(mock, "f2")
	expectApplied(mock)
	_, err := s.CreateRun(ctx, createReq())
	if stale := stalePlanDetail(t, err); stale.PlanId != planID || stale.Reason != controlplane.StaleHistory ||
		!strings.Contains(stale.Hint, "statements changed after re-plan") {
		t.Fatalf("statements changed: %+v", stale)
	}

	s.validator = failingValidator{err: fmt.Errorf("%w: column a is gone", controlplane.ErrValidationFailed)}
	expectReadyPlan(mock, "f1", nil, nil)
	expectSnapshot(mock, "f2")
	expectApplied(mock)
	_, err = s.CreateRun(ctx, createReq())
	if stale := stalePlanDetail(t, err); stale.Reason != controlplane.StaleValidation ||
		!strings.Contains(stale.Hint, "the set no longer validates on the target's history: ") || !strings.Contains(stale.Hint, "column a is gone") {
		t.Fatalf("validation: %+v", stale)
	}

	expectReadyPlan(mock, "f1", nil, nil)
	mock.ExpectQuery("FROM cp_snapshots").WithArgs("app").WillReturnError(pgx.ErrNoRows)
	_, err = s.CreateRun(ctx, createReq())
	if stale := stalePlanDetail(t, err); stale.Reason != controlplane.StaleSchema || !strings.Contains(stale.Hint, "godwit drift accept app") {
		t.Fatalf("no baseline: %+v", stale)
	}

	s.validator = failingValidator{err: errors.New("scratch down")}
	expectReadyPlan(mock, "f1", nil, nil)
	expectSnapshot(mock, "f2")
	expectApplied(mock)
	if _, err := s.CreateRun(ctx, createReq()); connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("validator down: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

type stubValidator struct{ val controlplane.Validation }

func (v stubValidator) Validate(context.Context, string, []engine.Plan, string) (controlplane.Validation, error) {
	return v.val, nil
}

func TestPlanRunDetectsWithValidation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	obs := controlplane.Observation{Fingerprint: "f2", Definition: "table a\ntable b\n"}
	val := controlplane.Validation{Base: "table a\n", Effects: [][]string{{"+ table b"}}, Fingerprints: []string{"f1", "f2"}}
	s, mock := planServer(t, obs, stubValidator{val: val})
	files := []*godwitv1.MigrationFile{
		{Name: "20260901120000_t.up.sql", Body: "CREATE TABLE b (id int);"},
		{Name: "20260901120000_t.down.sql", Body: "DROP TABLE b;"},
	}

	expectIdle(mock)
	expectApplied(mock)
	mock.ExpectExec("DELETE FROM cp_plan_files").WithArgs("app", pgxmock.AnyArg()).WillReturnError(errors.New("save down"))
	_, err := s.PlanRun(ctx, connect.NewRequest(&godwitv1.PlanRunRequest{Target: "app", Files: files, Persist: true}))
	if connect.CodeOf(err) != connect.CodeInternal || !strings.Contains(err.Error(), "save down") {
		t.Fatalf("save error: %v", err)
	}

	expectReadyPlan(mock, "f1", nil, nil)
	expectSnapshot(mock, "f2")
	expectApplied(mock)
	_, err = s.CreateRun(ctx, connect.NewRequest(&godwitv1.CreateRunRequest{Target: "app", Files: files}))
	if stale := stalePlanDetail(t, err); stale.Reason != controlplane.StaleHistory {
		t.Fatalf("re-plan marks changed: %+v", stale)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPlanMigrationsDetects(t *testing.T) {
	t.Parallel()
	spec, err := upSpec("app", "", []*godwitv1.MigrationFile{
		{Name: "20260901120000_t.up.sql", Body: "CREATE TABLE b (id int);"},
		{Name: "20260901120000_t.down.sql", Body: "DROP TABLE b;"},
	})
	if err != nil {
		t.Fatal(err)
	}
	obs := controlplane.Observation{Fingerprint: "f2", Definition: "table a\ntable b\n"}

	migs, drift, detected := planMigrations(spec, admission{}, obs)
	if detected || drift != "" || migs[0].AlreadyApplied {
		t.Fatalf("no validation: %+v %q %t", migs, drift, detected)
	}

	val := controlplane.Validation{Base: "table a\n", Effects: [][]string{{"+ table b"}}, Fingerprints: []string{"f1", "f2"}}
	migs, drift, detected = planMigrations(spec, admission{validation: &val}, obs)
	if !detected || drift != "" || !migs[0].AlreadyApplied || migs[0].Effect != "+ table b" {
		t.Fatalf("validation: %+v %q %t", migs, drift, detected)
	}
}

func TestErrMessage(t *testing.T) {
	t.Parallel()

	if got := errMessage(errors.New("plain")); got != "plain" {
		t.Fatalf("plain = %q", got)
	}
	if got := errMessage(connect.NewError(connect.CodeInternal, errors.New("wrapped"))); got != "wrapped" {
		t.Fatalf("connect = %q", got)
	}
}

func TestAuditTruncatesDetail(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mock.Close)
	s := NewServer(controlplane.NewStore(mock), nil, nil, nil)

	mock.ExpectExec("INSERT INTO cp_audit").WithArgs(AnonymousActor, "x", "", "app", strings.Repeat("é", auditDetailLimit)+"…").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	s.audit(context.Background(), "x", "", "app", strings.Repeat("é", auditDetailLimit+1))
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestObservedSearchPath(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := NewServer(nil, nil, nil, nil)
	if path, err := s.observedSearchPath(ctx, "app"); path != "" || err != nil {
		t.Fatalf("no inspector: %q, %v", path, err)
	}
	s.Inspector = stubInspector{err: errors.New("target down")}
	if _, err := s.observedSearchPath(ctx, "app"); connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("observe error: %v", err)
	}
	s.Inspector = stubInspector{obs: controlplane.Observation{SearchPath: "app,public"}}
	if path, err := s.observedSearchPath(ctx, "app"); path != "app,public" || err != nil {
		t.Fatalf("path = %q, err = %v", path, err)
	}
}
