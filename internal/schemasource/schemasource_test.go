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
