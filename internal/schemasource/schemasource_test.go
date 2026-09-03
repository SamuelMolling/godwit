package schemasource

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const postgresSchema = `// datasource commented { provider = "mysql" }
generator client {
  provider = "prisma-client-js"
}

datasource db {
  provider = "postgresql"
  url      = env("DATABASE_URL")
}

model Order {
  id     Int    @id @default(autoincrement())
  status String @default("new")
}
`

const orderDDL = "-- CreateTable\nCREATE TABLE \"Order\" (\n    \"id\" SERIAL NOT NULL,\n    \"status\" TEXT NOT NULL DEFAULT 'new',\n\n    CONSTRAINT \"Order_pkey\" PRIMARY KEY (\"id\")\n);\n\n"

type call struct {
	name string
	args []string
}

type fakeRunner struct {
	calls   []call
	version string
	out     string
	errOut  string
	err     error
	verErr  error
}

func (f *fakeRunner) run(_ context.Context, name string, args ...string) ([]byte, []byte, error) {
	f.calls = append(f.calls, call{name, args})
	if args[len(args)-1] == "--version" {
		return []byte(f.version), nil, f.verErr
	}

	return []byte(f.out), []byte(f.errOut), f.err
}

func writeFile(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

func TestFileLoad(t *testing.T) {
	t.Parallel()
	path := writeFile(t, "schema.sql", "CREATE TABLE t (id int);\n")
	ddl, err := File{Path: path}.Load(context.Background())
	if err != nil || ddl != "CREATE TABLE t (id int);\n" {
		t.Fatalf("ddl = %q, err = %v", ddl, err)
	}
	if _, err := (File{Path: filepath.Join(t.TempDir(), "nope.sql")}).Load(context.Background()); err == nil {
		t.Fatal("missing file must fail")
	}
}

func TestPrismaLoadPicksTheFlagByMajor(t *testing.T) {
	t.Parallel()
	schema := writeFile(t, "schema.prisma", postgresSchema)
	for _, tc := range []struct {
		version string
		bin     string
		name    string
		prefix  []string
		flag    string
	}{
		{"Prisma schema loaded from schema.prisma\nprisma                  : 6.19.3\n@prisma/client          : Not found\n", "npx prisma", "npx", []string{"prisma"}, "--to-schema-datamodel"},
		{"prisma               : 7.10.0\n", "", "npx", []string{"prisma"}, "--to-schema"},
		{"prisma : 5.22.0\n", "/opt/prisma/bin/prisma", "/opt/prisma/bin/prisma", nil, "--to-schema-datamodel"},
		{"prisma : v7.0.0\n", "node  node_modules/prisma/build/index.js", "node", []string{"node_modules/prisma/build/index.js"}, "--to-schema"},
	} {
		runner := &fakeRunner{version: tc.version, out: orderDDL}
		ddl, err := Prisma{Schema: schema, Bin: tc.bin, Run: runner.run}.Load(context.Background())
		if err != nil || ddl != orderDDL {
			t.Fatalf("%s: ddl = %q, err = %v", tc.version, ddl, err)
		}
		want := []call{
			{tc.name, append(append([]string{}, tc.prefix...), "--version")},
			{tc.name, append(append([]string{}, tc.prefix...), "migrate", "diff", "--from-empty", tc.flag, schema, "--script")},
		}
		if len(runner.calls) != 2 || runner.calls[0].name != want[0].name || runner.calls[1].name != want[1].name ||
			strings.Join(runner.calls[0].args, " ") != strings.Join(want[0].args, " ") ||
			strings.Join(runner.calls[1].args, " ") != strings.Join(want[1].args, " ") {
			t.Fatalf("%s: calls = %+v, want %+v", tc.version, runner.calls, want)
		}
	}
}

func TestPrismaLoadRefusesOtherProviders(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"mysql", "datasource db {\n  provider = \"mysql\"\n  url = env(\"DATABASE_URL\")\n}\n", `datasource provider is "mysql"; godwit targets PostgreSQL only`},
		{"env", "datasource db {\n  provider = env(\"PROVIDER\")\n}\n", `datasource provider is env("PROVIDER")`},
		{"no provider", "datasource db {\n  url = env(\"DATABASE_URL\")\n}\n", "datasource has no provider"},
		{"no datasource", "// datasource db { provider = \"postgresql\" }\nmodel A {\n  id Int @id\n}\n", "no datasource block"},
	} {
		schema := writeFile(t, "schema.prisma", tc.body)
		runner := &fakeRunner{}
		_, err := Prisma{Schema: schema, Run: runner.run}.Load(context.Background())
		if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.HasPrefix(err.Error(), schema+": ") {
			t.Fatalf("%s: err = %v", tc.name, err)
		}
		if len(runner.calls) != 0 {
			t.Fatalf("%s: prisma ran %d time(s)", tc.name, len(runner.calls))
		}
	}
}

