package cli

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
)

func diffStub() *stubService {
	return &stubService{diff: &godwitv1.DiffResponse{
		Target:  "app",
		UpSql:   "ALTER TABLE \"public\".\"t\" DROP COLUMN \"a\";\nCREATE INDEX CONCURRENTLY t_b_idx ON public.t USING btree (b);",
		DownSql: "DROP INDEX CONCURRENTLY \"public\".\"t_b_idx\";\nALTER TABLE \"public\".\"t\" ADD COLUMN \"a\" integer;",
		Statements: []*godwitv1.PlannedStatement{
			{Sql: "ALTER TABLE \"public\".\"t\" DROP COLUMN \"a\"", Hazards: []*godwitv1.PlannedHazard{{Code: "H003", Detail: "DROP COLUMN is destructive", Recipe: "-- expand then contract:\n-- drop t.a later"}}},
			{Sql: "CREATE INDEX CONCURRENTLY t_b_idx ON public.t USING btree (b)", NoTx: true},
		},
		Drift:    "+ column public.t.extra integer null=YES default=<none>",
		Observed: &godwitv1.PlanObservation{HistoryHash: "h", SchemaFingerprint: "f", AppliedCount: 2, NewestApplied: 20260901120002, At: timestamppb.New(time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC))},
	}}
}

func schemaFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "schema.sql")
	if err := os.WriteFile(path, []byte("CREATE TABLE t (id int, b int);\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

func TestDiffWritesMigration(t *testing.T) {
	diffNow = func() time.Time { return time.Date(2026, 9, 2, 10, 30, 0, 0, time.UTC) }
	defer func() { diffNow = time.Now }()
	stub := diffStub()
	url := startStub(t, stub)
	dir, schema := t.TempDir(), schemaFile(t)

	code, out, errOut := runCLI("diff", "--server", url, "--token", "tok", "--target", "app", "--schema", schema, "--name", "drop_a", "--dir", dir)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
	want := "drift (the target's live schema, not its history, is the starting point):\n" +
		"  + column public.t.extra integer null=YES default=<none>\n" +
		"app -> " + schema + ": 2 statement(s)\n" +
		"  [0] tx    ALTER TABLE \"public\".\"t\" DROP COLUMN \"a\"\n" +
		"        hazard H003: DROP COLUMN is destructive\n" +
		"          -- expand then contract:\n" +
		"          -- drop t.a later\n" +
		"  [1] no-tx CREATE INDEX CONCURRENTLY t_b_idx ON public.t USING btree (b)\n" +
		"\n-- up\n" + stub.diff.UpSql + "\n-- down\n" + stub.diff.DownSql + "\n" +
		"wrote " + filepath.Join(dir, "20260902103000_drop_a.up.sql") + "\n" +
		"wrote " + filepath.Join(dir, "20260902103000_drop_a.down.sql") + "\n"
	if out != want {
		t.Fatalf("out = %q\nwant %q", out, want)
	}
	if stub.diffed.Target != "app" || stub.diffed.Schema != "CREATE TABLE t (id int, b int);\n" || stub.auth != "Bearer tok" {
		t.Fatalf("request = %+v, auth = %q", stub.diffed, stub.auth)
	}
	up, err := os.ReadFile(filepath.Join(dir, "20260902103000_drop_a.up.sql"))
	if err != nil || string(up) != stub.diff.UpSql+"\n" {
		t.Fatalf("up = %q, err = %v", up, err)
	}
	down, err := os.ReadFile(filepath.Join(dir, "20260902103000_drop_a.down.sql"))
	if err != nil || string(down) != stub.diff.DownSql+"\n" {
		t.Fatalf("down = %q, err = %v", down, err)
	}
	if _, err := migrationFiles(dir); err != nil {
		t.Fatalf("generated files must load: %v", err)
	}
}

func TestDiffDryRunAndJSON(t *testing.T) {
	t.Parallel()
	stub := diffStub()
	url := startStub(t, stub)
	dir, schema := t.TempDir(), schemaFile(t)

	code, out, errOut := runCLI("diff", "--server", url, "--target", "app", "--schema", schema, "--dir", dir, "--dry-run")
	if code != 0 || !strings.Contains(out, "-- up\n") || strings.Contains(out, "wrote ") {
		t.Fatalf("code = %d, out = %q, stderr = %s", code, out, errOut)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("dry run wrote %d file(s)", len(entries))
	}

	code, out, errOut = runCLI("diff", "--server", url, "--target", "app", "--schema", schema, "--dir", dir, "--name", "drop_a", "--json")
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
	m := decodeJSON(t, out)
	stmts := m["statements"].([]any)
	files := m["files"].([]any)
	if m["target"] != "app" || m["changed"] != true || m["up_sql"] != stub.diff.UpSql || m["down_sql"] != stub.diff.DownSql ||
		len(stmts) != 2 || stmts[1].(map[string]any)["mode"] != "no-tx" || m["drift"] != stub.diff.Drift || len(files) != 2 ||
		m["observed"].(map[string]any)["applied_count"] != float64(2) {
		t.Fatalf("json = %v", m)
	}
	hz := stmts[0].(map[string]any)["hazards"].([]any)[0].(map[string]any)
	if hz["code"] != "H003" || hz["recipe"] != "-- expand then contract:\n-- drop t.a later" {
		t.Fatalf("hazard = %v", hz)
	}
	if _, err := os.Stat(files[0].(string)); err != nil {
		t.Fatal(err)
	}
}

func TestDiffNoChanges(t *testing.T) {
	t.Parallel()
	stub := &stubService{diff: &godwitv1.DiffResponse{Target: "app", Observed: &godwitv1.PlanObservation{AppliedCount: 2}}}
	url := startStub(t, stub)
	dir, schema := t.TempDir(), schemaFile(t)

	code, out, errOut := runCLI("diff", "--server", url, "--target", "app", "--schema", schema, "--dir", dir, "--name", "noop")
	if code != 0 || out != "no changes: app already matches "+schema+"\n" {
		t.Fatalf("code = %d, out = %q, stderr = %s", code, out, errOut)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("no-op diff wrote %d file(s)", len(entries))
	}

	code, out, _ = runCLI("diff", "--server", url, "--target", "app", "--schema", schema, "--dir", dir, "--name", "noop", "--json")
	if m := decodeJSON(t, out); code != 0 || m["changed"] != false || len(m["files"].([]any)) != 0 || len(m["statements"].([]any)) != 0 {
		t.Fatalf("code = %d, json = %v", code, m)
	}
}

func TestDiffErrors(t *testing.T) {
	stub := diffStub()
	url := startStub(t, stub)
	dir, schema := t.TempDir(), schemaFile(t)
	base := []string{"diff", "--server", url, "--target", "app", "--schema", schema, "--dir", dir}

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no target", []string{"diff", "--server", url, "--schema", schema}, "--target"},
		{"no schema", []string{"diff", "--server", url, "--target", "app"}, "one of --schema, --prisma, --exec, --gorm, --django, or schema_source in godwit.yaml is required"},
		{"schema and prisma", []string{"diff", "--server", url, "--target", "app", "--schema", schema, "--prisma", "schema.prisma"}, "--schema and --prisma are exclusive: one desired schema per diff"},
		{"three sources", []string{"diff", "--server", url, "--target", "app", "--schema", schema, "--exec", "dump", "--django", "manage.py"}, "--schema, --exec and --django are exclusive: one desired schema per diff"},
		{"empty prisma bin", []string{"diff", "--server", url, "--target", "app", "--prisma", "schema.prisma", "--prisma-bin", " "}, "--prisma-bin (or GODWIT_PRISMA_BIN) must name the Prisma CLI"},
		{"empty exec", []string{"diff", "--server", url, "--target", "app", "--exec", " "}, "--exec (or schema_source.command) must name a command that prints the desired schema as DDL"},
		{"empty go bin", []string{"diff", "--server", url, "--target", "app", "--gorm", "./cmd/schema", "--go-bin", " "}, "--go-bin (or GODWIT_GO_BIN) must name the Go toolchain"},
		{"empty python bin", []string{"diff", "--server", url, "--target", "app", "--django", "manage.py", "--python-bin", " "}, "--python-bin (or GODWIT_PYTHON_BIN) must name the Python interpreter"},
		{"no name", base, "--name is required"},
		{"bad name", append(base, "--name", "Drop-A"), "snake_case"},
		{"missing schema file", []string{"diff", "--server", url, "--target", "app", "--schema", filepath.Join(dir, "nope.sql"), "--name", "x"}, "no such file"},
		{"bad dir", []string{"diff", "--server", url, "--target", "app", "--schema", schema, "--name", "x", "--dir", filepath.Join(dir, "missing")}, "no such file"},
	} {
		if code, _, errOut := runCLI(tc.args...); code != 1 || !strings.Contains(errOut, tc.want) {
			t.Fatalf("%s: code = %d, stderr = %q", tc.name, code, errOut)
		}
	}

	readOnly := t.TempDir()
	if err := os.Chmod(readOnly, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(readOnly, 0o700) })
	if code, _, errOut := runCLI("diff", "--server", url, "--target", "app", "--schema", schema, "--name", "x", "--dir", readOnly); code != 1 ||
		!strings.Contains(errOut, "permission denied") {
		t.Fatalf("read-only dir: code = %d, stderr = %q", code, errOut)
	}

	stub.err = connect.NewError(connect.CodeInvalidArgument, errors.New("desired schema failed to apply: type nosuchtype does not exist"))
	if code, _, errOut := runCLI(append(base, "--name", "x")...); code != 1 || !strings.Contains(errOut, "type nosuchtype does not exist") {
		t.Fatalf("service error: code = %d, stderr = %q", code, errOut)
	}
}

