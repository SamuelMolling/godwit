package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/gen/godwit/v1/godwitv1connect"
)

func registerTarget(t *testing.T, client godwitv1connect.GodwitServiceClient, dsn string) {
	t.Helper()
	if _, err := client.RegisterTarget(context.Background(), connect.NewRequest(&godwitv1.RegisterTargetRequest{
		Name: "app", Provider: "static", Dsn: dsn,
	})); err != nil {
		t.Fatal(err)
	}
}

func runToSuccess(t *testing.T, client godwitv1connect.GodwitServiceClient, files []*godwitv1.MigrationFile, ack []string) string {
	t.Helper()
	created, err := client.CreateRun(context.Background(), connect.NewRequest(&godwitv1.CreateRunRequest{
		Target: "app", Files: files, AcknowledgeHazards: ack,
	}))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		r, err := client.GetRun(context.Background(), connect.NewRequest(&godwitv1.GetRunRequest{RunId: created.Msg.RunId}))
		if err != nil {
			t.Fatal(err)
		}
		if r.Msg.Run.State == godwitv1.RunState_RUN_STATE_SUCCEEDED {
			return created.Msg.RunId
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("run never succeeded")

	return ""
}

func TestDriftEndToEnd(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client := newClient(startService(t, newDatabase(t, "st"), "r1", nil), "")
	targetDSN := newDatabase(t, "tg")
	registerTarget(t, client, targetDSN)

	// Migration runs → baseline recorded → no drift.
	runToSuccess(t, client, migrationFiles(), nil)
	drift, err := client.CheckDrift(ctx, connect.NewRequest(&godwitv1.CheckDriftRequest{Target: "app"}))
	if err != nil || drift.Msg.Drifted {
		t.Fatalf("drift = %+v, err = %v", drift, err)
	}

	// Manual change → CheckDrift reports it with the diff.
	conn, err := pgx.Connect(ctx, targetDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, "CREATE TABLE rogue (id int)"); err != nil {
		t.Fatal(err)
	}
	drift, err = client.CheckDrift(ctx, connect.NewRequest(&godwitv1.CheckDriftRequest{Target: "app"}))
	if err != nil || !drift.Msg.Drifted || !strings.Contains(drift.Msg.Diff, "rogue") {
		t.Fatalf("drift = %+v, err = %v", drift, err)
	}

	events, err := client.ListDriftEvents(ctx, connect.NewRequest(&godwitv1.ListDriftEventsRequest{Target: "app"}))
	if err != nil || len(events.Msg.Events) != 1 || events.Msg.Events[0].ResolvedAt != nil {
		t.Fatalf("events = %+v, err = %v", events, err)
	}

	// Accepting the baseline blesses the manual change.
	if _, err := client.AcceptBaseline(ctx, connect.NewRequest(&godwitv1.AcceptBaselineRequest{Target: "app"})); err != nil {
		t.Fatal(err)
	}
	drift, err = client.CheckDrift(ctx, connect.NewRequest(&godwitv1.CheckDriftRequest{Target: "app"}))
	if err != nil || drift.Msg.Drifted {
		t.Fatalf("after accept: drift = %+v, err = %v", drift, err)
	}
	events, _ = client.ListDriftEvents(ctx, connect.NewRequest(&godwitv1.ListDriftEventsRequest{}))
	if len(events.Msg.Events) != 1 || events.Msg.Events[0].ResolvedAt == nil {
		t.Fatalf("events = %+v", events)
	}

	// Unknown target maps to NotFound.
	if _, err := client.CheckDrift(ctx, connect.NewRequest(&godwitv1.CheckDriftRequest{Target: "ghost"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("ghost drift: %v", err)
	}
}

func TestHazardAcknowledgment(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client := newClient(startService(t, newDatabase(t, "st"), "r1", nil), "")
	registerTarget(t, client, newDatabase(t, "tg"))

	runToSuccess(t, client, migrationFiles(), nil)

	hazardous := []*godwitv1.MigrationFile{
		{Name: "20260901130000_drop.up.sql", Body: "DROP TABLE t;"},
		{Name: "20260901130000_drop.down.sql", Body: "CREATE TABLE t (id int);"},
	}

	// Refused without acknowledgment, naming the hazard.
	_, err := client.CreateRun(ctx, connect.NewRequest(&godwitv1.CreateRunRequest{
		Target: "app", Files: hazardous,
	}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "H002") {
		t.Fatalf("err = %v", err)
	}

	// Accepted with the code acknowledged.
	runToSuccess(t, client, hazardous, []string{"H002"})
}

func TestAdmissionValidation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client := newClient(startService(t, newDatabase(t, "st"), "r1", nil), "")
	registerTarget(t, client, newDatabase(t, "tg"))

	// Parses fine, fails at execution: caught by the scratch database, never queued.
	broken := []*godwitv1.MigrationFile{
		{Name: "20260901120000_broken.up.sql", Body: "SELECT 1/0;"},
		{Name: "20260901120000_broken.down.sql", Body: "SELECT 1;"},
	}
	_, err := client.CreateRun(ctx, connect.NewRequest(&godwitv1.CreateRunRequest{
		Target: "app", Files: broken,
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "failed validation") {
		t.Fatalf("err = %v", err)
	}

	// skip_validation lets it through (and it fails later at execution).
	created, err := client.CreateRun(ctx, connect.NewRequest(&godwitv1.CreateRunRequest{
		Target: "app", Files: broken, SkipValidation: true,
	}))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		r, err := client.GetRun(ctx, connect.NewRequest(&godwitv1.GetRunRequest{RunId: created.Msg.RunId}))
		if err != nil {
			t.Fatal(err)
		}
		if r.Msg.Run.State == godwitv1.RunState_RUN_STATE_FAILED {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("skipped-validation run never failed")
}
