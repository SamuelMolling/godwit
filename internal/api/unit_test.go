package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/pashagolub/pgxmock/v4"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/gen/godwit/v1/godwitv1connect"
	"github.com/SamuelMolling/godwit/internal/controlplane"
	"github.com/SamuelMolling/godwit/internal/engine"
	"github.com/SamuelMolling/godwit/internal/metrics"
	"github.com/SamuelMolling/godwit/internal/notify"
)

func TestRPCErrMapping(t *testing.T) {
	t.Parallel()

	cases := []struct {
		err  error
		code connect.Code
	}{
		{controlplane.ErrNotFound, connect.CodeNotFound},
		{controlplane.ErrNotResumable, connect.CodeFailedPrecondition},
		{controlplane.ErrNotAwaitingContract, connect.CodeFailedPrecondition},
		{controlplane.ErrNotRevertable, connect.CodeFailedPrecondition},
		{errors.New("other"), connect.CodeInternal},
	}
	for _, tc := range cases {
		if got := connect.CodeOf(rpcErr(tc.err)); got != tc.code {
			t.Fatalf("rpcErr(%v) = %v, want %v", tc.err, got, tc.code)
		}
	}
}

func TestAuthAllowed(t *testing.T) {
	t.Parallel()

	open := newAuth(nil)
	if !open.allowed("") {
		t.Fatal("empty token set must allow everything")
	}
	locked := newAuth([]string{"t1"})
	if locked.allowed("") || locked.allowed("Bearer nope") || locked.allowed("t1") {
		t.Fatal("bad credentials must be rejected")
	}
	if !locked.allowed("Bearer t1") {
		t.Fatal("valid token must pass")
	}
}

func TestAuthWrapStreamingClientPassthrough(t *testing.T) {
	t.Parallel()

	called := false
	next := connect.StreamingClientFunc(func(context.Context, connect.Spec) connect.StreamingClientConn {
		called = true

		return nil
	})
	newAuth(nil).WrapStreamingClient(next)(context.Background(), connect.Spec{})
	if !called {
		t.Fatal("must pass through")
	}
}

func TestToProtoFinished(t *testing.T) {
	t.Parallel()

	now := time.Now()
	pb := toProto(controlplane.Run{ID: "r", State: controlplane.StateSucceeded, FinishedAt: &now})
	if pb.FinishedAt == nil {
		t.Fatal("finished_at must be set")
	}
}

func TestSleepCtxCancelled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepCtx(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if err := sleepCtx(context.Background(), time.Millisecond); err != nil {
		t.Fatalf("err = %v", err)
	}
}

func TestDriftDisabled(t *testing.T) {
	t.Parallel()

	s := NewServer(nil, nil, nil, nil)
	ctx := context.Background()
	if _, err := s.CheckDrift(ctx, connect.NewRequest(&godwitv1.CheckDriftRequest{})); connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("err = %v", err)
	}
	if _, err := s.AcceptBaseline(ctx, connect.NewRequest(&godwitv1.AcceptBaselineRequest{})); connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("err = %v", err)
	}
	if _, err := s.BaselineTarget(ctx, connect.NewRequest(&godwitv1.BaselineTargetRequest{})); connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("err = %v", err)
	}
	if _, err := s.GetTargetStatus(ctx, connect.NewRequest(&godwitv1.GetTargetStatusRequest{})); connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("err = %v", err)
	}
}

func TestCheckHazards(t *testing.T) {
	t.Parallel()

	m := engine.Migration{Version: 1, Name: "d", UpSQL: "DROP TABLE x;", DownSQL: "SELECT 1;"}
	p, err := engine.BuildPlan(m, engine.DirectionUp)
	if err != nil {
		t.Fatal(err)
	}
	if err := NewServer(nil, nil, nil, nil).checkHazards([]engine.Plan{p}, nil); err == nil || !strings.Contains(err.Error(), "H002") {
		t.Fatalf("err = %v", err)
	}
	if err := NewServer(nil, nil, nil, nil).checkHazards([]engine.Plan{p}, []string{"H002"}); err != nil {
		t.Fatalf("acked: %v", err)
	}
}

