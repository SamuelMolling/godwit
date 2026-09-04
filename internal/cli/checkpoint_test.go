package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
)

const stubCheckpointBody = "-- godwit: checkpoint through=20260101000002\nCREATE TABLE public.a (id int);\n"

func checkpointStub() *stubService {
	return &stubService{checkpoint: &godwitv1.CheckpointResponse{
		Version: 20260901000000, Name: "squash", Through: 20260101000002,
		Covers: []string{"20260101000001_a", "20260101000002_b"}, Body: stubCheckpointBody,
	}}
}

func checkpointDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{
		"20260101000001_a.up.sql":   "CREATE TABLE public.a (id int);",
		"20260101000001_a.down.sql": "DROP TABLE public.a;",
		"20260101000002_b.up.sql":   "CREATE TABLE public.b (id int);",
		"20260101000002_b.down.sql": "DROP TABLE public.b;",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	return dir
}

func TestCheckpointWritesTheFile(t *testing.T) {
	t.Parallel()
	stub := checkpointStub()
	url := startStub(t, stub)
	dir := checkpointDir(t)

	code, out, errOut := runCLI("checkpoint", "--server", url, "--token", "tok", "--name", "squash", "--dir", dir, "--at", "20260101000002")
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
	file := filepath.Join(dir, "20260901000000_squash.up.sql")
	body, err := os.ReadFile(file)
	if err != nil || string(body) != stubCheckpointBody {
		t.Fatalf("file = %q, err = %v", body, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "20260901000000_squash.down.sql")); err == nil {
		t.Fatal("a checkpoint has no down file")
	}
	if !strings.Contains(out, "collapses 2 migration(s), 20260101000001_a through 20260101000002_b") ||
		!strings.Contains(out, "wrote "+file) {
		t.Fatalf("out = %q", out)
	}
	if stub.checkpointed.At != 20260101000002 || stub.checkpointed.Name != "squash" || len(stub.checkpointed.Files) != 4 {
		t.Fatalf("request = %+v", stub.checkpointed)
	}
}

func TestCheckpointDryRunAndJSON(t *testing.T) {
	t.Parallel()
	url := startStub(t, checkpointStub())
	dir := checkpointDir(t)

	code, out, errOut := runCLI("checkpoint", "--server", url, "--name", "squash", "--dir", dir, "--dry-run")
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
	if !strings.Contains(out, stubCheckpointBody) {
		t.Fatalf("out = %q", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "20260901000000_squash.up.sql")); err == nil {
		t.Fatal("--dry-run must write nothing")
	}

	code, out, errOut = runCLI("checkpoint", "--server", url, "--name", "squash", "--dir", dir, "--dry-run", "--json")
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
	got := decodeJSON(t, out)
	if got["through"] != float64(20260101000002) || got["file"] != nil {
		t.Fatalf("json = %v", got)
	}
}

func TestCheckpointRefusals(t *testing.T) {
	t.Parallel()
	dir := checkpointDir(t)

	code, _, errOut := runCLI("checkpoint", "--server", startStub(t, checkpointStub()), "--name", "Bad Name", "--dir", dir)
	if code == 0 || !strings.Contains(errOut, "snake_case") {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
	code, _, errOut = runCLI("checkpoint", "--server", startStub(t, checkpointStub()), "--name", "squash", "--dir", filepath.Join(dir, "gone"))
	if code == 0 || !strings.Contains(errOut, "read migration dir") {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
	stub := checkpointStub()
	stub.err = connect.NewError(connect.CodeInvalidArgument, errors.New("checkpoint: at least two"))
	code, _, errOut = runCLI("checkpoint", "--server", startStub(t, stub), "--name", "squash", "--dir", dir)
	if code == 0 || !strings.Contains(errOut, "at least two") {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
}

func TestCheckpointWriteFailure(t *testing.T) {
	t.Parallel()
	url := startStub(t, checkpointStub())
	dir := checkpointDir(t)
	if err := os.Mkdir(filepath.Join(dir, "20260901000000_squash.up.sql"), 0o755); err != nil {
		t.Fatal(err)
	}
	code, _, errOut := runCLI("checkpoint", "--server", url, "--name", "squash", "--dir", dir)
	if code == 0 || !strings.Contains(errOut, "20260901000000_squash.up.sql") {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
}

func execCLIDSN(t *testing.T, dsn, sql string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, sql); err != nil {
		t.Fatal(err)
	}
}

func offlineCheckpointDir(t *testing.T) string {
	t.Helper()

	return writeMigs(t, map[string]string{
		"20260101000001_a.up.sql":   "CREATE TABLE public.a (id int);",
		"20260101000001_a.down.sql": "DROP TABLE public.a;",
		"20260101000002_b.up.sql":   "CREATE TABLE public.b (id int);",
		"20260101000002_b.down.sql": "DROP TABLE public.b;",
		"20260101000003_squash.up.sql": "-- godwit: checkpoint through=20260101000002\n" +
			"CREATE TABLE public.a (id int);\nCREATE TABLE public.b (id int);",
	})
}

// Offline, a checkpoint has only an up side to plan and print.
func TestPlanPrintsACheckpointWithoutADown(t *testing.T) {
	t.Parallel()
	code, out, errOut := runCLI("plan", "--dir", offlineCheckpointDir(t))
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
	if !strings.Contains(out, "20260101000003_squash (up)") || strings.Contains(out, "20260101000003_squash (down)") {
		t.Fatalf("out = %s", out)
	}
}

// A database with no history runs the checkpoint and records what it collapses, offline too.
func TestApplyRunsTheCheckpointOnAFreshDatabase(t *testing.T) {
	t.Parallel()
	dsn := newTestDSN(t)
	code, out, errOut := runCLI("apply", "--dsn", dsn, "--dir", offlineCheckpointDir(t))
	if code != 0 {
		t.Fatalf("code = %d, out = %s, stderr = %s", code, out, errOut)
	}
	if strings.Count(out, "applied") != 3 || !strings.Contains(out, "20260101000001_a: applied (0 statement(s))") {
		t.Fatalf("out = %s", out)
	}
}

func TestApplyReportsAnUnreadableHistory(t *testing.T) {
	t.Parallel()
	dsn := newTestDSN(t)
	execCLIDSN(t, dsn, "CREATE SCHEMA godwit; CREATE TABLE godwit.migrations (x int)")
	code, _, errOut := runCLI("apply", "--dsn", dsn, "--dir", offlineCheckpointDir(t))
	if code != 1 || !strings.Contains(errOut, "list applied") {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
}

func TestDownRefusesACheckpoint(t *testing.T) {
	t.Parallel()
	code, _, errOut := runCLI("down", "--dsn", "x", "--dir", offlineCheckpointDir(t), "--version", "20260101000003", "--yes")
	if code != 1 || !strings.Contains(errOut, "is a checkpoint: it has no inverse") {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
}
