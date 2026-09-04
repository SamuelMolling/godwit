package ui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
)

const viewSQL = "CREATE OR REPLACE VIEW t_totals AS SELECT id FROM t;\n"

type diffStub struct {
	*stub
	resp      *godwitv1.DiffResponse
	fail      error
	sent      []*godwitv1.MigrationFile
	planList  []*godwitv1.Plan
	planFiles []*godwitv1.MigrationFile
	runFiles  []*godwitv1.MigrationFile
	planErr   error
	runErr    error
	listErr   error
	runsErr   error
	statusErr error
}

func (d *diffStub) GetTargetStatus(ctx context.Context, req *connect.Request[godwitv1.GetTargetStatusRequest]) (*connect.Response[godwitv1.GetTargetStatusResponse], error) {
	if d.statusErr != nil {
		return nil, d.statusErr
	}

	return d.stub.GetTargetStatus(ctx, req)
}

func (d *diffStub) Diff(ctx context.Context, req *connect.Request[godwitv1.DiffRequest]) (*connect.Response[godwitv1.DiffResponse], error) {
	if err := d.call(ctx, "Diff:"+req.Msg.Target+":"+req.Msg.Base.String()+":"+req.Msg.Schema); err != nil {
		return nil, err
	}
	d.sent = req.Msg.Files
	if d.fail != nil {
		return nil, d.fail
	}

	return connect.NewResponse(d.resp), nil
}

func (d *diffStub) ListPlans(ctx context.Context, req *connect.Request[godwitv1.ListPlansRequest]) (*connect.Response[godwitv1.ListPlansResponse], error) {
	if err := d.call(ctx, "ListPlans:"+req.Msg.Target); err != nil {
		return nil, err
	}
	if d.listErr != nil {
		return nil, d.listErr
	}

	return connect.NewResponse(&godwitv1.ListPlansResponse{Plans: d.planList}), nil
}

func (d *diffStub) GetPlan(ctx context.Context, req *connect.Request[godwitv1.GetPlanRequest]) (*connect.Response[godwitv1.GetPlanResponse], error) {
	if err := d.call(ctx, "GetPlan:"+req.Msg.PlanId); err != nil {
		return nil, err
	}
	if d.planErr != nil {
		return nil, d.planErr
	}

	return connect.NewResponse(&godwitv1.GetPlanResponse{Plan: d.planList[0], Files: d.planFiles}), nil
}

func (d *diffStub) GetRun(ctx context.Context, req *connect.Request[godwitv1.GetRunRequest]) (*connect.Response[godwitv1.GetRunResponse], error) {
	if err := d.call(ctx, "GetRun:"+req.Msg.RunId); err != nil {
		return nil, err
	}
	if d.runErr != nil {
		return nil, d.runErr
	}

	return connect.NewResponse(&godwitv1.GetRunResponse{Files: d.runFiles}), nil
}

func (d *diffStub) ListRuns(ctx context.Context, req *connect.Request[godwitv1.ListRunsRequest]) (*connect.Response[godwitv1.ListRunsResponse], error) {
	if err := d.call(ctx, "ListRuns"); err != nil {
		return nil, err
	}
	if d.runsErr != nil {
		return nil, d.runsErr
	}

	return d.stub.ListRuns(ctx, req)
}

func checksum(body string) string {
	sum := sha256.Sum256([]byte(body))

	return hex.EncodeToString(sum[:])
}

func pair(name, body string) []*godwitv1.MigrationFile {
	return []*godwitv1.MigrationFile{
		{Name: "R__" + name + ".up.sql", Body: body},
		{Name: "R__" + name + ".down.sql", Body: "DROP VIEW IF EXISTS " + name + ";"},
	}
}

func records(names ...string) []*godwitv1.AppliedMigration {
	out := []*godwitv1.AppliedMigration{{Version: 20260901120000, Name: "add_index", Checksum: "9f1e2d"}}
	for _, n := range names {
		out = append(out, &godwitv1.AppliedMigration{Name: n, Repeatable: true, Checksum: checksum(viewSQL)})
	}

	return out
}

