package ui

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/internal/engine"
)

func (s *stub) ListPlans(ctx context.Context, req *connect.Request[godwitv1.ListPlansRequest]) (*connect.Response[godwitv1.ListPlansResponse], error) {
	if err := s.call(ctx, "ListPlans:"+req.Msg.Target); err != nil {
		return nil, err
	}
	out := []*godwitv1.Plan{}
	for _, p := range s.plans {
		if p.Target == req.Msg.Target {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.AsTime().After(out[j].CreatedAt.AsTime()) })

	return connect.NewResponse(&godwitv1.ListPlansResponse{Plans: out}), nil
}

type listPlansFail struct {
	*stub
}

func (l *listPlansFail) ListPlans(context.Context, *connect.Request[godwitv1.ListPlansRequest]) (*connect.Response[godwitv1.ListPlansResponse], error) {
	return nil, connect.NewError(connect.CodeUnavailable, errors.New("plan list down"))
}

func planFixture() *stub {
	s := fixture()
	s.plans["6513270e-269e-4d37-b2a7-4de452e6b438"].CreatedAt = at(2 * time.Hour)
	s.plans["6513270e-269e-4d37-b2a7-4de452e6b438"].Key = "fef563411147ed6b45a16f9d133e9437d96200ad7ae3f9393f4afa7ec98b5b03"
	s.plans["6513270e-269e-4d37-b2a7-4de452e6b438"].RunId = "r-ok-000001"
	s.plans["6513270e-269e-4d37-b2a7-4de452e6b438"].Rollout = "direct"
	s.plans["6513270e-269e-4d37-b2a7-4de452e6b438"].CreatedBy = "github:firefliesai/backend-go-tests:samuelmolling"
	s.plans["6513270e-269e-4d37-b2a7-4de452e6b438"].Source = "github.com/firefliesai/backend-go-tests@ddeba9f27acd0623ea8f512a2047f4a39f2e3422:db/migrations"
	s.plans["6513270e-269e-4d37-b2a7-4de452e6b438"].Drift = "- index t_a_idx"
	s.plans["6513270e-269e-4d37-b2a7-4de452e6b438"].Observed = &godwitv1.PlanObservation{
		HistoryHash: "h1234567890", SchemaFingerprint: "f1234567890",
		AppliedCount: 4, NewestApplied: 20260901110000, SearchPath: "app, public", At: at(2 * time.Hour),
	}
	s.plans["d23f0824-128b-4f33-8c5c-7fd0a6a3a450"] = &godwitv1.Plan{
		Id: "d23f0824-128b-4f33-8c5c-7fd0a6a3a450", Target: "app", State: "ready", Rollout: "expand-contract",
		Key:       "5595a1ea758dc8d189fd5bf86c84c963bcc1f1518ac6524b44df81684c4f6970",
		CreatedBy: "sam", CreatedAt: at(72 * time.Hour),
		Migrations: []*godwitv1.PlannedMigration{
			{
				Version: 20260902120000, Name: "users_age_bigint", Phase: "expand", Expanded: true,
				Directives: []string{"-- godwit: change-type users.age bigint"},
				Notes:      []string{"leaves public.users.age_old for rollback"},
				Statements: []*godwitv1.PlannedStatement{
					{Sql: "ALTER TABLE public.users ADD COLUMN age_new bigint;"},
					{Sql: "CREATE INDEX CONCURRENTLY i3 ON public.users (age_new);", NoTx: true},
					{Sql: "UPDATE public.users SET age_new = age::bigint;", Batch: &godwitv1.PlannedBatch{
						Key: `"id"`, Kind: "int", Size: 5000, Pause: "100ms",
					}},
					{Sql: "ALTER TABLE public.users RENAME COLUMN age TO age_old;", Phase: "contract"},
					{Sql: "ALTER TABLE public.users RENAME COLUMN age_new TO age;", Phase: "contract"},
				},
			},
		},
	}
	s.plans["9531985d-5d9d-49f8-9818-e811892f902b"] = &godwitv1.Plan{
		Id: "9531985d-5d9d-49f8-9818-e811892f902b", Target: "billing", State: "superseded", Rollout: "direct",
		Key:       "daeb3afa64b62153a8a49089436a0c52044b10483140d15c143f9af7294c43a0",
		Validated: true, SupersededBy: "d23f0824-128b-4f33-8c5c-7fd0a6a3a450", CreatedBy: "ci", Source: "db/migrations",
		CreatedAt: at(3 * time.Hour),
	}

	return s
}