func TestPrismaLoadAcceptsPostgresAlias(t *testing.T) {
	t.Parallel()
	schema := writeFile(t, "schema.prisma", "datasource db {\n  provider=\"postgres\"\n}\n")
	runner := &fakeRunner{version: "prisma : 6.0.0\n", out: orderDDL}
	if ddl, err := (Prisma{Schema: schema, Run: runner.run}).Load(context.Background()); err != nil || ddl != orderDDL {
		t.Fatalf("ddl = %q, err = %v", ddl, err)
	}
}

func TestPrismaLoadErrors(t *testing.T) {
	t.Parallel()
	schema := writeFile(t, "schema.prisma", postgresSchema)
	for _, tc := range []struct {
		name   string
		runner *fakeRunner
		want   string
	}{
		{"version fails", &fakeRunner{verErr: errors.New("exit status 1")}, "prisma --version failed: exit status 1"},
		{"version unreadable", &fakeRunner{version: `{"kind":"result","version":"8.0.0-rc.12"}`}, `cannot read the Prisma CLI major version from "{\"kind\":\"result\",\"version\":\"8.0.0-rc.12\"}" (supported: Prisma 5, 6 and 7)`},
		{"diff fails with stderr", &fakeRunner{version: "prisma : 6.1.0\n", err: errors.New("exit status 1"), errOut: "Error: P1012\n\nerror: Error validating: This line is invalid.\n"}, "prisma migrate diff failed: exit status 1: Error: P1012\n\nerror: Error validating: This line is invalid."},
		{"diff fails silently", &fakeRunner{version: "prisma : 6.1.0\n", err: errors.New("signal: killed")}, "prisma migrate diff failed: signal: killed"},
		{"empty output", &fakeRunner{version: "prisma : 7.1.0\n", out: "\n  \n"}, "prisma migrate diff produced no DDL (on Prisma 7 the prisma.config.ts must set datasource.url"},
	} {
		_, err := Prisma{Schema: schema, Run: tc.runner.run}.Load(context.Background())
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: err = %v", tc.name, err)
		}
	}
	if _, err := (Prisma{Schema: filepath.Join(t.TempDir(), "nope.prisma")}).Load(context.Background()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing schema: err = %v", err)
	}
}

func TestPrismaLoadMissingBinary(t *testing.T) {
	t.Parallel()
	schema := writeFile(t, "schema.prisma", postgresSchema)
	for _, bin := range []string{filepath.Join(t.TempDir(), "prisma"), "godwit-no-such-prisma"} {
		_, err := Prisma{Schema: schema, Bin: bin}.Load(context.Background())
		if err == nil || !strings.Contains(err.Error(), "prisma CLI not found") || !strings.Contains(err.Error(), "--prisma-bin / GODWIT_PRISMA_BIN") {
			t.Fatalf("%s: err = %v", bin, err)
		}
	}
}

func fakePrisma(t *testing.T, major int) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	script := "#!/bin/sh\necho \"$@\" >> " + log + "\ncase \"$*\" in\n  *--version) echo \"prisma : " + string(rune('0'+major)) + ".0.0\" ;;\n" +
		"  *\"migrate diff\"*) printf '%s' '" + strings.ReplaceAll(orderDDL, "'", "'\\''") + "' ;;\nesac\n"
	path := filepath.Join(dir, "prisma")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	return path
}

