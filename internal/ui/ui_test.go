package ui

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/gen/godwit/v1/godwitv1connect"
	"github.com/SamuelMolling/godwit/internal/api"
	"github.com/SamuelMolling/godwit/internal/controlplane"
)

var (
	errBoom = errors.New("boom")
	now     = time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
)

type stub struct {
	godwitv1connect.UnimplementedGodwitServiceHandler
	actor    string
	err      error
	runs     []*godwitv1.Run
	audit    []*godwitv1.AuditEntry
	plans    map[string]*godwitv1.Plan
	status   *godwitv1.GetTargetStatusResponse
	sums     []*godwitv1.TargetSummary
	fleet    *godwitv1.ListMigrationsResponse
	fleetErr error
	events   []*godwitv1.DriftEvent
	drift    *godwitv1.CheckDriftResponse
	calls    []string
}

func (s *stub) call(ctx context.Context, name string) error {
	s.actor = api.Actor(ctx)
	s.calls = append(s.calls, name)

	return s.err
}

func (s *stub) ListRuns(ctx context.Context, _ *connect.Request[godwitv1.ListRunsRequest]) (*connect.Response[godwitv1.ListRunsResponse], error) {
	if err := s.call(ctx, "ListRuns"); err != nil {
		return nil, err
	}

	return connect.NewResponse(&godwitv1.ListRunsResponse{Runs: s.runs}), nil
}

func (s *stub) GetRun(ctx context.Context, req *connect.Request[godwitv1.GetRunRequest]) (*connect.Response[godwitv1.GetRunResponse], error) {
	if err := s.call(ctx, "GetRun:"+req.Msg.RunId); err != nil {
		return nil, err
	}
	for _, r := range s.runs {
		if r.Id == req.Msg.RunId {
			return connect.NewResponse(&godwitv1.GetRunResponse{Run: r}), nil
		}
	}

	return nil, connect.NewError(connect.CodeNotFound, errors.New("no such run"))
}

func (s *stub) ListAudit(ctx context.Context, req *connect.Request[godwitv1.ListAuditRequest]) (*connect.Response[godwitv1.ListAuditResponse], error) {
	if err := s.call(ctx, "ListAudit:"+req.Msg.RunId); err != nil {
		return nil, err
	}

	return connect.NewResponse(&godwitv1.ListAuditResponse{Entries: s.audit}), nil
}

func (s *stub) GetPlan(ctx context.Context, req *connect.Request[godwitv1.GetPlanRequest]) (*connect.Response[godwitv1.GetPlanResponse], error) {
	if err := s.call(ctx, "GetPlan:"+req.Msg.PlanId); err != nil {
		return nil, err
	}
	if p, ok := s.plans[req.Msg.PlanId]; ok {
		return connect.NewResponse(&godwitv1.GetPlanResponse{Plan: p}), nil
	}

	return nil, connect.NewError(connect.CodeNotFound, errors.New("no such plan"))
}

func (s *stub) ListTargets(ctx context.Context, _ *connect.Request[godwitv1.ListTargetsRequest]) (*connect.Response[godwitv1.ListTargetsResponse], error) {
	if err := s.call(ctx, "ListTargets"); err != nil {
		return nil, err
	}

	return connect.NewResponse(&godwitv1.ListTargetsResponse{Targets: s.sums}), nil
}

func (s *stub) GetTargetStatus(ctx context.Context, req *connect.Request[godwitv1.GetTargetStatusRequest]) (*connect.Response[godwitv1.GetTargetStatusResponse], error) {
	if err := s.call(ctx, "GetTargetStatus:"+req.Msg.Target); err != nil {
		return nil, err
	}
	if s.status == nil {
		return connect.NewResponse(&godwitv1.GetTargetStatusResponse{Target: req.Msg.Target}), nil
	}

	return connect.NewResponse(s.status), nil
}

func (s *stub) ResumeRun(ctx context.Context, req *connect.Request[godwitv1.ResumeRunRequest]) (*connect.Response[godwitv1.ResumeRunResponse], error) {
	return connect.NewResponse(&godwitv1.ResumeRunResponse{}), s.call(ctx, "ResumeRun:"+req.Msg.RunId)
}

func (s *stub) ParkRun(ctx context.Context, req *connect.Request[godwitv1.ParkRunRequest]) (*connect.Response[godwitv1.ParkRunResponse], error) {
	return connect.NewResponse(&godwitv1.ParkRunResponse{}), s.call(ctx, "ParkRun:"+req.Msg.Reason)
}

func (s *stub) ConfirmRollout(ctx context.Context, req *connect.Request[godwitv1.ConfirmRolloutRequest]) (*connect.Response[godwitv1.ConfirmRolloutResponse], error) {
	return connect.NewResponse(&godwitv1.ConfirmRolloutResponse{}), s.call(ctx, "ConfirmRollout:"+req.Msg.RunId)
}