const prismaSchema = "datasource db {\n  provider = \"postgresql\"\n  url      = env(\"DATABASE_URL\")\n}\n\nmodel T {\n  id Int @id\n  b  Int\n}\n"

func fakePrisma(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	script := "#!/bin/sh\necho \"$@\" >> " + log + "\ncase \"$*\" in\n  *--version) echo 'prisma : 6.19.3' ;;\n" +
		"  *\"migrate diff\"*) printf '%s' '-- CreateTable\nCREATE TABLE \"T\" (\"id\" INTEGER NOT NULL, \"b\" INTEGER NOT NULL);\n' ;;\nesac\n"
	bin := filepath.Join(dir, "prisma")
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	return bin, log
}

func TestDiffPrisma(t *testing.T) {
	diffNow = func() time.Time { return time.Date(2026, 9, 2, 10, 30, 0, 0, time.UTC) }
	defer func() { diffNow = time.Now }()
	stub := diffStub()
	url := startStub(t, stub)
	dir := t.TempDir()
	schema := filepath.Join(t.TempDir(), "schema.prisma")
	if err := os.WriteFile(schema, []byte(prismaSchema), 0o600); err != nil {
		t.Fatal(err)
	}
	bin, log := fakePrisma(t)

	code, out, errOut := runCLI("diff", "--server", url, "--target", "app", "--prisma", schema, "--prisma-bin", bin, "--name", "sync_prisma", "--dir", dir)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
	if stub.diffed.Schema != "-- CreateTable\nCREATE TABLE \"T\" (\"id\" INTEGER NOT NULL, \"b\" INTEGER NOT NULL);\n" {
		t.Fatalf("schema sent = %q", stub.diffed.Schema)
	}
	if !strings.Contains(out, "app -> "+schema+": 2 statement(s)\n") || !strings.Contains(out, "wrote "+filepath.Join(dir, "20260902103000_sync_prisma.up.sql")+"\n") {
		t.Fatalf("out = %q", out)
	}
	calls, err := os.ReadFile(log)
	if err != nil || string(calls) != "--version\nmigrate diff --from-empty --to-schema-datamodel "+schema+" --script\n" {
		t.Fatalf("calls = %q, err = %v", calls, err)
	}

	t.Setenv("GODWIT_PRISMA_BIN", bin)
	if code, _, errOut := runCLI("diff", "--server", url, "--target", "app", "--prisma", schema, "--dir", dir, "--dry-run"); code != 0 {
		t.Fatalf("env bin: code = %d, stderr = %s", code, errOut)
	}
	if calls, _ := os.ReadFile(log); strings.Count(string(calls), "--version\n") != 2 {
		t.Fatalf("calls = %q", calls)
	}

	mysql := filepath.Join(dir, "mysql.prisma")
	if err := os.WriteFile(mysql, []byte(strings.Replace(prismaSchema, "postgresql", "mysql", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, errOut = runCLI("diff", "--server", url, "--target", "app", "--prisma", mysql, "--dir", dir, "--dry-run")
	if code != 1 || !strings.Contains(errOut, `datasource provider is "mysql"; godwit targets PostgreSQL only`) {
		t.Fatalf("mysql: code = %d, stderr = %q", code, errOut)
	}
}

func configRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o750); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		path := filepath.Join(repo, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	return repo
}

func chdir(t *testing.T, dir string) string {
	t.Helper()
	t.Chdir(dir)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	return wd
}

func TestDiffSchemaSourceFromConfig(t *testing.T) {
	diffNow = func() time.Time { return time.Date(2026, 9, 2, 10, 30, 0, 0, time.UTC) }
	defer func() { diffNow = time.Now }()
	stub := diffStub()
	url := startStub(t, stub)
	repo := configRepo(t, map[string]string{
		"a/godwit.yaml":      "target: app\nschema_source:\n  kind: file\n  path: db/schema.sql\n",
		"a/db/schema.sql":    "CREATE TABLE a (id int);\n",
		"a/migrations/.keep": "",
		"b/godwit.yaml":      "target: b_app\nschema_source:\n  kind: file\n  path: schema.sql\n",
		"b/schema.sql":       "CREATE TABLE b (id int);\n",
		"b/sub/keep":         "",
	})

	wd := chdir(t, filepath.Join(repo, "a"))
	code, out, errOut := runCLI("diff", "--server", url, "--name", "add_status")
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
	if stub.diffed.Target != "app" || stub.diffed.Schema != "CREATE TABLE a (id int);\n" {
		t.Fatalf("request = %+v", stub.diffed)
	}
	if !strings.Contains(out, "app -> "+filepath.Join(wd, "db/schema.sql")+": 2 statement(s)\n") {
		t.Fatalf("out = %q", out)
	}
	if _, err := os.Stat(filepath.Join(wd, "migrations", "20260902103000_add_status.up.sql")); err != nil {
		t.Fatal(err)
	}

	wd = chdir(t, filepath.Join(repo, "b", "sub"))
	if code, _, errOut := runCLI("diff", "--server", url, "--dry-run"); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
	if stub.diffed.Target != "b_app" || stub.diffed.Schema != "CREATE TABLE b (id int);\n" {
		t.Fatalf("request = %+v", stub.diffed)
	}
	if _, err := os.Stat(filepath.Join(wd, "..", "schema.sql")); err != nil {
		t.Fatal(err)
	}
}

func TestDiffSchemaSourcePrismaFromConfig(t *testing.T) {
	stub := diffStub()
	url := startStub(t, stub)
	bin, log := fakePrisma(t)
	repo := configRepo(t, map[string]string{
		"godwit.yaml":   "target: app\nschema_source:\n  kind: prisma\n  path: schema.prisma\n  bin: " + bin + "\n",
		"schema.prisma": prismaSchema,
	})
	chdir(t, repo)

	code, _, errOut := runCLI("diff", "--server", url, "--dry-run")
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
	if !strings.HasPrefix(stub.diffed.Schema, "-- CreateTable\n") {
		t.Fatalf("schema sent = %q", stub.diffed.Schema)
	}
	if calls, _ := os.ReadFile(log); !strings.Contains(string(calls), "migrate diff") {
		t.Fatalf("calls = %q", calls)
	}

	flagBin, flagLog := fakePrisma(t)
	if code, _, errOut := runCLI("diff", "--server", url, "--dry-run", "--prisma-bin", flagBin); code != 0 {
		t.Fatalf("flag bin: code = %d, stderr = %s", code, errOut)
	}
	if calls, err := os.ReadFile(flagLog); err != nil || !strings.Contains(string(calls), "migrate diff") {
		t.Fatalf("--prisma-bin must win over schema_source.bin: calls = %q, err = %v", calls, err)
	}

	envBin, envLog := fakePrisma(t)
	chdir(t, configRepo(t, map[string]string{
		"godwit.yaml":   "target: app\nschema_source:\n  kind: prisma\n  path: schema.prisma\n",
		"schema.prisma": prismaSchema,
	}))
	t.Setenv("GODWIT_PRISMA_BIN", envBin)
	if code, _, errOut := runCLI("diff", "--server", url, "--dry-run"); code != 0 {
		t.Fatalf("env bin: code = %d, stderr = %s", code, errOut)
	}
	if calls, err := os.ReadFile(envLog); err != nil || !strings.Contains(string(calls), "migrate diff") {
		t.Fatalf("calls = %q, err = %v", calls, err)
	}
}

func TestDiffSchemaSourceConfigErrors(t *testing.T) {
	url := startStub(t, diffStub())

	for name, tc := range map[string]struct{ body, want string }{
		"missing path":    {"schema_source:\n  kind: gorm\n", "schema_source.path is required for kind gorm"},
		"missing command": {"schema_source:\n  kind: command\n", "--exec (or schema_source.command) must name a command that prints the desired schema as DDL"},
		"unknown kind":    {"schema_source:\n  kind: sqlite\n", "schema_source.kind \"sqlite\" is not one of file, prisma, gorm, django, command"},
	} {
		chdir(t, configRepo(t, map[string]string{"godwit.yaml": "target: app\n" + tc.body}))
		if code, _, errOut := runCLI("diff", "--server", url, "--dry-run"); code != 1 || !strings.Contains(errOut, tc.want) {
			t.Fatalf("%s: code = %d, stderr = %q", name, code, errOut)
		}
	}
}

func fakeBin(t *testing.T, name, body string) (string, string) {
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

func fakePython(t *testing.T) (string, string) {
	t.Helper()

	return fakeBin(t, "python", "case \"$2\" in\n  showmigrations) echo '[X]  orders.0001_initial' ;;\n"+
		"  sqlmigrate) printf 'BEGIN;\\nCREATE TABLE t (id int);\\nCOMMIT;\\n' ;;\nesac\n")
}

const djangoSettings = "DATABASES = {'default': {'ENGINE': 'django.db.backends.postgresql', 'NAME': 'app'}}\n"

func djangoFiles(managePy string) map[string]string {
	return map[string]string{
		managePy:           "import os\nos.environ.setdefault('DJANGO_SETTINGS_MODULE', 'shop.settings')\n",
		"shop/settings.py": djangoSettings,
		"shop/__init__.py": "",
		"db/.keep":         "",
	}
}

func TestDiffExec(t *testing.T) {
	stub := diffStub()
	url := startStub(t, stub)
	bin, log := fakeBin(t, "dump", "echo 'CREATE TABLE t (id int);'\n")

	code, out, errOut := runCLI("diff", "--server", url, "--target", "app", "--exec", bin+"  --all", "--dry-run")
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
	if stub.diffed.Schema != "CREATE TABLE t (id int);\n" {
		t.Fatalf("schema sent = %q", stub.diffed.Schema)
	}
	if !strings.Contains(out, "app -> "+bin+" --all: 2 statement(s)\n") {
		t.Fatalf("out = %q", out)
	}
	if calls, err := os.ReadFile(log); err != nil || string(calls) != "--all\n" {
		t.Fatalf("calls = %q, err = %v", calls, err)
	}
}

func TestDiffGorm(t *testing.T) {
	stub := diffStub()
	url := startStub(t, stub)
	bin, log := fakeBin(t, "go", "echo 'CREATE TABLE t (id int);'\n")

	code, out, errOut := runCLI("diff", "--server", url, "--target", "app", "--gorm", "./cmd/schema", "--go-bin", bin, "--dry-run")
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
	if stub.diffed.Schema != "CREATE TABLE t (id int);\n" || !strings.Contains(out, "app -> go run ./cmd/schema: 2 statement(s)\n") {
		t.Fatalf("schema sent = %q, out = %q", stub.diffed.Schema, out)
	}
	if calls, err := os.ReadFile(log); err != nil || string(calls) != "run ./cmd/schema\n" {
		t.Fatalf("calls = %q, err = %v", calls, err)
	}
}

func TestDiffDjango(t *testing.T) {
	stub := diffStub()
	url := startStub(t, stub)
	repo := configRepo(t, djangoFiles("manage.py"))
	manage := filepath.Join(repo, "manage.py")
	bin, log := fakePython(t)

	code, out, errOut := runCLI("diff", "--server", url, "--target", "app", "--django", manage, "--python-bin", bin, "--django-database", "default", "--dry-run")
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
	if stub.diffed.Schema != "CREATE TABLE t (id int);\n\n" || !strings.Contains(out, "app -> "+manage+": 2 statement(s)\n") {
		t.Fatalf("schema sent = %q, out = %q", stub.diffed.Schema, out)
	}
	want := manage + " showmigrations --plan --no-color --database=default\n" + manage + " sqlmigrate orders 0001_initial --no-color --database=default\n"
	if calls, err := os.ReadFile(log); err != nil || string(calls) != want {
		t.Fatalf("calls = %q, err = %v", calls, err)
	}

	t.Setenv("DJANGO_SETTINGS_MODULE", "shop.settings")
	envBin, envLog := fakePython(t)
	if code, _, errOut := runCLI("diff", "--server", url, "--target", "app", "--django", manage, "--python-bin", envBin, "--dry-run"); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
	want = manage + " showmigrations --plan --no-color --settings=shop.settings\n" + manage + " sqlmigrate orders 0001_initial --no-color --settings=shop.settings\n"
	if calls, err := os.ReadFile(envLog); err != nil || string(calls) != want {
		t.Fatalf("calls = %q, err = %v", calls, err)
	}
}

func TestDiffSchemaSourceKindsFromConfig(t *testing.T) {
	stub := diffStub()
	url := startStub(t, stub)
	dump, dumpLog := fakeBin(t, "dump", "echo 'CREATE TABLE t (id int);'\n")
	goBin, goLog := fakeBin(t, "go", "echo 'CREATE TABLE t (id int);'\n")
	python, pythonLog := fakePython(t)

	for _, tc := range []struct {
		name  string
		files map[string]string
		log   string
		want  func(repo string) string
	}{
		{
			"command",
			map[string]string{"godwit.yaml": "target: app\nschema_source:\n  kind: command\n  command: [\"" + dump + "\", \"--all\"]\n"},
			dumpLog, func(string) string { return "--all\n" },
		},
		{
			"gorm",
			map[string]string{"godwit.yaml": "target: app\nschema_source:\n  kind: gorm\n  path: ./cmd/schema\n  bin: " + goBin + "\n"},
			goLog, func(repo string) string { return "run " + filepath.Join(repo, "cmd", "schema") + "\n" },
		},
	} {
		repo := chdir(t, configRepo(t, tc.files))
		if code, _, errOut := runCLI("diff", "--server", url, "--dry-run"); code != 0 {
			t.Fatalf("%s: code = %d, stderr = %s", tc.name, code, errOut)
		}
		if calls, err := os.ReadFile(tc.log); err != nil || string(calls) != tc.want(repo) {
			t.Fatalf("%s: calls = %q, err = %v", tc.name, calls, err)
		}
		if stub.diffed.Schema != "CREATE TABLE t (id int);\n" {
			t.Fatalf("%s: schema sent = %q", tc.name, stub.diffed.Schema)
		}
	}

	files := djangoFiles("manage.py")
	files["godwit.yaml"] = "target: app\nschema_source:\n  kind: django\n  path: manage.py\n  bin: " + python + "\n"
	repo := chdir(t, configRepo(t, files))
	if code, _, errOut := runCLI("diff", "--server", url, "--dry-run"); code != 0 {
		t.Fatalf("django: code = %d, stderr = %s", code, errOut)
	}
	manage := filepath.Join(repo, "manage.py")
	want := manage + " showmigrations --plan --no-color\n" + manage + " sqlmigrate orders 0001_initial --no-color\n"
	if calls, err := os.ReadFile(pythonLog); err != nil || string(calls) != want {
		t.Fatalf("django: calls = %q, err = %v", calls, err)
	}
}

func TestDiffSendsTheMigrationDirectory(t *testing.T) {
	t.Parallel()
	stub := diffStub()
	stub.diff.RepeatableObjects = []string{"public.t_totals"}
	url := startStub(t, stub)
	dir, schema := t.TempDir(), schemaFile(t)
	for name, body := range map[string]string{
		"20260901120001_t.up.sql":   "CREATE TABLE t (id int);",
		"20260901120001_t.down.sql": "DROP TABLE t;",
		"R__t_totals.up.sql":        "CREATE OR REPLACE VIEW t_totals AS SELECT id FROM t;",
		"R__t_totals.down.sql":      "DROP VIEW IF EXISTS t_totals;",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	code, out, errOut := runCLI("diff", "--server", url, "--target", "app", "--schema", schema, "--dir", dir, "--dry-run")
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
	sent := map[string]string{}
	for _, f := range stub.diffed.Files {
		sent[f.Name] = f.Body
	}
	if len(sent) != 4 || sent["R__t_totals.up.sql"] != "CREATE OR REPLACE VIEW t_totals AS SELECT id FROM t;" {
		t.Fatalf("files sent = %v", sent)
	}
	if !strings.Contains(out, "declared by repeatable migrations, so the desired schema keeps them: public.t_totals\n") {
		t.Fatalf("out = %q", out)
	}

	code, out, _ = runCLI("diff", "--server", url, "--target", "app", "--schema", schema, "--dir", dir, "--dry-run", "--json")
	if m := decodeJSON(t, out); code != 0 || m["repeatable_objects"].([]any)[0] != "public.t_totals" {
		t.Fatalf("code = %d, json = %v", code, m)
	}
}