func TestPrismaLoadExec(t *testing.T) {
	t.Parallel()
	schema := writeFile(t, "schema.prisma", postgresSchema)
	bin := fakePrisma(t, 7)
	ddl, err := Prisma{Schema: schema, Bin: bin}.Load(context.Background())
	if err != nil || ddl != orderDDL {
		t.Fatalf("ddl = %q, err = %v", ddl, err)
	}
	calls, err := os.ReadFile(filepath.Join(filepath.Dir(bin), "calls.log"))
	if err != nil || string(calls) != "--version\nmigrate diff --from-empty --to-schema "+schema+" --script\n" {
		t.Fatalf("calls = %q, err = %v", calls, err)
	}
}

func TestPrismaIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	bin := os.Getenv("GODWIT_PRISMA_BIN")
	if bin == "" {
		if _, err := exec.LookPath("prisma"); err != nil {
			t.Skip("prisma is not on PATH and GODWIT_PRISMA_BIN is unset")
		}
		bin = "prisma"
	}
	fields := strings.Fields(bin)
	major, err := prismaMajor(context.Background(), Exec, fields)
	if err != nil {
		t.Fatal(err)
	}
	body := postgresSchema
	if major >= 7 {
		body = strings.ReplaceAll(body, "  url      = env(\"DATABASE_URL\")\n", "")
	}
	schema := writeFile(t, "schema.prisma", body)
	ddl, err := Prisma{Schema: schema, Bin: bin}.Load(context.Background())
	if err != nil || !strings.Contains(ddl, `CREATE TABLE "Order"`) || !strings.Contains(ddl, `"status" TEXT NOT NULL DEFAULT 'new'`) {
		t.Fatalf("ddl = %q, err = %v", ddl, err)
	}
}

type reply struct {
	out    string
	errOut string
	err    error
}

type fakeExec struct {
	calls   []string
	replies []reply
}

func (f *fakeExec) run(_ context.Context, name string, args ...string) ([]byte, []byte, error) {
	f.calls = append(f.calls, strings.Join(append([]string{name}, args...), " "))
	if len(f.replies) == 0 {
		return nil, nil, nil
	}
	r := f.replies[min(len(f.calls)-1, len(f.replies)-1)]

	return []byte(r.out), []byte(r.errOut), r.err
}

func (f *fakeExec) assert(t *testing.T, want ...string) {
	t.Helper()
	if strings.Join(f.calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls =\n%s\nwant\n%s", strings.Join(f.calls, "\n"), strings.Join(want, "\n"))
	}
}

func shellQuoted(s string) string {
	return strings.ReplaceAll(s, "'", "'\\''")
}

func shellFixture(t *testing.T, name, body string) (string, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	bin := filepath.Join(dir, name)
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho \"$@\" >> "+log+"\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}

	return bin, log
}

func TestCommandLoad(t *testing.T) {
	t.Parallel()
	runner := &fakeExec{replies: []reply{{out: orderDDL}}}
	ddl, err := Command{Argv: []string{"go", "run", "./cmd/schema"}, Run: runner.run}.Load(context.Background())
	if err != nil || ddl != orderDDL {
		t.Fatalf("ddl = %q, err = %v", ddl, err)
	}
	runner.assert(t, "go run ./cmd/schema")
}

func TestCommandLoadErrors(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		argv  []string
		reply reply
		want  string
	}{
		{"no command", nil, reply{}, "no command configured; pass --exec or set schema_source.command"},
		{"empty output", []string{"dump"}, reply{out: " \n"}, "dump produced no DDL on stdout"},
		{"exit with stderr", []string{"dump", "--all"}, reply{err: errors.New("exit status 2"), errOut: "cannot open db\n"}, "dump --all failed: exit status 2: cannot open db"},
		{"exit without stderr", []string{"dump"}, reply{err: errors.New("signal: killed")}, "dump failed: signal: killed"},
		{"missing binary", []string{"dump"}, reply{err: exec.ErrNotFound}, "dump not found (executable file not found in $PATH); check --exec / schema_source.command"},
	} {
		runner := &fakeExec{replies: []reply{tc.reply}}
		_, err := Command{Argv: tc.argv, Run: runner.run}.Load(context.Background())
		if err == nil || err.Error() != tc.want {
			t.Fatalf("%s: err = %v", tc.name, err)
		}
	}
}

