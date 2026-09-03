package cli

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/internal/lint"
)

func TestLintText(t *testing.T) {
	t.Parallel()

	dir := goodMigs(t)
	code, out, errOut := runCLI("lint", "--dir", dir)
	if code != 1 || !strings.Contains(errOut, "1 blocking finding(s)") {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
	if !strings.Contains(out, "20260901120000_users.up.sql: error H001 CREATE INDEX without CONCURRENTLY blocks writes on users\n    -- or let godwit run it: -- godwit: add-index users (id) name=idx_users\n    CREATE INDEX CONCURRENTLY idx_users ON users USING btree (id);\n") ||
		!strings.Contains(out, "1 finding(s), 1 blocking") {
		t.Fatalf("out = %s", out)
	}

	code, out, _ = runCLI("lint", "--dir", dir, "--ack", "H001,H002")
	if code != 0 || out != "0 finding(s), 0 blocking\n" {
		t.Fatalf("code = %d, out = %q", code, out)
	}
}

func TestLintWarningsDoNotFail(t *testing.T) {
	t.Parallel()

	dir := writeMigs(t, map[string]string{
		"20260901120000_i.up.sql":   "CREATE INDEX CONCURRENTLY i ON t (v);",
		"20260901120000_i.down.sql": "SELECT 1;",
	})
	code, out, _ := runCLI("lint", "--dir", dir)
	if code != 0 || !strings.Contains(out, "20260901120000_i.down.sql: warning W001 down migration is a no-op") {
		t.Fatalf("code = %d, out = %s", code, out)
	}
}

func TestLintMarkdown(t *testing.T) {
	t.Parallel()

	code, out, _ := runCLI("lint", "--dir", goodMigs(t), "--format", "markdown")
	for _, want := range []string{
		"## godwit lint", "| Migration | Level | Code | Message |", "| `20260901120000_users.up.sql` | error | H001 |",
		"|\n\n<details><summary>recipe for H001 in `20260901120000_users.up.sql`</summary>\n\n```sql\n-- or let godwit run it: -- godwit: add-index users (id) name=idx_users\nCREATE INDEX CONCURRENTLY idx_users ON users USING btree (id);\n```\n\n</details>\n\n❌ 1 blocking finding(s)",
	} {
		if code != 1 || !strings.Contains(out, want) {
			t.Fatalf("code = %d, output missing %q:\n%s", code, want, out)
		}
	}

	code, out, _ = runCLI("lint", "--dir", goodMigs(t), "--format", "markdown", "--ack", "H001")
	if code != 0 || out != "## godwit lint\n\n✅ no unacknowledged hazards\n" {
		t.Fatalf("code = %d, out = %q", code, out)
	}
}

func TestLintJSON(t *testing.T) {
	t.Parallel()

	code, out, _ := runCLI("lint", "--dir", goodMigs(t), "--format", "json")
	var rep lint.Report
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if code != 1 || rep.Blocking != 1 || len(rep.Findings) != 1 || rep.Findings[0].Code != "H001" ||
		rep.Findings[0].Recipe != "-- or let godwit run it: -- godwit: add-index users (id) name=idx_users\nCREATE INDEX CONCURRENTLY idx_users ON users USING btree (id);" {
		t.Fatalf("code = %d, report = %+v", code, rep)
	}

	_, out, _ = runCLI("lint", "--dir", goodMigs(t), "--format", "json", "--ack", "H001")
	if out != `{"findings":[],"blocking":0}`+"\n" {
		t.Fatalf("out = %q", out)
	}
}

func TestLintErrors(t *testing.T) {
	t.Parallel()

	if code, _, errOut := runCLI("lint", "--dir", goodMigs(t), "--format", "yaml"); code != 1 || !strings.Contains(errOut, "unknown format") {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
	if code, _, errOut := runCLI("lint", "--dir", goodMigs(t), "--base", "no-such-ref"); code != 1 || !strings.Contains(errOut, "git diff no-such-ref") {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
	if code, out, _ := runCLI("lint", "--dir", t.TempDir()+"/nope"); code != 1 || !strings.Contains(out, "E001") {
		t.Fatalf("code = %d, out = %s", code, out)
	}
}

func TestLintDirective(t *testing.T) {
	t.Parallel()

	dir := writeMigs(t, map[string]string{
		"20260901120000_a.up.sql":   "-- godwit: change-type users.age biginteger batch=x\nSELECT 1;",
		"20260901120000_a.down.sql": "SELECT 1;",
	})
	code, out, _ := runCLI("lint", "--dir", dir)
	if code != 1 || !strings.Contains(out, "20260901120000_a.up.sql: error E004 20260901120000_a.up.sql:1: godwit directive: ") {
		t.Fatalf("code = %d, out = %s", code, out)
	}
}

const lintConfig = "dir: migrations\ntarget: app\nschema_source:\n  kind: file\n  path: schema.sql\n"

func lintRepo(t *testing.T, config string) string {
	t.Helper()

	return configRepo(t, map[string]string{
		"godwit.yaml":                              config,
		"schema.sql":                               "CREATE TABLE users (id int, email text);\n",
		"migrations/20260901120000_users.up.sql":   "CREATE TABLE users (id int);",
		"migrations/20260901120000_users.down.sql": "DROP TABLE users;",
	})
}

func TestLintSchemaMatches(t *testing.T) {
	stub := &stubService{diff: &godwitv1.DiffResponse{Target: "app"}}
	url := startStub(t, stub)
	chdir(t, lintRepo(t, lintConfig))

	code, out, errOut := runCLI("lint", "--server", url, "--token", "tok")
	if code != 0 || out != "0 finding(s), 0 blocking\n" {
		t.Fatalf("code = %d, out = %q, stderr = %s", code, out, errOut)
	}
	if stub.diffed.Base != godwitv1.DiffBase_DIFF_BASE_FILES || stub.diffed.Target != "app" ||
		stub.diffed.Schema != "CREATE TABLE users (id int, email text);\n" || stub.auth != "Bearer tok" {
		t.Fatalf("request = %+v, auth = %q", stub.diffed, stub.auth)
	}
	if len(stub.diffed.Files) != 2 || stub.diffed.Files[0].Name != "20260901120000_users.down.sql" ||
		stub.diffed.Files[1].Body != "CREATE TABLE users (id int);" {
		t.Fatalf("files = %+v", stub.diffed.Files)
	}
}

func TestLintSchemaDrift(t *testing.T) {
	residue := `ALTER TABLE "public"."users" ADD COLUMN "email" text;`
	url := startStub(t, &stubService{diff: &godwitv1.DiffResponse{Target: "app", UpSql: residue}})
	chdir(t, lintRepo(t, lintConfig))

	code, out, errOut := runCLI("lint", "--server", url)
	if code != 1 || !strings.Contains(errOut, "1 blocking finding(s)") {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
	if !strings.Contains(out, "schema.sql: error E005 the migration generated from ") ||
		!strings.Contains(out, "\n    "+residue+"\n") || !strings.Contains(out, "1 finding(s), 1 blocking") {
		t.Fatalf("out = %q", out)
	}

	code, out, _ = runCLI("lint", "--server", url, "--format", "markdown")
	if code != 1 || !strings.Contains(out, "| error | E005 |") || !strings.Contains(out, "```sql\n"+residue+"\n```") {
		t.Fatalf("markdown: code = %d, out = %q", code, out)
	}

	code, out, _ = runCLI("lint", "--server", url, "--format", "json")
	var rep lint.Report
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if code != 1 || rep.Blocking != 1 || len(rep.Findings) != 1 || rep.Findings[0].Code != "E005" || rep.Findings[0].Recipe != residue {
		t.Fatalf("code = %d, report = %+v", code, rep)
	}
}

func TestLintSchemaCheckWarnsOrSkips(t *testing.T) {
	url := startStub(t, &stubService{diff: &godwitv1.DiffResponse{Target: "app", UpSql: "ALTER TABLE users ADD COLUMN email text;"}})

	chdir(t, lintRepo(t, lintConfig+"  lint: false\n"))
	code, out, _ := runCLI("lint", "--server", url)
	if code != 0 || !strings.Contains(out, "schema.sql: warning E005 ") {
		t.Fatalf("lint false: code = %d, out = %q", code, out)
	}

	chdir(t, lintRepo(t, lintConfig))
	code, out, _ = runCLI("lint", "--server", url, "--no-schema-check")
	if code != 0 || out != "0 finding(s), 0 blocking\n" {
		t.Fatalf("--no-schema-check: code = %d, out = %q", code, out)
	}

	code, out, _ = runCLI("lint")
	if code != 0 || !strings.Contains(out, "warning W002 ") || !strings.Contains(out, "not checked: no server configured") {
		t.Fatalf("no server: code = %d, out = %q", code, out)
	}
}

func TestLintSchemaCheckLabels(t *testing.T) {
	for name, tc := range map[string]struct{ block, want string }{
		"command": {"schema_source:\n  kind: command\n  command: [\"go\", \"run\", \"./cmd/schema\"]\n", "go run ./cmd/schema not checked"},
		"gorm":    {"schema_source:\n  kind: gorm\n  path: ./cmd/schema\n", "warning W002 go run /"},
		"django":  {"schema_source:\n  kind: django\n  path: manage.py\n", "manage.py not checked"},
	} {
		chdir(t, configRepo(t, map[string]string{
			"godwit.yaml":                              "dir: migrations\n" + tc.block,
			"migrations/20260901120000_users.up.sql":   "CREATE TABLE users (id int);",
			"migrations/20260901120000_users.down.sql": "DROP TABLE users;",
		}))
		if code, out, _ := runCLI("lint"); code != 0 || !strings.Contains(out, tc.want) {
			t.Fatalf("%s: code = %d, out = %q", name, code, out)
		}
	}
}

func TestLintSchemaCheckErrors(t *testing.T) {
	url := startStub(t, &stubService{diff: &godwitv1.DiffResponse{Target: "app"}})

	chdir(t, lintRepo(t, "dir: migrations\nschema_source:\n  kind: file\n  path: schema.sql\n"))
	if code, _, errOut := runCLI("lint", "--server", url); code != 1 ||
		!strings.Contains(errOut, "--target (or target in godwit.yaml) is required by the schema check") {
		t.Fatalf("no target: code = %d, stderr = %q", code, errOut)
	}

	chdir(t, lintRepo(t, "dir: migrations\ntarget: app\nschema_source:\n  kind: prisma\n"))
	if code, _, errOut := runCLI("lint"); code != 1 ||
		!strings.Contains(errOut, "schema_source.path is required for kind prisma") {
		t.Fatalf("missing path: code = %d, stderr = %q", code, errOut)
	}

	chdir(t, lintRepo(t, "dir: migrations\ntarget: app\nschema_source:\n  kind: file\n  path: missing.sql\n"))
	if code, _, errOut := runCLI("lint", "--server", url); code != 1 || !strings.Contains(errOut, "missing.sql") {
		t.Fatalf("missing schema: code = %d, stderr = %q", code, errOut)
	}

	chdir(t, lintRepo(t, lintConfig))
	if code, _, errOut := runCLI("lint", "--server", "://nope"); code != 1 || errOut == "" {
		t.Fatalf("bad server: code = %d, stderr = %q", code, errOut)
	}

	down := startStub(t, &stubService{err: errors.New("service is down")})
	if code, _, errOut := runCLI("lint", "--server", down); code != 1 || !strings.Contains(errOut, "service is down") {
		t.Fatalf("service error: code = %d, stderr = %q", code, errOut)
	}
}