func (s *stub) RevertRun(ctx context.Context, req *connect.Request[godwitv1.RevertRunRequest]) (*connect.Response[godwitv1.RevertRunResponse], error) {
	if err := s.call(ctx, "RevertRun:"+strings.Join(req.Msg.AcknowledgeHazards, "+")); err != nil {
		return nil, err
	}

	return connect.NewResponse(&godwitv1.RevertRunResponse{RunId: "r-down"}), nil
}

func (s *stub) CheckDrift(ctx context.Context, req *connect.Request[godwitv1.CheckDriftRequest]) (*connect.Response[godwitv1.CheckDriftResponse], error) {
	if err := s.call(ctx, "CheckDrift:"+req.Msg.Target); err != nil {
		return nil, err
	}

	return connect.NewResponse(s.drift), nil
}

func (s *stub) ListDriftEvents(ctx context.Context, _ *connect.Request[godwitv1.ListDriftEventsRequest]) (*connect.Response[godwitv1.ListDriftEventsResponse], error) {
	if err := s.call(ctx, "ListDriftEvents"); err != nil {
		return nil, err
	}

	return connect.NewResponse(&godwitv1.ListDriftEventsResponse{Events: s.events}), nil
}

func (s *stub) AcceptBaseline(ctx context.Context, req *connect.Request[godwitv1.AcceptBaselineRequest]) (*connect.Response[godwitv1.AcceptBaselineResponse], error) {
	return connect.NewResponse(&godwitv1.AcceptBaselineResponse{}), s.call(ctx, "AcceptBaseline:"+req.Msg.Target)
}

// newUI opts the open handler back up to operator; New itself gives an anonymous visitor read only,
// which TestAnonymousScopeIsRead covers.
func newUI(s godwitv1connect.GodwitServiceHandler, cfg Config) *Handler {
	if cfg.AnonymousScope == "" {
		cfg.AnonymousScope = api.ScopeOperator
	}
	h := New(s, cfg)
	h.now = func() time.Time { return now }

	return h
}

func do(h http.Handler, method, target string, form url.Values, hdr ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if !safeMethod(method) {
		req.Header.Set("Sec-Fetch-Site", "same-origin")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	return rec
}

func want(t *testing.T, rec *httptest.ResponseRecorder, code int, parts ...string) {
	t.Helper()
	if rec.Code != code {
		t.Fatalf("code = %d, body:\n%s", rec.Code, rec.Body.String())
	}
	for _, p := range parts {
		if !strings.Contains(rec.Body.String(), p) {
			t.Fatalf("missing %q in:\n%s", p, rec.Body.String())
		}
	}
}

func absent(t *testing.T, rec *httptest.ResponseRecorder, parts ...string) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, body:\n%s", rec.Code, rec.Body.String())
	}
	for _, p := range parts {
		if strings.Contains(rec.Body.String(), p) {
			t.Fatalf("unexpected %q in:\n%s", p, rec.Body.String())
		}
	}
}

func redirect(t *testing.T, rec *httptest.ResponseRecorder, location string) {
	t.Helper()
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != location {
		t.Fatalf("code = %d location = %q", rec.Code, rec.Header().Get("Location"))
	}
}

