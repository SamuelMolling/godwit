package server

import (
	"context"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/gen/godwit/v1/godwitv1connect"
	"github.com/SamuelMolling/godwit/internal/controlplane"
)

func flakyFiles(code string) []*godwitv1.MigrationFile {
	return []*godwitv1.MigrationFile{
		{Name: "20260901120000_seq.up.sql", Body: "CREATE SEQUENCE flaky;"},
		{Name: "20260901120000_seq.down.sql", Body: "DROP SEQUENCE flaky;"},
		{Name: "20260901120001_flaky.up.sql", Body: "DO $$ BEGIN IF nextval('flaky') < 2 THEN RAISE EXCEPTION 'flaky' USING ERRCODE = '" + code + "'; END IF; END $$;"},
		{Name: "20260901120001_flaky.down.sql", Body: "SELECT 1;"},
	}
}

func countRuns(t *testing.T, client godwitv1connect.GodwitServiceClient) int {
	t.Helper()
	list, err := client.ListRuns(context.Background(), connect.NewRequest(&godwitv1.ListRunsRequest{Target: "app"}))
	if err != nil {
		t.Fatal(err)
	}

	return len(list.Msg.Runs)
}

func TestCreateRun_ReattachRunning(t *testing.T) {
	t.Parallel()
	client := newClient(startService(t, newDatabase(t, "st"), "r1", nil), "")
	registerTarget(t, client, newDatabase(t, "tg"))
	files := []*godwitv1.MigrationFile{
		{Name: "20260901120001_slow.up.sql", Body: "SELECT pg_sleep(2);"},
		{Name: "20260901120001_slow.down.sql", Body: "SELECT 1;"},
	}
	persistPlan(t, client, files, nil)
	created, err := createRun(t, client, files, false)
	if err != nil || created.Reattached {
		t.Fatalf("created = %+v, err = %v", created, err)
	}
	waitRunState(t, client, created.RunId, godwitv1.RunState_RUN_STATE_RUNNING)

	again, err := createRun(t, client, files, false)
	if err != nil || !again.Reattached || again.RunId != created.RunId || again.PlanId != created.PlanId {
		t.Fatalf("again = %+v, err = %v", again, err)
	}
	if n := countRuns(t, client); n != 1 {
		t.Fatalf("runs = %d, want 1", n)
	}
	run := waitRun(t, client, created.RunId)
	if run.Retries != 0 || run.NotBefore != nil {
		t.Fatalf("run = %+v", run)
	}
	actions := auditActions(t, client)
	if !slices.Contains(actions, controlplane.AuditRunReattach) {
		t.Fatalf("audit = %v", actions)
	}
}

func TestCreateRun_ResumesFailedWhenFresh(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client := newClient(startService(t, newDatabase(t, "st"), "r1", nil), "")
	registerTarget(t, client, newDatabase(t, "tg"))
	files := flakyFiles("22012")
	persistPlan(t, client, files, nil)
	created, err := createRun(t, client, files, false)
	if err != nil {
		t.Fatal(err)
	}
	failed := waitRunState(t, client, created.RunId, godwitv1.RunState_RUN_STATE_FAILED)
	if !strings.HasPrefix(failed.Error, "sql: ") || failed.Retries != 0 {
		t.Fatalf("failed = %+v", failed)
	}

	again, err := createRun(t, client, files, false)
	if err != nil || !again.Reattached || again.RunId != created.RunId {
		t.Fatalf("again = %+v, err = %v", again, err)
	}
	run := waitRun(t, client, created.RunId)
	if run.Attempts != 1 || run.Error != "" || countRuns(t, client) != 1 {
		t.Fatalf("run = %+v", run)
	}
	res, err := client.ListAudit(ctx, connect.NewRequest(&godwitv1.ListAuditRequest{RunId: created.RunId}))
	if err != nil {
		t.Fatal(err)
	}
	var reattach string
	for _, e := range res.Msg.Entries {
		if e.Action == controlplane.AuditRunReattach {
			reattach = e.Detail
		}
	}
	if !strings.Contains(reattach, "state=failed") || !strings.HasSuffix(reattach, "resumed=true") {
		t.Fatalf("reattach audit = %q", reattach)
	}
}

