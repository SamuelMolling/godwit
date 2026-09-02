package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeMigs(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	return dir
}

func goodMigs(t *testing.T) string {
	t.Helper()

	return writeMigs(t, map[string]string{
		"20260901120000_users.up.sql":   "CREATE TABLE users (id int);\nCREATE INDEX idx_users ON users (id);",
		"20260901120000_users.down.sql": "DROP TABLE users;",
	})
}

func TestPlanCommand(t *testing.T) {
	t.Parallel()

	code, out, errOut := runCLI("plan", "--dir", goodMigs(t))
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
	for _, want := range []string{"20260901120000_users (up): 2 statement(s)", "hazard H001", "(down): 1 statement(s)"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

func TestPlanNoTxMarker(t *testing.T) {
	t.Parallel()

	dir := writeMigs(t, map[string]string{
		"20260901120000_i.up.sql":   "CREATE INDEX CONCURRENTLY i ON t (v);",
		"20260901120000_i.down.sql": "DROP INDEX i;",
	})
	code, out, _ := runCLI("plan", "--dir", dir)
	if code != 0 || !strings.Contains(out, "no-tx") {
		t.Fatalf("code = %d, out = %s", code, out)
	}
}

func TestPlanErrors(t *testing.T) {
	t.Parallel()

	if code, _, _ := runCLI("plan", "--dir", filepath.Join(t.TempDir(), "nope")); code != 1 {
		t.Fatal("missing dir must fail")
	}
	dir := writeMigs(t, map[string]string{
		"20260901120000_x.up.sql":   "NOT SQL",
		"20260901120000_x.down.sql": "SELECT 1;",
	})
	if code, _, errOut := runCLI("plan", "--dir", dir); code != 1 || !strings.Contains(errOut, "parse") {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
}

func TestRunApplyAndSkip(t *testing.T) {
	t.Parallel()
	dsn := newTestDSN(t)
	dir := goodMigs(t)

	code, out, errOut := runCLI("apply", "--dsn", dsn, "--dir", dir)
	if code != 0 || !strings.Contains(out, "applied (2 statement(s))") {
		t.Fatalf("code = %d, out = %s, stderr = %s", code, out, errOut)
	}
	code, out, _ = runCLI("apply", "--dsn", dsn, "--dir", dir)
	if code != 0 || !strings.Contains(out, "skipped") {
		t.Fatalf("re-run: code = %d, out = %s", code, out)
	}
}

func TestRunErrors(t *testing.T) {
	t.Parallel()

	dir := goodMigs(t)
	if code, _, errOut := runCLI("apply", "--dsn", "postgres://bad:bad@127.0.0.1:1/x", "--dir", dir); code != 1 ||
		!strings.Contains(errOut, "connect") {
		t.Fatalf("bad dsn: code should be 1, stderr = %s", errOut)
	}
	if code, _, _ := runCLI("apply", "--dsn", "x", "--dir", filepath.Join(t.TempDir(), "nope")); code != 1 {
		t.Fatal("missing dir must fail")
	}

	bad := writeMigs(t, map[string]string{
		"20260901120000_x.up.sql":   "NOT SQL",
		"20260901120000_x.down.sql": "SELECT 1;",
	})
	if code, _, _ := runCLI("apply", "--dsn", newTestDSN(t), "--dir", bad); code != 1 {
		t.Fatal("bad sql must fail")
	}

	failing := writeMigs(t, map[string]string{
		"20260901120000_x.up.sql":   "SELECT 1/0;",
		"20260901120000_x.down.sql": "SELECT 1;",
	})
	if code, _, errOut := runCLI("apply", "--dsn", newTestDSN(t), "--dir", failing); code != 1 ||
		!strings.Contains(errOut, "statement 0") {
		t.Fatalf("failing migration: stderr = %s", errOut)
	}
}

func TestStatusCommand(t *testing.T) {
	t.Parallel()
	dsn := newTestDSN(t)
	dir := goodMigs(t)

	if code, _, errOut := runCLI("apply", "--dsn", dsn, "--dir", dir); code != 0 {
		t.Fatal(errOut)
	}
	pending := filepath.Join(dir, "20260901130000_later.up.sql")
	if err := os.WriteFile(pending, []byte("SELECT 1;"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(strings.Replace(pending, ".up.", ".down.", 1), []byte("SELECT 1;"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, out, errOut := runCLI("status", "--dsn", dsn, "--dir", dir)
	if code != 0 {
		t.Fatal(errOut)
	}
	if !strings.Contains(out, "applied") || !strings.Contains(out, "pending") {
		t.Fatalf("out = %s", out)
	}

	// Editing an applied migration shows checksum drift.
	if err := os.WriteFile(filepath.Join(dir, "20260901120000_users.up.sql"),
		[]byte("CREATE TABLE users (id bigint);"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, out, _ = runCLI("status", "--dsn", dsn, "--dir", dir)
	if !strings.Contains(out, "checksum drift") {
		t.Fatalf("out = %s", out)
	}
}

func TestStatusErrors(t *testing.T) {
	t.Parallel()

	if code, _, _ := runCLI("status", "--dsn", "x", "--dir", filepath.Join(t.TempDir(), "nope")); code != 1 {
		t.Fatal("missing dir must fail")
	}
	if code, _, _ := runCLI("status", "--dsn", "postgres://bad:bad@127.0.0.1:1/x", "--dir", goodMigs(t)); code != 1 {
		t.Fatal("bad dsn must fail")
	}

	broken := newTestDSN(t)
	execSQL(t, broken, "CREATE SCHEMA godwit; CREATE TABLE godwit.migrations (version bigint PRIMARY KEY)")
	if code, _, errOut := runCLI("status", "--dsn", broken, "--dir", goodMigs(t)); code != 1 ||
		!strings.Contains(errOut, "list applied") {
		t.Fatalf("broken schema: stderr = %s", errOut)
	}
}

func TestDownCommand(t *testing.T) {
	t.Parallel()
	dsn := newTestDSN(t)
	dir := goodMigs(t)

	if code, _, errOut := runCLI("apply", "--dsn", dsn, "--dir", dir); code != 0 {
		t.Fatal(errOut)
	}
	code, out, errOut := runCLI("down", "--dsn", dsn, "--dir", dir, "--version", "20260901120000", "--yes")
	if code != 0 || !strings.Contains(out, "applied (1 statement(s))") {
		t.Fatalf("code = %d, out = %s, stderr = %s", code, out, errOut)
	}
	code, out, _ = runCLI("down", "--dsn", dsn, "--dir", dir, "--version", "20260901120000", "--yes")
	if code != 0 || !strings.Contains(out, "skipped") {
		t.Fatalf("re-down: code = %d, out = %s", code, out)
	}
}

func TestDownErrors(t *testing.T) {
	t.Parallel()
	dir := goodMigs(t)

	if code, _, errOut := runCLI("down", "--dsn", "x", "--dir", dir, "--version", "1"); code != 1 ||
		!strings.Contains(errOut, "--yes") {
		t.Fatalf("confirmation: stderr = %s", errOut)
	}
	if code, _, _ := runCLI("down", "--dsn", "x", "--dir", filepath.Join(t.TempDir(), "nope"), "--version", "1", "--yes"); code != 1 {
		t.Fatal("missing dir must fail")
	}
	if code, _, errOut := runCLI("down", "--dsn", "x", "--dir", dir, "--version", "9", "--yes"); code != 1 ||
		!strings.Contains(errOut, "not found") {
		t.Fatalf("not found: stderr = %s", errOut)
	}
	if code, _, _ := runCLI("down", "--dsn", "postgres://bad:bad@127.0.0.1:1/x", "--dir", dir,
		"--version", "20260901120000", "--yes"); code != 1 {
		t.Fatal("bad dsn must fail")
	}

	badDown := writeMigs(t, map[string]string{
		"20260901120000_x.up.sql":   "SELECT 1;",
		"20260901120000_x.down.sql": "NOT SQL",
	})
	if code, _, _ := runCLI("down", "--dsn", "x", "--dir", badDown, "--version", "20260901120000", "--yes"); code != 1 {
		t.Fatal("bad down sql must fail")
	}

	failingDown := writeMigs(t, map[string]string{
		"20260901120000_x.up.sql":   "SELECT 1;",
		"20260901120000_x.down.sql": "SELECT 1/0;",
	})
	dsn := newTestDSN(t)
	if code, _, errOut := runCLI("apply", "--dsn", dsn, "--dir", failingDown); code != 0 {
		t.Fatal(errOut)
	}
	if code, _, _ := runCLI("down", "--dsn", dsn, "--dir", failingDown, "--version", "20260901120000", "--yes"); code != 1 {
		t.Fatal("failing down must fail")
	}
}

func TestConfigFileSetsDir(t *testing.T) {
	t.Setenv("GODWIT_DIR", "")
	root := t.TempDir()
	migs := filepath.Join(root, "db")
	if err := os.Rename(goodMigs(t), migs); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "godwit.yaml"), []byte("dir: db\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)

	code, out, errOut := runCLI("plan")
	if code != 0 || !strings.Contains(out, "20260901120000_users (up)") {
		t.Fatalf("code = %d, out = %s, stderr = %s", code, out, errOut)
	}
	if code, _, _ = runCLI("plan", "--dir", filepath.Join(root, "nope")); code != 1 {
		t.Fatal("explicit --dir must beat the file")
	}
	if code, _, errOut = runCLI("plan", "--config", filepath.Join(root, "missing.yaml")); code != 1 ||
		!strings.Contains(errOut, "missing.yaml") {
		t.Fatalf("missing config: code = %d, stderr = %s", code, errOut)
	}
	if code, _, _ = runCLI("version", "--config", filepath.Join(root, "missing.yaml")); code != 0 {
		t.Fatal("commands without target flags ignore the config")
	}
}

func TestConfigFileSetsTimeouts(t *testing.T) {
	t.Setenv("GODWIT_LOCK_TIMEOUT", "")
	t.Setenv("GODWIT_STATEMENT_TIMEOUT", "")
	dir := writeMigs(t, map[string]string{
		"20260901120000_slow.up.sql":   "SELECT pg_sleep(5);",
		"20260901120000_slow.down.sql": "SELECT 1;",
	})
	cfg := filepath.Join(t.TempDir(), "godwit.yaml")
	if err := os.WriteFile(cfg, []byte("statement_timeout: 100ms\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, _, errOut := runCLI("apply", "--config", cfg, "--dsn", newTestDSN(t), "--dir", dir)
	if code != 1 || !strings.Contains(errOut, "statement timeout") {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
}