func basic(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

func at(d time.Duration) *timestamppb.Timestamp {
	return timestamppb.New(now.Add(-d))
}

func fixture() *stub {
	return &stub{
		runs: []*godwitv1.Run{
			{Id: "r-run-00001", Target: "app", State: godwitv1.RunState_RUN_STATE_RUNNING, Attempts: 1, CreatedAt: at(10 * time.Second)},
			{Id: "r-queue-001", Target: "app", State: godwitv1.RunState_RUN_STATE_QUEUED, CreatedAt: at(time.Second)},
			{Id: "r-retry-001", Target: "app", State: godwitv1.RunState_RUN_STATE_QUEUED, Attempts: 1, Retries: 2, NotBefore: at(-20 * time.Second), CreatedAt: at(time.Minute)},
			{Id: "r-ok-000001", Target: "app", Kind: "migrate", CreatedBy: "ci", Source: "github", PlanId: "p-plan-0001", State: godwitv1.RunState_RUN_STATE_SUCCEEDED, Attempts: 1, Rollout: "direct", CreatedAt: at(2 * time.Hour), FinishedAt: at(2*time.Hour - 90*time.Second)},
			{Id: "r-bad-00001", Target: "billing", State: godwitv1.RunState_RUN_STATE_NEEDS_ATTENTION, Attempts: 3, Error: "lock timeout", CreatedAt: at(3 * 24 * time.Hour), FinishedAt: at(2 * 24 * time.Hour)},
			{Id: "r-wait-0001", Target: "app", State: godwitv1.RunState_RUN_STATE_AWAITING_CONTRACT, Rollout: "expand-contract", Phase: "expand", CreatedAt: at(5 * time.Minute)},
			{Id: "r-rev-00001", Target: "app", State: godwitv1.RunState_RUN_STATE_REVERTED, Reverts: "r-ok-000001", CreatedAt: at(time.Hour), FinishedAt: at(time.Hour - 300*time.Millisecond)},
			{Id: "r-fail-0001", Target: "app", State: godwitv1.RunState_RUN_STATE_FAILED, Attempts: 1, PlanId: "p-gone-0001", Error: "relation not_there does not exist", CreatedAt: at(30 * time.Hour), FinishedAt: at(30*time.Hour - 3*time.Second)},
			{Id: "r-old-00001", Target: "app", State: godwitv1.RunState_RUN_STATE_SUCCEEDED, CreatedAt: at(48 * time.Hour), FinishedAt: at(47 * time.Hour)},
		},
		audit: []*godwitv1.AuditEntry{
			{Action: controlplane.AuditRunConfirm, Actor: "ui:sam", At: at(time.Minute)},
			{Action: controlplane.AuditRunReattach, Actor: "ci", Detail: "state=queued plan=p-plan-0001 resumed=false", At: at(90 * time.Second)},
			{Action: controlplane.AuditRunPark, Actor: "ui:sam", Detail: "waiting on dba", At: at(2 * time.Minute)},
			{Action: controlplane.AuditRunResume, Actor: "ci", At: at(3 * time.Minute)},
			{Action: controlplane.AuditRunCreate, Actor: "ci", At: at(4 * time.Minute)},
		},
		plans: map[string]*godwitv1.Plan{
			"p-plan-0001": {
				Id: "p-plan-0001", Target: "app", State: "bound", Validated: true, AcknowledgedHazards: []string{"H002"},
				Migrations: []*godwitv1.PlannedMigration{
					{
						Version: 20260901120000, Name: "add_index", Phase: "expand",
						Statements: []*godwitv1.PlannedStatement{
							{Sql: "CREATE INDEX i1 ON t (a);", Hazards: []*godwitv1.PlannedHazard{
								{Code: "H001", Detail: "CREATE INDEX without CONCURRENTLY blocks writes on t", Recipe: "CREATE INDEX CONCURRENTLY i1 ON t (a);"},
							}},
							{Sql: "CREATE INDEX i2 ON t (b);", Hazards: []*godwitv1.PlannedHazard{
								{Code: "H001", Detail: "CREATE INDEX without CONCURRENTLY blocks writes on t"},
							}},
						},
					},
					{
						Version: 20260901130000, Name: "drop_legacy", AlreadyApplied: true, Effect: "- column legacy",
						Statements: []*godwitv1.PlannedStatement{{Sql: "ALTER TABLE t DROP COLUMN legacy;"}},
					},
					{
						Version: 20260901140000, Name: "backfill", Applied: true, Note: "DML is not inspectable",
						Statements: []*godwitv1.PlannedStatement{{Sql: "UPDATE t SET a = 1;"}},
					},
				},
			},
		},
		sums: []*godwitv1.TargetSummary{
			{
				Name: "app", Provider: "static", SearchPath: "app,public", LockTimeout: "3s", StatementTimeout: "1m",
				RequirePlan: true, KeepOld: false, AttentionRuns: 1, UnresolvedDrift: true, ReadyPlans: 1, AppliedCount: 2,
				LastRun: &godwitv1.Run{Id: "r-wait-0001", Target: "app", State: godwitv1.RunState_RUN_STATE_AWAITING_CONTRACT},
			},
			{Name: "billing", Provider: "vault", KeepOld: true, AttentionRuns: 1},
		},
		status: &godwitv1.GetTargetStatusResponse{
			Target: "app", Provider: "static", LockTimeout: "3s", StatementTimeout: "1m", SearchPath: "app,public",
			Applied: []*godwitv1.AppliedMigration{
				{Version: 20260901120000, Name: "add_index", Checksum: "9f1e2d3c4b5a", AppliedAt: at(3 * time.Hour)},
				{Version: 20260901130000, Name: "drop_legacy", Checksum: "deadbeefcafe", AppliedAt: at(2 * time.Hour), ChecksumMismatch: true},
				{Name: "views", Repeatable: true, Checksum: "abc123def456", AppliedAt: at(time.Hour)},
			},
			DriftBaseline: &godwitv1.DriftBaseline{TakenAt: at(time.Hour), RunId: "r-ok-000001", UnresolvedDrift: true},
			ReadyPlans:    1,
		},
		events: []*godwitv1.DriftEvent{
			{Id: 8, Target: "app", Diff: "column extra added", DetectedAt: at(time.Minute)},
			{Id: 7, Target: "app", Diff: "index gone", DetectedAt: at(time.Hour), ResolvedAt: at(30 * time.Minute)},
			{Id: 6, Target: "billing", Diff: "old", DetectedAt: at(24 * time.Hour), ResolvedAt: at(23 * time.Hour)},
		},
		drift: &godwitv1.CheckDriftResponse{Drifted: true, Diff: "column extra added"},
	}
}

func TestIndex(t *testing.T) {
	t.Parallel()
	s := fixture()
	h := newUI(s, Config{Replica: "godwit-0"})

	want(t, do(h, http.MethodGet, "/ui/", nil), http.StatusOK,
		"<title>godwit</title>", "godwit-0", "Needs you", "r-bad-00", "needs attention", "awaiting contract", "lock timeout",
		"oldest 2 days ago", "since 5 min ago", "Confirm rollout", "Resume", "revert of r-ok-000", "by ci", "1m30s", "300ms", "3.0s", "24h0m", "1h0m",
		"No sign-in configured", `class="cnt">2<`, `class="dot bad"`)
	if s.actor != "ui:anonymous" {
		t.Fatalf("actor = %q", s.actor)
	}

	partial := do(h, http.MethodGet, "/ui/?target=billing", nil, "HX-Request", "true")
	want(t, partial, http.StatusOK, `id="runs"`, "Runs on billing", "r-bad-00")
	if b := partial.Body.String(); strings.Contains(b, "<title>") || strings.Contains(b, "r-ok-000") {
		t.Fatalf("partial must be filtered and bare:\n%s", b)
	}

	want(t, do(newUI(&stub{}, Config{}), http.MethodGet, "/ui/", nil), http.StatusOK, "No runs yet", "Nothing needs you")

	s.err = connect.NewError(connect.CodeUnavailable, errBoom)
	want(t, do(h, http.MethodGet, "/ui/", nil), http.StatusBadGateway, "boom", "Back to runs")

	want(t, do(newUI(&runsFail{stub: fixture()}, Config{}), http.MethodGet, "/ui/", nil), http.StatusBadGateway, "runs down")
}

type runsFail struct {
	*stub
}

func (r *runsFail) ListRuns(context.Context, *connect.Request[godwitv1.ListRunsRequest]) (*connect.Response[godwitv1.ListRunsResponse], error) {
	return nil, connect.NewError(connect.CodeUnavailable, errors.New("runs down"))
}

func TestRunPage(t *testing.T) {
	t.Parallel()
	s := fixture()
	h := newUI(s, Config{})

	want(t, do(h, http.MethodGet, "/ui/runs/r-bad-00001", nil), http.StatusOK, "Resume run", "Park", "lock timeout",
		"Resumed", "by <b>ci</b>", "Parked", "waiting on dba", "Contract confirmed", "The journal on",
		"Re-attached by a repeated request", "state=queued plan=p-plan-0001")
	want(t, do(h, http.MethodGet, "/ui/runs/r-fail-0001", nil), http.StatusOK, "Resume run", "Park", "not_there", "Failed", "p-gone-0", "pruned")
	want(t, do(h, http.MethodGet, "/ui/runs/r-wait-0001", nil), http.StatusOK, "Confirm rollout", "expand-contract · expand", "waiting for contract confirmation")
	plan := do(h, http.MethodGet, "/ui/runs/r-ok-000001", nil)
	want(t, plan, http.StatusOK, "Revert", "Succeeded", "source github", "via github",
		"p-plan-0", "bound", "replayed on a scratch database", "H002 acknowledged",
		"CREATE INDEX without CONCURRENTLY", "CREATE INDEX CONCURRENTLY i1", "2 statements",
		"already applied by hand", "recorded without executing, 1 statement skipped", "- column legacy",
		"in history", "DML is not inspectable")
	if n := strings.Count(plan.Body.String(), "<b>H001</b>"); n != 1 {
		t.Fatalf("hazard H001 listed %d times", n)
	}
	want(t, do(h, http.MethodGet, "/ui/runs/r-rev-00001", nil), http.StatusOK, "/ui/runs/r-ok-000001", "Created as revert of r-ok-000", "Reverted", "Nothing to do.")
	want(t, do(h, http.MethodGet, "/ui/runs/r-run-00001", nil), http.StatusOK, "Running, attempt 1", "until it settles")
	want(t, do(h, http.MethodGet, "/ui/runs/r-queue-001", nil), http.StatusOK, "waiting for a replica")
	want(t, do(h, http.MethodGet, "/ui/runs/r-retry-001", nil), http.StatusOK,
		"2 transient failures retried by the scheduler", "Backing off, next attempt at 2026-09-02 12:00:20Z", "· 2 retried")

	s.audit = nil
	want(t, do(h, http.MethodGet, "/ui/runs/r-bad-00001", nil), http.StatusOK, "Stopped after 3 attempts")

	rec := do(h, http.MethodGet, "/ui/runs/r-ok-000001", nil, "HX-Request", "true")
	want(t, rec, http.StatusOK, `id="run"`)
	if strings.Contains(rec.Body.String(), "<title>") {
		t.Fatal("partial must not carry the layout")
	}
	want(t, do(h, http.MethodGet, "/ui/runs/nope", nil), http.StatusNotFound, "no such run")
	s.err = errBoom
	want(t, do(h, http.MethodGet, "/ui/runs/r-ok-000001", nil), http.StatusBadGateway, "boom")

	h = newUI(&auditFail{stub: fixture()}, Config{})
	want(t, do(h, http.MethodGet, "/ui/runs/r-ok-000001", nil), http.StatusBadGateway, "audit down")

	h = newUI(&planFail{stub: fixture()}, Config{})
	want(t, do(h, http.MethodGet, "/ui/runs/r-ok-000001", nil), http.StatusBadGateway, "plans down")
}

type auditFail struct {
	*stub
}

func (a *auditFail) ListAudit(context.Context, *connect.Request[godwitv1.ListAuditRequest]) (*connect.Response[godwitv1.ListAuditResponse], error) {
	return nil, errors.New("audit down")
}

type planFail struct {
	*stub
}

func (p *planFail) GetPlan(context.Context, *connect.Request[godwitv1.GetPlanRequest]) (*connect.Response[godwitv1.GetPlanResponse], error) {
	return nil, connect.NewError(connect.CodeUnavailable, errors.New("plans down"))
}

func TestRunActions(t *testing.T) {
	t.Parallel()
	s := fixture()
	h := newUI(s, Config{User: "sam", Password: "pw"})

	cases := []struct{ action, form, call, location string }{
		{"resume", "", "ResumeRun:r-bad-00001", "/ui/runs/r-bad-00001"},
		{"park", "reason=on hold", "ParkRun:on hold", "/ui/runs/r-bad-00001"},
		{"confirm", "", "ConfirmRollout:r-bad-00001", "/ui/runs/r-bad-00001"},
		{"revert", "ack=H001, H002,", "RevertRun:H001+H002", "/ui/runs/r-down"},
	}
	for _, c := range cases {
		form, _ := url.ParseQuery(c.form)
		rec := do(h, http.MethodPost, "/ui/runs/r-bad-00001/"+c.action, form, "Authorization", basic("sam", "pw"))
		redirect(t, rec, c.location)
		if s.calls[len(s.calls)-1] != c.call || s.actor != "ui:sam" {
			t.Fatalf("%s: calls = %v actor = %q", c.action, s.calls, s.actor)
		}
	}
	if rec := do(h, http.MethodPost, "/ui/runs/x/explode", nil, "Authorization", basic("sam", "pw")); rec.Code != http.StatusNotFound {
		t.Fatalf("code = %d", rec.Code)
	}

	s.err = connect.NewError(connect.CodeFailedPrecondition, errBoom)
	want(t, do(h, http.MethodPost, "/ui/runs/r-bad-00001/resume", nil, "Authorization", basic("sam", "pw")), http.StatusBadGateway, "boom", "Back to runs")
	want(t, do(h, http.MethodPost, "/ui/runs/r-bad-00001/revert", nil, "Authorization", basic("sam", "pw")), http.StatusBadGateway, "boom")
}

func TestDrift(t *testing.T) {
	t.Parallel()
	s := fixture()
	h := newUI(s, Config{})

	want(t, do(h, http.MethodGet, "/ui/drift", nil), http.StatusOK, "app drifted from its baseline", "Detected 1 min ago",
		"column extra added", "index gone", "resolved", `class="on">app <span class="cnt">drifted`, "billing <span class=\"cnt\">clean", "Accept as baseline")
	want(t, do(h, http.MethodGet, "/ui/drift?target=billing&checked=clean", nil), http.StatusOK, "billing matches its baseline", "Checked just now", "old")
	want(t, do(h, http.MethodGet, "/ui/drift?target=app&checked=drifted", nil), http.StatusOK, "confirmed by the check you just ran")
	want(t, do(h, http.MethodGet, "/ui/drift?target=ghost", nil), http.StatusOK, "No open drift event", "No drift recorded")
	want(t, do(newUI(&stub{}, Config{}), http.MethodGet, "/ui/drift", nil), http.StatusOK, "No targets yet")

	redirect(t, do(h, http.MethodPost, "/ui/drift/app/check", nil), "/ui/drift?target=app&checked=drifted")
	s.drift = &godwitv1.CheckDriftResponse{}
	redirect(t, do(h, http.MethodPost, "/ui/drift/app/check", nil), "/ui/drift?target=app&checked=clean")
	redirect(t, do(h, http.MethodPost, "/ui/drift/app/accept", nil), "/ui/drift?target=app")
	if s.calls[len(s.calls)-1] != "AcceptBaseline:app" {
		t.Fatalf("calls = %v", s.calls)
	}
	if rec := do(h, http.MethodPost, "/ui/drift/app/explode", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("code = %d", rec.Code)
	}

	s.err = connect.NewError(connect.CodeInternal, errBoom)
	want(t, do(h, http.MethodGet, "/ui/drift", nil), http.StatusBadGateway, "boom")
	want(t, do(h, http.MethodPost, "/ui/drift/app/check", nil), http.StatusBadGateway, "boom")
	want(t, do(h, http.MethodPost, "/ui/drift/app/accept", nil), http.StatusBadGateway, "boom")

	h = newUI(&eventsFail{stub: fixture()}, Config{})
	want(t, do(h, http.MethodGet, "/ui/drift", nil), http.StatusBadGateway, "events down")
}

type eventsFail struct {
	*stub
}

func (e *eventsFail) ListDriftEvents(context.Context, *connect.Request[godwitv1.ListDriftEventsRequest]) (*connect.Response[godwitv1.ListDriftEventsResponse], error) {
	return nil, connect.NewError(connect.CodeUnavailable, errors.New("events down"))
}

func TestBasicAuth(t *testing.T) {
	t.Parallel()
	s := fixture()
	h := newUI(s, Config{User: "sam", Password: "pw"})

	for _, hdr := range []string{"", basic("sam", "wrong"), basic("eve", "pw"), "Bearer x"} {
		rec := do(h, http.MethodGet, "/ui/", nil, "Authorization", hdr)
		if rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") != `Basic realm="godwit"` {
			t.Fatalf("%q: code = %d headers = %v", hdr, rec.Code, rec.Header())
		}
	}
	if s.calls != nil {
		t.Fatalf("rejected requests must not reach the service: %v", s.calls)
	}

	want(t, do(h, http.MethodGet, "/ui/", nil, "Authorization", basic("sam", "pw")), http.StatusOK, "Signed in as", "sam")
	if s.actor != "ui:sam" {
		t.Fatalf("actor = %q", s.actor)
	}
}