func TestSettled(t *testing.T) {
	t.Parallel()

	for state, want := range map[string]bool{
		controlplane.StateQueued:           false,
		controlplane.StateRunning:          false,
		controlplane.StateSucceeded:        true,
		controlplane.StateFailed:           true,
		controlplane.StateNeedsAttention:   true,
		controlplane.StateAwaitingContract: true,
	} {
		if settled(state) != want {
			t.Fatalf("settled(%s) != %v", state, want)
		}
	}
}

func TestHealthAndReadiness(t *testing.T) {
	t.Parallel()

	get := func(h http.Handler, path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

		return rec
	}
	up := Handler(&Server{Metrics: metrics.New(), ready: func(context.Context) error { return nil }}, []string{"secret"})
	down := Handler(&Server{Metrics: metrics.New(), ready: func(context.Context) error { return errors.New("boom") }}, nil)

	if rec := get(up, "/healthz"); rec.Code != http.StatusOK || rec.Body.String() != "ok\n" {
		t.Fatalf("healthz = %d %q", rec.Code, rec.Body.String())
	}
	if rec := get(up, "/readyz"); rec.Code != http.StatusOK || rec.Body.String() != "ok\n" {
		t.Fatalf("readyz = %d %q", rec.Code, rec.Body.String())
	}
	if rec := get(down, "/healthz"); rec.Code != http.StatusOK {
		t.Fatalf("healthz with store down = %d", rec.Code)
	}
	if rec := get(down, "/readyz"); rec.Code != http.StatusServiceUnavailable ||
		!strings.Contains(rec.Body.String(), "store unavailable: boom") {
		t.Fatalf("readyz with store down = %d %q", rec.Code, rec.Body.String())
	}
	if body := get(up, "/metrics").Body.String(); strings.Contains(body, "healthz") || strings.Contains(body, "readyz") {
		t.Fatal("probes must not appear in API metrics")
	}
}

func TestWatchRunCancelledWhileSleeping(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mock.Close)
	mock.MatchExpectationsInOrder(false)
	mock.ExpectQuery("SELECT id, target, state").WithArgs("r1").
		WillReturnRows(pgxmock.NewRows(
			[]string{"id", "target", "state", "coalesce", "attempts", "rollout", "phase", "coalesce", "kind", "coalesce", "coalesce", "created_at", "finished_at"}).
			AddRow("r1", "app", controlplane.StateRunning, "", 1, controlplane.RolloutDirect, controlplane.PhaseExpand, "", controlplane.KindMigrate, "", "", time.Now(), (*time.Time)(nil)))

	s := NewServer(controlplane.NewStore(mock), nil, nil, nil)
	s.watchInterval = time.Hour
	srv := httptest.NewServer(Handler(s, nil))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	stream, err := godwitv1connect.NewGodwitServiceClient(srv.Client(), srv.URL).
		WatchRun(ctx, connect.NewRequest(&godwitv1.WatchRunRequest{RunId: "r1"}))
	if err != nil {
		t.Fatal(err)
	}
	if !stream.Receive() {
		t.Fatalf("first snapshot missing: %v", stream.Err())
	}
	cancel()
	for stream.Receive() {
	}
	if connect.CodeOf(stream.Err()) != connect.CodeCanceled {
		t.Fatalf("stream err = %v", stream.Err())
	}
}

