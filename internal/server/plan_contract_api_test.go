package server

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/gen/godwit/v1/godwitv1connect"
	"github.com/SamuelMolling/godwit/internal/controlplane"
)

func persistPlan(t *testing.T, client godwitv1connect.GodwitServiceClient, files []*godwitv1.MigrationFile, ack []string) *godwitv1.PlanRunResponse {
	t.Helper()
	res, err := client.PlanRun(context.Background(), connect.NewRequest(&godwitv1.PlanRunRequest{
		Target: "app", Files: files, AcknowledgeHazards: ack, Persist: true, SkipValidation: true, Source: "repo@sha",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if res.Msg.PlanId == "" || res.Msg.PlanKey == "" || res.Msg.Observed == nil {
		t.Fatalf("stored plan = %+v", res.Msg)
	}

	return res.Msg
}

func createRun(t *testing.T, client godwitv1connect.GodwitServiceClient, files []*godwitv1.MigrationFile, allowOutOfOrder bool) (*godwitv1.CreateRunResponse, error) {
	t.Helper()
	res, err := client.CreateRun(context.Background(), connect.NewRequest(&godwitv1.CreateRunRequest{
		Target: "app", Files: files, SkipValidation: true, AllowOutOfOrder: allowOutOfOrder,
	}))
	if err != nil {
		return nil, err
	}

	return res.Msg, nil
}

func waitRun(t *testing.T, client godwitv1connect.GodwitServiceClient, id string) *godwitv1.Run {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		r, err := client.GetRun(context.Background(), connect.NewRequest(&godwitv1.GetRunRequest{RunId: id}))
		if err != nil {
			t.Fatal(err)
		}
		if r.Msg.Run.State == godwitv1.RunState_RUN_STATE_SUCCEEDED {
			return r.Msg.Run
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("run %s never succeeded", id)

	return nil
}

func planDetail(t *testing.T, err error) (*godwitv1.PlanStale, *godwitv1.PlanRequired) {
	t.Helper()
	var cerr *connect.Error
	if !errors.As(err, &cerr) || cerr.Code() != connect.CodeFailedPrecondition {
		t.Fatalf("err = %v, want failed_precondition", err)
	}
	for _, d := range cerr.Details() {
		msg, err := d.Value()
		if err != nil {
			t.Fatal(err)
		}
		switch m := msg.(type) {
		case *godwitv1.PlanStale:
			return m, nil
		case *godwitv1.PlanRequired:
			return nil, m
		}
	}
	t.Fatalf("no plan detail on %v", err)

	return nil, nil
}

func auditActions(t *testing.T, client godwitv1connect.GodwitServiceClient) []string {
	t.Helper()
	res, err := client.ListAudit(context.Background(), connect.NewRequest(&godwitv1.ListAuditRequest{Target: "app"}))
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(res.Msg.Entries))
	for _, e := range res.Msg.Entries {
		out = append(out, e.Action)
	}

	return out
}

func TestPlanRun_PersistStoresKeyAndObservation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := startService(t, newDatabase(t, "st"), "r1", []string{"ci:read:r", "ops:admin:a"})
	client := newClient(base, "a")
	targetDSN := newDatabase(t, "tg")
	registerTarget(t, client, targetDSN)
	all := orderedFiles()

	first := persistPlan(t, newClient(base, "r"), all, nil)
	if first.Observed.AppliedCount != 0 || first.Observed.NewestApplied != 0 || first.Observed.HistoryHash == "" ||
		first.Observed.SchemaFingerprint == "" || first.Observed.At == nil || first.Drift != "" || len(first.Migrations) != 3 {
		t.Fatalf("first plan = %+v", first)
	}
	again := persistPlan(t, client, all, nil)
	if again.PlanKey != first.PlanKey || again.PlanId == first.PlanId {
		t.Fatalf("re-plan = %+v, first = %+v", again, first)
	}
	if list, err := client.ListRuns(ctx, connect.NewRequest(&godwitv1.ListRunsRequest{Target: "app"})); err != nil || len(list.Msg.Runs) != 0 {
		t.Fatalf("runs after plan = %+v, err = %v", list, err)
	}

	runToSuccess(t, client, all[:2], nil)
	execStore(t, targetDSN, "CREATE TABLE rogue (id int)")
	after := persistPlan(t, client, all[:4], nil)
	if after.PlanKey == first.PlanKey || after.Observed.AppliedCount != 1 || after.Observed.NewestApplied != 20260901120001 ||
		!strings.Contains(after.Drift, "rogue") || !after.Migrations[0].Applied || after.Migrations[1].Applied {
		t.Fatalf("plan after run = %+v", after)
	}

	edited := []*godwitv1.MigrationFile{{Name: all[0].Name, Body: "CREATE TABLE t (id bigint);"}, all[1]}
	_, err := client.PlanRun(ctx, connect.NewRequest(&godwitv1.PlanRunRequest{Target: "app", Files: edited, Persist: true, SkipValidation: true}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "20260901120001_t applied with different content") {
		t.Fatalf("edited applied file: %v", err)
	}
	if _, err := client.PlanRun(ctx, connect.NewRequest(&godwitv1.PlanRunRequest{Target: "ghost", Files: all, Persist: true})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("unknown target: %v", err)
	}
	if actions := auditActions(t, client); !strings.Contains(strings.Join(actions, ","), controlplane.AuditPlanCreate) {
		t.Fatalf("audit = %v", actions)
	}
}

func TestCreateRun_BindsFreshPlan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client := newClient(startService(t, newDatabase(t, "st"), "r1", nil), "")
	registerTarget(t, client, newDatabase(t, "tg"))
	all := orderedFiles()
	all[4].Body = "ALTER TABLE t DROP COLUMN a;"

	plan := persistPlan(t, client, all, []string{"H003"})
	created, err := createRun(t, client, all, false)
	if err != nil || created.PlanId != plan.PlanId {
		t.Fatalf("created = %+v, err = %v", created, err)
	}
	run := waitRun(t, client, created.RunId)
	if run.PlanId != plan.PlanId {
		t.Fatalf("run = %+v", run)
	}
	again, err := client.CreateRun(ctx, connect.NewRequest(&godwitv1.CreateRunRequest{Target: "app", Files: all, AcknowledgeHazards: []string{"H003"}}))
	if err != nil || !again.Msg.Reattached || again.Msg.RunId != created.RunId || again.Msg.PlanId != plan.PlanId {
		t.Fatalf("again = %+v, err = %v", again, err)
	}
	res, err := client.ListAudit(ctx, connect.NewRequest(&godwitv1.ListAuditRequest{RunId: created.RunId}))
	if err != nil || len(res.Msg.Entries) == 0 || !strings.Contains(res.Msg.Entries[0].Detail, "plan="+plan.PlanId) {
		t.Fatalf("audit = %+v, err = %v", res, err)
	}
}

func TestCreateRun_NoPlanImplicit(t *testing.T) {
	t.Parallel()
	client := newClient(startService(t, newDatabase(t, "st"), "r1", nil), "")
	registerTarget(t, client, newDatabase(t, "tg"))

	created, err := createRun(t, client, orderedFiles()[:2], false)
	if err != nil || created.PlanId != "" {
		t.Fatalf("created = %+v, err = %v", created, err)
	}
	waitRun(t, client, created.RunId)

	edited := []*godwitv1.MigrationFile{{Name: "20260901120001_t.up.sql", Body: "CREATE TABLE t (id bigint);"}, orderedFiles()[1]}
	_, err = createRun(t, client, edited, false)
	stale, _ := planDetail(t, err)
	if stale.Reason != controlplane.StaleContent || stale.PlanId != "" || !strings.Contains(err.Error(), "migration set on app cannot bind") {
		t.Fatalf("edited applied file: %+v, %v", stale, err)
	}
}

func TestCreateRun_StaleSchema(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client := newClient(startService(t, newDatabase(t, "st"), "r1", nil), "")
	targetDSN := newDatabase(t, "tg")
	registerTarget(t, client, targetDSN)
	all := orderedFiles()
	runToSuccess(t, client, all[:2], nil)

	plan := persistPlan(t, client, all[:4], nil)
	execStore(t, targetDSN, "CREATE TABLE rogue (id int)")
	_, err := createRun(t, client, all[:4], false)
	stale, _ := planDetail(t, err)
	if stale.PlanId != plan.PlanId || stale.Reason != controlplane.StaleSchema || !strings.Contains(stale.SchemaDiff, "rogue") ||
		len(stale.HistoryAdded)+len(stale.HistoryRemoved) != 0 || !strings.Contains(stale.Hint, "godwit drift accept app") {
		t.Fatalf("stale = %+v", stale)
	}
	for _, want := range []string{
		"is stale (planned ", " by anonymous, repo@sha)", "reason : schema", "schema : + column godwit.rogue.id",
		"(1 changes not made by any run since the plan)", "files  : unchanged (key " + plan.PlanKey[:8], "fix: push to the pull request (re-plan)",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("message lacks %q:\n%v", want, err)
		}
	}

	if _, err := client.AcceptBaseline(ctx, connect.NewRequest(&godwitv1.AcceptBaselineRequest{Target: "app"})); err != nil {
		t.Fatal(err)
	}
	created, err := createRun(t, client, all[:4], false)
	if err != nil || created.PlanId == "" || created.PlanId == plan.PlanId {
		t.Fatalf("after accept = %+v, err = %v", created, err)
	}
	waitRun(t, client, created.RunId)
	if actions := strings.Join(auditActions(t, client), ","); !strings.Contains(actions, controlplane.AuditPlanSupersede) {
		t.Fatalf("audit = %s", actions)
	}
}

func TestCreateRun_ExplainedHistory(t *testing.T) {
	t.Parallel()
	client := newClient(startService(t, newDatabase(t, "st"), "r1", nil), "")
	registerTarget(t, client, newDatabase(t, "tg"))
	all := orderedFiles()
	runToSuccess(t, client, all[:2], nil)

	mine := append(append([]*godwitv1.MigrationFile{}, all[:2]...), all[4:]...)
	plan := persistPlan(t, client, mine, nil)
	runToSuccess(t, client, all[:4], nil)

	created, err := createRun(t, client, mine, false)
	if err != nil || created.PlanId == "" || created.PlanId == plan.PlanId {
		t.Fatalf("created = %+v, err = %v", created, err)
	}
	waitRun(t, client, created.RunId)
	if again, err := createRun(t, client, mine, false); err != nil || !again.Reattached || again.RunId != created.RunId {
		t.Fatalf("again = %+v, err = %v", again, err)
	}
}

func TestCreateRun_UnexplainedHistory(t *testing.T) {
	t.Parallel()
	client := newClient(startService(t, newDatabase(t, "st"), "r1", nil), "")
	targetDSN := newDatabase(t, "tg")
	registerTarget(t, client, targetDSN)
	all := orderedFiles()
	runToSuccess(t, client, all[:4], nil)

	plan := persistPlan(t, client, all, nil)
	execStore(t, targetDSN, "INSERT INTO godwit.migrations (version, name, checksum) VALUES (20260901120004, 'x', 'x')")
	_, err := createRun(t, client, all, false)
	stale, _ := planDetail(t, err)
	if stale.PlanId != plan.PlanId || stale.Reason != controlplane.StaleHistory || len(stale.HistoryAdded) != 1 ||
		stale.HistoryAdded[0] != "20260901120004_x" || !strings.Contains(err.Error(), "by no run (unexplained)") ||
		!strings.Contains(err.Error(), "after checking who changed godwit.migrations on app") {
		t.Fatalf("stale = %+v, err = %v", stale, err)
	}

	execStore(t, targetDSN, "DELETE FROM godwit.migrations WHERE version IN (20260901120002, 20260901120004)")
	mine := append(append([]*godwitv1.MigrationFile{}, all[:2]...), all[4:]...)
	_, err = createRun(t, client, mine, false)
	stale, _ = planDetail(t, err)
	if stale.PlanId != plan.PlanId || stale.Reason != controlplane.StaleHistory || len(stale.HistoryRemoved) != 1 ||
		stale.HistoryRemoved[0] != "20260901120002_a" || !strings.Contains(err.Error(), "removed from history") {
		t.Fatalf("stale = %+v, err = %v", stale, err)
	}
}

func TestCreateRun_OrderAfterHistory(t *testing.T) {
	t.Parallel()
	client := newClient(startService(t, newDatabase(t, "st"), "r1", nil), "")
	registerTarget(t, client, newDatabase(t, "tg"))
	all := orderedFiles()
	runToSuccess(t, client, all[:2], nil)

	plan := persistPlan(t, client, all[:4], nil)
	other := append(append([]*godwitv1.MigrationFile{}, all[:2]...), all[4:]...)
	runToSuccess(t, client, other, nil)

	_, err := createRun(t, client, all[:4], false)
	stale, _ := planDetail(t, err)
	if stale.PlanId != plan.PlanId || stale.Reason != controlplane.StaleOrder || len(stale.HistoryAdded) != 1 ||
		!strings.Contains(err.Error(), "(explained)") || !strings.Contains(stale.Hint, "allow_out_of_order") {
		t.Fatalf("stale = %+v, err = %v", stale, err)
	}
	created, err := createRun(t, client, all[:4], true)
	if err != nil || created.PlanId == "" || created.PlanId == plan.PlanId {
		t.Fatalf("created = %+v, err = %v", created, err)
	}
	waitRun(t, client, created.RunId)
}

func TestCreateRun_RequirePlanRefuses(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client := newClient(startService(t, newDatabase(t, "st"), "r1", nil), "")
	if _, err := client.RegisterTarget(ctx, connect.NewRequest(&godwitv1.RegisterTargetRequest{
		Name: "app", Provider: "static", Dsn: newDatabase(t, "tg"), RequirePlan: true,
	})); err != nil {
		t.Fatal(err)
	}
	all := orderedFiles()

	_, err := createRun(t, client, all[:2], false)
	_, required := planDetail(t, err)
	if required.Target != "app" || required.Key == "" || len(required.NearestPlanIds) != 0 || required.FilesDiff != "" ||
		!strings.Contains(err.Error(), "nearest: none") || !strings.Contains(err.Error(), "fix: run `godwit plan --target app`") {
		t.Fatalf("required = %+v, err = %v", required, err)
	}

	plan := persistPlan(t, client, all[:2], nil)
	_, err = createRun(t, client, all[:4], false)
	_, required = planDetail(t, err)
	if len(required.NearestPlanIds) != 1 || required.NearestPlanIds[0] != plan.PlanId ||
		!strings.Contains(required.FilesDiff, "20260901120002_a (not in plan)") || !strings.Contains(err.Error(), "covers 20260901120001_t") {
		t.Fatalf("required = %+v, err = %v", required, err)
	}

	created, err := createRun(t, client, all[:2], false)
	if err != nil || created.PlanId != plan.PlanId {
		t.Fatalf("created = %+v, err = %v", created, err)
	}
	waitRun(t, client, created.RunId)
}

func TestCreateRun_ServerWideRequirePlanAndTTL(t *testing.T) {
	t.Parallel()
	client := newClient(startServiceCfg(t, Config{
		Listen: "127.0.0.1:0", StoreDSN: newDatabase(t, "st"), Keys: testKeys, Holder: "r1",
		Scheduler: controlplane.Config{Interval: 50 * time.Millisecond}, Log: testLog,
		RequirePlan: true, PlanTTL: time.Nanosecond,
	}), "")
	registerTarget(t, client, newDatabase(t, "tg"))
	all := orderedFiles()

	plan := persistPlan(t, client, all[:2], nil)
	time.Sleep(time.Millisecond)
	_, err := createRun(t, client, all[:2], false)
	_, required := planDetail(t, err)
	if len(required.NearestPlanIds) != 1 || required.NearestPlanIds[0] != plan.PlanId {
		t.Fatalf("expired plan: %+v, err = %v", required, err)
	}
}

func TestPlanContractStoreErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storeDSN := newDatabase(t, "st")
	client := newClient(startService(t, storeDSN, "r1", nil), "")
	registerTarget(t, client, newDatabase(t, "tg"))
	all := orderedFiles()

	execStore(t, storeDSN, "DROP TABLE cp_plan_files, cp_plans CASCADE")
	if _, err := client.PlanRun(ctx, connect.NewRequest(&godwitv1.PlanRunRequest{Target: "app", Files: all, Persist: true, SkipValidation: true})); connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("persist without tables: %v", err)
	}
	if _, err := createRun(t, client, all, false); connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("bind without tables: %v", err)
	}
}