var uiTokens = []api.Token{
	{Name: "viewer", Scope: api.ScopeRead, Secret: "s-read"},
	{Name: "ci", Scope: api.ScopePipeline, Secret: "s-pipe"},
	{Name: "sam", Scope: api.ScopeOperator, Secret: "s-op"},
}

const noAction = `method="post"`

func TestTokenSignIn(t *testing.T) {
	t.Parallel()
	s := fixture()
	h := newUI(s, Config{Tokens: uiTokens})

	rec := do(h, http.MethodGet, "/ui/", nil, "Authorization", basic("whoever", "s-read"))
	want(t, rec, http.StatusOK, "Signed in as", "viewer", `class="chip">read<`)
	absent(t, rec, noAction)
	if s.actor != "ui:viewer" {
		t.Fatalf("actor = %q", s.actor)
	}

	rec = do(h, http.MethodGet, "/ui/", nil, "Authorization", basic("whoever", "s-op"))
	want(t, rec, http.StatusOK, "sam", `class="chip">operator<`, "/ui/runs/r-wait-0001/confirm", "/ui/runs/r-bad-00001/resume")
	if s.actor != "ui:sam" {
		t.Fatalf("actor = %q", s.actor)
	}

	for _, hdr := range []string{"", basic("viewer", "s-nope"), "Bearer s-read"} {
		rec := do(h, http.MethodGet, "/ui/", nil, "Authorization", hdr)
		if rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") != `Basic realm="godwit"` {
			t.Fatalf("%q: code = %d headers = %v", hdr, rec.Code, rec.Header())
		}
	}
}

