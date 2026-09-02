package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
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

func TestAuthActor(t *testing.T) {
	t.Parallel()

	if p, ok := newAuth(nil).actor(""); !ok || p != anonymousAdmin {
		t.Fatalf("empty token set = %+v %v, want anonymous admin", p, ok)
	}
	locked := newAuth([]Token{{Name: "ci", Scope: ScopePipeline, Secret: "t1"}})
	for _, h := range []string{"", "Bearer nope", "t1"} {
		if _, ok := locked.actor(h); ok {
			t.Fatalf("header %q must be rejected", h)
		}
	}
	if p, ok := locked.actor("Bearer t1"); !ok || p != (Principal{Name: "ci", Scope: ScopePipeline}) {
		t.Fatalf("valid token = %+v %v, want ci/pipeline", p, ok)
	}
	if Actor(context.Background()) != AnonymousActor || Caller(context.Background()) != anonymousAdmin {
		t.Fatal("bare context must be an anonymous admin")
	}
}

func TestParseTokens(t *testing.T) {
	t.Parallel()

	got, err := ParseTokens([]string{"ci:s1", " samuel:s2 ", "s3", "bot:read:s4", "deploy:pipeline:s5", "ops:operator:s6", "root:admin:s7"})
	if err != nil {
		t.Fatal(err)
	}
	want := []Token{
		{Name: "ci", Scope: ScopeAdmin, Secret: "s1"},
		{Name: "samuel", Scope: ScopeAdmin, Secret: "s2"},
		{Name: AnonymousActor, Scope: ScopeAdmin, Secret: "s3"},
		{Name: "bot", Scope: ScopeRead, Secret: "s4"},
		{Name: "deploy", Scope: ScopePipeline, Secret: "s5"},
		{Name: "ops", Scope: ScopeOperator, Secret: "s6"},
		{Name: "root", Scope: ScopeAdmin, Secret: "s7"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tokens = %+v, want %+v", got, want)
	}
	for _, specs := range [][]string{{""}, {":s"}, {"ci:"}, {"ci:read:"}, {"ci:same", "ops:same"}, {"ci:root:same"}, {"ci::same"}} {
		_, err := ParseTokens(specs)
		if err == nil || strings.Contains(err.Error(), "same") {
			t.Fatalf("ParseTokens(%q) = %v, want an error without the secret", specs, err)
		}
	}
	if _, err := ParseTokens([]string{"ci:root:s"}); err == nil || !strings.Contains(err.Error(), `unknown scope "root"`) {
		t.Fatalf("unknown scope: %v", err)
	}
}

func TestScopeTableCoversEveryProcedure(t *testing.T) {
	t.Parallel()

	svc := godwitv1.File_godwit_v1_godwit_proto.Services().ByName("GodwitService")
	methods := svc.Methods()
	if methods.Len() != len(procedureScopes) {
		t.Fatalf("scope table has %d procedures, service has %d", len(procedureScopes), methods.Len())
	}
	for i := range methods.Len() {
		procedure := "/" + string(svc.FullName()) + "/" + string(methods.Get(i).Name())
		if _, ok := procedureScopes[procedure]; !ok {
			t.Fatalf("%s has no scope in the auth table", procedure)
		}
	}
}

func TestScopeAllows(t *testing.T) {
	t.Parallel()

	ordered := []Scope{ScopeRead, ScopePipeline, ScopeOperator, ScopeAdmin}
	for i, have := range ordered {
		for j, need := range ordered {
			if have.allows(need) != (i >= j) {
				t.Fatalf("%s.allows(%s) = %v", have, need, i >= j)
			}
		}
		if have.allows("") || have.allows("root") {
			t.Fatalf("%s must not allow an unknown scope", have)
		}
	}
}

func TestAuthorizeByScope(t *testing.T) {
	t.Parallel()

	a := newAuth([]Token{
		{Name: "bot", Scope: ScopeRead, Secret: "r"},
		{Name: "deploy", Scope: ScopePipeline, Secret: "p"},
		{Name: "ops", Scope: ScopeOperator, Secret: "o"},
		{Name: "root", Scope: ScopeAdmin, Secret: "a"},
	})
	allowed := map[string][]Scope{
		godwitv1connect.GodwitServiceListRunsProcedure:       {ScopeRead, ScopePipeline, ScopeOperator, ScopeAdmin},
		godwitv1connect.GodwitServiceCreateRunProcedure:      {ScopePipeline, ScopeOperator, ScopeAdmin},
		godwitv1connect.GodwitServiceResumeRunProcedure:      {ScopeOperator, ScopeAdmin},
		godwitv1connect.GodwitServiceRegisterTargetProcedure: {ScopeAdmin},
		"/godwit.v1.GodwitService/Unlisted":                  {},
	}
	for procedure, scopes := range allowed {
		for secret, scope := range map[string]Scope{"r": ScopeRead, "p": ScopePipeline, "o": ScopeOperator, "a": ScopeAdmin} {
			ctx, err := a.authorize(context.Background(), procedure, "Bearer "+secret)
			if slices.Contains(scopes, scope) {
				if err != nil || Caller(ctx).Scope != scope {
					t.Fatalf("%s with %s: err = %v, caller = %+v", procedure, scope, err, Caller(ctx))
				}

				continue
			}
			if connect.CodeOf(err) != connect.CodePermissionDenied {
				t.Fatalf("%s with %s: err = %v, want permission denied", procedure, scope, err)
			}
			if Caller(ctx) != anonymousAdmin {
				t.Fatalf("denied call must not carry the principal: %+v", Caller(ctx))
			}
		}
	}
	_, err := a.authorize(context.Background(), godwitv1connect.GodwitServiceRegisterTargetProcedure, "Bearer p")
	if err == nil || err.Error() != "permission_denied: RegisterTarget requires scope admin; token deploy has scope pipeline" {
		t.Fatalf("denial message = %v", err)
	}
	if _, err := a.authorize(context.Background(), godwitv1connect.GodwitServiceListRunsProcedure, "Bearer nope"); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("unknown secret: %v", err)
	}
}