func diffFixture() *diffStub {
	s := &diffStub{stub: fixture(), resp: &godwitv1.DiffResponse{
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
		RepeatableObjects: []string{"public.t_totals"},
		Observed: &godwitv1.PlanObservation{
			SearchPath: "public", AppliedCount: 3, NewestApplied: 20260901120000,
			HistoryHash: "abcdef1234567890", At: at(time.Minute),
		},
	}}
	s.status.Applied = records("t_totals")
	s.planList = []*godwitv1.Plan{{Id: "p-plan-0001", Target: "app", State: "ready", CreatedAt: at(time.Hour)}}
	s.planFiles = append(pair("t_totals", viewSQL),
		&godwitv1.MigrationFile{Name: "20260901120000_add_index.up.sql", Body: "CREATE INDEX i1 ON t (a);"})
	s.runFiles = pair("t_totals", viewSQL)

	return s
}

func diffForm(name, schema string) url.Values {
	return url.Values{"target": {"app"}, "name": {name}, "schema": {schema}}
}

func sentNames(s *diffStub) string {
	out := make([]string, 0, len(s.sent))
	for _, f := range s.sent {
		out = append(out, f.Name)
	}

	return strings.Join(out, ",")
}

func TestDiffForm(t *testing.T) {
	t.Parallel()
	h := newUI(diffFixture(), Config{Replica: "godwit-0"})

	rec := do(h, http.MethodGet, "/ui/diff", nil)
	want(t, rec, http.StatusOK, "Schema diff", `name="schema"`, `value="changes"`,
		`<option value="app">`, `<option value="billing">`, "Generate migration",
		"godwit diff --prisma", "This page only takes DDL", "Nothing is written to disk here",
		`name="files"`, `<option value="auto" selected>`, "The run that last succeeded")
	absent(t, rec, "No changes", "Save both files", "Repeatable migrations")

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
		"CREATE TABLE t (a int);",
		"Repeatable migrations", "1 recorded on app",
		"Supplied from plan p-plan-0 (ready), stored 2026-09-02 11:00:00Z",
		"not necessarily what your repository holds now",
		"matches the snapshot, byte for byte",
		"Declared by a repeatable", "public.t_totals")
	absent(t, rec, "No changes", "No repeatable files to send", `name="body.t_totals"`)
	if got := sentNames(s); got != "R__t_totals.down.sql,R__t_totals.up.sql" {
		t.Fatalf("files = %q", got)
	}
	if got := s.calls[len(s.calls)-1]; got != "Diff:app:DIFF_BASE_LIVE:CREATE TABLE t (a int);" {
		t.Fatalf("call = %q", got)
	}
}

func TestDiffWithoutRepeatablesAsksForNoFiles(t *testing.T) {
	t.Parallel()
	s := diffFixture()
	s.status.Applied = records()
	rec := do(newUI(s, Config{}), http.MethodPost, "/ui/diff", diffForm("add_index", "CREATE TABLE t (a int);"))
	want(t, rec, http.StatusOK, "Statements")
	absent(t, rec, "Repeatable migrations")
	if len(s.sent) != 0 {
		t.Fatalf("files = %v", s.sent)
	}
	if strings.Contains(strings.Join(s.calls, " "), "ListPlans") {
		t.Fatalf("no snapshot is needed: %v", s.calls)
	}
}

func TestDiffFilesFromTheLastRun(t *testing.T) {
	t.Parallel()
	s := diffFixture()
	form := diffForm("add_index", "CREATE TABLE t (a int);")
	form.Set("files", "run")
	want(t, do(newUI(s, Config{}), http.MethodPost, "/ui/diff", form), http.StatusOK,
		"Supplied from run r-ok-000, which succeeded 2026-09-02 10:01:30Z", "when the run was created")
	if got := sentNames(s); got != "R__t_totals.down.sql,R__t_totals.up.sql" {
		t.Fatalf("files = %q", got)
	}
}