func TestScopeGatesActions(t *testing.T) {
	t.Parallel()
	s := fixture()
	h := newUI(s, Config{Tokens: uiTokens})

	read := []string{"Authorization", basic("x", "s-read")}
	for _, path := range []string{"/ui/runs/r-bad-00001", "/ui/runs/r-wait-0001", "/ui/runs/r-ok-000001", "/ui/drift"} {
		rec := do(h, http.MethodGet, path, nil, read...)
		want(t, rec, http.StatusOK, "Actions on this page need a wider scope")
		absent(t, rec, noAction)
	}
	absent(t, do(h, http.MethodGet, "/ui/runs/r-run-00001", nil, read...),
		"Actions on this page need a wider scope")

	pipe := []string{"Authorization", basic("x", "s-pipe")}
	want(t, do(h, http.MethodGet, "/ui/runs/r-wait-0001", nil, pipe...), http.StatusOK, "/ui/runs/r-wait-0001/confirm")
	want(t, do(h, http.MethodGet, "/ui/runs/r-ok-000001", nil, pipe...), http.StatusOK, "/ui/runs/r-ok-000001/revert")
	want(t, do(h, http.MethodGet, "/ui/runs/r-bad-00001", nil, pipe...), http.StatusOK, "Actions on this page need a wider scope")
	want(t, do(h, http.MethodGet, "/ui/drift", nil, pipe...), http.StatusOK, "Actions on this page need a wider scope")

	op := []string{"Authorization", basic("x", "s-op")}
	want(t, do(h, http.MethodGet, "/ui/runs/r-bad-00001", nil, op...), http.StatusOK,
		"/ui/runs/r-bad-00001/resume", "/ui/runs/r-bad-00001/park")
	want(t, do(h, http.MethodGet, "/ui/drift", nil, op...), http.StatusOK,
		"/ui/drift/app/check", "/ui/drift/app/accept")
}

