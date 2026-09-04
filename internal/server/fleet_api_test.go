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
	"github.com/SamuelMolling/godwit/gen/godwit/v1/godwitv1connect"
	"github.com/SamuelMolling/godwit/internal/controlplane"
)

func migrateTarget(t *testing.T, client godwitv1connect.GodwitServiceClient, target string, files []*godwitv1.MigrationFile) {
	t.Helper()
	ctx := context.Background()
	created, err := client.CreateRun(ctx, connect.NewRequest(&godwitv1.CreateRunRequest{Target: target, Files: files}))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		r, err := client.GetRun(ctx, connect.NewRequest(&godwitv1.GetRunRequest{RunId: created.Msg.RunId}))
		if err != nil {
			t.Fatal(err)
		}
		if r.Msg.Run.State == godwitv1.RunState_RUN_STATE_SUCCEEDED {
			return
		}
		if r.Msg.Run.State == godwitv1.RunState_RUN_STATE_FAILED || r.Msg.Run.State == godwitv1.RunState_RUN_STATE_NEEDS_ATTENTION {
			t.Fatalf("run on %s: %s", target, r.Msg.Run.Error)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("run on %s never succeeded", target)
}

func fleetFile(version, name, body string) []*godwitv1.MigrationFile {
	return []*godwitv1.MigrationFile{
		{Name: version + "_" + name + ".up.sql", Body: body},
		{Name: version + "_" + name + ".down.sql", Body: "SELECT 1;"},
	}
}

func migrationOf(res *godwitv1.ListMigrationsResponse, id string, nth int) *godwitv1.FleetMigration {
	for _, m := range res.Migrations {
		if m.Migration == id {
			if nth == 0 {
				return m
			}
			nth--
		}
	}

	return nil
}

// TestListMigrationsEndToEnd runs the same migrations against two real targets, the way an owner promotes
// staging to production, and reads the fleet back from the control plane alone.
func TestListMigrationsEndToEnd(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	baseURL := startServiceCfg(t, Config{
		Listen: "127.0.0.1:0", StoreDSN: newDatabase(t, "st"), Keys: testKeys, Holder: "r1",
		Tokens:    []string{"viewer:read:s-read", "root:admin:s-admin"},
		Scheduler: controlplane.Config{Interval: 50 * time.Millisecond}, Log: testLog,
		UI: true,
	})
	admin, viewer := newClient(baseURL, "s-admin"), newClient(baseURL, "s-read")

	for _, name := range []string{"production", "staging"} {
		if _, err := admin.RegisterTarget(ctx, connect.NewRequest(&godwitv1.RegisterTargetRequest{
			Name: name, Provider: "static", Dsn: newDatabase(t, "tg"),
		})); err != nil {
			t.Fatal(err)
		}
	}
	base := baselineFiles()
	migrateTarget(t, admin, "production", base)
	migrateTarget(t, admin, "staging", base)
	status := fleetFile("20260902090000", "add_status", "ALTER TABLE t ADD COLUMN status text;")
	migrateTarget(t, admin, "staging", append(base, status...))
	migrateTarget(t, admin, "production", append(base, fleetFile("20260903100000", "x", "ALTER TABLE t ADD COLUMN x int;")...))
	migrateTarget(t, admin, "staging",
		append(append(base, status...), fleetFile("20260903100000", "x", "ALTER TABLE t ADD COLUMN x bigint;")...))

	res, err := viewer.ListMigrations(ctx, connect.NewRequest(&godwitv1.ListMigrationsRequest{}))
	if err != nil {
		t.Fatalf("read must list migrations: %v", err)
	}
	if got := res.Msg.Targets; len(got) != 2 || got[0] != "production" || got[1] != "staging" {
		t.Fatalf("targets = %v", got)
	}
	if len(res.Msg.Migrations) != 5 {
		t.Fatalf("migrations = %+v", res.Msg.Migrations)
	}

	shared := migrationOf(res.Msg, "20260901120000_name", 0)
	if shared == nil || len(shared.AppliedOn) != 2 || len(shared.MissingFrom) != 0 || shared.Divergent {
		t.Fatalf("both targets carry it: %+v", shared)
	}
	if shared.AppliedOn[0].RunId == "" || shared.AppliedOn[0].AppliedAt == nil || shared.Checksum == "" {
		t.Fatalf("applied on = %+v", shared.AppliedOn[0])
	}

	// staging alone has add_status, and production is past it: it did not skip a version it never saw.
	only := migrationOf(res.Msg, "20260902090000_add_status", 0)
	if only == nil || len(only.AppliedOn) != 1 || only.AppliedOn[0].Target != "staging" {
		t.Fatalf("add_status = %+v", only)
	}
	if g := only.MissingFrom[0]; g.Target != "production" || g.Behind || g.Holds || g.NewestVersion != 20260903100000 {
		t.Fatalf("production gap = %+v", g)
	}

	// The alarming one: the same version, applied from two different files.
	first, second := migrationOf(res.Msg, "20260903100000_x", 0), migrationOf(res.Msg, "20260903100000_x", 1)
	if first == nil || second == nil || !first.Divergent || !second.Divergent || first.Checksum == second.Checksum {
		t.Fatalf("x = %+v / %+v", first, second)
	}
	for _, m := range []*godwitv1.FleetMigration{first, second} {
		if len(m.AppliedOn) != 1 || len(m.MissingFrom) != 1 {
			t.Fatalf("x entry = %+v", m)
		}
		if g := m.MissingFrom[0]; !g.Holds || g.OtherChecksum == m.Checksum {
			t.Fatalf("the other target holds it under other content: %+v", g)
		}
	}

	// The question the owner asked: what is in staging that is not in production yet.
	ahead, err := viewer.ListMigrations(ctx, connect.NewRequest(&godwitv1.ListMigrationsRequest{
		InTarget: "staging", NotInTarget: "production",
	}))
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, m := range ahead.Msg.Migrations {
		ids = append(ids, m.Migration)
	}
	if strings.Join(ids, ",") != "20260902090000_add_status,20260903100000_x" {
		t.Fatalf("in staging, not in production = %v", ids)
	}

	if _, err := viewer.ListMigrations(ctx, connect.NewRequest(&godwitv1.ListMigrationsRequest{
		Targets: []string{"nope"},
	})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("unregistered target = %v", err)
	}

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/ui/migrations", nil)
	req.SetBasicAuth("whoever", "s-read")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	for _, want := range []string{"20260903100000_x", "stands under more than one checksum", "differs", "missing"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("fleet page lacks %q: %s", want, body)
		}
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("code = %d", resp.StatusCode)
	}
}