func TestCommandLoadExec(t *testing.T) {
	t.Parallel()
	bin, log := shellFixture(t, "dump", "printf '%s' '"+shellQuoted(orderDDL)+"'\n")
	ddl, err := Command{Argv: []string{bin, "--all"}}.Load(context.Background())
	if err != nil || ddl != orderDDL {
		t.Fatalf("ddl = %q, err = %v", ddl, err)
	}
	if calls, err := os.ReadFile(log); err != nil || string(calls) != "--all\n" {
		t.Fatalf("calls = %q, err = %v", calls, err)
	}
}

func TestGormLoad(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		bin  string
		want string
	}{
		{"", "go run ./cmd/schema"},
		{"go1.26.0", "go1.26.0 run ./cmd/schema"},
		{"/usr/local/go/bin/go  -C  .", "/usr/local/go/bin/go -C . run ./cmd/schema"},
	} {
		runner := &fakeExec{replies: []reply{{out: orderDDL}}}
		ddl, err := Gorm{Pkg: "./cmd/schema", Bin: tc.bin, Run: runner.run}.Load(context.Background())
		if err != nil || ddl != orderDDL {
			t.Fatalf("%s: ddl = %q, err = %v", tc.bin, ddl, err)
		}
		runner.assert(t, tc.want)
	}
}

func TestGormLoadErrors(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		pkg   string
		reply reply
		want  string
	}{
		{"no package", " ", reply{}, "no Go package configured; pass --gorm ./cmd/schema or set schema_source.path"},
		{"compile failure", "./cmd/schema", reply{err: errors.New("exit status 1"), errOut: "cmd/schema/main.go:9:2: undefined: Order\n"}, "go run ./cmd/schema failed: exit status 1: cmd/schema/main.go:9:2: undefined: Order (" + gormExample + ")"},
		{"empty output", "./cmd/schema", reply{out: "\n"}, "go run ./cmd/schema produced no DDL: " + gormExample},
		{"missing toolchain", "./cmd/schema", reply{err: exec.ErrNotFound}, "the Go toolchain was not found (executable file not found in $PATH); install Go or point --go-bin / GODWIT_GO_BIN at it"},
	} {
		runner := &fakeExec{replies: []reply{tc.reply}}
		_, err := Gorm{Pkg: tc.pkg, Run: runner.run}.Load(context.Background())
		if err == nil || err.Error() != tc.want {
			t.Fatalf("%s: err = %v", tc.name, err)
		}
	}
}

func TestGormLoadExec(t *testing.T) {
	t.Parallel()
	bin, log := shellFixture(t, "go", "printf '%s' '"+shellQuoted(orderDDL)+"'\n")
	ddl, err := Gorm{Pkg: "./cmd/schema", Bin: bin}.Load(context.Background())
	if err != nil || ddl != orderDDL {
		t.Fatalf("ddl = %q, err = %v", ddl, err)
	}
	if calls, err := os.ReadFile(log); err != nil || string(calls) != "run ./cmd/schema\n" {
		t.Fatalf("calls = %q, err = %v", calls, err)
	}
}

func TestGormLoadMissingBinary(t *testing.T) {
	t.Parallel()
	_, err := Gorm{Pkg: "./cmd/schema", Bin: "godwit-no-such-go"}.Load(context.Background())
	if err == nil || !strings.Contains(err.Error(), "the Go toolchain was not found") || !strings.Contains(err.Error(), "--go-bin / GODWIT_GO_BIN") {
		t.Fatalf("err = %v", err)
	}
}

const managePy = `#!/usr/bin/env python
import os
import sys


def main():
    os.environ.setdefault("DJANGO_SETTINGS_MODULE", "shop.settings")
`

const djangoSettings = `# DATABASES = {"default": {"ENGINE": "django.db.backends.mysql"}}
DATABASES = {
    "default": {
        "ENGINE": "django.db.backends.postgresql",
        "NAME": "shop",
        "OPTIONS": {"connect_timeout": 5},
    },
    "reporting": {
        "ENGINE": "django.db.backends.mysql",
    },
}
`