func TestCreateRun_FailedButStaleRefuses(t *testing.T) {
	t.Parallel()
	client := newClient(startService(t, newDatabase(t, "st"), "r1", nil), "")
	targetDSN := newDatabase(t, "tg")
	registerTarget(t, client, targetDSN)
	files := flakyFiles("22012")
	plan := persistPlan(t, client, files, nil)
	created, err := createRun(t, client, files, false)
	if err != nil {
		t.Fatal(err)
	}
	waitRunState(t, client, created.RunId, godwitv1.RunState_RUN_STATE_FAILED)
	execStore(t, targetDSN, "INSERT INTO godwit.migrations (version, name, checksum) VALUES (20260901120009, 'x', 'x')")

	_, err = createRun(t, client, files, false)
	stale, _ := planDetail(t, err)
	if stale.PlanId != plan.PlanId || stale.Reason != controlplane.StaleHistory || len(stale.HistoryAdded) != 2 ||
		stale.HistoryAdded[1] != "20260901120009_x" || !strings.Contains(err.Error(), "20260901120009_x   applied") ||
		!strings.Contains(err.Error(), "by no run (unexplained)") {
		t.Fatalf("stale = %+v, err = %v", stale, err)
	}
	if n := countRuns(t, client); n != 1 {
		t.Fatalf("runs = %d, want 1", n)
	}
}

func TestCreateRun_SucceededButRemovedRefuses(t *testing.T) {
	t.Parallel()
	client := newClient(startService(t, newDatabase(t, "st"), "r1", nil), "")
	targetDSN := newDatabase(t, "tg")
	registerTarget(t, client, targetDSN)
	files := orderedFiles()[:4]
	plan := persistPlan(t, client, files, nil)
	created, err := createRun(t, client, files, false)
	if err != nil {
		t.Fatal(err)
	}
	waitRun(t, client, created.RunId)
	execStore(t, targetDSN, "DELETE FROM godwit.migrations WHERE version = 20260901120002")

	_, err = createRun(t, client, files, false)
	stale, _ := planDetail(t, err)
	if stale.PlanId != plan.PlanId || stale.Reason != controlplane.StaleHistory || len(stale.HistoryRemoved) != 1 ||
		stale.HistoryRemoved[0] != "20260901120002_a" || !strings.Contains(err.Error(), "applied 1 migrations the target no longer has") ||
		!strings.Contains(err.Error(), "20260901120002_a   removed from history") {
		t.Fatalf("stale = %+v, err = %v", stale, err)
	}
}

func TestCreateRun_RevertedRetiresPlan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client := newClient(startService(t, newDatabase(t, "st"), "r1", nil), "")
	registerTarget(t, client, newDatabase(t, "tg"))
	files := orderedFiles()[:2]
	plan := persistPlan(t, client, files, nil)
	created, err := createRun(t, client, files, false)
	if err != nil {
		t.Fatal(err)
	}
	waitRun(t, client, created.RunId)
	reverted, err := client.RevertRun(ctx, connect.NewRequest(&godwitv1.RevertRunRequest{RunId: created.RunId, AcknowledgeHazards: []string{"H002"}}))
	if err != nil {
		t.Fatal(err)
	}
	waitRun(t, client, reverted.Msg.RunId)
	waitRunState(t, client, created.RunId, godwitv1.RunState_RUN_STATE_REVERTED)

	fresh, err := createRun(t, client, files, false)
	if err != nil || fresh.Reattached || fresh.RunId == created.RunId || fresh.PlanId != "" {
		t.Fatalf("fresh = %+v, err = %v", fresh, err)
	}
	waitRun(t, client, fresh.RunId)
	if again, err := createRun(t, client, files, false); err != nil || again.Reattached {
		t.Fatalf("plan %s still bound: again = %+v, err = %v", plan.PlanId, again, err)
	}
}
