package cli

import (
	"encoding/json"
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
	for _, want := range []string{
		"20260901120000_users (up): 2 statement(s)", "hazard H001", "(down): 1 statement(s)",
		"        hazard H001: CREATE INDEX without CONCURRENTLY blocks writes on users\n          -- or let godwit run it: -- godwit: add-index users (id) name=idx_users\n          CREATE INDEX CONCURRENTLY idx_users ON users USING btree (id);\n",
		"        hazard H002: DROP TABLE is destructive\n          -- expand then contract: ship the application version that no longer uses users",
	} {
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

func TestPlanMarkdown(t *testing.T) {
	t.Parallel()

	dir := writeMigs(t, map[string]string{
		"20260901120000_users.up.sql":   "CREATE TABLE users (\n  id int,\n  " + strings.Repeat("a", 120) + " int\n);\nCREATE INDEX idx_users ON users (id) WHERE id > 0 OR id | 1 = 1;",
		"20260901120000_users.down.sql": "DROP TABLE users;",
	})
	code, out, errOut := runCLI("plan", "--dir", dir, "--format", "markdown")
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
	for _, want := range []string{
		"## godwit plan",
		"| Migration | Direction | # | Mode | Statement | Hazards |",
		"| `20260901120000_users` | up | 0 | tx | `CREATE TABLE users ( id int, " + strings.Repeat("a", 90) + "…` |  |",
		"| `20260901120000_users` | up | 1 | tx | `CREATE INDEX idx_users ON users (id) WHERE id > 0 OR id \\| 1 = 1` | H001: CREATE INDEX without CONCURRENTLY blocks writes on users |",
		"| `20260901120000_users` | down | 0 | tx | `DROP TABLE users` | H002: DROP TABLE is destructive |",
		"<details><summary>recipe for H001 in `20260901120000_users` (up) #1</summary>\n\n```sql\n-- or let godwit run it: -- godwit: add-index users (id) name=idx_users where='id > 0 OR (id | 1) = 1'\nCREATE INDEX CONCURRENTLY idx_users ON users USING btree (id) WHERE id > 0 OR (id | 1) = 1;\n```\n\n</details>\n\n",
		"<details><summary>recipe for H002 in `20260901120000_users` (down) #0</summary>\n\n```sql\n-- expand then contract: ship the application version that no longer uses users, then run this DROP TABLE as a contract migration (rollout: expand-contract)\n```\n\n</details>\n\n⚠️ 2 hazard(s); acknowledge them with `--ack`",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}

	safe := writeMigs(t, map[string]string{
		"20260901120000_i.up.sql":   "CREATE INDEX CONCURRENTLY i ON t (v);",
		"20260901120000_i.down.sql": "SELECT 1;",
	})
	code, out, _ = runCLI("plan", "--dir", safe, "--format", "markdown")
	if code != 0 || !strings.Contains(out, "| no-tx |") || !strings.HasSuffix(out, "\n✅ no hazards\n") {
		t.Fatalf("code = %d, out = %q", code, out)
	}

	code, out, _ = runCLI("plan", "--dir", t.TempDir(), "--format", "markdown")
	if code != 0 || out != "## godwit plan\n\n✅ no hazards\n" {
		t.Fatalf("code = %d, out = %q", code, out)
	}
}

func TestPlanMarkdown_WithObservationAndDrift(t *testing.T) {
	t.Parallel()

	var b strings.Builder
	writePlanMarkdown(&b, planReport{
		live: true, target: "app", rollout: "direct", validated: true, planID: "p1", planKey: "k1", drift: "+ column public.rogue.id integer null=YES default=<none>",
		observed: &planObservation{HistoryHash: "h1", SchemaFingerprint: "f1", AppliedCount: 2, NewestApplied: 20260901120000, At: "2026-09-01T10:00:00Z"},
	})
	want := "## godwit plan p1\n\n" +
		"Target `app`, rollout `direct`, validated on a scratch database.\n\n" +
		"key: k1\n\n" +
		"observed: 2 applied, newest 20260901120000, history h1, schema f1, at 2026-09-01T10:00:00Z\n\n" +
		"### Changes outside migrations\n\n```diff\n+ column public.rogue.id integer null=YES default=<none>\n```\n\n" +
		"✅ no hazards\n"
	if b.String() != want {
		t.Fatalf("markdown = %q, want %q", b.String(), want)
	}

	b.Reset()
	writePlanMarkdown(&b, planReport{live: true, target: "app", rollout: "direct"})
	if got := b.String(); got != "## godwit dry run\n\nTarget `app`, rollout `direct`, not validated.\n\n✅ no hazards\n" {
		t.Fatalf("markdown = %q", got)
	}
}

func TestPlanJSON(t *testing.T) {
	t.Parallel()

	code, out, errOut := runCLI("plan", "--dir", goodMigs(t), "--format", "json")
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
	var plans []planJSON
	if err := json.Unmarshal([]byte(out), &plans); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if len(plans) != 2 || plans[0].Direction != "up" || plans[1].Direction != "down" || plans[0].Name != "users" {
		t.Fatalf("plans = %+v", plans)
	}
	up := plans[0].Statements
	if len(up) != 2 || up[0].Mode != "tx" || len(up[0].Hazards) != 0 || up[1].Hazards[0].Code != "H001" ||
		up[1].Hazards[0].Recipe != "-- or let godwit run it: -- godwit: add-index users (id) name=idx_users\nCREATE INDEX CONCURRENTLY idx_users ON users USING btree (id);" {
		t.Fatalf("up = %+v", up)
	}
	if !strings.Contains(out, `"hazards":[]`) {
		t.Fatalf("hazards must marshal as an empty list: %s", out)
	}
}

func TestPlanErrors(t *testing.T) {
	t.Parallel()

	if code, _, _ := runCLI("plan", "--dir", filepath.Join(t.TempDir(), "nope")); code != 1 {
		t.Fatal("missing dir must fail")
	}
	if code, _, errOut := runCLI("plan", "--dir", goodMigs(t), "--format", "yaml"); code != 1 || !strings.Contains(errOut, "unknown format") {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
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
	if code != 0 || !strings.Contains(out, "reverted (1 statement(s))") {
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

func TestLocalDSNFromEnv(t *testing.T) {
	dir := t.TempDir()
	code, _, errOut := runCLI("status", "--dir", dir)
	if code != 1 || !strings.Contains(errOut, "--dsn (or GODWIT_DSN) is required") {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}

	t.Setenv("GODWIT_DSN", newTestDSN(t))
	if code, _, errOut = runCLI("status", "--dir", dir); code != 0 {
		t.Fatalf("env DSN must be accepted: code = %d, stderr = %s", code, errOut)
	}
}
