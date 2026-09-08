package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixedNow(t *testing.T) {
	t.Helper()
	newNow = func() time.Time { return time.Date(2026, 9, 8, 14, 30, 12, 0, time.UTC) }
	t.Cleanup(func() { newNow = time.Now })
}

func TestNewWritesPair(t *testing.T) {
	fixedNow(t)
	dir := t.TempDir()

	code, out, errOut := runCLI("new", "add_status", "--dir", dir)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, errOut)
	}
	up := filepath.Join(dir, "20260908143012_add_status.up.sql")
	down := filepath.Join(dir, "20260908143012_add_status.down.sql")
	if !strings.Contains(out, "wrote "+up+"\n") || !strings.Contains(out, "wrote "+down+"\n") {
		t.Fatalf("out = %q", out)
	}
	for path, want := range map[string]string{up: upScaffold, down: downScaffold} {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != want {
			t.Fatalf("%s = %q, want %q", path, body, want)
		}
	}
}

func TestNewScaffoldLoadsButFailsLint(t *testing.T) {
	fixedNow(t)
	dir := t.TempDir()

	if code, _, errOut := runCLI("new", "add_status", "--dir", dir); code != 0 {
		t.Fatalf("new = %d, stderr = %q", code, errOut)
	}
	code, out, errOut := runCLI("lint", "--dir", dir)
	if code != 1 || !strings.Contains(errOut, "2 blocking finding(s)") {
		t.Fatalf("lint = %d, stderr = %q", code, errOut)
	}
	for _, side := range []string{"up", "down"} {
		want := "20260908143012_add_status." + side + ".sql: error E002 20260908143012_add_status (" + side + "): no statements"
		if !strings.Contains(out, want) {
			t.Fatalf("lint out = %q, want %q", out, want)
		}
	}
}

func TestNewStepsOverATakenSecond(t *testing.T) {
	fixedNow(t)
	dir := t.TempDir()

	if code, _, errOut := runCLI("new", "first", "--dir", dir); code != 0 {
		t.Fatalf("first = %d, stderr = %q", code, errOut)
	}
	code, out, errOut := runCLI("new", "second", "--dir", dir)
	if code != 0 {
		t.Fatalf("second = %d, stderr = %q", code, errOut)
	}
	if !strings.Contains(out, filepath.Join(dir, "20260908143013_second.up.sql")) {
		t.Fatalf("out = %q", out)
	}
}

func TestNewRefusesAFullMinute(t *testing.T) {
	fixedNow(t)
	dir := t.TempDir()

	start := newNow().UTC()
	for i := range versionProbe {
		name := filepath.Join(dir, start.Add(time.Duration(i)*time.Second).Format(versionLayout)+"_taken.up.sql")
		if err := os.WriteFile(name, []byte("SELECT 1;\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	code, _, errOut := runCLI("new", "add_status", "--dir", dir)
	if code != 1 || !strings.Contains(errOut, "already holds a migration for every second from 20260908143012 onwards") {
		t.Fatalf("code = %d, stderr = %q", code, errOut)
	}
}

func TestNewRepeatable(t *testing.T) {
	fixedNow(t)
	dir := t.TempDir()

	code, out, errOut := runCLI("new", "order_stats", "--dir", dir, "--repeatable")
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, errOut)
	}
	if !strings.Contains(out, filepath.Join(dir, "R__order_stats.up.sql")) ||
		!strings.Contains(out, filepath.Join(dir, "R__order_stats.down.sql")) {
		t.Fatalf("out = %q", out)
	}

	code, _, errOut = runCLI("new", "order_stats", "--dir", dir, "--repeatable")
	if code != 1 || !strings.Contains(errOut, "R__order_stats already exists in "+dir+": a repeatable is keyed by its name") {
		t.Fatalf("second: code = %d, stderr = %q", code, errOut)
	}
}

func TestNewRepeatableIgnoresALongerName(t *testing.T) {
	fixedNow(t)
	dir := t.TempDir()

	if code, _, errOut := runCLI("new", "order_stats_daily", "--dir", dir, "--repeatable"); code != 0 {
		t.Fatalf("first = %d, stderr = %q", code, errOut)
	}
	if code, _, errOut := runCLI("new", "order_stats", "--dir", dir, "--repeatable"); code != 0 {
		t.Fatalf("second = %d, stderr = %q", code, errOut)
	}
}

func TestNewFailures(t *testing.T) {
	fixedNow(t)
	dir := t.TempDir()

	for _, tc := range []struct{ name, arg, want string }{
		{"camel case", "addStatus", `name "addStatus" must be snake_case ([a-z0-9_]+)`},
		{"dashes", "add-status", "must be snake_case"},
		{"empty", "", "must be snake_case"},
	} {
		if code, _, errOut := runCLI("new", tc.arg, "--dir", dir); code != 1 || !strings.Contains(errOut, tc.want) {
			t.Fatalf("%s: code = %d, stderr = %q", tc.name, code, errOut)
		}
	}

	if code, _, errOut := runCLI("new", "add_status", "--dir", filepath.Join(dir, "missing")); code != 1 ||
		!strings.Contains(errOut, "read migration dir") || !strings.Contains(errOut, "no such file") {
		t.Fatalf("missing dir: code = %d, stderr = %q", code, errOut)
	}

	if code, _, errOut := runCLI("new", "--dir", dir); code != 1 || !strings.Contains(errOut, "accepts 1 arg") {
		t.Fatalf("no name: code = %d, stderr = %q", code, errOut)
	}

	readOnly := t.TempDir()
	if err := os.Chmod(readOnly, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(readOnly, 0o700) })
	if code, _, errOut := runCLI("new", "add_status", "--dir", readOnly); code != 1 ||
		!strings.Contains(errOut, "permission denied") {
		t.Fatalf("read-only dir: code = %d, stderr = %q", code, errOut)
	}
}

func TestNewDirFromConfig(t *testing.T) {
	fixedNow(t)
	repo := configRepo(t, map[string]string{
		"godwit.yaml":         "dir: db/migrations\n",
		"db/migrations/.keep": "",
	})
	wd := chdir(t, repo)

	code, out, errOut := runCLI("new", "add_status")
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, errOut)
	}
	want := filepath.Join(wd, "db/migrations", "20260908143012_add_status.up.sql")
	if !strings.Contains(out, "wrote "+want+"\n") {
		t.Fatalf("out = %q, want %s", out, want)
	}
}

func TestNewHelpMentionsTheRules(t *testing.T) {
	t.Parallel()

	code, out, errOut := runCLI("new", "--help")
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, errOut)
	}
	for _, want := range []string{"--repeatable", "--dir", "refuses an empty side"} {
		if !strings.Contains(out, want) {
			t.Fatalf("help missing %q: %s", want, out)
		}
	}
}
