package cli

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
)

func (s *stubService) GetPlan(_ context.Context, req *connect.Request[godwitv1.GetPlanRequest]) (*connect.Response[godwitv1.GetPlanResponse], error) {
	s.planGot = req.Msg.PlanId
	if err := s.record(req.Header()); err != nil {
		return nil, err
	}

	return connect.NewResponse(&godwitv1.GetPlanResponse{Plan: s.stored}), nil
}

func (s *stubService) ListPlans(_ context.Context, req *connect.Request[godwitv1.ListPlansRequest]) (*connect.Response[godwitv1.ListPlansResponse], error) {
	s.plansListed = req.Msg
	if err := s.record(req.Header()); err != nil {
		return nil, err
	}

	return connect.NewResponse(&godwitv1.ListPlansResponse{Plans: s.plans}), nil
}

func storedPlanProto() *godwitv1.Plan {
	return &godwitv1.Plan{
		Id: "p1", Target: "app", Key: "k1", Rollout: "expand-contract", State: "bound", RunId: "r1", Validated: true,
		Observed: &godwitv1.PlanObservation{
			HistoryHash: "h1", SchemaFingerprint: "f1", AppliedCount: 1, NewestApplied: 20260901120000,
			At: timestamppb.New(time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)),
		},
		Drift: "+ table public.orders", AcknowledgedHazards: []string{"H003"}, AllowOutOfOrder: true,
		CreatedBy: "ci", Source: "repo@sha:db", CreatedAt: timestamppb.New(time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)),
		Migrations: dryRunStub().plan.Migrations,
	}
}

func TestPlans(t *testing.T) {
	t.Parallel()
	p2 := storedPlanProto()
	p2.Id, p2.State, p2.RunId, p2.SupersededBy, p2.Source, p2.Validated = "p2", "superseded", "", "p1", "", false
	stub := &stubService{plans: []*godwitv1.Plan{storedPlanProto(), p2}}
	url := startStub(t, stub)

	code, out, errOut := runCLI("plans", "--server", url, "--target", "app", "--limit", "5")
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
	want := "ID  STATE             ROLLOUT          PENDING  VALIDATED  BY  SOURCE       CREATED               RUN\n" +
		"p1  bound             expand-contract  1        true       ci  repo@sha:db  2026-09-01T12:00:00Z  r1\n" +
		"p2  superseded by p1  expand-contract  1        false      ci               2026-09-01T12:00:00Z  \n"
	if out != want || stub.plansListed.Target != "app" || stub.plansListed.Limit != 5 {
		t.Fatalf("out = %q, request = %v", out, stub.plansListed)
	}

	code, out, _ = runCLI("plans", "--server", url, "--target", "app", "--json")
	if code != 0 || len(decodeJSON(t, out)["plans"].([]any)) != 2 {
		t.Fatalf("code = %d, out = %q", code, out)
	}

	stub.err = connect.NewError(connect.CodeInvalidArgument, errors.New("target is required"))
	if code, _, errOut := runCLI("plans", "--server", url); code != 1 || errOut != "godwit: target is required\n" {
		t.Fatalf("code = %d, stderr = %q", code, errOut)
	}
}