func TestDiffFallsBackFromPlanToRun(t *testing.T) {
	t.Parallel()
	s := diffFixture()
	s.planFiles = []*godwitv1.MigrationFile{{Name: "20260901120000_add_index.up.sql", Body: "CREATE INDEX i1 ON t (a);"}}
	want(t, do(newUI(s, Config{}), http.MethodPost, "/ui/diff", diffForm("n", "CREATE TABLE t (a int);")), http.StatusOK,
		"Supplied from run r-ok-000")

	s = diffFixture()
	s.planList = nil
	want(t, do(newUI(s, Config{}), http.MethodPost, "/ui/diff", diffForm("n", "CREATE TABLE t (a int);")), http.StatusOK,
		"Supplied from run r-ok-000")
}

func TestDiffWithNothingStoredRefusesAndSaysWhatWouldFixIt(t *testing.T) {
	t.Parallel()
	s := diffFixture()
	s.planList, s.runs = nil, nil
	s.fail = connect.NewError(connect.CodeFailedPrecondition,
		errors.New("target records repeatable migrations and the request carried no migration files: R__t_totals"))

	rec := do(newUI(s, Config{}), http.MethodPost, "/ui/diff", diffForm("n", "CREATE TABLE t (a int);"))
	want(t, rec, http.StatusPreconditionFailed, "The diff was refused", "carried no migration files",
		"No repeatable files to send", "no run has succeeded on app",
		"godwit plan --target app", `name="body.t_totals"`, "R__t_totals.up.sql",
		"a diff builds what a repeatable declares and never runs a down")
	if len(s.sent) != 0 {
		t.Fatalf("files = %v", s.sent)
	}

	bare := diffFixture()
	bare.planList, bare.runFiles, bare.fail = nil, nil, s.fail
	want(t, do(newUI(bare, Config{}), http.MethodPost, "/ui/diff", diffForm("n", "CREATE TABLE t (a int);")),
		http.StatusPreconditionFailed, "run r-ok-000, which succeeded 2026-09-02 10:01:30Z carrying no repeatable files")
}

func TestDiffPlanSweptByRetention(t *testing.T) {
	t.Parallel()
	s := diffFixture()
	s.planErr = connect.NewError(connect.CodeNotFound, errors.New("no such plan"))
	form := diffForm("n", "CREATE TABLE t (a int);")
	form.Set("files", "plan")
	s.fail = connect.NewError(connect.CodeFailedPrecondition, errors.New("no migration files"))
	want(t, do(newUI(s, Config{}), http.MethodPost, "/ui/diff", form), http.StatusPreconditionFailed,
		"plan p-plan-0 was swept by retention before it could be read")
}

func TestDiffReportsASnapshotOutOfStepWithTheTarget(t *testing.T) {
	t.Parallel()
	stale := diffFixture()
	stale.planFiles = pair("t_totals", "CREATE OR REPLACE VIEW t_totals AS SELECT id, a FROM t;\n")
	want(t, do(newUI(stale, Config{}), http.MethodPost, "/ui/diff", diffForm("n", "CREATE TABLE t (a int);")),
		http.StatusOK, "R__t_totals</span> differs from what app recorded",
		"this snapshot is not the body that built the object on app")

	missing := diffFixture()
	missing.status.Applied = records("t_totals", "t_audit")
	want(t, do(newUI(missing, Config{}), http.MethodPost, "/ui/diff", diffForm("n", "CREATE TABLE t (a int);")),
		http.StatusOK, "app records <span class=\"mono\">R__t_audit</span> and the snapshot has no file for it")

	unknown := diffFixture()
	unknown.planFiles = append(unknown.planFiles, pair("t_extra", viewSQL)...)
	want(t, do(newUI(unknown, Config{}), http.MethodPost, "/ui/diff", diffForm("n", "CREATE TABLE t (a int);")),
		http.StatusOK, "R__t_extra</span> is in the snapshot and app has never recorded it")
}

