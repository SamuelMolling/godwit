package api

import (
	"context"
	"errors"
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

type storedPlanRow struct {
	state, rollout, key, runID, supersededBy string
	createdAt                                time.Time
	applied                                  []engine.Applied
}

func expectPlanByID(mock pgxmock.PgxPoolIface, row storedPlanRow) {
	mock.ExpectQuery("FROM cp_plans WHERE id = \\$1").WithArgs(planID).WillReturnRows(pgxmock.NewRows([]string{
		"id", "target", "key", "rollout", "state", "history_hash", "applied", "repeatables", "schema_fingerprint", "schema_definition", "search_path", "drift", "plan",
		"validated", "acked", "allow_out_of_order", "created_by", "source", "created_at", "coalesce", "coalesce", "expansions",
	}).AddRow(planID, "app", row.key, row.rollout, row.state, controlplane.HistoryHash(row.applied, nil), row.applied, []engine.Repeatable{},
		"f1", "table a\n", "public", "+ x", []controlplane.PlanMigration{}, true, []string{"H002"}, true, "ci", "repo@sha", row.createdAt, row.runID, row.supersededBy, map[string]controlplane.Expansion{}))
}

func readyRow(t *testing.T) storedPlanRow {
	t.Helper()
	spec, err := (&Server{}).upSpec("app", "", planFiles())
	if err != nil {
		t.Fatal(err)
	}
	pending, err := controlplane.Pending(migrations(spec.plans), nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	return storedPlanRow{
		state: controlplane.PlanReady, rollout: controlplane.RolloutDirect, createdAt: time.Now(),
		key: controlplane.PlanKey("app", controlplane.RolloutDirect, pending),
	}
}

func TestGetPlanUnit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, mock := planServer(t, controlplane.Observation{}, nil)

	if _, err := s.GetPlan(ctx, connect.NewRequest(&godwitv1.GetPlanRequest{})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("no id: %v", err)
	}
	mock.ExpectQuery("FROM cp_plans WHERE id = \\$1").WithArgs(planID).WillReturnError(pgx.ErrNoRows)
	if _, err := s.GetPlan(ctx, connect.NewRequest(&godwitv1.GetPlanRequest{PlanId: planID})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("missing: %v", err)
	}
	row := readyRow(t)
	row.applied = []engine.Applied{{Version: 3, Name: "c", Checksum: "x"}, {Version: 2, Name: "b", Checksum: "y"}}
	row.runID, row.supersededBy = "r1", "p2"
	expectPlanByID(mock, row)
	res, err := s.GetPlan(ctx, connect.NewRequest(&godwitv1.GetPlanRequest{PlanId: planID}))
	if err != nil {
		t.Fatal(err)
	}
	p := res.Msg.Plan
	if p.Id != planID || p.Target != "app" || p.Key != row.key || p.Rollout != "direct" || p.State != "ready" || p.Drift != "+ x" ||
		!p.Validated || p.AcknowledgedHazards[0] != "H002" || !p.AllowOutOfOrder || p.CreatedBy != "ci" || p.Source != "repo@sha" ||
		p.CreatedAt == nil || p.RunId != "r1" || p.SupersededBy != "p2" || p.Observed.AppliedCount != 2 || p.Observed.NewestApplied != 3 ||
		p.Observed.HistoryHash != controlplane.HistoryHash(row.applied, nil) || p.Observed.SchemaFingerprint != "f1" {
		t.Fatalf("plan = %+v", p)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestListPlansUnit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, mock := planServer(t, controlplane.Observation{}, nil)

	if _, err := s.ListPlans(ctx, connect.NewRequest(&godwitv1.ListPlansRequest{})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("no target: %v", err)
	}
	mock.ExpectQuery("ORDER BY created_at DESC, id LIMIT").WithArgs("app", 5).WillReturnError(errors.New("list down"))
	if _, err := s.ListPlans(ctx, connect.NewRequest(&godwitv1.ListPlansRequest{Target: "app", Limit: 5})); connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("store error: %v", err)
	}
	mock.ExpectQuery("ORDER BY created_at DESC, id LIMIT").WithArgs("app", 100).WillReturnRows(pgxmock.NewRows([]string{"id"}))
	res, err := s.ListPlans(ctx, connect.NewRequest(&godwitv1.ListPlansRequest{Target: "app"}))
	if err != nil || len(res.Msg.Plans) != 0 {
		t.Fatalf("plans = %+v, err = %v", res, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func byPlan(target, rollout string, files []*godwitv1.MigrationFile) *connect.Request[godwitv1.CreateRunRequest] {
	return connect.NewRequest(&godwitv1.CreateRunRequest{PlanId: planID, Target: target, Rollout: rollout, Files: files})
}

func TestExplicitPlanRefusals(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, mock := planServer(t, controlplane.Observation{Fingerprint: "f1", Definition: "table a\n"}, nil)

	s.Inspector = nil
	if _, err := s.CreateRun(ctx, byPlan("", "", nil)); connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("no inspector: %v", err)
	}
	s.Inspector = stubInspector{obs: controlplane.Observation{Fingerprint: "f1", Definition: "table a\n"}}

	mock.ExpectQuery("FROM cp_plans WHERE id = \\$1").WithArgs(planID).WillReturnError(pgx.ErrNoRows)
	if _, err := s.CreateRun(ctx, byPlan("", "", nil)); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("missing plan: %v", err)
	}

	row := readyRow(t)
	expectPlanByID(mock, row)
	if _, err := s.CreateRun(ctx, byPlan("other", "", nil)); connect.CodeOf(err) != connect.CodeInvalidArgument ||
		!strings.Contains(err.Error(), "belongs to target app") {
		t.Fatalf("target mismatch: %v", err)
	}
	expectPlanByID(mock, row)
	if _, err := s.CreateRun(ctx, byPlan("app", "expand-contract", nil)); connect.CodeOf(err) != connect.CodeInvalidArgument ||
		!strings.Contains(err.Error(), "uses rollout direct") {
		t.Fatalf("rollout mismatch: %v", err)
	}

	expectPlanByID(mock, row)
	mock.ExpectQuery("SELECT name, body FROM cp_plan_files").WithArgs(planID).WillReturnError(errors.New("files down"))
	if _, err := s.CreateRun(ctx, byPlan("", "", nil)); connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("files error: %v", err)
	}
	expectPlanByID(mock, row)
	mock.ExpectQuery("SELECT name, body FROM cp_plan_files").WithArgs(planID).WillReturnRows(pgxmock.NewRows([]string{"name", "body"}))
	if _, err := s.CreateRun(ctx, byPlan("", "", nil)); connect.CodeOf(err) != connect.CodeInvalidArgument ||
		!strings.Contains(err.Error(), "at least one migration file") {
		t.Fatalf("no files: %v", err)
	}

	expectPlanByID(mock, row)
	expectIdle(mock)
	mock.ExpectQuery("FROM cp_plans WHERE id = \\$1").WithArgs(planID).WillReturnError(errors.New("plans down"))
	if _, err := s.CreateRun(ctx, byPlan("", "", planFiles())); connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("second load error: %v", err)
	}

	for name, tc := range map[string]struct {
		mutate func(*storedPlanRow)
		code   connect.Code
		want   string
	}{
		"bound":      {mutate: func(r *storedPlanRow) { r.state, r.runID = controlplane.PlanBound, "r1" }, code: connect.CodeFailedPrecondition, want: "is bound to run r1"},
		"superseded": {mutate: func(r *storedPlanRow) { r.state, r.supersededBy = controlplane.PlanSuperseded, "p2" }, code: connect.CodeFailedPrecondition, want: "was superseded by p2"},
		"expired":    {mutate: func(r *storedPlanRow) { r.createdAt = time.Now().Add(-2 * time.Hour) }, code: connect.CodeFailedPrecondition, want: "expired"},
		"key":        {mutate: func(r *storedPlanRow) { r.key = "other" }, code: connect.CodeInvalidArgument, want: "files do not match plan"},
		"content": {
			mutate: func(r *storedPlanRow) {
				r.applied = []engine.Applied{{Version: 20260901120000, Name: "t", Checksum: "nope"}}
			},
			code: connect.CodeInvalidArgument, want: "files do not match plan",
		},
	} {
		t.Run(name, func(t *testing.T) {
			s.PlanTTL = time.Hour
			r := row
			tc.mutate(&r)
			expectPlanByID(mock, r)
			expectIdle(mock)
			expectPlanByID(mock, r)
			_, err := s.CreateRun(ctx, byPlan("", "", planFiles()))
			if connect.CodeOf(err) != tc.code || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v", err)
			}
		})
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
