package server

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/gen/godwit/v1/godwitv1connect"
	"github.com/SamuelMolling/godwit/internal/controlplane"
)

// checkpointFiles is a four-migration directory: three tables and a column on the first.
func checkpointFiles() []*godwitv1.MigrationFile {
	return []*godwitv1.MigrationFile{
		{Name: "20260101000001_a.up.sql", Body: "CREATE TABLE public.a (id bigint PRIMARY KEY);"},
		{Name: "20260101000001_a.down.sql", Body: "DROP TABLE public.a;"},
		{Name: "20260101000002_b.up.sql", Body: "CREATE TABLE public.b (id bigint PRIMARY KEY);"},
		{Name: "20260101000002_b.down.sql", Body: "DROP TABLE public.b;"},
		{Name: "20260101000003_c.up.sql", Body: "CREATE TABLE public.c (id bigint PRIMARY KEY);"},
		{Name: "20260101000003_c.down.sql", Body: "DROP TABLE public.c;"},
		{Name: "20260101000004_note.up.sql", Body: "ALTER TABLE public.a ADD COLUMN note text;"},
		{Name: "20260101000004_note.down.sql", Body: "ALTER TABLE public.a DROP COLUMN note;"},
	}
}

func checkpointService(t *testing.T) (godwitv1connect.GodwitServiceClient, string) {
	t.Helper()
	client := newClient(startServiceCfg(t, Config{
		Listen: "127.0.0.1:0", StoreDSN: newDatabase(t, "st") + "&search_path=public", MasterKey: testKey, Holder: "r1",
		Scheduler: controlplane.Config{Interval: 50 * time.Millisecond}, Log: testLog,
	}), "")
	targetDSN := newDatabase(t, "tg") + "&search_path=public"
	registerTarget(t, client, targetDSN)

	return client, targetDSN
}

// makeCheckpoint asks the service for a checkpoint over files and returns the directory with it added.
func makeCheckpoint(t *testing.T, client godwitv1connect.GodwitServiceClient, files []*godwitv1.MigrationFile) ([]*godwitv1.MigrationFile, *godwitv1.CheckpointResponse) {
	t.Helper()
	res, err := client.Checkpoint(context.Background(), connect.NewRequest(&godwitv1.CheckpointRequest{
		Files: files, Name: "squash",
	}))
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	name := fmt.Sprintf("%014d_%s.up.sql", res.Msg.Version, res.Msg.Name)

	return append(files[:len(files):len(files)], &godwitv1.MigrationFile{Name: name, Body: res.Msg.Body}), res.Msg
}

// A database with no history runs the checkpoint and records everything below it, exactly once, and ends
// up with the schema the whole directory would have produced.
func TestCheckpoint_FreshTargetStartsAtTheCheckpoint(t *testing.T) {
	t.Parallel()
	client, targetDSN := checkpointService(t)
	files, cp := makeCheckpoint(t, client, checkpointFiles())

	plan, err := client.PlanRun(context.Background(), connect.NewRequest(&godwitv1.PlanRunRequest{Target: "app", Files: files}))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	var seen int
	for _, m := range plan.Msg.Migrations {
		if !m.Checkpoint {
			continue
		}
		seen++
		if m.CollapsesThrough != cp.Through || !strings.Contains(m.Note, "records the 4 migration(s)") {
			t.Fatalf("checkpoint migration = %+v", m)
		}
	}
	if seen != 1 {
		t.Fatalf("the plan must name the checkpoint once, saw %d", seen)
	}

	runToSuccess(t, client, files, nil)
	if got := appliedVersions(t, targetDSN); len(got) != 5 || got[4] != cp.Version {
		t.Fatalf("applied = %v; the checkpoint and everything it collapses must be recorded", got)
	}
	conn := targetConn(t, targetDSN)
	for _, table := range []string{"a", "b", "c"} {
		if !tableExists(t, conn, table) {
			t.Fatalf("table %s missing: the checkpoint must build the whole schema", table)
		}
	}
	if !hasColumn(t, targetDSN, "a", "note") {
		t.Fatal("the checkpoint must carry the column a later migration added")
	}
}