func TestParkRunNotifiesWithoutLookup(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mock.Close)
	mock.ExpectExec("UPDATE cp_runs SET state").WithArgs("r1", controlplane.StateNeedsAttention, "why").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectQuery("SELECT id, target, state").WithArgs("r1").WillReturnError(errors.New("gone"))

	rec := &recordingNotifier{}
	s := NewServer(controlplane.NewStore(mock), nil, nil, nil)
	s.Notifier = rec
	if _, err := s.ParkRun(context.Background(), connect.NewRequest(&godwitv1.ParkRunRequest{RunId: "r1", Reason: "why"})); err != nil {
		t.Fatal(err)
	}
	if len(rec.events) != 0 {
		t.Fatalf("events = %+v", rec.events)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

type recordingNotifier struct{ events []notify.Event }

func (r *recordingNotifier) Notify(_ context.Context, e notify.Event) error {
	r.events = append(r.events, e)

	return nil
}

func TestCheckOrder(t *testing.T) {
	t.Parallel()

	plan := func(v int64) engine.Plan {
		p, err := engine.BuildPlan(engine.Migration{Version: v, Name: "m", UpSQL: "SELECT 1;", DownSQL: "SELECT 1;"}, engine.DirectionUp)
		if err != nil {
			t.Fatal(err)
		}

		return p
	}
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mock.Close)
	applied := func(versions ...int64) {
		rows := pgxmock.NewRows([]string{"version"})
		for _, v := range versions {
			rows.AddRow(v)
		}
		mock.ExpectQuery("SELECT DISTINCT left").WithArgs("app").WillReturnRows(rows)
	}
	s := NewServer(controlplane.NewStore(mock), nil, nil, nil)
	ctx := context.Background()

	mock.ExpectQuery("SELECT DISTINCT left").WithArgs("app").WillReturnError(errors.New("down"))
	if err := s.checkOrder(ctx, "app", []engine.Plan{plan(2)}, false); connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("store error: %v", err)
	}
	applied()
	if err := s.checkOrder(ctx, "app", []engine.Plan{plan(2)}, false); err != nil {
		t.Fatalf("empty history: %v", err)
	}
	applied(1, 3)
	if err := s.checkOrder(ctx, "app", []engine.Plan{plan(1), plan(3), plan(4)}, false); err != nil {
		t.Fatalf("applied and newer: %v", err)
	}
	applied(1, 3)
	err = s.checkOrder(ctx, "app", []engine.Plan{plan(1), plan(2), plan(3)}, false)
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "out-of-order migrations 2: newest applied version on app is 3") {
		t.Fatalf("behind: %v", err)
	}
	applied(1, 3)
	if err := s.checkOrder(ctx, "app", []engine.Plan{plan(2)}, true); err != nil {
		t.Fatalf("allowed: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

type failingValidator struct{ err error }

func (f failingValidator) Validate(context.Context, string, []engine.Plan) error { return f.err }

func TestCreateRunInternalErrors(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mock.Close)
	ctx := context.Background()
	req := func() *connect.Request[godwitv1.CreateRunRequest] {
		return connect.NewRequest(&godwitv1.CreateRunRequest{
			Target: "app",
			Files: []*godwitv1.MigrationFile{
				{Name: "20260901120000_t.up.sql", Body: "SELECT 1;"},
				{Name: "20260901120000_t.down.sql", Body: "SELECT 1;"},
			},
		})
	}

	mock.ExpectQuery("SELECT DISTINCT left").WithArgs("app").WillReturnRows(pgxmock.NewRows([]string{"version"}))
	s := NewServer(controlplane.NewStore(mock), nil, failingValidator{err: errors.New("scratch down")}, nil)
	if _, err := s.CreateRun(ctx, req()); connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("validator error: %v", err)
	}

	mock.ExpectQuery("SELECT DISTINCT left").WithArgs("app").WillReturnRows(pgxmock.NewRows([]string{"version"}))
	mock.ExpectExec("WITH r AS \\(INSERT INTO cp_runs").WithArgs(pgxmock.AnyArg(), "app", pgxmock.AnyArg(), pgxmock.AnyArg(), controlplane.RolloutDirect, "", "").WillReturnError(errors.New("insert down"))
	s = NewServer(controlplane.NewStore(mock), nil, nil, nil)
	if _, err := s.CreateRun(ctx, req()); connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("store error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