func TestPlansList(t *testing.T) {
	t.Parallel()
	s := planFixture()
	h := newUI(s, Config{Replica: "godwit-0"})

	rec := do(h, http.MethodGet, "/ui/plans", nil)
	want(t, rec, http.StatusOK, "<title>Godwit</title>", "All plans", `class="n">3<`,
		"/ui/plans/d23f0824-128b-4f33-8c5c-7fd0a6a3a450", "/ui/plans/6513270e-269e-4d37-b2a7-4de452e6b438", "/ui/plans/9531985d-5d9d-49f8-9818-e811892f902b",
		"by sam", "by github:firefliesai/backend-go-tests:samuelmolling", "expand-contract", "not validated",
		"waiting for an apply", "/ui/runs/r-ok-000001", `replaced by <a href="/ui/plans/d23f0824-128b-4f33-8c5c-7fd0a6a3a450"`, "1 pending",
		"nothing applies a plan on its own", "never the pull request",
		`title="fef563411147ed6b45a16f9d133e9437d96200ad7ae3f9393f4afa7ec98b5b03">fef56341<`,
		`title="github.com/firefliesai/backend-go-tests@ddeba9f27acd0623ea8f512a2047f4a39f2e3422:db/migrations"`,
		`<a href="https://github.com/firefliesai/backend-go-tests/commit/ddeba9f27acd0623ea8f512a2047f4a39f2e3422">firefliesai/backend-go-tests@ddeba9f</a>`,
		">db/migrations<", "no source recorded",
		`>all <span class="cnt">3<`, `>ready <span class="cnt">1<`, `>bound <span class="cnt">1<`, `>superseded <span class="cnt">1<`,
		`title="2026-09-02 10:00:00Z">2 hours ago`, "3 days ago")
	body := rec.Body.String()
	bound, super, ready := strings.Index(body, "fef56341"), strings.Index(body, "daeb3afa"), strings.Index(body, "5595a1ea")
	if bound >= super || super >= ready {
		t.Fatalf("newest first: bound at %d, superseded at %d, ready at %d", bound, super, ready)
	}
	if !strings.Contains(body, `href="/ui/plans?state=ready"`) {
		t.Fatalf("state filter link missing:\n%s", body)
	}

	filtered := do(h, http.MethodGet, "/ui/plans?state=bound", nil)
	want(t, filtered, http.StatusOK, "6513270e-269e-4d37-b2a7-4de452e6b438", `href="/ui/plans?state=ready"`)
	if strings.Contains(filtered.Body.String(), "/ui/plans/d23f0824-128b-4f33-8c5c-7fd0a6a3a450") {
		t.Fatal("state=bound must not list the ready plan")
	}

	byTarget := do(h, http.MethodGet, "/ui/plans?target=billing", nil)
	want(t, byTarget, http.StatusOK, "Plans on billing", "9531985d-5d9d-49f8-9818-e811892f902b", "superseded", ">db/migrations<",
		`href="/ui/plans?state=ready&amp;target=billing"`, `href="/ui/plans?target=billing"`)
	if strings.Contains(byTarget.Body.String(), "6513270e-269e-4d37-b2a7-4de452e6b438") {
		t.Fatal("target=billing must not list app's plans")
	}
	if last := s.calls[len(s.calls)-1]; last != "ListPlans:billing" {
		t.Fatalf("calls = %v", s.calls)
	}

	want(t, do(h, http.MethodGet, "/ui/plans?state=ghost", nil), http.StatusOK, "No plans in state ghost")
	want(t, do(newUI(&stub{}, Config{}), http.MethodGet, "/ui/plans", nil), http.StatusOK, "No plans")

	s.err = connect.NewError(connect.CodeUnavailable, errBoom)
	want(t, do(h, http.MethodGet, "/ui/plans", nil), http.StatusBadGateway, "boom")

	want(t, do(newUI(&listPlansFail{stub: planFixture()}, Config{}), http.MethodGet, "/ui/plans", nil),
		http.StatusBadGateway, "plan list down")
}

