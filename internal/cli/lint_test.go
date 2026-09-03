package cli

import (
	"encoding/json"
	"strings"
	"testing"

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