func TestDiffFromTheBoxesOnThePage(t *testing.T) {
	t.Parallel()
	s := diffFixture()
	form := diffForm("n", "CREATE TABLE t (a int);")
	form.Set("files", "paste")
	form.Set("body.t_totals", viewSQL)
	form.Set("body.t_blank", "  ")

	want(t, do(newUI(s, Config{}), http.MethodPost, "/ui/diff", form), http.StatusOK,
		"Supplied from the boxes below", "when you pasted it", "matches the snapshot", `name="body.t_totals"`)
	if got := sentNames(s); got != "R__t_totals.down.sql,R__t_totals.up.sql" {
		t.Fatalf("files = %q", got)
	}
	if body := s.sent[1].Body; body != viewSQL {
		t.Fatalf("body = %q", body)
	}
	if strings.Contains(strings.Join(s.calls, " "), "ListPlans") {
		t.Fatalf("pasted bodies need no snapshot: %v", s.calls)
	}
}

func TestDiffLeavesAnUnloadableSnapshotToTheService(t *testing.T) {
	t.Parallel()
	s := diffFixture()
	s.planFiles = []*godwitv1.MigrationFile{{Name: "R__t_totals.up.sql", Body: viewSQL}}
	s.fail = connect.NewError(connect.CodeInvalidArgument, errors.New("migration files failed to replay: R__t_totals: missing down file"))

	want(t, do(newUI(s, Config{}), http.MethodPost, "/ui/diff", diffForm("n", "CREATE TABLE t (a int);")),
		http.StatusBadRequest, "missing down file", "No repeatable files to send")
	if got := sentNames(s); got != "R__t_totals.up.sql" {
		t.Fatalf("files = %q", got)
	}
}

func TestDiffNoChanges(t *testing.T) {
	t.Parallel()
	s := diffFixture()
	s.resp = &godwitv1.DiffResponse{Target: "app"}
	rec := do(newUI(s, Config{}), http.MethodPost, "/ui/diff", diffForm("add_index", "CREATE TABLE t (a int);"))
	want(t, rec, http.StatusOK, "No changes", "already matches the schema you pasted")
	absent(t, rec, "Save both files", "Statements", "Declared by a repeatable")
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
	form := diffForm("n", "CREATE TABLE t (a int);")

	s.fail = connect.NewError(connect.CodeUnavailable, errBoom)
	want(t, do(h, http.MethodPost, "/ui/diff", form), http.StatusBadGateway, "boom", "Back to runs")

	s.fail = nil
	for _, tc := range []struct {
		set  func(*diffStub)
		want string
	}{
		{func(d *diffStub) { d.listErr = connect.NewError(connect.CodeUnavailable, errors.New("plans down")) }, "plans down"},
		{func(d *diffStub) { d.planErr = connect.NewError(connect.CodeUnavailable, errors.New("plan down")) }, "plan down"},
		{func(d *diffStub) {
			d.planList = nil
			d.runsErr = connect.NewError(connect.CodeUnavailable, errors.New("runs down"))
		}, "runs down"},
		{func(d *diffStub) {
			d.planList = nil
			d.runErr = connect.NewError(connect.CodeUnavailable, errors.New("run down"))
		}, "run down"},
		{func(d *diffStub) {
			d.statusErr = connect.NewError(connect.CodeUnavailable, errors.New("target down"))
		}, "target down"},
	} {
		bad := diffFixture()
		tc.set(bad)
		want(t, do(newUI(bad, Config{}), http.MethodPost, "/ui/diff", form), http.StatusBadGateway, tc.want, "Back to runs")
	}

	s.err = connect.NewError(connect.CodeInternal, errBoom)
	want(t, do(h, http.MethodGet, "/ui/diff", nil), http.StatusBadGateway, "boom", "Back to runs")
	want(t, do(h, http.MethodPost, "/ui/diff", form), http.StatusBadGateway, "boom")
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