func TestScopeRefusesAction(t *testing.T) {
	t.Parallel()
	s := fixture()
	h := newUI(s, Config{Tokens: uiTokens})

	rec := do(h, http.MethodPost, "/ui/runs/r-bad-00001/resume", nil, "Authorization", basic("x", "s-read"))
	want(t, rec, http.StatusForbidden, "ResumeRun requires scope operator; token ui:viewer has scope read")
	rec = do(h, http.MethodPost, "/ui/drift/app/accept", nil, "Authorization", basic("x", "s-pipe"))
	want(t, rec, http.StatusForbidden, "AcceptBaseline requires scope operator; token ui:ci has scope pipeline")
	rec = do(h, http.MethodPost, "/ui/drift/app/check", nil, "Authorization", basic("x", "s-pipe"))
	want(t, rec, http.StatusForbidden, "CheckDrift requires scope operator")
	rec = do(h, http.MethodPost, "/ui/runs/r-ok-000001/revert", nil, "Authorization", basic("x", "s-read"))
	want(t, rec, http.StatusForbidden, "RevertRun requires scope pipeline")
	if s.calls != nil {
		t.Fatalf("refused actions must not reach the service: %v", s.calls)
	}

	redirect(t, do(h, http.MethodPost, "/ui/runs/r-wait-0001/confirm", nil, "Authorization", basic("x", "s-pipe")), "/ui/runs/r-wait-0001")
	if s.actor != "ui:ci" {
		t.Fatalf("actor = %q", s.actor)
	}
}

