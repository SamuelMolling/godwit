package ui

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
)

type diffStub struct {
	*stub
	resp *godwitv1.DiffResponse
	fail error
}

func (d *diffStub) Diff(ctx context.Context, req *connect.Request[godwitv1.DiffRequest]) (*connect.Response[godwitv1.DiffResponse], error) {
	if err := d.call(ctx, "Diff:"+req.Msg.Target+":"+req.Msg.Base.String()+":"+req.Msg.Schema); err != nil {
		return nil, err
	}
	if d.fail != nil {
		return nil, d.fail
	}

	return connect.NewResponse(d.resp), nil
}

func diffFixture() *diffStub {
	return &diffStub{stub: fixture(), resp: &godwitv1.DiffResponse{
		Target:  "app",
		UpSql:   "CREATE INDEX i1 ON t (a);",
		DownSql: "DROP INDEX i1;",
		Drift:   "+ column extra text",
		Statements: []*godwitv1.PlannedStatement{
			{Sql: "CREATE INDEX i1 ON t (a);", Hazards: []*godwitv1.PlannedHazard{
				{Code: "H001", Detail: "CREATE INDEX without CONCURRENTLY blocks writes on t", Recipe: "CREATE INDEX CONCURRENTLY i1 ON t (a);"},
			}},
			{Sql: "VACUUM t;", NoTx: true},
		},
		Observed: &godwitv1.PlanObservation{
			SearchPath: "public", AppliedCount: 3, NewestApplied: 20260901120000,
			HistoryHash: "abcdef1234567890", At: at(time.Minute),
		},
	}}
}

func diffForm(name, schema string) url.Values {
	return url.Values{"target": {"app"}, "name": {name}, "schema": {schema}}
}

func TestDiffForm(t *testing.T) {
	t.Parallel()
	h := newUI(diffFixture(), Config{Replica: "godwit-0"})

	rec := do(h, http.MethodGet, "/ui/diff", nil)
	want(t, rec, http.StatusOK, "Schema diff", `name="schema"`, `value="changes"`,
		`<option value="app">`, `<option value="billing">`, "Generate migration",
		"godwit diff --prisma", "This page only takes DDL", "Nothing is written to disk here")
	absent(t, rec, "No changes", "Save both files")

	want(t, do(h, http.MethodGet, "/ui/diff?target=app&name=add_index", nil), http.StatusOK,
		`value="app"`, `value="add_index"`)
	want(t, do(h, http.MethodGet, "/ui/diff?name=Add%20Index", nil), http.StatusOK, `value="changes"`)
}

func TestDiffRun(t *testing.T) {
	t.Parallel()
	s := diffFixture()
	h := newUI(s, Config{})

	rec := do(h, http.MethodPost, "/ui/diff", diffForm("add_index", "CREATE TABLE t (a int);"))
	want(t, rec, http.StatusOK,
		"CREATE INDEX i1 ON t (a);", "DROP INDEX i1;",
		"20260902120000_add_index.up.sql", "20260902120000_add_index.down.sql",
		"<b>H001</b>", "CREATE INDEX without CONCURRENTLY", "CREATE INDEX CONCURRENTLY i1 ON t (a);",
		"&#43; column extra text", "live schema, not its history",
		`class="chip">no-tx<`, `class="chip">tx<`, "Statements <span class=\"n\">2<",
		"search_path", "public", "abcdef12", "2026-09-02 11:59:00Z",
		"CREATE TABLE t (a int);")
	absent(t, rec, "No changes")
	if got := s.calls[len(s.calls)-1]; got != "Diff:app:DIFF_BASE_LIVE:CREATE TABLE t (a int);" {
		t.Fatalf("call = %q", got)
	}
}

func TestDiffNoChanges(t *testing.T) {
	t.Parallel()
	s := diffFixture()
	s.resp = &godwitv1.DiffResponse{Target: "app"}
	rec := do(newUI(s, Config{}), http.MethodPost, "/ui/diff", diffForm("add_index", "CREATE TABLE t (a int);"))
	want(t, rec, http.StatusOK, "No changes", "already matches the schema you pasted")
	absent(t, rec, "Save both files", ".up.sql", "Statements")
}

func TestDiffRefusalStaysOnTheForm(t *testing.T) {
	t.Parallel()
	s := diffFixture()
	h := newUI(s, Config{})

	cases := []struct {
		code    connect.Code
		message string
		shown   string
		status  int
	}{
		{
			connect.CodeInvalidArgument,
			`desired schema failed to apply: ERROR: type "nosuchtype" does not exist (SQLSTATE 42704)`,
			"desired schema failed to apply: ERROR: type &#34;nosuchtype&#34; does not exist (SQLSTATE 42704)",
			http.StatusBadRequest,
		},
		{
			connect.CodeFailedPrecondition,
			"diffing against the committed files needs validation",
			"diffing against the committed files needs validation",
			http.StatusPreconditionFailed,
		},
		{connect.CodeUnimplemented, "schema diff is not enabled", "schema diff is not enabled", http.StatusNotImplemented},
	}
	for _, c := range cases {
		s.fail = connect.NewError(c.code, errors.New(c.message))
		rec := do(h, http.MethodPost, "/ui/diff", diffForm("add_index", "CREATE TABLE t (a nosuchtype);"))
		want(t, rec, c.status, "The diff was refused", c.shown,
			"CREATE TABLE t (a nosuchtype);", `value="app"`, "Generate migration")
		if body := rec.Body.String(); strings.Contains(body, "Back to runs") {
			t.Fatalf("%s must not fall through to the error page:\n%s", c.code, body)
		}
	}
}

func TestDiffBackendFailure(t *testing.T) {
	t.Parallel()
	s := diffFixture()
	h := newUI(s, Config{})

	s.fail = connect.NewError(connect.CodeUnavailable, errBoom)
	want(t, do(h, http.MethodPost, "/ui/diff", diffForm("n", "CREATE TABLE t (a int);")), http.StatusBadGateway, "boom", "Back to runs")

	s.err = connect.NewError(connect.CodeInternal, errBoom)
	want(t, do(h, http.MethodGet, "/ui/diff", nil), http.StatusBadGateway, "boom", "Back to runs")
	want(t, do(h, http.MethodPost, "/ui/diff", diffForm("n", "CREATE TABLE t (a int);")), http.StatusBadGateway, "boom")
}

func TestDiffIsOfferedToEveryScope(t *testing.T) {
	t.Parallel()
	s := diffFixture()
	h := newUI(s, Config{Tokens: uiTokens})

	read := []string{"Authorization", basic("x", "s-read")}
	rec := do(h, http.MethodGet, "/ui/diff", nil, read...)
	want(t, rec, http.StatusOK, "Generate migration")
	absent(t, rec, "Actions on this page need a wider scope")

	want(t, do(h, http.MethodPost, "/ui/diff", diffForm("add_index", "CREATE TABLE t (a int);"), read...),
		http.StatusOK, "20260902120000_add_index.up.sql")
	if s.actor != "ui:viewer" {
		t.Fatalf("actor = %q", s.actor)
	}
}
