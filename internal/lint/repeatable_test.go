package lint

import (
	"os"
	"path/filepath"
	"testing"
)

func repeatableRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	git(t, repo, "init", "-q", "-b", "main")
	dir := filepath.Join(repo, "migrations")
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	writeMigs(t, dir, map[string]string{
		"R__stats.up.sql":   "CREATE OR REPLACE VIEW stats AS SELECT 1 AS n;",
		"R__stats.down.sql": "DROP VIEW IF EXISTS stats;",
	})
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-q", "-m", "base")

	return dir
}

func TestCheckRepeatableHazards(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeMigs(t, dir, map[string]string{
		"R__stats.up.sql":   "DROP TABLE t;",
		"R__stats.down.sql": "SELECT 1;",
	})
	rep := check(t, dir, nil, Options{})
	if got, want := codes(rep), "R__stats.up.sql:error:H002 R__stats.down.sql:warning:W001"; got != want {
		t.Fatalf("codes = %q, want %q", got, want)
	}
}

// E003 freezes merged versioned files; a repeatable is meant to be edited in place.
func TestCheckRepeatableEditedAfterMergeIsNotE003(t *testing.T) {
	t.Parallel()

	dir := repeatableRepo(t)
	writeMigs(t, dir, map[string]string{"R__stats.up.sql": "CREATE OR REPLACE VIEW stats AS SELECT 2 AS n;"})
	rep := check(t, dir, nil, Options{Base: "main"})
	if len(rep.Findings) != 0 {
		t.Fatalf("report = %+v", rep)
	}
}

func TestCheckRepeatableAddedIsInScope(t *testing.T) {
	t.Parallel()

	dir := repeatableRepo(t)
	writeMigs(t, dir, map[string]string{
		"R__audit.up.sql":   "DROP TABLE t;",
		"R__audit.down.sql": "SELECT 1;",
	})
	git(t, dir, "add", ".")
	rep := check(t, dir, nil, Options{Base: "main"})
	if got, want := codes(rep), "R__audit.up.sql:error:H002 R__audit.down.sql:warning:W001"; got != want {
		t.Fatalf("codes = %q, want %q", got, want)
	}
}
