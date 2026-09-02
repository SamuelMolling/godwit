package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const fullFile = `dir: db/migrations
target: orders
rollout: canary
server: http://godwit:8474
lock_timeout: 3s
statement_timeout: 1m
`

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"GODWIT_DIR", "GODWIT_TARGET", "GODWIT_ROLLOUT", "GODWIT_SERVER", "GODWIT_LOCK_TIMEOUT", "GODWIT_STATEMENT_TIMEOUT",
	} {
		t.Setenv(k, "")
	}
}

func TestDefaults(t *testing.T) {
	t.Parallel()

	if got, want := Defaults(), (Config{Dir: "migrations", LockTimeout: 5 * time.Second}); got != want {
		t.Fatalf("Defaults() = %+v, want %+v", got, want)
	}
}

func TestLoadExplicitPath(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "custom.yaml")
	write(t, path, fullFile)

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		Dir:              filepath.Join(dir, "db/migrations"),
		Target:           "orders",
		Rollout:          "canary",
		Server:           "http://godwit:8474",
		LockTimeout:      3 * time.Second,
		StatementTimeout: time.Minute,
	}
	if cfg != want {
		t.Fatalf("Load() = %+v, want %+v", cfg, want)
	}
}

func TestLoadAbsoluteDirKept(t *testing.T) {
	clearEnv(t)
	path := filepath.Join(t.TempDir(), FileName)
	write(t, path, "dir: /srv/migrations\n")

	cfg, err := Load(path)
	if err != nil || cfg.Dir != "/srv/migrations" || cfg.LockTimeout != 5*time.Second {
		t.Fatalf("Load() = %+v, %v", cfg, err)
	}
}

func TestLoadFoundInCwd(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	write(t, filepath.Join(dir, FileName), "target: orders\n")
	t.Chdir(dir)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := Load("")
	if err != nil || cfg.Target != "orders" || cfg.Dir != filepath.Join(wd, "migrations") {
		t.Fatalf("Load() = %+v, %v", cfg, err)
	}
}

func TestLoadFoundInParent(t *testing.T) {
	clearEnv(t)
	repo := t.TempDir()
	write(t, filepath.Join(repo, FileName), "target: orders\n")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o750); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(repo, "services", "api")
	if err := os.MkdirAll(nested, 0o750); err != nil {
		t.Fatal(err)
	}
	t.Chdir(nested)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := Load("")
	if err != nil || cfg.Target != "orders" || cfg.Dir != filepath.Join(wd, "..", "..", "migrations") {
		t.Fatalf("Load() = %+v, %v", cfg, err)
	}
}

func TestLoadStopsAtRepoRoot(t *testing.T) {
	clearEnv(t)
	outer := t.TempDir()
	write(t, filepath.Join(outer, FileName), "target: leaked\n")
	nested := filepath.Join(outer, "repo", "sub")
	if err := os.MkdirAll(nested, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(outer, "repo", ".git"), 0o750); err != nil {
		t.Fatal(err)
	}
	t.Chdir(nested)

	cfg, err := Load("")
	if err != nil || cfg != Defaults() {
		t.Fatalf("Load() = %+v, %v; want defaults", cfg, err)
	}
}

func TestLoadNotFound(t *testing.T) {
	clearEnv(t)
	t.Chdir(t.TempDir())

	cfg, err := Load("")
	if err != nil || cfg != Defaults() {
		t.Fatalf("Load() = %+v, %v; want defaults", cfg, err)
	}
}

func TestLoadGetwdError(t *testing.T) {
	clearEnv(t)
	orig := getwd
	getwd = func() (string, error) { return "", errors.New("no cwd") }
	defer func() { getwd = orig }()

	if _, err := Load(""); err == nil || err.Error() != "no cwd" {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadFileErrors(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()

	if _, err := Load(filepath.Join(dir, "missing.yaml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file: err = %v", err)
	}
	for name, body := range map[string]string{
		"unknown key":  "dirr: x\n",
		"bad yaml":     "dir: [\n",
		"bad duration": "lock_timeout: soon\n",
	} {
		path := filepath.Join(dir, strings.ReplaceAll(name, " ", "_")+".yaml")
		write(t, path, body)
		if _, err := Load(path); err == nil || !strings.Contains(err.Error(), path) {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
}

func TestLoadEnvOverrides(t *testing.T) {
	clearEnv(t)
	path := filepath.Join(t.TempDir(), FileName)
	write(t, path, fullFile)
	t.Setenv("GODWIT_DIR", "env/migrations")
	t.Setenv("GODWIT_TARGET", "env-target")
	t.Setenv("GODWIT_ROLLOUT", "env-rollout")
	t.Setenv("GODWIT_SERVER", "http://env:1")
	t.Setenv("GODWIT_LOCK_TIMEOUT", "7s")
	t.Setenv("GODWIT_STATEMENT_TIMEOUT", "2m")

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		Dir:              "env/migrations",
		Target:           "env-target",
		Rollout:          "env-rollout",
		Server:           "http://env:1",
		LockTimeout:      7 * time.Second,
		StatementTimeout: 2 * time.Minute,
	}
	if cfg != want {
		t.Fatalf("Load() = %+v, want %+v", cfg, want)
	}
}

func TestLoadEnvBadDuration(t *testing.T) {
	clearEnv(t)
	t.Chdir(t.TempDir())
	t.Setenv("GODWIT_STATEMENT_TIMEOUT", "later")

	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "GODWIT_STATEMENT_TIMEOUT") {
		t.Fatalf("err = %v", err)
	}
}
