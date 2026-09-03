package api

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/pashagolub/pgxmock/v4"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/internal/controlplane"
	"github.com/SamuelMolling/godwit/internal/engine"
)

type stubInspector struct {
	status   controlplane.TargetStatus
	obs      controlplane.Observation
	losses   []engine.Loss
	lossErr  error
	err      error
	observed *[]engine.Drop
}

func (i stubInspector) Status(context.Context, string) (controlplane.TargetStatus, error) {
	return i.status, i.err
}

func (i stubInspector) Observe(context.Context, string) (controlplane.Observation, error) {
	return i.obs, i.err
}

func (i stubInspector) DataLoss(_ context.Context, _ string, drops []engine.Drop) ([]engine.Loss, error) {
	if i.observed != nil {
		*i.observed = append(*i.observed, drops...)
	}
	if i.lossErr != nil {
		return nil, i.lossErr
	}
	var out []engine.Loss
	for _, l := range i.losses {
		if slices.Contains(drops, l.Drop) {
			out = append(out, l)
		}
	}

	return out, nil
}

func TestGetTargetStatus(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mock.Close)
	s := NewServer(controlplane.NewStore(mock), nil, nil, nil)
	now := time.Now()
	finished := now.Add(time.Minute)
	s.Inspector = stubInspector{status: controlplane.TargetStatus{
		Target: "app", Provider: "static", Timeouts: controlplane.Timeouts{Lock: "3s", Statement: "1m"},
		Applied: []engine.Applied{
			{Version: 1, Name: "init", Checksum: "old", AppliedAt: now},
			{Version: 2, Name: "extra", Checksum: "x", AppliedAt: now},
		},
		LastRun:   &controlplane.Run{ID: "r1", Kind: controlplane.KindMigrate, State: controlplane.StateSucceeded, FinishedAt: &finished},
		Snapshot:  &controlplane.Snapshot{RunID: "r1", TakenAt: now},
		OpenDrift: true,
	}}
	files := []*godwitv1.MigrationFile{
		{Name: "00000000000001_init.up.sql", Body: "CREATE TABLE t (id int);"},
		{Name: "00000000000001_init.down.sql", Body: "DROP TABLE t;"},
		{Name: "00000000000003_next.up.sql", Body: "SELECT 1;"},
		{Name: "00000000000003_next.down.sql", Body: "SELECT 1;"},
	}

	if _, err := s.GetTargetStatus(ctx, connect.NewRequest(&godwitv1.GetTargetStatusRequest{})); connect.CodeOf(err) != connect.CodeInvalidArgument ||
		!strings.Contains(err.Error(), "target is required") {
		t.Fatalf("no target: %v", err)
	}
	if _, err := s.GetTargetStatus(ctx, connect.NewRequest(&godwitv1.GetTargetStatusRequest{
		Target: "app", Files: files[:1],
	})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("bad files: %v", err)
	}

	expectReadyCount(mock, 2)
	res, err := s.GetTargetStatus(ctx, connect.NewRequest(&godwitv1.GetTargetStatusRequest{Target: "app", Files: files}))
	if err != nil {
		t.Fatal(err)
	}
	got := res.Msg
	if got.Target != "app" || got.Provider != "static" || got.LockTimeout != "3s" || got.StatementTimeout != "1m" || got.ReadyPlans != 2 {
		t.Fatalf("head = %+v", got)
	}
	if len(got.Applied) != 2 || !got.Applied[0].ChecksumMismatch || got.Applied[1].ChecksumMismatch ||
		got.Applied[0].Checksum != "old" || got.Applied[0].AppliedAt.AsTime().IsZero() {
		t.Fatalf("applied = %+v", got.Applied)
	}
	if len(got.Pending) != 1 || got.Pending[0].Version != 3 || got.Pending[0].Name != "next" {
		t.Fatalf("pending = %+v", got.Pending)
	}
	if got.LastRun == nil || got.LastRun.Id != "r1" || got.LastRun.State != godwitv1.RunState_RUN_STATE_SUCCEEDED || got.LastRun.FinishedAt == nil {
		t.Fatalf("last run = %+v", got.LastRun)
	}
	if got.DriftBaseline == nil || got.DriftBaseline.RunId != "r1" || !got.DriftBaseline.UnresolvedDrift || got.DriftBaseline.TakenAt == nil {
		t.Fatalf("baseline = %+v", got.DriftBaseline)
	}

	s.Inspector = stubInspector{status: controlplane.TargetStatus{Target: "bare", Provider: "static"}}
	expectReadyCount(mock, 0)
	res, err = s.GetTargetStatus(ctx, connect.NewRequest(&godwitv1.GetTargetStatusRequest{Target: "bare"}))
	if err != nil || len(res.Msg.Applied) != 0 || len(res.Msg.Pending) != 0 || res.Msg.LastRun != nil || res.Msg.DriftBaseline != nil ||
		res.Msg.ReadyPlans != 0 {
		t.Fatalf("bare = %+v, err = %v", res, err)
	}
	mock.ExpectQuery("SELECT count").WithArgs("bare", pgxmock.AnyArg()).WillReturnError(errors.New("count down"))
	if _, err := s.GetTargetStatus(ctx, connect.NewRequest(&godwitv1.GetTargetStatusRequest{Target: "bare"})); connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("count error: %v", err)
	}

	s.Inspector = stubInspector{err: controlplane.ErrNotFound}
	if _, err := s.GetTargetStatus(ctx, connect.NewRequest(&godwitv1.GetTargetStatusRequest{Target: "ghost"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("unknown target: %v", err)
	}
	s.Inspector = stubInspector{err: errors.New("boom")}
	if _, err := s.GetTargetStatus(ctx, connect.NewRequest(&godwitv1.GetTargetStatusRequest{Target: "app"})); connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("internal: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func expectReadyCount(mock pgxmock.PgxPoolIface, n int) {
	mock.ExpectQuery("SELECT count").WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(n))
}

func TestListTargets(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mock.Close)
	s := NewServer(controlplane.NewStore(mock), nil, nil, nil)

	expectTargetRows(mock)
	res, err := s.ListTargets(ctx, connect.NewRequest(&godwitv1.ListTargetsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	got := res.Msg.Targets
	if len(got) != 2 {
		t.Fatalf("targets = %+v", got)
	}
	app := got[0]
	if app.Name != "app" || app.Provider != "static" || app.SearchPath != "app,public" || app.LockTimeout != "3s" ||
		app.StatementTimeout != "1m" || !app.RequirePlan || app.KeepOld || !app.UnresolvedDrift ||
		app.ReadyPlans != 2 || app.AppliedCount != 7 || app.AttentionRuns != 1 ||
		app.LastRun == nil || app.LastRun.Id != "r1" || app.LastRun.State != godwitv1.RunState_RUN_STATE_NEEDS_ATTENTION {
		t.Fatalf("app = %+v last = %+v", app, app.LastRun)
	}
	if bare := got[1]; bare.Name != "bare" || bare.RequirePlan || !bare.KeepOld || bare.LastRun != nil ||
		bare.SearchPath != "" || bare.AppliedCount != 0 {
		t.Fatalf("bare = %+v", bare)
	}

	s.RequirePlan = true
	expectTargetRows(mock)
	res, err = s.ListTargets(ctx, connect.NewRequest(&godwitv1.ListTargetsRequest{}))
	if err != nil || !res.Msg.Targets[1].RequirePlan {
		t.Fatalf("--require-plan must show on every target: %+v, err = %v", res, err)
	}

	mock.ExpectQuery("DISTINCT ON").WillReturnError(errors.New("runs down"))
	if _, err := s.ListTargets(ctx, connect.NewRequest(&godwitv1.ListTargetsRequest{})); connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("store error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func runRow() *pgxmock.Rows {
	return pgxmock.NewRows([]string{
		"id", "target", "state", "coalesce", "attempts", "rollout", "phase", "coalesce", "kind", "coalesce", "coalesce",
		"created_at", "finished_at", "created_by", "source", "coalesce", "retries", "not_before", "progress", "expansions",
	}).AddRow("r1", "app", controlplane.StateNeedsAttention, "boom", 3, controlplane.RolloutDirect, controlplane.PhaseExpand,
		"", controlplane.KindMigrate, "", "", time.Now(), (*time.Time)(nil), "ci", "", "", 0, (*time.Time)(nil),
		(*controlplane.RunProgress)(nil), map[string]controlplane.Expansion{})
}

func expectTargetRows(mock pgxmock.PgxPoolIface) {
	mock.ExpectQuery("DISTINCT ON").WillReturnRows(runRow())
	mock.ExpectQuery("FROM cp_targets").WithArgs(pgxmock.AnyArg()).WillReturnRows(
		pgxmock.NewRows([]string{"name", "provider", "config", "applied", "attention", "ready", "drift"}).
			AddRow("app", "static",
				[]byte(`{"lock_timeout":"3s","statement_timeout":"1m","require_plan":"true","keep_old":"false","search_path":"app,public"}`),
				7, 1, 2, true).
			AddRow("bare", "static", []byte(`{}`), 0, 0, 0, false))
}

func TestGetRunLedgerError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mock.Close)
	s := NewServer(controlplane.NewStore(mock), nil, nil, nil)

	mock.ExpectQuery("FROM cp_runs WHERE id").WithArgs("r1").WillReturnRows(runRow())
	mock.ExpectQuery("FROM cp_run_applied").WithArgs("r1").WillReturnError(errors.New("ledger down"))
	if _, err := s.GetRun(ctx, connect.NewRequest(&godwitv1.GetRunRequest{RunId: "r1"})); connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("err = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
