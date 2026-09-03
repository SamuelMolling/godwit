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

const boundRunID = "7b1e2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d"

func expectBoundPlan(mock pgxmock.PgxPoolIface, applied []engine.Applied, migs []controlplane.PlanMigration) {
	mock.ExpectQuery("AND files_hash = \\$3 AND state = 'bound'").WithArgs("app", controlplane.RolloutDirect, pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "target", "key", "rollout", "state", "history_hash", "applied", "schema_fingerprint", "schema_definition", "search_path", "drift", "plan",
			"validated", "acked", "allow_out_of_order", "created_by", "source", "created_at", "coalesce", "coalesce",
		}).AddRow(planID, "app", "k", controlplane.RolloutDirect, controlplane.PlanBound, controlplane.HistoryHash(applied), applied,
			"f2", "table a\n", "", "", migs, false, []string{}, false, "ci", "", time.Now(), boundRunID, ""))
}

func expectBoundRun(mock pgxmock.PgxPoolIface, state string) {
	mock.ExpectQuery("FROM cp_runs WHERE id = \\$1").WithArgs(boundRunID).
		WillReturnRows(pgxmock.NewRows(
			[]string{"id", "target", "state", "coalesce", "attempts", "rollout", "phase", "coalesce", "kind", "coalesce", "coalesce", "created_at", "finished_at", "created_by", "source", "coalesce", "retries", "not_before"}).
			AddRow(boundRunID, "app", state, "", 1, controlplane.RolloutDirect, controlplane.PhaseExpand, "", controlplane.KindMigrate, "", "", time.Now(), (*time.Time)(nil), AnonymousActor, "", planID, 0, (*time.Time)(nil)))
}

func expectRunsApplying(mock pgxmock.PgxPoolIface, byVersion map[int64]string) {
	rows := pgxmock.NewRows([]string{"version", "id"})
	for v, id := range byVersion {
		rows.AddRow(v, id)
	}
	mock.ExpectQuery("SELECT DISTINCT ON \\(version\\)").WithArgs("app", pgxmock.AnyArg()).WillReturnRows(rows)
}

func TestReattachStoreErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	obs := controlplane.Observation{Fingerprint: "f2", Definition: "table a\n"}
	s, mock := planServer(t, obs, nil)

	mock.ExpectQuery("AND files_hash = \\$3 AND state = 'bound'").WithArgs("app", controlplane.RolloutDirect, pgxmock.AnyArg()).WillReturnError(errors.New("plans down"))
	if _, err := s.CreateRun(ctx, createReq()); connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("bound plan error: %v", err)
	}

	expectBoundPlan(mock, nil, nil)
	mock.ExpectQuery("FROM cp_runs WHERE id = \\$1").WithArgs(boundRunID).WillReturnError(errors.New("runs down"))
	if _, err := s.CreateRun(ctx, createReq()); connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("run load error: %v", err)
	}

	expectBoundPlan(mock, nil, nil)
	expectBoundRun(mock, controlplane.StateReverted)
	mock.ExpectExec("SET state = 'superseded' WHERE id = \\$1 AND state = 'bound'").WithArgs(planID).WillReturnError(errors.New("retire down"))
	if _, err := s.CreateRun(ctx, createReq()); connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("retire error: %v", err)
	}

	obs.Applied = []engine.Applied{{Version: 20260901120000, Name: "t", Checksum: "c"}}
	s.Inspector = stubInspector{obs: obs}
	for _, state := range []string{controlplane.StateSucceeded, controlplane.StateFailed} {
		expectBoundPlan(mock, nil, nil)
		expectBoundRun(mock, state)
		mock.ExpectQuery("SELECT DISTINCT ON \\(version\\)").WithArgs("app", pgxmock.AnyArg()).WillReturnError(errors.New("runs down"))
		if _, err := s.CreateRun(ctx, createReq()); connect.CodeOf(err) != connect.CodeInternal {
			t.Fatalf("%s attribute error: %v", state, err)
		}
	}

	expectBoundPlan(mock, nil, nil)
	expectBoundRun(mock, controlplane.StateFailed)
	expectRunsApplying(mock, nil)
	expectApplied(mock)
	mock.ExpectBegin()
	mock.ExpectQuery("UPDATE cp_runs SET state = 'queued', attempts = 0").WithArgs(boundRunID).WillReturnError(errors.New("resume down"))
	mock.ExpectRollback()
	if _, err := s.CreateRun(ctx, createReq()); connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("resume error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestReattachRefusals(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	applied := []engine.Applied{{Version: 20260901120000, Name: "t", Checksum: "c"}}
	obs := controlplane.Observation{Fingerprint: "f2", Definition: "table a\n", Applied: applied}
	s, mock := planServer(t, obs, nil)

	expectBoundPlan(mock, applied, plannedMigrations(t))
	expectBoundRun(mock, controlplane.StateFailed)
	s.Inspector = stubInspector{obs: controlplane.Observation{Fingerprint: "f2", Definition: "table a\n"}}
	_, err := s.CreateRun(ctx, createReq())
	if stale := stalePlanDetail(t, err); stale.PlanId != planID || stale.Reason != controlplane.StaleHistory || len(stale.HistoryRemoved) != 1 {
		t.Fatalf("removed history: %+v", stale)
	}

	s.Inspector = stubInspector{obs: obs}
	expectBoundPlan(mock, nil, plannedMigrations(t))
	expectBoundRun(mock, controlplane.StateSucceeded)
	expectRunsApplying(mock, map[int64]string{20260901120000: boundRunID})
	mock.ExpectExec("INSERT INTO cp_audit").WithArgs(AnonymousActor, controlplane.AuditRunReattach, boundRunID, "app", "state=succeeded plan="+planID+" resumed=false").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	if res, err := s.CreateRun(ctx, createReq()); err != nil || !res.Msg.Reattached || res.Msg.RunId != boundRunID || res.Msg.PlanId != planID {
		t.Fatalf("succeeded and applied: %+v, %v", res, err)
	}

	stranger := []engine.Applied{{Version: 20260901120000, Name: "t", Checksum: "c"}, {Version: 20260901130000, Name: "z", Checksum: "z"}}
	s.Inspector = stubInspector{obs: controlplane.Observation{Fingerprint: "f2", Definition: "table a\n", Applied: stranger}}
	expectBoundPlan(mock, nil, plannedMigrations(t))
	expectBoundRun(mock, controlplane.StateFailed)
	expectRunsApplying(mock, nil)
	_, err = s.CreateRun(ctx, createReq())
	if stale := stalePlanDetail(t, err); stale.Reason != controlplane.StaleHistory || len(stale.HistoryAdded) != 2 ||
		!strings.Contains(err.Error(), "20260901130000_z   applied") {
		t.Fatalf("unexplained history: %+v, %v", stale, err)
	}

	s.Inspector = stubInspector{obs: controlplane.Observation{Fingerprint: "f2", Definition: "table a\n"}}
	s.validator = failingValidator{err: fmt.Errorf("%w: column a is gone", controlplane.ErrValidationFailed)}
	expectBoundPlan(mock, nil, plannedMigrations(t))
	expectBoundRun(mock, controlplane.StateNeedsAttention)
	expectApplied(mock)
	_, err = s.CreateRun(ctx, createReq())
	if stale := stalePlanDetail(t, err); stale.Reason != controlplane.StaleValidation || !strings.Contains(stale.Hint, "column a is gone") {
		t.Fatalf("validation on resume: %+v", stale)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestReattachSkipsUnboundSet(t *testing.T) {
	t.Parallel()
	s, mock := planServer(t, controlplane.Observation{Fingerprint: "f2", Definition: "table a\n"}, nil)
	if run, ok, err := s.reattach(context.Background(), &godwitv1.CreateRunRequest{Target: "app", PlanId: planID}, runSpec{}, controlplane.Observation{}); ok || err != nil || run.ID != "" {
		t.Fatalf("explicit plan: %+v, %v, %v", run, ok, err)
	}
	expectNoBound(mock)
	mock.ExpectQuery("AND state = 'ready' AND created_at >= \\$3").WithArgs("app", pgxmock.AnyArg(), pgxmock.AnyArg()).WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery("SELECT provider, config FROM cp_targets").WithArgs("app").WillReturnError(pgx.ErrNoRows)
	if _, err := s.CreateRun(context.Background(), createReq()); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("no bound plan: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