func hasColumn(t *testing.T, dsn, table, column string) bool {
	t.Helper()
	var exists bool
	if err := targetConn(t, dsn).QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = $1 AND column_name = $2)`,
		table, column).Scan(&exists); err != nil {
		t.Fatal(err)
	}

	return exists
}

// A run above a checkpoint plans and applies normally; the checkpoint is already recorded and inert.
func TestCheckpoint_PlanAfterTheCheckpoint(t *testing.T) {
	t.Parallel()
	client, targetDSN := checkpointService(t)
	files, cp := makeCheckpoint(t, client, checkpointFiles())
	runToSuccess(t, client, files, nil)

	above := cp.Version + 1
	next := append(files[:len(files):len(files)],
		&godwitv1.MigrationFile{Name: fmt.Sprintf("%014d_d.up.sql", above), Body: "CREATE TABLE public.d (id bigint PRIMARY KEY);"},
		&godwitv1.MigrationFile{Name: fmt.Sprintf("%014d_d.down.sql", above), Body: "DROP TABLE public.d;"})
	plan, err := client.PlanRun(context.Background(), connect.NewRequest(&godwitv1.PlanRunRequest{Target: "app", Files: next}))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	pending := 0
	for _, m := range plan.Msg.Migrations {
		if !m.Applied {
			pending++
			if m.Name != "d" {
				t.Fatalf("only the new migration is pending, got %s", m.Name)
			}
		}
		if m.Checkpoint && !m.Applied {
			t.Fatal("the checkpoint must read as applied once the target holds it")
		}
	}
	if pending != 1 {
		t.Fatalf("pending = %d", pending)
	}

	runToSuccess(t, client, next, nil)
	if !tableExists(t, targetConn(t, targetDSN), "d") {
		t.Fatal("the migration above the checkpoint never ran")
	}
	if got := appliedVersions(t, targetDSN); len(got) != 6 || got[5] != above || got[4] != cp.Version {
		t.Fatalf("applied = %v", got)
	}
}

// A target that stopped between two collapsed versions moves forward file by file and then records the
// checkpoint: it is never run on a schema that already holds half of what it carries.
func TestCheckpoint_TargetMidHistory(t *testing.T) {
	t.Parallel()
	client, targetDSN := checkpointService(t)
	base := checkpointFiles()
	runToSuccess(t, client, base[:4], nil)

	files, cp := makeCheckpoint(t, client, base)
	runToSuccess(t, client, files, nil)

	conn := targetConn(t, targetDSN)
	for _, table := range []string{"a", "b", "c"} {
		if !tableExists(t, conn, table) {
			t.Fatalf("table %s missing", table)
		}
	}
	if got := appliedVersions(t, targetDSN); len(got) != 5 || got[4] != cp.Version {
		t.Fatalf("applied = %v", got)
	}
}

// With the collapsed files gone from the directory, a target below the checkpoint can neither run it nor
// record it, and godwit says so instead of guessing.
func TestCheckpoint_RefusesAGapUnderTheCheckpoint(t *testing.T) {
	t.Parallel()
	client, _ := checkpointService(t)
	base := checkpointFiles()
	runToSuccess(t, client, base[:2], nil)

	files, _ := makeCheckpoint(t, client, base)
	_, err := client.PlanRun(context.Background(), connect.NewRequest(&godwitv1.PlanRunRequest{
		Target: "app", Files: files[len(base):],
	}))
	if err == nil || connect.CodeOf(err) != connect.CodeFailedPrecondition ||
		!strings.Contains(err.Error(), "not in the migration directory") {
		t.Fatalf("err = %v", err)
	}
}

// Below a checkpoint there is nothing left to undo, and godwit refuses rather than run a down file
// against a state its target never passed through.
func TestCheckpoint_RevertRefusedBelowIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client, _ := checkpointService(t)
	files, _ := makeCheckpoint(t, client, checkpointFiles())

	created, err := client.CreateRun(ctx, connect.NewRequest(&godwitv1.CreateRunRequest{Target: "app", Files: files}))
	if err != nil {
		t.Fatal(err)
	}
	waitRunState(t, client, created.Msg.RunId, godwitv1.RunState_RUN_STATE_SUCCEEDED)

	_, err = client.RevertRun(ctx, connect.NewRequest(&godwitv1.RevertRunRequest{RunId: created.Msg.RunId}))
	if err == nil || !strings.Contains(err.Error(), "cannot be reverted") {
		t.Fatalf("err = %v", err)
	}
}

func TestCheckpoint_RefusesToCollapseTwice(t *testing.T) {
	t.Parallel()
	client, _ := checkpointService(t)
	files, _ := makeCheckpoint(t, client, checkpointFiles())

	_, err := client.Checkpoint(context.Background(), connect.NewRequest(&godwitv1.CheckpointRequest{
		Files: files, Name: "again",
	}))
	if err == nil || connect.CodeOf(err) != connect.CodeInvalidArgument ||
		!strings.Contains(err.Error(), "already collapses history") {
		t.Fatalf("err = %v", err)
	}
}