func TestSharedIdentityScope(t *testing.T) {
	t.Parallel()
	s := fixture()
	h := newUI(s, Config{Tokens: uiTokens, User: "sam", Password: "pw", Scope: api.ScopeRead})

	rec := do(h, http.MethodGet, "/ui/", nil, "Authorization", basic("sam", "pw"))
	want(t, rec, http.StatusOK, "Signed in as", "sam", `class="chip">read<`)
	absent(t, rec, noAction)
	if s.actor != "ui:sam" {
		t.Fatalf("actor = %q", s.actor)
	}
	if rec := do(h, http.MethodGet, "/ui/", nil, "Authorization", basic("eve", "pw")); rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d", rec.Code)
	}

	half := newUI(s, Config{Tokens: uiTokens, User: "sam"})
	if rec := do(half, http.MethodGet, "/ui/", nil, "Authorization", basic("sam", "")); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a password-less shared identity must not sign in: code = %d", rec.Code)
	}

	admin := newUI(fixture(), Config{User: "sam", Password: "pw", Scope: api.ScopeAdmin})
	want(t, do(admin, http.MethodGet, "/ui/runs/r-bad-00001", nil, "Authorization", basic("sam", "pw")),
		http.StatusOK, "/ui/runs/r-bad-00001/resume")

	open := newUI(fixture(), Config{AnonymousScope: api.ScopeRead})
	rec = do(open, http.MethodGet, "/ui/", nil)
	want(t, rec, http.StatusOK, "No sign-in configured")
	absent(t, rec, noAction)
}

// A service with no way to sign anyone in must not hand a visitor the rights of the identity it would
// have authenticated: New defaults the anonymous principal to read, whatever Scope says.
func TestAnonymousScopeIsRead(t *testing.T) {
	t.Parallel()

	h := New(fixture(), Config{Scope: api.ScopeAdmin})
	h.now = func() time.Time { return now }
	rec := do(h, http.MethodGet, "/ui/", nil)
	want(t, rec, http.StatusOK, `class="chip">read<`)
	absent(t, rec, noAction)
	if p, ok := h.principal(httptest.NewRequest(http.MethodGet, "/ui/", nil)); !ok || p.Scope != api.ScopeRead {
		t.Fatalf("anonymous principal = %+v %v, want read", p, ok)
	}
}

func TestRenderError(t *testing.T) {
	t.Parallel()
	h := newUI(fixture(), Config{})
	rec := httptest.NewRecorder()
	h.render(rec, http.StatusOK, "missing.html", page{})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code = %d", rec.Code)
	}
}

func TestFuncs(t *testing.T) {
	t.Parallel()
	fns := newUI(fixture(), Config{}).funcs()
	for name, fn := range map[string]func(*timestamppb.Timestamp) string{
		"clock": fns["clock"].(func(*timestamppb.Timestamp) string),
		"stamp": fns["stamp"].(func(*timestamppb.Timestamp) string),
		"ago":   fns["ago"].(func(*timestamppb.Timestamp) string),
	} {
		if got := fn(nil); got != "—" {
			t.Fatalf("%s(nil) = %q", name, got)
		}
	}
	if got := fns["took"].(func(*godwitv1.Run) string)(&godwitv1.Run{}); got != "—" {
		t.Fatalf("took = %q", got)
	}
	if got := fns["short"].(func(string) string)("abc"); got != "abc" {
		t.Fatalf("short = %q", got)
	}
	for d, s := range map[time.Duration]string{
		30 * time.Second: "just now", time.Minute: "1 min ago", 3 * time.Hour: "3 hours ago", time.Hour: "1 hour ago", 72 * time.Hour: "3 days ago",
	} {
		if got := ago(d); got != s {
			t.Fatalf("ago(%s) = %q, want %q", d, got, s)
		}
	}
	if got := splitCSV(" , "); got != nil {
		t.Fatalf("splitCSV = %v", got)
	}
}

