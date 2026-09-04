package server

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/internal/controlplane"
)

func TestListTargetsEndToEnd(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storeDSN := newDatabase(t, "st")
	baseURL := startServiceCfg(t, Config{
		Listen: "127.0.0.1:0", StoreDSN: storeDSN, Keys: testKeys, Holder: "r1",
		Tokens:    []string{"viewer:read:s-read", "root:admin:s-admin"},
		Scheduler: controlplane.Config{Interval: 50 * time.Millisecond}, Log: testLog,
		UI: true,
	})
	admin, viewer := newClient(baseURL, "s-admin"), newClient(baseURL, "s-read")

	if _, err := admin.RegisterTarget(ctx, connect.NewRequest(&godwitv1.RegisterTargetRequest{
		Name: "app", Provider: "static", Dsn: newDatabase(t, "tg"), SearchPath: "app,public",
		LockTimeout: "3s", StatementTimeout: "1m", RequirePlan: true,
	})); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.RegisterTarget(ctx, connect.NewRequest(&godwitv1.RegisterTargetRequest{
		Name: "billing", Provider: "static", Dsn: newDatabase(t, "tg"),
	})); err != nil {
		t.Fatal(err)
	}

	res, err := viewer.ListTargets(ctx, connect.NewRequest(&godwitv1.ListTargetsRequest{}))
	if err != nil {
		t.Fatalf("read must list targets: %v", err)
	}
	got := res.Msg.Targets
	if len(got) != 2 || got[0].Name != "app" || got[1].Name != "billing" {
		t.Fatalf("targets = %+v", got)
	}
	fresh := got[0]
	if fresh.Provider != "static" || fresh.SearchPath != "app,public" || fresh.LockTimeout != "3s" ||
		fresh.StatementTimeout != "1m" || !fresh.RequirePlan || !fresh.KeepOld || fresh.LastRun != nil ||
		fresh.AppliedCount != 0 || fresh.ReadyPlans != 0 || fresh.AttentionRuns != 0 || fresh.UnresolvedDrift {
		t.Fatalf("never migrated target = %+v", fresh)
	}

	if _, err := admin.PlanRun(ctx, connect.NewRequest(&godwitv1.PlanRunRequest{
		Target: "app", Files: baselineFiles(), Persist: true,
	})); err != nil {
		t.Fatal(err)
	}
	runToSuccess(t, admin, baselineFiles(), nil)
	next := append(baselineFiles(),
		&godwitv1.MigrationFile{Name: "20260901130000_extra.up.sql", Body: "ALTER TABLE t ADD COLUMN extra int;"},
		&godwitv1.MigrationFile{Name: "20260901130000_extra.down.sql", Body: "ALTER TABLE t DROP COLUMN extra;"})
	if _, err := admin.PlanRun(ctx, connect.NewRequest(&godwitv1.PlanRunRequest{
		Target: "app", Files: next, Persist: true,
	})); err != nil {
		t.Fatal(err)
	}
	execStore(t, storeDSN, `INSERT INTO cp_drift_events (target, diff) VALUES ('app', '+ column extra')`)

	res, err = viewer.ListTargets(ctx, connect.NewRequest(&godwitv1.ListTargetsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	app := res.Msg.Targets[0]
	if app.AppliedCount != 2 || app.ReadyPlans != 1 || !app.UnresolvedDrift || app.LastRun == nil ||
		app.LastRun.State != godwitv1.RunState_RUN_STATE_SUCCEEDED {
		t.Fatalf("migrated target = %+v last = %+v", app, app.LastRun)
	}

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/ui/targets/app", nil)
	req.SetBasicAuth("whoever", "s-read")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	want := []string{
		"00000000000001_baseline", "20260901120000_name", "20260901130000_extra",
		"app,public", "require_plan", "app drifted from its baseline", "Actions on this page need a wider scope",
	}
	for _, want := range want {
		if !strings.Contains(string(body), want) {
			t.Fatalf("target page lacks %q: %s", want, body)
		}
	}
	if resp.StatusCode != http.StatusOK || strings.Contains(string(body), `method="post"`) {
		t.Fatalf("code = %d body = %s", resp.StatusCode, body)
	}
}