func djangoProject(t *testing.T, manage, settings string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "manage.py")
	if err := os.WriteFile(path, []byte(manage), 0o600); err != nil {
		t.Fatal(err)
	}
	if settings == "" {
		return path
	}
	if err := os.MkdirAll(filepath.Join(dir, "shop"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "shop", "settings.py"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

const showmigrations = `[X]  contenttypes.0001_initial
[X]  orders.0001_initial
[ ]  orders.0002_status
[X]  auth.0001_initial
(no migrations)
`

func TestDjangoLoadKeepsPlanOrder(t *testing.T) {
	t.Parallel()
	manage := djangoProject(t, managePy, djangoSettings)
	runner := &fakeExec{replies: []reply{
		{out: showmigrations},
		{out: "BEGIN;\nCREATE TABLE django_content_type (id serial);\nCOMMIT;\n"},
		{out: "begin;\nCREATE TABLE orders (id serial);\ncommit;\n"},
		{out: "BEGIN;\nALTER TABLE orders ADD COLUMN status text;\nCOMMIT;\n"},
		{out: "CREATE TABLE auth_user (id serial);\n"},
	}}
	ddl, err := Django{ManagePy: manage, Run: runner.run}.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := "CREATE TABLE django_content_type (id serial);\n\nCREATE TABLE orders (id serial);\n\nALTER TABLE orders ADD COLUMN status text;\n\nCREATE TABLE auth_user (id serial);\n\n"
	if ddl != want {
		t.Fatalf("ddl = %q, want %q", ddl, want)
	}
	runner.assert(t,
		"python "+manage+" showmigrations --plan --no-color",
		"python "+manage+" sqlmigrate contenttypes 0001_initial --no-color",
		"python "+manage+" sqlmigrate orders 0001_initial --no-color",
		"python "+manage+" sqlmigrate orders 0002_status --no-color",
		"python "+manage+" sqlmigrate auth 0001_initial --no-color",
	)
}

func TestDjangoLoadForwardsSettingsAndDatabase(t *testing.T) {
	t.Parallel()
	manage := djangoProject(t, "print('no settings module here')\n", djangoSettings)
	runner := &fakeExec{replies: []reply{{out: "[X]  orders.0001_initial\n"}, {out: "CREATE TABLE orders (id serial);\n"}}}
	source := Django{ManagePy: manage, Bin: "python3  -W  ignore", Settings: "shop.settings", Database: "default", Run: runner.run}
	if _, err := source.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	runner.assert(t,
		"python3 -W ignore "+manage+" showmigrations --plan --no-color --settings=shop.settings --database=default",
		"python3 -W ignore "+manage+" sqlmigrate orders 0001_initial --no-color --settings=shop.settings --database=default",
	)
}

func TestDjangoLoadRefusesOtherEnginesBeforeRunning(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		manage   string
		settings string
		database string
		want     string
	}{
		{"mysql", managePy, djangoSettings, "reporting", `DATABASES["reporting"]["ENGINE"] is "django.db.backends.mysql"; godwit targets PostgreSQL only (django.db.backends.postgresql)`},
		{"sqlite", managePy, "DATABASES = {'default': {'ENGINE': 'django.db.backends.sqlite3'}}\n", "", `DATABASES["default"]["ENGINE"] is "django.db.backends.sqlite3"`},
		{"unknown alias", managePy, djangoSettings, "analytics", `DATABASES has no "analytics" alias`},
		{"no databases", managePy, "SECRET_KEY = 'x'\n", "", "no closed DATABASES dict"},
		{"unclosed databases", managePy, "DATABASES = {\n    'default': {'ENGINE': 'django.db.backends.postgresql'}\n", "", "no closed DATABASES dict"},
		{"no engine", managePy, "DATABASES = {'default': {'NAME': 'shop'}}\n", "", `DATABASES["default"] has no ENGINE`},
		{"no settings module", "print('hello')\n", djangoSettings, "", "sets no DJANGO_SETTINGS_MODULE; set it there or in the environment"},
		{"no settings file", managePy, "", "", "cannot read the Django settings of shop.settings"},
	} {
		manage := djangoProject(t, tc.manage, tc.settings)
		runner := &fakeExec{}
		_, err := Django{ManagePy: manage, Database: tc.database, Run: runner.run}.Load(context.Background())
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: err = %v", tc.name, err)
		}
		if len(runner.calls) != 0 {
			t.Fatalf("%s: manage.py ran %d time(s)", tc.name, len(runner.calls))
		}
	}
}

