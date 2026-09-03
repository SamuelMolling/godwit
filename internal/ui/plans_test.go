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
	s.plans["p-plan-0001"].CreatedAt = at(2 * time.Hour)
	s.plans["p-plan-0001"].Key = "k-bound-00000001"
	s.plans["p-plan-0001"].RunId = "r-ok-000001"
	s.plans["p-plan-0001"].Rollout = "direct"
	s.plans["p-plan-0001"].CreatedBy = "ci"
	s.plans["p-plan-0001"].Source = "github"
	s.plans["p-plan-0001"].Drift = "- index t_a_idx"
	s.plans["p-plan-0001"].Observed = &godwitv1.PlanObservation{
		HistoryHash: "h1234567890", SchemaFingerprint: "f1234567890",
		AppliedCount: 4, NewestApplied: 20260901110000, SearchPath: "app, public", At: at(2 * time.Hour),
	}
	s.plans["p-ready-001"] = &godwitv1.Plan{
		Id: "p-ready-001", Target: "app", Key: "k-ready-00000001", State: "ready", Rollout: "expand-contract",
		CreatedBy: "sam", CreatedAt: at(time.Minute),
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
	s.plans["p-super-001"] = &godwitv1.Plan{
		Id: "p-super-001", Target: "billing", Key: "k-super-00000001", State: "superseded", Rollout: "direct",
		Validated: true, SupersededBy: "p-ready-001", CreatedAt: at(3 * time.Hour),
	}

	return s
}

func TestPlansList(t *testing.T) {
	t.Parallel()
	s := planFixture()
	h := newUI(s, Config{Replica: "godwit-0"})

	rec := do(h, http.MethodGet, "/ui/plans", nil)
	want(t, rec, http.StatusOK, "<title>godwit</title>", "All plans", `class="n">3<`,
		"/ui/plans/p-ready-001", "/ui/plans/p-plan-0001", "/ui/plans/p-super-001",
		"key k-ready-", "by sam", "by ci", "github", "expand-contract", "not validated",
		"/ui/runs/r-ok-000001", "not bound", "1 pending",
		`>all <span class="cnt">3<`, `>ready <span class="cnt">1<`, `>bound <span class="cnt">1<`, `>superseded <span class="cnt">1<`)
	body := rec.Body.String()
	if i, j := strings.Index(body, "p-ready-001"), strings.Index(body, "p-plan-0001"); i > j {
		t.Fatalf("newest first: ready at %d, bound at %d", i, j)
	}
	if !strings.Contains(body, `href="/ui/plans?state=ready"`) {
		t.Fatalf("state filter link missing:\n%s", body)
	}

	filtered := do(h, http.MethodGet, "/ui/plans?state=bound", nil)
	want(t, filtered, http.StatusOK, "p-plan-0001", `href="/ui/plans?state=ready"`)
	if strings.Contains(filtered.Body.String(), "/ui/plans/p-ready-001") {
		t.Fatal("state=bound must not list the ready plan")
	}

	byTarget := do(h, http.MethodGet, "/ui/plans?target=billing", nil)
	want(t, byTarget, http.StatusOK, "Plans on billing", "p-super-001", "superseded",
		`href="/ui/plans?state=ready&amp;target=billing"`, `href="/ui/plans?target=billing"`)
	if strings.Contains(byTarget.Body.String(), "p-plan-0001") {
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

	bound := do(h, http.MethodGet, "/ui/plans/p-plan-0001", nil)
	want(t, bound, http.StatusOK, "p-plan-0001", "bound", "Open run r-ok-000",
		"20260901120000", "add_index", "2 statements",
		"CREATE INDEX i1 ON t (a);", "CREATE INDEX i2 ON t (b);",
		"CREATE INDEX CONCURRENTLY i1 ON t (a);",
		"already applied by hand", "records it without executing", "- column legacy",
		"in history", "DML is not inspectable",
		"H002", "replayed on a scratch database", "k-bound-",
		"Observation", "h1234567", "f1234567", "app, public", "newest 20260901110000",
		"Changes outside migrations", "- index t_a_idx")
	if n := strings.Count(bound.Body.String(), "<b>H001</b>"); n != 2 {
		t.Fatalf("the detail lists every hazard occurrence, got %d", n)
	}

	expanded := do(h, http.MethodGet, "/ui/plans/p-ready-001", nil)
	want(t, expanded, http.StatusOK, "ready", "expanded from a directive",
		"-- godwit: change-type users.age bigint", "leaves public.users.age_old for rollback",
		"expand phase", "contract phase", "held until the rollout is confirmed",
		`batch over &#34;id&#34; (int), 5000 rows per transaction, pausing 100ms`,
		"no-tx", "not bound", "not validated", "5 statements")
	if i, j := strings.Index(expanded.Body.String(), "expand phase"), strings.Index(expanded.Body.String(), "contract phase"); i > j {
		t.Fatal("the expand phase renders before the contract phase")
	}

	want(t, do(h, http.MethodGet, "/ui/plans/p-super-001", nil), http.StatusOK,
		"superseded", "Superseded by", "/ui/plans/p-ready-001", "Nothing pending")

	want(t, do(h, http.MethodGet, "/ui/plans/p-gone-0001", nil), http.StatusOK, "This plan was pruned", "p-gone-0001")

	s.err = connect.NewError(connect.CodeUnavailable, errBoom)
	want(t, do(h, http.MethodGet, "/ui/plans/p-plan-0001", nil), http.StatusBadGateway, "boom")

	want(t, do(newUI(&planFail{stub: planFixture()}, Config{}), http.MethodGet, "/ui/plans/p-plan-0001", nil),
		http.StatusBadGateway, "plans down")
}

func TestPlansScope(t *testing.T) {
	t.Parallel()
	h := newUI(planFixture(), Config{Tokens: uiTokens})

	for _, path := range []string{"/ui/plans", "/ui/plans/p-plan-0001", "/ui/plans/p-ready-001"} {
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