func TestPlanShow(t *testing.T) {
	t.Parallel()
	stub := &stubService{stored: storedPlanProto()}
	url := startStub(t, stub)

	code, out, errOut := runCLI("plan", "show", "p1", "--server", url)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
	want := "plan p1 on app (rollout expand-contract, validated on a scratch database)\n" +
		"key: k1\n" +
		"state: bound (run r1)\n" +
		"by: ci at 2026-09-01T12:00:00Z, source repo@sha:db, acked H003, out-of-order allowed\n" +
		"observed: 1 applied, newest 20260901120000, history h1, schema f1, at 2026-09-01T10:00:00Z\n" +
		"drift since baseline:\n" +
		"  + table public.orders\n" +
		"20260901120000_users (up): 2 statement(s) [expand, applied]\n"
	if !strings.HasPrefix(out, want) || !strings.Contains(out, "hazard H003: DROP COLUMN is destructive") || stub.planGot != "p1" {
		t.Fatalf("out = %q, want prefix %q", out, want)
	}

	stub.stored.State, stub.stored.RunId, stub.stored.SupersededBy = "superseded", "", "p2"
	stub.stored.Source, stub.stored.AcknowledgedHazards, stub.stored.AllowOutOfOrder = "", nil, false
	_, out, _ = runCLI("plan", "show", "p1", "--server", url, "--format", "markdown")
	for _, want := range []string{"## godwit plan p1\n", "\nstate: superseded (by p2)\n", "\nby: ci at 2026-09-01T12:00:00Z\n", "```diff\n+ table public.orders\n```"} {
		if !strings.Contains(out, want) {
			t.Fatalf("markdown lacks %q:\n%s", want, out)
		}
	}

	_, out, _ = runCLI("plan", "show", "p1", "--server", url, "--format", "json")
	var got dryRunJSON
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if got.PlanID != "p1" || got.Stored == nil || got.Stored.State != "superseded" || got.Stored.SupersededBy != "p2" || got.Stored.CreatedBy != "ci" {
		t.Fatalf("json = %+v", got)
	}

	_, out, _ = runCLI("plan", "show", "p1", "--server", url, "--json")
	if m := decodeJSON(t, out); m["plan"].(map[string]any)["id"] != "p1" {
		t.Fatalf("raw json = %s", out)
	}

	if code, _, errOut := runCLI("plan", "show", "p1", "--server", url, "--format", "yaml"); code != 1 || !strings.Contains(errOut, `unknown format "yaml"`) {
		t.Fatalf("code = %d, stderr = %q", code, errOut)
	}
	stub.err = connect.NewError(connect.CodeNotFound, errors.New("plan p1: not found"))
	if code, _, errOut := runCLI("plan", "show", "p1", "--server", url); code != 1 || errOut != "godwit: plan p1: not found\n" {
		t.Fatalf("code = %d, stderr = %q", code, errOut)
	}
}

func TestMigrate_ByPlan(t *testing.T) {
	t.Parallel()
	stub := &stubService{planID: "p1", events: []*godwitv1.Run{run("r1", godwitv1.RunState_RUN_STATE_SUCCEEDED, 1)}}
	url := startStub(t, stub)

	code, out, errOut := runCLI("migrate", "--server", url, "--plan", "p1")
	if code != 0 || out != "plan p1: bound\nrun r1: succeeded (attempt 1)\n" {
		t.Fatalf("code = %d, out = %q, stderr = %q", code, out, errOut)
	}
	c := stub.created
	if c.PlanId != "p1" || c.Target != "" || c.Rollout != "" || len(c.Files) != 0 {
		t.Fatalf("request = %v", c)
	}

	code, _, errOut = runCLI("migrate", "--server", url, "--plan", "p1", "--target", "app", "--rollout", "expand-contract", "--dir", goodMigs(t))
	c = stub.created
	if code != 0 || c.PlanId != "p1" || c.Target != "app" || c.Rollout != "expand-contract" || len(c.Files) != 2 {
		t.Fatalf("code = %d, stderr = %q, request = %v", code, errOut, c)
	}

	if code, _, errOut := runCLI("migrate", "--server", url, "--plan", "p1", "--dry-run"); code != 1 || errOut != "godwit: --plan cannot be combined with --dry-run\n" {
		t.Fatalf("code = %d, stderr = %q", code, errOut)
	}
	if code, _, errOut := runCLI("migrate", "--server", url, "--dir", goodMigs(t)); code != 1 || !strings.Contains(errOut, "--target") {
		t.Fatalf("code = %d, stderr = %q", code, errOut)
	}
}