func TestPlanDetail(t *testing.T) {
	t.Parallel()
	s := planFixture()
	h := newUI(s, Config{})

	bound := do(h, http.MethodGet, "/ui/plans/6513270e-269e-4d37-b2a7-4de452e6b438", nil)
	want(t, bound, http.StatusOK, "6513270e-269e-4d37-b2a7-4de452e6b438", "bound", "Open run r-ok-000",
		"20260901120000", "add_index", "2 statements",
		"CREATE INDEX i1 ON t (a);", "CREATE INDEX i2 ON t (b);",
		"CREATE INDEX CONCURRENTLY i1 ON t (a);",
		"already applied by hand", "records it without executing", "- column public.widgets.legacy_code",
		"in history", engine.OpaqueDML, "nothing applies a plan on its own",
		"H002", "replayed on a scratch database",
		"fef563411147ed6b45a16f9d133e9437d96200ad7ae3f9393f4afa7ec98b5b03",
		`<a href="https://github.com/firefliesai/backend-go-tests/commit/ddeba9f27acd0623ea8f512a2047f4a39f2e3422">firefliesai/backend-go-tests@ddeba9f</a>`,
		`<span class="faint">db/migrations</span>`,
		"Observation", "h1234567", "f1234567", "app, public", "newest 20260901110000",
		"Changes outside migrations", "- index t_a_idx")
	if n := strings.Count(bound.Body.String(), "<b>H001</b>"); n != 2 {
		t.Fatalf("the detail lists every hazard occurrence, got %d", n)
	}

	expanded := do(h, http.MethodGet, "/ui/plans/d23f0824-128b-4f33-8c5c-7fd0a6a3a450", nil)
	want(t, expanded, http.StatusOK, "ready", "expanded from a directive",
		"-- godwit: change-type users.age bigint", "leaves public.users.age_old for rollback",
		"expand phase", "contract phase", "held until the rollout is confirmed",
		`batch over &#34;id&#34; (int), 5000 rows per transaction, pausing 100ms`,
		"no-tx", "not bound", "not validated", "5 statements", "<dt>Source</dt><dd>—</dd>")
	if i, j := strings.Index(expanded.Body.String(), "expand phase"), strings.Index(expanded.Body.String(), "contract phase"); i > j {
		t.Fatal("the expand phase renders before the contract phase")
	}

	want(t, do(h, http.MethodGet, "/ui/plans/9531985d-5d9d-49f8-9818-e811892f902b", nil), http.StatusOK,
		"superseded", "Superseded by", "/ui/plans/d23f0824-128b-4f33-8c5c-7fd0a6a3a450", "Nothing pending",
		"<dt>Source</dt><dd>db/migrations</dd>")

	want(t, do(h, http.MethodGet, "/ui/plans/36f675cc-81e7-4ef5-a8e2-5d940ed90475", nil), http.StatusOK, "This plan was pruned", "36f675cc-81e7-4ef5-a8e2-5d940ed90475")

	s.err = connect.NewError(connect.CodeUnavailable, errBoom)
	want(t, do(h, http.MethodGet, "/ui/plans/6513270e-269e-4d37-b2a7-4de452e6b438", nil), http.StatusBadGateway, "boom")

	want(t, do(newUI(&planFail{stub: planFixture()}, Config{}), http.MethodGet, "/ui/plans/6513270e-269e-4d37-b2a7-4de452e6b438", nil),
		http.StatusBadGateway, "plans down")
}

func TestPlansScope(t *testing.T) {
	t.Parallel()
	h := newUI(planFixture(), Config{Tokens: uiTokens})

	for _, path := range []string{"/ui/plans", "/ui/plans/6513270e-269e-4d37-b2a7-4de452e6b438", "/ui/plans/d23f0824-128b-4f33-8c5c-7fd0a6a3a450"} {
		rec := do(h, http.MethodGet, path, nil, "Authorization", basic("x", "s-read"))
		want(t, rec, http.StatusOK, `class="chip">read<`)
		absent(t, rec, noAction)
	}
	want(t, do(h, http.MethodGet, "/ui/plans", nil, "Authorization", basic("x", "s-op")),
		http.StatusOK, `href="/ui/plans"`, "All plans")
}

func TestBatchLine(t *testing.T) {
	t.Parallel()
	if got := batchLine(nil); got != "" {
		t.Fatalf("batchLine(nil) = %q", got)
	}
	got := batchLine(&godwitv1.PlannedBatch{Key: "id", Kind: "uuid", Size: 100})
	if got != "batch over id (uuid), 100 rows per transaction" {
		t.Fatalf("batchLine = %q", got)
	}
}

func TestAssertLine(t *testing.T) {
	t.Parallel()
	if got := assertLine(nil); got != "" {
		t.Fatalf("assertLine(nil) = %q", got)
	}
	if got := assertLine(&godwitv1.PlannedAssert{Op: "=", Kind: "int", Value: "0"}); got != "assert = 0" {
		t.Fatalf("assertLine = %q", got)
	}
}
