package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/gen/godwit/v1/godwitv1connect"
	"github.com/SamuelMolling/godwit/internal/controlplane"
)

func getPlan(t *testing.T, client godwitv1connect.GodwitServiceClient, id string) *godwitv1.Plan {
	t.Helper()
	res, err := client.GetPlan(context.Background(), connect.NewRequest(&godwitv1.GetPlanRequest{PlanId: id}))
	if err != nil {
		t.Fatal(err)
	}

	return res.Msg.Plan
}

func readyPlans(t *testing.T, client godwitv1connect.GodwitServiceClient) int32 {
	t.Helper()
	res, err := client.GetTargetStatus(context.Background(), connect.NewRequest(&godwitv1.GetTargetStatusRequest{Target: "app"}))
	if err != nil {
		t.Fatal(err)
	}

	return res.Msg.ReadyPlans
}

func TestGetPlan_ShowsBoundRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := startService(t, newDatabase(t, "st"), "r1", []string{"ci:read:r", "ops:admin:a"})
	client, reader := newClient(base, "a"), newClient(base, "r")
	registerTarget(t, client, newDatabase(t, "tg"))
	all := orderedFiles()
	all[4].Body = "ALTER TABLE t DROP COLUMN a;"

	if _, err := reader.GetPlan(ctx, connect.NewRequest(&godwitv1.GetPlanRequest{PlanId: "00000000-0000-0000-0000-000000000000"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("missing plan: %v", err)
	}
	stored := persistPlan(t, client, all, []string{"H003"})
	if n := readyPlans(t, reader); n != 1 {
		t.Fatalf("ready plans = %d", n)
	}
	p := getPlan(t, reader, stored.PlanId)
	if p.Id != stored.PlanId || p.Target != "app" || p.Key != stored.PlanKey || p.Rollout != "direct" || p.State != controlplane.PlanReady ||
		p.RunId != "" || p.SupersededBy != "" || len(p.Migrations) != 3 || p.CreatedBy != "ops" || p.Source != "repo@sha" ||
		p.AcknowledgedHazards[0] != "H003" || p.Observed.HistoryHash != stored.Observed.HistoryHash || p.CreatedAt == nil {
		t.Fatalf("plan = %+v", p)
	}
	var hazards []string
	for _, st := range p.Migrations[2].Statements {
		for _, h := range st.Hazards {
			hazards = append(hazards, h.Code)
		}
	}
	if strings.Join(hazards, ",") != "H003" {
		t.Fatalf("hazards = %v", hazards)
	}

	created, err := createRun(t, client, all, false)
	if err != nil || created.PlanId != stored.PlanId {
		t.Fatalf("created = %+v, err = %v", created, err)
	}
	waitRun(t, client, created.RunId)
	if p := getPlan(t, reader, stored.PlanId); p.State != controlplane.PlanBound || p.RunId != created.RunId {
		t.Fatalf("bound plan = %+v", p)
	}
	if n := readyPlans(t, reader); n != 0 {
		t.Fatalf("ready plans = %d", n)
	}
}

func TestListPlans_NewestFirst(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client := newClient(startService(t, newDatabase(t, "st"), "r1", nil), "")
	registerTarget(t, client, newDatabase(t, "tg"))
	all := orderedFiles()

	first := persistPlan(t, client, all[:2], nil)
	second := persistPlan(t, client, all[:4], nil)
	res, err := client.ListPlans(ctx, connect.NewRequest(&godwitv1.ListPlansRequest{Target: "app"}))
	if err != nil || len(res.Msg.Plans) != 2 || res.Msg.Plans[0].Id != second.PlanId || res.Msg.Plans[1].Id != first.PlanId {
		t.Fatalf("plans = %+v, err = %v", res, err)
	}
	if n := readyPlans(t, client); n != 2 {
		t.Fatalf("ready plans = %d", n)
	}
	res, err = client.ListPlans(ctx, connect.NewRequest(&godwitv1.ListPlansRequest{Target: "app", Limit: 1}))
	if err != nil || len(res.Msg.Plans) != 1 || res.Msg.Plans[0].Id != second.PlanId {
		t.Fatalf("limited = %+v, err = %v", res, err)
	}
	res, err = client.ListPlans(ctx, connect.NewRequest(&godwitv1.ListPlansRequest{Target: "other"}))
	if err != nil || len(res.Msg.Plans) != 0 {
		t.Fatalf("other = %+v, err = %v", res, err)
	}
	if _, err := client.ListPlans(ctx, connect.NewRequest(&godwitv1.ListPlansRequest{})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("no target: %v", err)
	}
}

func TestCreateRun_ByPlanIdWithoutFiles(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client := newClient(startService(t, newDatabase(t, "st"), "r1", nil), "")
	registerTarget(t, client, newDatabase(t, "tg"))
	all := orderedFiles()
	all[4].Body = "ALTER TABLE t DROP COLUMN a;"

	stored := persistPlan(t, client, all, []string{"H003"})
	_, err := client.CreateRun(ctx, connect.NewRequest(&godwitv1.CreateRunRequest{PlanId: stored.PlanId, Files: all[:2], SkipValidation: true}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "files do not match plan "+stored.PlanId) {
		t.Fatalf("other files: %v", err)
	}
	created, err := client.CreateRun(ctx, connect.NewRequest(&godwitv1.CreateRunRequest{PlanId: stored.PlanId, SkipValidation: true}))
	if err != nil || created.Msg.PlanId != stored.PlanId {
		t.Fatalf("created = %+v, err = %v", created, err)
	}
	run := waitRun(t, client, created.Msg.RunId)
	if run.PlanId != stored.PlanId || run.Rollout != "direct" {
		t.Fatalf("run = %+v", run)
	}
	if p := getPlan(t, client, stored.PlanId); p.State != controlplane.PlanBound || p.RunId != run.Id {
		t.Fatalf("plan = %+v", p)
	}
	_, err = client.CreateRun(ctx, connect.NewRequest(&godwitv1.CreateRunRequest{PlanId: stored.PlanId, SkipValidation: true}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "is bound to run "+run.Id) {
		t.Fatalf("rebind: %v", err)
	}
	_, err = client.CreateRun(ctx, connect.NewRequest(&godwitv1.CreateRunRequest{PlanId: stored.PlanId, Target: "other"}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "belongs to target app") {
		t.Fatalf("other target: %v", err)
	}
}

func TestCreateRun_ByPlanIdStale(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client := newClient(startService(t, newDatabase(t, "st"), "r1", nil), "")
	targetDSN := newDatabase(t, "tg")
	registerTarget(t, client, targetDSN)
	all := orderedFiles()
	runToSuccess(t, client, all[:2], nil)

	stored := persistPlan(t, client, all[:4], nil)
	execStore(t, targetDSN, "CREATE TABLE rogue (id int)")
	_, err := client.CreateRun(ctx, connect.NewRequest(&godwitv1.CreateRunRequest{PlanId: stored.PlanId, SkipValidation: true}))
	stale, _ := planDetail(t, err)
	if stale.PlanId != stored.PlanId || stale.Reason != controlplane.StaleSchema || !strings.Contains(stale.SchemaDiff, "rogue") {
		t.Fatalf("stale = %+v", stale)
	}
	if p := getPlan(t, client, stored.PlanId); p.State != controlplane.PlanReady {
		t.Fatalf("plan after refusal = %+v", p)
	}

	if _, err := client.AcceptBaseline(ctx, connect.NewRequest(&godwitv1.AcceptBaselineRequest{Target: "app"})); err != nil {
		t.Fatal(err)
	}
	created, err := client.CreateRun(ctx, connect.NewRequest(&godwitv1.CreateRunRequest{PlanId: stored.PlanId, SkipValidation: true}))
	if err != nil || created.Msg.PlanId == "" || created.Msg.PlanId == stored.PlanId {
		t.Fatalf("after accept = %+v, err = %v", created, err)
	}
	waitRun(t, client, created.Msg.RunId)
	old := getPlan(t, client, stored.PlanId)
	if old.State != controlplane.PlanSuperseded || old.SupersededBy != created.Msg.PlanId {
		t.Fatalf("old plan = %+v", old)
	}
	_, err = client.CreateRun(ctx, connect.NewRequest(&godwitv1.CreateRunRequest{PlanId: stored.PlanId, SkipValidation: true}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "was superseded by "+created.Msg.PlanId) {
		t.Fatalf("superseded: %v", err)
	}
}

func TestSweep_KeepsReady(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client := newClient(startServiceCfg(t, Config{
		Listen: "127.0.0.1:0", StoreDSN: newDatabase(t, "st"), MasterKey: testKey, Holder: "r1",
		Scheduler: controlplane.Config{Interval: 50 * time.Millisecond}, Log: testLog,
		DriftInterval: 50 * time.Millisecond, PlanRetention: time.Millisecond,
	}), "")
	registerTarget(t, client, newDatabase(t, "tg"))
	all := orderedFiles()

	bound := persistPlan(t, client, all[:2], nil)
	created, err := createRun(t, client, all[:2], false)
	if err != nil || created.PlanId != bound.PlanId {
		t.Fatalf("created = %+v, err = %v", created, err)
	}
	waitRun(t, client, created.RunId)
	ready := persistPlan(t, client, all[:4], nil)

	deadline := time.Now().Add(10 * time.Second)
	for {
		res, err := client.ListPlans(ctx, connect.NewRequest(&godwitv1.ListPlansRequest{Target: "app"}))
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Msg.Plans) == 1 && res.Msg.Plans[0].Id == ready.PlanId {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("plans never swept: %+v", res.Msg.Plans)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := client.GetPlan(ctx, connect.NewRequest(&godwitv1.GetPlanRequest{PlanId: bound.PlanId})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("swept plan: %v", err)
	}
	run, err := client.GetRun(ctx, connect.NewRequest(&godwitv1.GetRunRequest{RunId: created.RunId}))
	if err != nil || run.Msg.Run.PlanId != "" {
		t.Fatalf("run = %+v, err = %v", run, err)
	}
	audit, err := client.ListAudit(ctx, connect.NewRequest(&godwitv1.ListAuditRequest{RunId: created.RunId}))
	if err != nil || len(audit.Msg.Entries) == 0 || !strings.Contains(audit.Msg.Entries[0].Detail, "plan="+bound.PlanId) {
		t.Fatalf("audit = %+v, err = %v", audit, err)
	}
	if n := readyPlans(t, client); n != 1 {
		t.Fatalf("ready plans = %d", n)
	}
}

func TestGetPlanAndRun_IncludeFiles(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storeDSN := newDatabase(t, "st")
	client := newClient(startService(t, storeDSN, "r1", nil), "")
	registerTarget(t, client, newDatabase(t, "tg"))
	all := orderedFiles()[:2]

	stored := persistPlan(t, client, all, nil)
	plan, err := client.GetPlan(ctx, connect.NewRequest(&godwitv1.GetPlanRequest{PlanId: stored.PlanId}))
	if err != nil || len(plan.Msg.Files) != 0 {
		t.Fatalf("files must be opt-in: %+v, err = %v", plan.Msg.Files, err)
	}
	plan, err = client.GetPlan(ctx, connect.NewRequest(&godwitv1.GetPlanRequest{PlanId: stored.PlanId, IncludeFiles: true}))
	if err != nil || len(plan.Msg.Files) != 2 ||
		plan.Msg.Files[0].Name != all[1].Name || plan.Msg.Files[1].Body != all[0].Body {
		t.Fatalf("plan files = %+v, err = %v", plan.Msg.Files, err)
	}

	created, err := createRun(t, client, all, false)
	if err != nil {
		t.Fatal(err)
	}
	waitRun(t, client, created.RunId)
	run, err := client.GetRun(ctx, connect.NewRequest(&godwitv1.GetRunRequest{RunId: created.RunId, IncludeFiles: true}))
	if err != nil || len(run.Msg.Files) != 2 || run.Msg.Files[0].Name != all[1].Name {
		t.Fatalf("run files = %+v, err = %v", run.Msg.Files, err)
	}

	execStore(t, storeDSN, "DROP TABLE cp_plan_files")
	if _, err := client.GetPlan(ctx, connect.NewRequest(&godwitv1.GetPlanRequest{PlanId: stored.PlanId, IncludeFiles: true})); connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("missing plan files table: %v", err)
	}
	execStore(t, storeDSN, "DROP TABLE cp_run_files")
	if _, err := client.GetRun(ctx, connect.NewRequest(&godwitv1.GetRunRequest{RunId: created.RunId, IncludeFiles: true})); connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("missing run files table: %v", err)
	}
}