func TestMark(t *testing.T) {
	t.Parallel()
	rec := do(newUI(fixture(), Config{}), http.MethodGet, "/ui/mark.svg", nil)
	want(t, rec, http.StatusOK, "<svg", `aria-label="godwit mark"`)
	if ct := rec.Header().Get("Content-Type"); ct != "image/svg+xml" {
		t.Fatalf("content-type = %q", ct)
	}
}

func TestTargetsList(t *testing.T) {
	t.Parallel()
	s := fixture()
	h := newUI(s, Config{Replica: "godwit-0"})

	rec := do(h, http.MethodGet, "/ui/targets", nil)
	want(t, rec, http.StatusOK, "Registered", `href="/ui/targets/app"`, `href="/ui/targets/billing"`,
		"static", "vault", "app,public", "lock 3s · statement 1m · require_plan · keep_old=false",
		"drifted", "clean", "role default path", "never migrated", "r-wait-0", `class="cnt">2<`)
	if s.calls[0] != "ListTargets" {
		t.Fatalf("the rail must come from ListTargets: %v", s.calls)
	}

	want(t, do(newUI(&stub{}, Config{}), http.MethodGet, "/ui/targets", nil), http.StatusOK, "No targets yet")

	s.err = connect.NewError(connect.CodeUnavailable, errBoom)
	want(t, do(h, http.MethodGet, "/ui/targets", nil), http.StatusBadGateway, "boom")
}

func TestTargetPage(t *testing.T) {
	t.Parallel()
	s := fixture()
	s.plans["p-ready-0001"] = &godwitv1.Plan{
		Id: "p-ready-0001", Target: "app", State: "ready", Rollout: "expand-contract", Validated: true,
		CreatedBy: "ci", Source: "github", CreatedAt: at(10 * time.Minute),
		Migrations: []*godwitv1.PlannedMigration{
			{Version: 20260901120000, Name: "add_index", Applied: true},
			{Version: 20260901140000, Name: "backfill", Statements: []*godwitv1.PlannedStatement{{Sql: "UPDATE t SET a = 1;"}}},
			{Name: "views", Repeatable: true, Statements: []*godwitv1.PlannedStatement{{Sql: "CREATE VIEW v AS SELECT 1;"}}},
		},
	}
	h := newUI(s, Config{})

	rec := do(h, http.MethodGet, "/ui/targets/app", nil)
	want(t, rec, http.StatusOK,
		"20260901120000_add_index", "20260901130000_drop_legacy", "checksum mismatch", "R__views", "repeatable · unchanged",
		"20260901140000_backfill", "1 statement", `href="/ui/plans/p-ready-0001"`, "newest <b>ready</b> plan",
		"app drifted from its baseline", "column extra added", "Accept as baseline", "Check drift now",
		"app,public", "require_plan", "keep_old", "Ready plans", `href="/ui/plans?target=app"`, "9f1e2d3c")
	if strings.Contains(rec.Body.String(), "p-plan-000") {
		t.Fatalf("only ready plans belong on the page:\n%s", rec.Body.String())
	}

	s.status = &godwitv1.GetTargetStatusResponse{Target: "billing", Provider: "vault"}
	s.events = nil
	want(t, do(h, http.MethodGet, "/ui/targets/billing", nil), http.StatusOK, "Never migrated",
		"billing matches its baseline", "Nothing pending", "No stored plan is bindable", "role default", "none")

	want(t, do(h, http.MethodGet, "/ui/targets/ghost", nil), http.StatusNotFound, "target ghost is not registered")

	want(t, do(newUI(&stub{err: errBoom}, Config{}), http.MethodGet, "/ui/targets/app", nil), http.StatusBadGateway, "boom")
	want(t, do(newUI(&statusFail{stub: fixture()}, Config{}), http.MethodGet, "/ui/targets/app", nil), http.StatusBadGateway, "status down")
	want(t, do(newUI(&listPlansFail{stub: fixture()}, Config{}), http.MethodGet, "/ui/targets/app", nil), http.StatusBadGateway, "plan list down")
	want(t, do(newUI(&eventsFail{stub: fixture()}, Config{}), http.MethodGet, "/ui/targets/app", nil), http.StatusBadGateway, "events down")
}

type statusFail struct {
	*stub
}

func (s *statusFail) GetTargetStatus(context.Context, *connect.Request[godwitv1.GetTargetStatusRequest]) (*connect.Response[godwitv1.GetTargetStatusResponse], error) {
	return nil, connect.NewError(connect.CodeUnavailable, errors.New("status down"))
}

func TestTargetPageScope(t *testing.T) {
	t.Parallel()
	h := newUI(fixture(), Config{Tokens: uiTokens})

	rec := do(h, http.MethodGet, "/ui/targets/app", nil, "Authorization", basic("x", "s-read"))
	want(t, rec, http.StatusOK, "Actions on this page need a wider scope")
	absent(t, rec, noAction)
	want(t, do(h, http.MethodGet, "/ui/targets/app", nil, "Authorization", basic("x", "s-op")), http.StatusOK,
		"/ui/drift/app/check", "/ui/drift/app/accept")
}