func TestAuthInterceptorDenies(t *testing.T) {
	t.Parallel()

	a := newAuth([]Token{{Name: "bot", Scope: ScopeRead, Secret: "r"}})
	unary := a.WrapUnary(func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) {
		t.Fatal("handler must not run")

		return nil, nil
	})
	req := specRequest{procedure: godwitv1connect.GodwitServiceCreateRunProcedure, header: http.Header{"Authorization": {"Bearer r"}}}
	if _, err := unary(context.Background(), req); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("unary: %v", err)
	}
	stream := a.WrapStreamingHandler(func(context.Context, connect.StreamingHandlerConn) error {
		t.Fatal("handler must not run")

		return nil
	})
	if err := stream(context.Background(), streamConn{}); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("stream: %v", err)
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
	up := Handler(&Server{Metrics: metrics.New(), ready: func(context.Context) error { return nil }}, []Token{{Name: "ops", Secret: "secret"}})
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
			[]string{"id", "target", "state", "coalesce", "attempts", "rollout", "phase", "coalesce", "kind", "coalesce", "coalesce", "created_at", "finished_at", "created_by", "source", "coalesce", "retries", "not_before"}).
			AddRow("r1", "app", controlplane.StateRunning, "", 1, controlplane.RolloutDirect, controlplane.PhaseExpand, "", controlplane.KindMigrate, "", "", time.Now(), (*time.Time)(nil), AnonymousActor, "", "", 0, (*time.Time)(nil)))

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
	s := NewServer(nil, nil, nil, nil)

	if err := s.checkOrder("app", []engine.Plan{plan(2)}, nil, false); err != nil {
		t.Fatalf("empty history: %v", err)
	}
	if err := s.checkOrder("app", []engine.Plan{plan(1), plan(3), plan(4)}, []int64{1, 3}, false); err != nil {
		t.Fatalf("applied and newer: %v", err)
	}
	err := s.checkOrder("app", []engine.Plan{plan(1), plan(2), plan(3)}, []int64{1, 3}, false)
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "out-of-order migrations 2: newest applied version on app is 3") {
		t.Fatalf("behind: %v", err)
	}
	if err := s.checkOrder("app", []engine.Plan{plan(2)}, []int64{1, 3}, true); err != nil {
		t.Fatalf("allowed: %v", err)
	}
}