func TestDjangoLoadErrors(t *testing.T) {
	t.Parallel()
	manage := djangoProject(t, managePy, djangoSettings)
	for _, tc := range []struct {
		name    string
		replies []reply
		want    string
	}{
		{"showmigrations fails", []reply{{err: errors.New("exit status 1"), errOut: "django.core.exceptions.ImproperlyConfigured\n"}}, "manage.py showmigrations --plan failed: exit status 1: django.core.exceptions.ImproperlyConfigured"},
		{"no python", []reply{{err: exec.ErrNotFound}}, "the Python interpreter was not found (executable file not found in $PATH); install Python or point --python-bin / GODWIT_PYTHON_BIN at it"},
		{"empty plan", []reply{{out: "(no migrations)\n"}}, "manage.py showmigrations --plan listed no migrations in " + manage},
		{"sqlmigrate fails", []reply{{out: "[X]  orders.0001_initial\n"}, {err: errors.New("exit status 1"), errOut: "could not connect to server\n"}}, "manage.py sqlmigrate orders 0001_initial failed: exit status 1: could not connect to server"},
		{"empty sql", []reply{{out: "[X]  orders.0001_initial\n"}, {out: "BEGIN;\nCOMMIT;\n"}}, "manage.py sqlmigrate produced no DDL for the 1 migration(s) of " + manage},
	} {
		_, err := Django{ManagePy: manage, Run: (&fakeExec{replies: tc.replies}).run}.Load(context.Background())
		if err == nil || err.Error() != tc.want {
			t.Fatalf("%s: err = %v", tc.name, err)
		}
	}
	if _, err := (Django{ManagePy: " "}).Load(context.Background()); err == nil || err.Error() != "no Django project configured; pass --django manage.py or set schema_source.path" {
		t.Fatalf("empty project: err = %v", err)
	}
	if _, err := (Django{ManagePy: filepath.Join(t.TempDir(), "manage.py")}).Load(context.Background()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing manage.py: err = %v", err)
	}
}

func TestDjangoLoadExec(t *testing.T) {
	t.Parallel()
	manage := djangoProject(t, managePy, djangoSettings)
	bin, log := shellFixture(t, "python", "case \"$2\" in\n  showmigrations) echo '[X]  orders.0001_initial' ;;\n"+
		"  sqlmigrate) printf 'BEGIN;\\nCREATE TABLE orders (id serial);\\nCOMMIT;\\n' ;;\nesac\n")
	ddl, err := Django{ManagePy: manage, Bin: bin}.Load(context.Background())
	if err != nil || ddl != "CREATE TABLE orders (id serial);\n\n" {
		t.Fatalf("ddl = %q, err = %v", ddl, err)
	}
	want := manage + " showmigrations --plan --no-color\n" + manage + " sqlmigrate orders 0001_initial --no-color\n"
	if calls, err := os.ReadFile(log); err != nil || string(calls) != want {
		t.Fatalf("calls = %q, err = %v", calls, err)
	}
}

func TestGormIntegration(t *testing.T) {
	bin, pkg := os.Getenv("GODWIT_GORM_BIN"), os.Getenv("GODWIT_GORM_PKG")
	if bin == "" || pkg == "" {
		t.Skip("GODWIT_GORM_BIN and GODWIT_GORM_PKG are not both set")
	}
	ddl, err := Gorm{Pkg: pkg, Bin: bin}.Load(context.Background())
	if err != nil || !strings.Contains(strings.ToUpper(ddl), "CREATE TABLE") {
		t.Fatalf("ddl = %q, err = %v", ddl, err)
	}
}

func TestDjangoIntegration(t *testing.T) {
	bin, manage := os.Getenv("GODWIT_DJANGO_BIN"), os.Getenv("GODWIT_DJANGO_MANAGE_PY")
	if bin == "" || manage == "" {
		t.Skip("GODWIT_DJANGO_BIN and GODWIT_DJANGO_MANAGE_PY are not both set")
	}
	ddl, err := Django{ManagePy: manage, Bin: bin, Database: os.Getenv("GODWIT_DJANGO_DATABASE")}.Load(context.Background())
	if err != nil || !strings.Contains(strings.ToUpper(ddl), "CREATE TABLE") {
		t.Fatalf("ddl = %q, err = %v", ddl, err)
	}
}
