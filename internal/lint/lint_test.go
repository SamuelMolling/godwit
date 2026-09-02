package lint

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Git hooks export GIT_DIR and GIT_INDEX_FILE, which would point the temporary repos at the real one.
func TestMain(m *testing.M) {
	for _, kv := range os.Environ() {
		if key, _, _ := strings.Cut(kv, "="); strings.HasPrefix(key, "GIT_") {
			_ = os.Unsetenv(key)
		}
	}
	os.Exit(m.Run())
}

func writeMigs(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func check(t *testing.T, dir string, acked []string, opts Options) Report {
	t.Helper()
	rep, err := Check(dir, acked, opts)
	if err != nil {
		t.Fatal(err)
	}

	return rep
}

func codes(rep Report) string {
	parts := make([]string, 0, len(rep.Findings))
	for _, f := range rep.Findings {
		parts = append(parts, f.File+":"+f.Level+":"+f.Code)
	}

	return strings.Join(parts, " ")
}

func TestCheckClean(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeMigs(t, dir, map[string]string{
		"20260901120000_users.up.sql":   "CREATE TABLE users (id int);",
		"20260901120000_users.down.sql": "DROP TABLE users;",
	})
	rep := check(t, dir, nil, Options{})
	if rep.Blocking != 0 || len(rep.Findings) != 0 || rep.Findings == nil {
		t.Fatalf("report = %+v", rep)
	}
}

func TestCheckHazardsAndAck(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeMigs(t, dir, map[string]string{
		"20260901120000_users.up.sql":   "CREATE INDEX i ON users (id);\nALTER TABLE users DROP COLUMN a;",
		"20260901120000_users.down.sql": "DROP INDEX i;",
	})
	rep := check(t, dir, nil, Options{})
	if got, want := codes(rep), "20260901120000_users.up.sql:error:H001 20260901120000_users.up.sql:error:H003"; got != want {
		t.Fatalf("codes = %q, want %q", got, want)
	}
	if rep.Blocking != 2 || rep.Findings[0].Message == "" {
		t.Fatalf("report = %+v", rep)
	}

	rep = check(t, dir, []string{" h001 ", "H003"}, Options{})
	if rep.Blocking != 0 || len(rep.Findings) != 0 {
		t.Fatalf("acked report = %+v", rep)
	}
}

func TestCheckLockHazards(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeMigs(t, dir, map[string]string{
		"20260901120000_fk.up.sql":   "ALTER TABLE orders ADD CONSTRAINT fk FOREIGN KEY (user_id) REFERENCES users (id);\nALTER TABLE users RENAME COLUMN email TO mail;\nDROP INDEX i;",
		"20260901120000_fk.down.sql": "DROP INDEX CONCURRENTLY j;",
	})
	rep := check(t, dir, nil, Options{})
	want := "20260901120000_fk.up.sql:error:H006 20260901120000_fk.up.sql:error:H008 20260901120000_fk.up.sql:error:H009"
	if got := codes(rep); got != want {
		t.Fatalf("codes = %q, want %q", got, want)
	}
	if !strings.Contains(rep.Findings[0].Message, "NOT VALID") ||
		rep.Findings[0].Recipe != "ALTER TABLE orders ADD CONSTRAINT fk FOREIGN KEY (user_id) REFERENCES users (id) NOT VALID;\nALTER TABLE orders VALIDATE CONSTRAINT fk;" ||
		rep.Findings[2].Recipe != "DROP INDEX CONCURRENTLY i;" {
		t.Fatalf("report = %+v", rep)
	}

	rep = check(t, dir, []string{"H006", "H008", "H009"}, Options{})
	if rep.Blocking != 0 || len(rep.Findings) != 0 {
		t.Fatalf("acked report = %+v", rep)
	}
}

func TestCheckNoOpDown(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeMigs(t, dir, map[string]string{
		"20260901120000_i.up.sql":   "CREATE INDEX CONCURRENTLY i ON t (v);",
		"20260901120000_i.down.sql": "SELECT 1;\nSELECT 2;",
	})
	rep := check(t, dir, nil, Options{})
	if got, want := codes(rep), "20260901120000_i.down.sql:warning:W001"; got != want {
		t.Fatalf("codes = %q, want %q", got, want)
	}
	if rep.Blocking != 0 || rep.Findings[0].Message != "down migration is a no-op" {
		t.Fatalf("report = %+v", rep)
	}
}

func TestCheckParseErrors(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeMigs(t, dir, map[string]string{
		"20260901120000_x.up.sql":   "NOT SQL",
		"20260901120000_x.down.sql": "ALSO NOT SQL",
	})
	rep := check(t, dir, nil, Options{})
	if got, want := codes(rep), "20260901120000_x.up.sql:error:E002 20260901120000_x.down.sql:error:E002"; got != want {
		t.Fatalf("codes = %q, want %q", got, want)
	}
	if rep.Blocking != 2 || !strings.Contains(rep.Findings[0].Message, "parse") {
		t.Fatalf("report = %+v", rep)
	}
}

func TestCheckLoadError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeMigs(t, dir, map[string]string{"20260901120000_x.up.sql": "SELECT 1;"})
	rep := check(t, dir, nil, Options{})
	if got, want := codes(rep), dir+":error:E001"; got != want {
		t.Fatalf("codes = %q, want %q", got, want)
	}
	if rep.Blocking != 1 || !strings.Contains(rep.Findings[0].Message, "missing down") {
		t.Fatalf("report = %+v", rep)
	}
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func gitRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	git(t, repo, "init", "-q", "-b", "main")
	dir := filepath.Join(repo, "migrations")
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	writeMigs(t, dir, map[string]string{
		"20260901120000_old.up.sql":   "CREATE INDEX i ON t (v);",
		"20260901120000_old.down.sql": "DROP INDEX i;",
	})
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-q", "-m", "base")

	return dir
}