func TestAdmitStoreError(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mock.Close)
	s := NewServer(controlplane.NewStore(mock), nil, nil, nil)
	mock.ExpectQuery("SELECT provider, config FROM cp_targets").WithArgs("ghost").WillReturnError(pgx.ErrNoRows)
	if _, err := s.admit(context.Background(), "ghost", nil, nil, false, false, ""); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("unknown target: %v", err)
	}
	expectTarget(mock)
	mock.ExpectQuery("SELECT DISTINCT left").WithArgs("app").WillReturnError(errors.New("down"))
	if _, err := s.admit(context.Background(), "app", nil, nil, false, false, ""); connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("store error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPlanRunUnit(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mock.Close)
	ctx := context.Background()
	files := []*godwitv1.MigrationFile{
		{Name: "00000000000001_t.up.sql", Body: "CREATE TABLE t (id int);\nCREATE INDEX CONCURRENTLY t_id ON t (id);"},
		{Name: "00000000000001_t.down.sql", Body: "DROP TABLE t;"},
		{Name: "00000000000002_d.up.sql", Body: "ALTER TABLE t DROP COLUMN id;"},
		{Name: "00000000000002_d.down.sql", Body: "SELECT 1;"},
	}
	s := NewServer(controlplane.NewStore(mock), nil, nil, nil)

	if _, err := s.PlanRun(ctx, connect.NewRequest(&godwitv1.PlanRunRequest{Files: files})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("no target: %v", err)
	}
	if _, err := s.PlanRun(ctx, connect.NewRequest(&godwitv1.PlanRunRequest{Target: "app"})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("no files: %v", err)
	}
	if _, err := s.PlanRun(ctx, connect.NewRequest(&godwitv1.PlanRunRequest{Target: "app", Files: files, Rollout: "weird"})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("bad rollout: %v", err)
	}
	if _, err := s.PlanRun(ctx, connect.NewRequest(&godwitv1.PlanRunRequest{Target: "app", Files: files[:1]})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("bad files: %v", err)
	}
	if _, err := s.PlanRun(ctx, connect.NewRequest(&godwitv1.PlanRunRequest{Target: "app", Files: files})); connect.CodeOf(err) != connect.CodeFailedPrecondition ||
		!strings.Contains(err.Error(), "H003") {
		t.Fatalf("hazard gate: %v", err)
	}

	expectTarget(mock)
	mock.ExpectQuery("SELECT DISTINCT left").WithArgs("app").WillReturnRows(pgxmock.NewRows([]string{"version"}).AddRow(int64(1)))
	res, err := s.PlanRun(ctx, connect.NewRequest(&godwitv1.PlanRunRequest{
		Target: "app", Files: files, Rollout: controlplane.RolloutExpandContract, AcknowledgeHazards: []string{"H003"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	got := res.Msg
	if got.Target != "app" || got.Rollout != controlplane.RolloutExpandContract || got.Validated || len(got.Migrations) != 2 {
		t.Fatalf("plan = %+v", got)
	}
	first, second := got.Migrations[0], got.Migrations[1]
	if first.Version != 1 || first.Name != "t" || first.Checksum == "" || !first.Applied || first.Phase != controlplane.PhaseExpand ||
		len(first.Statements) != 2 || first.Statements[0].NoTx || !first.Statements[1].NoTx || len(first.Statements[1].Hazards) != 0 {
		t.Fatalf("first = %+v", first)
	}
	if second.Version != 2 || second.Applied || second.Phase != controlplane.PhaseContract || len(second.Statements) != 1 ||
		second.Statements[0].Sql != "ALTER TABLE t DROP COLUMN id" || len(second.Statements[0].Hazards) != 1 ||
		second.Statements[0].Hazards[0].Code != "H003" || second.Statements[0].Hazards[0].Detail == "" {
		t.Fatalf("second = %+v", second)
	}

	expectTarget(mock)
	mock.ExpectQuery("SELECT DISTINCT left").WithArgs("app").WillReturnRows(pgxmock.NewRows([]string{"version"}))
	res, err = s.PlanRun(ctx, connect.NewRequest(&godwitv1.PlanRunRequest{Target: "app", Files: files[:2]}))
	if err != nil || res.Msg.Migrations[0].Phase != controlplane.PhaseExpand || res.Msg.Migrations[0].Applied {
		t.Fatalf("direct = %+v, err = %v", res, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func expectTarget(mock pgxmock.PgxPoolIface) {
	mock.ExpectQuery("SELECT provider, config FROM cp_targets").WithArgs("app").
		WillReturnRows(pgxmock.NewRows([]string{"provider", "config"}).AddRow("static", []byte(`{}`)))
}

type failingValidator struct{ err error }

func (f failingValidator) Validate(context.Context, string, []engine.Plan, string) (controlplane.Validation, error) {
	return controlplane.Validation{}, f.err
}

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

	expectTarget(mock)
	mock.ExpectQuery("SELECT DISTINCT left").WithArgs("app").WillReturnRows(pgxmock.NewRows([]string{"version"}))
	s := NewServer(controlplane.NewStore(mock), nil, failingValidator{err: errors.New("scratch down")}, nil)
	if _, err := s.CreateRun(ctx, req()); connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("validator error: %v", err)
	}

	expectTarget(mock)
	mock.ExpectQuery("SELECT DISTINCT left").WithArgs("app").WillReturnRows(pgxmock.NewRows([]string{"version"}))
	mock.ExpectExec("WITH r AS \\(INSERT INTO cp_runs").WithArgs(pgxmock.AnyArg(), "app", pgxmock.AnyArg(), pgxmock.AnyArg(), controlplane.RolloutDirect, "", "", AnonymousActor, "", "").WillReturnError(errors.New("insert down"))
	s = NewServer(controlplane.NewStore(mock), nil, nil, nil)
	if _, err := s.CreateRun(ctx, req()); connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("store error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