func TestCheckBaseAddedOnly(t *testing.T) {
	t.Parallel()

	dir := gitRepo(t)
	writeMigs(t, dir, map[string]string{
		"20260901130000_new.up.sql":   "ALTER TABLE t DROP COLUMN v;",
		"20260901130000_new.down.sql": "SELECT 1;",
	})
	git(t, dir, "add", ".")
	rep := check(t, dir, nil, Options{Base: "main"})
	if got, want := codes(rep), "20260901130000_new.up.sql:error:H003 20260901130000_new.down.sql:warning:W001"; got != want {
		t.Fatalf("codes = %q, want %q", got, want)
	}
}

func TestCheckBaseModified(t *testing.T) {
	t.Parallel()

	dir := gitRepo(t)
	writeMigs(t, dir, map[string]string{"20260901120000_old.up.sql": "CREATE INDEX i ON t (w);"})
	rep := check(t, dir, nil, Options{Base: "main"})
	if got, want := codes(rep), "20260901120000_old.up.sql:error:E003"; got != want {
		t.Fatalf("codes = %q, want %q", got, want)
	}
	if rep.Findings[0].Message != "migration modified after merge" {
		t.Fatalf("report = %+v", rep)
	}
}

func TestCheckBaseGitFailure(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeMigs(t, dir, map[string]string{
		"20260901120000_x.up.sql":   "SELECT 1;",
		"20260901120000_x.down.sql": "SELECT 1;",
	})
	_, err := Check(dir, nil, Options{Base: "no-such-ref"})
	if err == nil || !strings.Contains(err.Error(), "git diff no-such-ref: exit status") {
		t.Fatalf("err = %v", err)
	}
}

func TestCheckBaseInjectedGit(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	calls := 0
	failSecond := func(_ string, _ ...string) ([]byte, error) {
		calls++
		if calls == 2 {
			return nil, errors.New("boom")
		}

		return []byte("migrations/README.md\nmigrations/20260901120000_x.up.sql\n"), nil
	}
	_, err := Check(dir, nil, Options{Base: "ref", Git: failSecond})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}

	noise := func(_ string, args ...string) ([]byte, error) {
		if args[2] == "--diff-filter=M" {
			return []byte("migrations/README.md\n"), nil
		}

		return []byte(""), nil
	}
	writeMigs(t, dir, map[string]string{
		"20260901120000_x.up.sql":   "DROP TABLE t;",
		"20260901120000_x.down.sql": "SELECT 1;",
	})
	rep := check(t, dir, nil, Options{Base: "ref", Git: noise})
	if len(rep.Findings) != 0 {
		t.Fatalf("report = %+v", rep)
	}
}
