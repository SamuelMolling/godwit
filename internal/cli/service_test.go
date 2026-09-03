package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/gen/godwit/v1/godwitv1connect"
)

type stubService struct {
	godwitv1connect.UnimplementedGodwitServiceHandler
	mu          sync.Mutex
	auth        string
	registered  *godwitv1.RegisterTargetRequest
	baselined   *godwitv1.BaselineTargetRequest
	statused    *godwitv1.GetTargetStatusRequest
	status      *godwitv1.GetTargetStatusResponse
	summaries   []*godwitv1.TargetSummary
	created     *godwitv1.CreateRunRequest
	planned     *godwitv1.PlanRunRequest
	plan        *godwitv1.PlanRunResponse
	reverted    *godwitv1.RevertRunRequest
	listed      *godwitv1.ListRunsRequest
	got         string
	watched     string
	resumed     string
	confirmed   string
	checked     string
	accepted    string
	audited     *godwitv1.ListAuditRequest
	entries     []*godwitv1.AuditEntry
	run         *godwitv1.Run
	runs        []*godwitv1.Run
	events      []*godwitv1.Run
	drift       *godwitv1.CheckDriftResponse
	revertID    string
	planID      string
	planGot     string
	plansListed *godwitv1.ListPlansRequest
	stored      *godwitv1.Plan
	plans       []*godwitv1.Plan
	reattached  bool
	diffed      *godwitv1.DiffRequest
	diff        *godwitv1.DiffResponse
	err         error
}

func (s *stubService) Diff(_ context.Context, req *connect.Request[godwitv1.DiffRequest]) (*connect.Response[godwitv1.DiffResponse], error) {
	s.diffed = req.Msg
	if err := s.record(req.Header()); err != nil {
		return nil, err
	}

	return connect.NewResponse(s.diff), nil
}

func (s *stubService) record(h http.Header) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.auth = h.Get("Authorization")

	return s.err
}

func (s *stubService) RegisterTarget(_ context.Context, req *connect.Request[godwitv1.RegisterTargetRequest]) (*connect.Response[godwitv1.RegisterTargetResponse], error) {
	s.registered = req.Msg
	if err := s.record(req.Header()); err != nil {
		return nil, err
	}

	return connect.NewResponse(&godwitv1.RegisterTargetResponse{}), nil
}

func (s *stubService) BaselineTarget(_ context.Context, req *connect.Request[godwitv1.BaselineTargetRequest]) (*connect.Response[godwitv1.BaselineTargetResponse], error) {
	s.baselined = req.Msg
	if err := s.record(req.Header()); err != nil {
		return nil, err
	}

	return connect.NewResponse(&godwitv1.BaselineTargetResponse{RunId: "b1"}), nil
}

func (s *stubService) GetTargetStatus(_ context.Context, req *connect.Request[godwitv1.GetTargetStatusRequest]) (*connect.Response[godwitv1.GetTargetStatusResponse], error) {
	s.statused = req.Msg
	if err := s.record(req.Header()); err != nil {
		return nil, err
	}

	return connect.NewResponse(s.status), nil
}

func (s *stubService) CreateRun(_ context.Context, req *connect.Request[godwitv1.CreateRunRequest]) (*connect.Response[godwitv1.CreateRunResponse], error) {
	s.created = req.Msg
	if err := s.record(req.Header()); err != nil {
		return nil, err
	}

	return connect.NewResponse(&godwitv1.CreateRunResponse{RunId: "r1", PlanId: s.planID, Reattached: s.reattached}), nil
}

func (s *stubService) PlanRun(_ context.Context, req *connect.Request[godwitv1.PlanRunRequest]) (*connect.Response[godwitv1.PlanRunResponse], error) {
	s.planned = req.Msg
	if err := s.record(req.Header()); err != nil {
		return nil, err
	}

	return connect.NewResponse(s.plan), nil
}

func (s *stubService) RevertRun(_ context.Context, req *connect.Request[godwitv1.RevertRunRequest]) (*connect.Response[godwitv1.RevertRunResponse], error) {
	s.reverted = req.Msg
	if err := s.record(req.Header()); err != nil {
		return nil, err
	}

	return connect.NewResponse(&godwitv1.RevertRunResponse{RunId: s.revertID}), nil
}

func (s *stubService) GetRun(_ context.Context, req *connect.Request[godwitv1.GetRunRequest]) (*connect.Response[godwitv1.GetRunResponse], error) {
	s.got = req.Msg.RunId
	if err := s.record(req.Header()); err != nil {
		return nil, err
	}

	return connect.NewResponse(&godwitv1.GetRunResponse{Run: s.run}), nil
}

func (s *stubService) ListRuns(_ context.Context, req *connect.Request[godwitv1.ListRunsRequest]) (*connect.Response[godwitv1.ListRunsResponse], error) {
	s.listed = req.Msg
	if err := s.record(req.Header()); err != nil {
		return nil, err
	}

	return connect.NewResponse(&godwitv1.ListRunsResponse{Runs: s.runs}), nil
}

func (s *stubService) WatchRun(_ context.Context, req *connect.Request[godwitv1.WatchRunRequest], stream *connect.ServerStream[godwitv1.WatchRunResponse]) error {
	s.watched = req.Msg.RunId
	if err := s.record(req.Header()); err != nil {
		return err
	}
	for _, r := range s.events {
		if err := stream.Send(&godwitv1.WatchRunResponse{Run: r}); err != nil {
			return err
		}
	}

	return nil
}

func (s *stubService) ResumeRun(_ context.Context, req *connect.Request[godwitv1.ResumeRunRequest]) (*connect.Response[godwitv1.ResumeRunResponse], error) {
	s.resumed = req.Msg.RunId
	if err := s.record(req.Header()); err != nil {
		return nil, err
	}

	return connect.NewResponse(&godwitv1.ResumeRunResponse{}), nil
}

func (s *stubService) ConfirmRollout(_ context.Context, req *connect.Request[godwitv1.ConfirmRolloutRequest]) (*connect.Response[godwitv1.ConfirmRolloutResponse], error) {
	s.confirmed = req.Msg.RunId
	if err := s.record(req.Header()); err != nil {
		return nil, err
	}

	return connect.NewResponse(&godwitv1.ConfirmRolloutResponse{}), nil
}

func (s *stubService) CheckDrift(_ context.Context, req *connect.Request[godwitv1.CheckDriftRequest]) (*connect.Response[godwitv1.CheckDriftResponse], error) {
	s.checked = req.Msg.Target
	if err := s.record(req.Header()); err != nil {
		return nil, err
	}

	return connect.NewResponse(s.drift), nil
}

func (s *stubService) AcceptBaseline(_ context.Context, req *connect.Request[godwitv1.AcceptBaselineRequest]) (*connect.Response[godwitv1.AcceptBaselineResponse], error) {
	s.accepted = req.Msg.Target
	if err := s.record(req.Header()); err != nil {
		return nil, err
	}

	return connect.NewResponse(&godwitv1.AcceptBaselineResponse{}), nil
}

func (s *stubService) ListAudit(_ context.Context, req *connect.Request[godwitv1.ListAuditRequest]) (*connect.Response[godwitv1.ListAuditResponse], error) {
	s.audited = req.Msg
	if err := s.record(req.Header()); err != nil {
		return nil, err
	}

	return connect.NewResponse(&godwitv1.ListAuditResponse{Entries: s.entries}), nil
}

func startStub(t *testing.T, svc *stubService) string {
	t.Helper()
	path, handler := godwitv1connect.NewGodwitServiceHandler(svc)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	srv := httptest.NewUnstartedServer(mux)
	srv.Config.Protocols = new(http.Protocols)
	srv.Config.Protocols.SetUnencryptedHTTP2(true)
	srv.Start()
	t.Cleanup(srv.Close)

	return srv.URL
}

func run(id string, state godwitv1.RunState, attempts int32) *godwitv1.Run {
	return &godwitv1.Run{Id: id, Target: "app", State: state, Attempts: attempts, Rollout: "direct"}
}

func decodeJSON(t *testing.T, line string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("invalid JSON %q: %v", line, err)
	}

	return m
}

func TestTargetAdd(t *testing.T) {
	t.Parallel()
	stub := &stubService{}
	url := startStub(t, stub)

	code, out, errOut := runCLI("target", "add", "app", "--server", url, "--token", "tok",
		"--provider", "vault", "--dsn", "d", "--secret-path", "s", "--vault-path", "v", "--vault-template", "tpl",
		"--lock-timeout", "2s", "--statement-timeout", "1m", "--search-path", "app,public")
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
	if out != "target app: registered (vault)\n" {
		t.Fatalf("out = %q", out)
	}
	if stub.auth != "Bearer tok" {
		t.Fatalf("auth = %q", stub.auth)
	}
	r := stub.registered
	if r.Name != "app" || r.Provider != "vault" || r.Dsn != "d" || r.SecretPath != "s" || r.VaultPath != "v" || r.VaultTemplate != "tpl" ||
		r.LockTimeout != "2s" || r.StatementTimeout != "1m" || r.SearchPath != "app,public" {
		t.Fatalf("request = %v", r)
	}
}

func TestTargetAddJSONAndError(t *testing.T) {
	t.Parallel()
	stub := &stubService{}
	url := startStub(t, stub)

	code, out, _ := runCLI("target", "add", "app", "--server", url, "--provider", "static", "--dsn", "d", "--json")
	if code != 0 || len(decodeJSON(t, out)) != 0 {
		t.Fatalf("code = %d, out = %q", code, out)
	}
	if stub.auth != "" {
		t.Fatalf("auth = %q, want none", stub.auth)
	}

	stub.err = connect.NewError(connect.CodeInvalidArgument, errors.New("dsn is required"))
	code, _, errOut := runCLI("target", "add", "app", "--server", url, "--provider", "static")
	if code != 1 || errOut != "godwit: dsn is required\n" {
		t.Fatalf("code = %d, stderr = %q", code, errOut)
	}
}

func TestTargetBaseline(t *testing.T) {
	t.Parallel()
	stub := &stubService{}
	url := startStub(t, stub)

	code, out, errOut := runCLI("target", "baseline", "app", "--server", url, "--dir", goodMigs(t), "--version", "20260901120000")
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
	if out != "target app: baselined to version 20260901120000 (run b1)\n" {
		t.Fatalf("out = %q", out)
	}
	b := stub.baselined
	if b.Target != "app" || b.Version != 20260901120000 || len(b.Files) != 2 || b.Files[0].Name != "20260901120000_users.up.sql" {
		t.Fatalf("request = %v", b)
	}

	code, out, _ = runCLI("target", "baseline", "app", "--server", url, "--dir", goodMigs(t), "--version", "1", "--json")
	if code != 0 || decodeJSON(t, out)["runId"] != "b1" {
		t.Fatalf("code = %d, out = %q", code, out)
	}

	if code, _, errOut := runCLI("target", "baseline", "app", "--server", url, "--dir", "/nope", "--version", "1"); code != 1 ||
		!strings.Contains(errOut, "read migration dir") {
		t.Fatalf("code = %d, stderr = %q", code, errOut)
	}
	if code, _, errOut := runCLI("target", "baseline", "app", "--server", url, "--dir", goodMigs(t)); code != 1 ||
		!strings.Contains(errOut, "version") {
		t.Fatalf("code = %d, stderr = %q", code, errOut)
	}
	stub.err = connect.NewError(connect.CodeFailedPrecondition, errors.New("target already has applied migrations"))
	if code, _, errOut := runCLI("target", "baseline", "app", "--server", url, "--dir", goodMigs(t), "--version", "1"); code != 1 ||
		errOut != "godwit: target already has applied migrations\n" {
		t.Fatalf("code = %d, stderr = %q", code, errOut)
	}
}

func TestClientMissingServer(t *testing.T) {
	t.Parallel()

	code, _, errOut := runCLI("runs")
	if code != 1 || !strings.Contains(errOut, "--server (or GODWIT_SERVER, or server in godwit.yaml) is required") {
		t.Fatalf("code = %d, stderr = %q", code, errOut)
	}
}

func TestClientEnv(t *testing.T) {
	stub := &stubService{}
	t.Setenv("GODWIT_SERVER", startStub(t, stub))
	t.Setenv("GODWIT_TOKEN", "envtok")

	code, _, errOut := runCLI("runs")
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
	if stub.auth != "Bearer envtok" {
		t.Fatalf("auth = %q", stub.auth)
	}
}

func TestClientConnectionRefused(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	code, _, errOut := runCLI("runs", "--server", "http://"+addr)
	if code != 1 || !strings.HasPrefix(errOut, "godwit: ") || !strings.Contains(errOut, "connection refused") {
		t.Fatalf("code = %d, stderr = %q", code, errOut)
	}
}

func TestMigrate(t *testing.T) {
	t.Parallel()
	stub := &stubService{events: []*godwitv1.Run{
		run("r1", godwitv1.RunState_RUN_STATE_QUEUED, 0),
		run("r1", godwitv1.RunState_RUN_STATE_RUNNING, 1),
		run("r1", godwitv1.RunState_RUN_STATE_RUNNING, 1),
		run("r1", godwitv1.RunState_RUN_STATE_SUCCEEDED, 1),
	}}
	url := startStub(t, stub)

	code, out, errOut := runCLI("migrate", "--server", url, "--target", "app", "--dir", goodMigs(t),
		"--rollout", "expand-contract", "--ack", "H001,H003", "--skip-validation", "--lock-timeout", "3s", "--statement-timeout", "0",
		"--source", "github.com/org/repo@abc:db")
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
	want := "no stored plan for this set: implicit plan\nrun r1: queued\nrun r1: running (attempt 1)\nrun r1: succeeded (attempt 1)\n"
	if out != want {
		t.Fatalf("out = %q, want %q", out, want)
	}
	c := stub.created
	if c.Target != "app" || c.Rollout != "expand-contract" || !c.SkipValidation || strings.Join(c.AcknowledgeHazards, ",") != "H001,H003" ||
		c.LockTimeout != "3s" || c.StatementTimeout != "0" || c.Source != "github.com/org/repo@abc:db" {
		t.Fatalf("request = %v", c)
	}
	if len(c.Files) != 2 || c.Files[0].Name != "20260901120000_users.up.sql" || c.Files[1].Name != "20260901120000_users.down.sql" ||
		c.Files[1].Body != "DROP TABLE users;" {
		t.Fatalf("files = %v", c.Files)
	}
	if stub.watched != "r1" {
		t.Fatalf("watched = %q", stub.watched)
	}
}

func dryRunStub() *stubService {
	return &stubService{plan: &godwitv1.PlanRunResponse{
		Target: "app", Rollout: "expand-contract", Validated: true,
		Migrations: []*godwitv1.PlannedMigration{
			{Version: 20260901120000, Name: "users", Checksum: "c1", Applied: true, Phase: "expand", Statements: []*godwitv1.PlannedStatement{
				{Sql: "CREATE TABLE users (id int)"},
				{Sql: "CREATE INDEX CONCURRENTLY idx_users ON users (id)", NoTx: true},
			}},
			{Version: 20260901120001, Name: "drop_a", Checksum: "c2", Phase: "contract", Statements: []*godwitv1.PlannedStatement{
				{Sql: "ALTER TABLE users DROP COLUMN a", Hazards: []*godwitv1.PlannedHazard{{Code: "H003", Detail: "DROP COLUMN is destructive", Recipe: "-- expand then contract:\n-- drop users.a later"}}},
			}},
		},
	}}
}

func TestMigrateDryRun(t *testing.T) {
	t.Parallel()
	stub := dryRunStub()
	url := startStub(t, stub)

	code, out, errOut := runCLI("migrate", "--dry-run", "--server", url, "--target", "app", "--dir", goodMigs(t),
		"--rollout", "expand-contract", "--ack", "H003", "--skip-validation", "--allow-out-of-order")
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
	want := "dry run on app (rollout expand-contract, validated on a scratch database)\n" +
		"20260901120000_users (up): 2 statement(s) [expand, applied]\n" +
		"  [0] tx    CREATE TABLE users (id int)\n" +
		"  [1] no-tx CREATE INDEX CONCURRENTLY idx_users ON users (id)\n" +
		"20260901120001_drop_a (up): 1 statement(s) [contract, pending]\n" +
		"  [0] tx    ALTER TABLE users DROP COLUMN a\n" +
		"        hazard H003: DROP COLUMN is destructive\n" +
		"          -- expand then contract:\n" +
		"          -- drop users.a later\n"
	if out != want {
		t.Fatalf("out = %q, want %q", out, want)
	}
	p := stub.planned
	if p.Target != "app" || p.Rollout != "expand-contract" || !p.SkipValidation || !p.AllowOutOfOrder || strings.Join(p.AcknowledgeHazards, ",") != "H003" ||
		len(p.Files) != 2 || p.Files[0].Name != "20260901120000_users.up.sql" {
		t.Fatalf("request = %v", p)
	}
	if stub.created != nil || stub.watched != "" {
		t.Fatal("dry run must not create or watch a run")
	}
}

func TestMigrateDryRunMarkdown(t *testing.T) {
	t.Parallel()
	stub := dryRunStub()
	stub.plan.Validated = false
	url := startStub(t, stub)

	code, out, errOut := runCLI("migrate", "--dry-run", "--format", "markdown", "--server", url, "--target", "app", "--dir", goodMigs(t))
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
	want := "## godwit dry run\n\nTarget `app`, rollout `expand-contract`, not validated.\n\n" +
		"| Migration | Direction | # | Mode | Statement | Hazards | Phase | Status |\n" +
		"|---|---|---|---|---|---|---|---|\n" +
		"| `20260901120000_users` | up | 0 | tx | `CREATE TABLE users (id int)` |  | expand | applied |\n" +
		"| `20260901120000_users` | up | 1 | no-tx | `CREATE INDEX CONCURRENTLY idx_users ON users (id)` |  | expand | applied |\n" +
		"| `20260901120001_drop_a` | up | 0 | tx | `ALTER TABLE users DROP COLUMN a` | H003: DROP COLUMN is destructive | contract | pending |\n\n" +
		"<details><summary>recipe for H003 in `20260901120001_drop_a` (up) #0</summary>\n\n```sql\n-- expand then contract:\n-- drop users.a later\n```\n\n</details>\n\n" +
		"⚠️ 1 hazard(s); acknowledge them with `--ack`\n"
	if out != want {
		t.Fatalf("out = %q, want %q", out, want)
	}
}

func TestMigrateDryRunJSON(t *testing.T) {
	t.Parallel()
	stub := dryRunStub()
	url := startStub(t, stub)

	code, out, errOut := runCLI("migrate", "--dry-run", "--format", "json", "--server", url, "--target", "app", "--dir", goodMigs(t))
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
	var got dryRunJSON
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if got.Target != "app" || got.Rollout != "expand-contract" || !got.Validated || len(got.Migrations) != 2 {
		t.Fatalf("plan = %+v", got)
	}
	first, second := got.Migrations[0], got.Migrations[1]
	if !first.Applied || first.Phase != "expand" || first.Direction != "up" || first.Statements[1].Mode != "no-tx" ||
		second.Applied || second.Phase != "contract" || second.Statements[0].Hazards[0].Code != "H003" ||
		second.Statements[0].Hazards[0].Recipe != "-- expand then contract:\n-- drop users.a later" {
		t.Fatalf("migrations = %+v", got.Migrations)
	}

	code, out, _ = runCLI("migrate", "--dry-run", "--json", "--server", url, "--target", "app", "--dir", goodMigs(t))
	raw := decodeJSON(t, out)
	if code != 0 || raw["target"] != "app" || raw["validated"] != true || len(raw["migrations"].([]any)) != 2 {
		t.Fatalf("code = %d, out = %q", code, out)
	}
}

func TestMigrateDryRunErrors(t *testing.T) {
	t.Parallel()
	stub := dryRunStub()
	url := startStub(t, stub)

	if code, _, errOut := runCLI("migrate", "--dry-run", "--format", "yaml", "--server", url, "--target", "app", "--dir", goodMigs(t)); code != 1 ||
		!strings.Contains(errOut, "unknown format") {
		t.Fatalf("code = %d, stderr = %q", code, errOut)
	}
	stub.err = connect.NewError(connect.CodeFailedPrecondition, errors.New("unacknowledged hazards: H003"))
	code, out, errOut := runCLI("migrate", "--dry-run", "--server", url, "--target", "app", "--dir", goodMigs(t))
	if code != 1 || out != "" || errOut != "godwit: unacknowledged hazards: H003\n" {
		t.Fatalf("code = %d, out = %q, stderr = %q", code, out, errOut)
	}
}

func TestMigrateJSON(t *testing.T) {
	t.Parallel()
	stub := &stubService{events: []*godwitv1.Run{
		run("r1", godwitv1.RunState_RUN_STATE_RUNNING, 1),
		run("r1", godwitv1.RunState_RUN_STATE_AWAITING_CONTRACT, 1),
	}}
	url := startStub(t, stub)

	code, out, errOut := runCLI("migrate", "--server", url, "--target", "app", "--dir", goodMigs(t), "--json")
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %q", lines)
	}
	last := decodeJSON(t, lines[1])["run"].(map[string]any)
	if last["state"] != "RUN_STATE_AWAITING_CONTRACT" || last["id"] != "r1" {
		t.Fatalf("last = %v", last)
	}
}

func TestMigrateExitPaths(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		events []*godwitv1.Run
		code   int
		stderr string
	}{
		{"awaiting", []*godwitv1.Run{run("r1", godwitv1.RunState_RUN_STATE_AWAITING_CONTRACT, 1)}, 0, ""},
		{"failed", []*godwitv1.Run{{Id: "r1", State: godwitv1.RunState_RUN_STATE_FAILED, Attempts: 1, Error: "boom"}}, 1, "godwit: run r1 failed: boom\n"},
		{"attention", []*godwitv1.Run{{Id: "r1", State: godwitv1.RunState_RUN_STATE_NEEDS_ATTENTION, Attempts: 3, Error: "budget"}}, 1, "godwit: run r1 needs_attention: budget\n"},
		{"empty", nil, 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			url := startStub(t, &stubService{events: tc.events})
			code, out, errOut := runCLI("migrate", "--server", url, "--target", "app", "--dir", goodMigs(t))
			if code != tc.code || errOut != tc.stderr {
				t.Fatalf("code = %d, stdout = %q, stderr = %q", code, out, errOut)
			}
		})
	}
}

func TestMigrateFailedPrintsErrorLine(t *testing.T) {
	t.Parallel()
	url := startStub(t, &stubService{events: []*godwitv1.Run{
		{Id: "r1", State: godwitv1.RunState_RUN_STATE_FAILED, Attempts: 2, Error: "boom"},
	}})

	_, out, _ := runCLI("migrate", "--server", url, "--target", "app", "--dir", goodMigs(t))
	if out != "no stored plan for this set: implicit plan\nrun r1: failed (attempt 2): boom\n" {
		t.Fatalf("out = %q", out)
	}
}

func TestMigrateBoundPlan(t *testing.T) {
	t.Parallel()
	stub := &stubService{planID: "p1", events: []*godwitv1.Run{run("r1", godwitv1.RunState_RUN_STATE_SUCCEEDED, 1)}}
	url := startStub(t, stub)

	code, out, errOut := runCLI("migrate", "--server", url, "--target", "app", "--dir", goodMigs(t))
	if code != 0 || out != "plan p1: bound\nrun r1: succeeded (attempt 1)\n" {
		t.Fatalf("code = %d, out = %q, stderr = %q", code, out, errOut)
	}
}

func TestMigrateReattached(t *testing.T) {
	t.Parallel()
	stub := &stubService{planID: "p1", reattached: true, events: []*godwitv1.Run{
		{Id: "r1", State: godwitv1.RunState_RUN_STATE_QUEUED, Attempts: 1, Error: "transient: lock", NotBefore: timestamppb.New(time.Now().Add(90 * time.Second))},
		run("r1", godwitv1.RunState_RUN_STATE_SUCCEEDED, 2),
	}}
	url := startStub(t, stub)

	code, out, errOut := runCLI("migrate", "--server", url, "--target", "app", "--dir", goodMigs(t))
	if code != 0 || !strings.HasPrefix(out, "re-attached to run r1\nrun r1: queued (attempt 1): transient: lock (retry in 1m") ||
		!strings.HasSuffix(out, "run r1: succeeded (attempt 2)\n") {
		t.Fatalf("code = %d, out = %q, stderr = %q", code, out, errOut)
	}
}

func TestMigrate_StaleExit3(t *testing.T) {
	t.Parallel()
	msg := "plan abcd1234 on app is stale (planned 2026-09-01T10:00:00Z by ci)\n  reason : schema\nfix: push to the pull request (re-plan)"
	cases := map[string]proto.Message{
		"stale":    &godwitv1.PlanStale{PlanId: "abcd1234", Reason: "schema"},
		"required": &godwitv1.PlanRequired{Target: "app", Key: "k"},
	}
	for name, detail := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cerr := connect.NewError(connect.CodeFailedPrecondition, errors.New(msg))
			d, err := connect.NewErrorDetail(detail)
			if err != nil {
				t.Fatal(err)
			}
			cerr.AddDetail(d)
			url := startStub(t, &stubService{err: cerr})

			code, out, errOut := runCLI("migrate", "--server", url, "--target", "app", "--dir", goodMigs(t))
			if code != ExitPlanRefused || out != "" || errOut != "godwit: "+msg+"\n" {
				t.Fatalf("code = %d, out = %q, stderr = %q", code, out, errOut)
			}
		})
	}
}

func TestMigrateOtherDetailExit1(t *testing.T) {
	t.Parallel()
	cerr := connect.NewError(connect.CodeFailedPrecondition, errors.New("nope"))
	d, err := connect.NewErrorDetail(&godwitv1.Run{Id: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	cerr.AddDetail(d)
	url := startStub(t, &stubService{err: cerr})

	code, _, errOut := runCLI("migrate", "--server", url, "--target", "app", "--dir", goodMigs(t))
	if code != 1 || errOut != "godwit: nope\n" {
		t.Fatalf("code = %d, stderr = %q", code, errOut)
	}
}

func TestTargetAdd_RequirePlan(t *testing.T) {
	t.Parallel()
	stub := &stubService{}
	url := startStub(t, stub)

	code, _, errOut := runCLI("target", "add", "app", "--server", url, "--provider", "static", "--dsn", "d", "--require-plan")
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
	if !stub.registered.RequirePlan {
		t.Fatalf("request = %v", stub.registered)
	}
}

func TestMigrateHazardRefusedVerbatim(t *testing.T) {
	t.Parallel()
	msg := "unacknowledged hazards: H002 (DROP TABLE users)"
	url := startStub(t, &stubService{err: connect.NewError(connect.CodeInvalidArgument, errors.New(msg))})

	code, _, errOut := runCLI("migrate", "--server", url, "--target", "app", "--dir", goodMigs(t))
	if code != 1 || errOut != "godwit: "+msg+"\n" {
		t.Fatalf("code = %d, stderr = %q", code, errOut)
	}
}

func TestMigrateBadDir(t *testing.T) {
	t.Parallel()
	url := startStub(t, &stubService{})

	code, _, errOut := runCLI("migrate", "--server", url, "--target", "app", "--dir", t.TempDir()+"/missing")
	if code != 1 || !strings.Contains(errOut, "read migration dir") {
		t.Fatalf("code = %d, stderr = %q", code, errOut)
	}
}

func TestRevert(t *testing.T) {
	t.Parallel()
	stub := &stubService{revertID: "r2", events: []*godwitv1.Run{
		run("r2", godwitv1.RunState_RUN_STATE_RUNNING, 1),
		run("r2", godwitv1.RunState_RUN_STATE_SUCCEEDED, 1),
	}}
	url := startStub(t, stub)

	code, out, errOut := runCLI("revert", "r1", "--server", url, "--ack", "H003", "--skip-validation", "--lock-timeout", "1s", "--statement-timeout", "30s")
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
	if out != "run r2: running (attempt 1)\nrun r2: succeeded (attempt 1)\n" {
		t.Fatalf("out = %q", out)
	}
	rv := stub.reverted
	if rv.RunId != "r1" || !rv.SkipValidation || strings.Join(rv.AcknowledgeHazards, ",") != "H003" || stub.watched != "r2" ||
		rv.LockTimeout != "1s" || rv.StatementTimeout != "30s" {
		t.Fatalf("request = %v, watched = %q", rv, stub.watched)
	}

	stub.err = connect.NewError(connect.CodeFailedPrecondition, errors.New("run is not the latest on its target"))
	code, _, errOut = runCLI("revert", "r1", "--server", url)
	if code != 1 || errOut != "godwit: run is not the latest on its target\n" {
		t.Fatalf("code = %d, stderr = %q", code, errOut)
	}
}

func TestRunGet(t *testing.T) {
	t.Parallel()
	created := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	stub := &stubService{run: &godwitv1.Run{
		Id: "r1", Target: "app", State: godwitv1.RunState_RUN_STATE_SUCCEEDED, Attempts: 1,
		Rollout: "expand-contract", Phase: "contract", Reverts: "r0", Kind: "migrate", LockTimeout: "2s", StatementTimeout: "1m",
		CreatedBy: "ci", Source: "github.com/org/repo@abc:db", PlanId: "p1", CreatedAt: timestamppb.New(created), FinishedAt: timestamppb.New(created.Add(time.Minute)),
	}}
	url := startStub(t, stub)

	code, out, errOut := runCLI("run", "get", "r1", "--server", url)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
	want := "run r1: succeeded (attempt 1)\n  target: app\n  kind: migrate\n  rollout: expand-contract\n  phase: contract\n  reverts: r0\n" +
		"  lock_timeout: 2s\n  statement_timeout: 1m\n  created_by: ci\n  source: github.com/org/repo@abc:db\n  plan: p1\n  created: 2026-09-01T12:00:00Z\n  finished: 2026-09-01T12:01:00Z\n"
	if out != want || stub.got != "r1" {
		t.Fatalf("out = %q, got = %q", out, stub.got)
	}

	code, out, _ = runCLI("run", "get", "r1", "--server", url, "--json")
	if code != 0 || decodeJSON(t, out)["run"].(map[string]any)["phase"] != "contract" {
		t.Fatalf("code = %d, out = %q", code, out)
	}

	stub.err = connect.NewError(connect.CodeNotFound, errors.New("run r1: not found"))
	code, _, errOut = runCLI("run", "get", "r1", "--server", url)
	if code != 1 || errOut != "godwit: run r1: not found\n" {
		t.Fatalf("code = %d, stderr = %q", code, errOut)
	}
}

func TestRunWatch(t *testing.T) {
	t.Parallel()
	stub := &stubService{events: []*godwitv1.Run{run("r1", godwitv1.RunState_RUN_STATE_SUCCEEDED, 1)}}
	url := startStub(t, stub)

	code, out, errOut := runCLI("run", "watch", "r1", "--server", url, "--token", "tok")
	if code != 0 || out != "run r1: succeeded (attempt 1)\n" || stub.auth != "Bearer tok" {
		t.Fatalf("code = %d, out = %q, stderr = %q, auth = %q", code, out, errOut, stub.auth)
	}

	stub.err = connect.NewError(connect.CodeNotFound, errors.New("run r1: not found"))
	code, _, errOut = runCLI("run", "watch", "r1", "--server", url)
	if code != 1 || errOut != "godwit: run r1: not found\n" {
		t.Fatalf("code = %d, stderr = %q", code, errOut)
	}

	code, _, errOut = runCLI("run", "watch", "r1", "--server", "::bad")
	if code != 1 || !strings.HasPrefix(errOut, "godwit: ") {
		t.Fatalf("code = %d, stderr = %q", code, errOut)
	}
}

func TestRunResumeAndConfirm(t *testing.T) {
	t.Parallel()
	stub := &stubService{}
	url := startStub(t, stub)

	code, out, _ := runCLI("run", "resume", "r1", "--server", url)
	if code != 0 || out != "run r1: resumed\n" || stub.resumed != "r1" {
		t.Fatalf("code = %d, out = %q, resumed = %q", code, out, stub.resumed)
	}
	code, out, _ = runCLI("run", "confirm", "r2", "--server", url, "--json")
	if code != 0 || len(decodeJSON(t, out)) != 0 || stub.confirmed != "r2" {
		t.Fatalf("code = %d, out = %q, confirmed = %q", code, out, stub.confirmed)
	}

	stub.err = connect.NewError(connect.CodeFailedPrecondition, errors.New("not resumable"))
	if code, _, errOut := runCLI("run", "resume", "r1", "--server", url); code != 1 || errOut != "godwit: not resumable\n" {
		t.Fatalf("code = %d, stderr = %q", code, errOut)
	}
	if code, _, errOut := runCLI("run", "confirm", "r1", "--server", url); code != 1 || errOut != "godwit: not resumable\n" {
		t.Fatalf("code = %d, stderr = %q", code, errOut)
	}
}

func TestRunConfirmLatest(t *testing.T) {
	t.Parallel()
	stub := &stubService{runs: []*godwitv1.Run{
		run("r3", godwitv1.RunState_RUN_STATE_SUCCEEDED, 1),
		run("r2", godwitv1.RunState_RUN_STATE_AWAITING_CONTRACT, 1),
		run("r1", godwitv1.RunState_RUN_STATE_AWAITING_CONTRACT, 1),
	}}
	url := startStub(t, stub)

	code, out, _ := runCLI("run", "confirm", "--latest", "--target", "app", "--server", url)
	if code != 0 || out != "run r2: contract confirmed\n" || stub.confirmed != "r2" || stub.listed.Target != "app" {
		t.Fatalf("code = %d, out = %q, confirmed = %q, listed = %v", code, out, stub.confirmed, stub.listed)
	}

	for _, tc := range []struct{ args, want string }{
		{"r1 --latest --target app", "pass a run id or --latest, not both"},
		{"--latest", "--latest requires --target (or target in godwit.yaml)"},
		{"", "a run id or --latest is required"},
	} {
		args := append([]string{"run", "confirm"}, strings.Fields(tc.args)...)
		code, _, errOut := runCLI(append(args, "--server", url)...)
		if code != 1 || errOut != "godwit: "+tc.want+"\n" {
			t.Fatalf("%q: code = %d, stderr = %q", tc.args, code, errOut)
		}
	}

	stub.runs = stub.runs[:1]
	code, _, errOut := runCLI("run", "confirm", "--latest", "--target", "app", "--server", url)
	if code != 1 || errOut != "godwit: target app: no run awaiting contract\n" {
		t.Fatalf("code = %d, stderr = %q", code, errOut)
	}
	stub.confirmed = ""
	code, out, _ = runCLI("run", "confirm", "--latest", "--target", "app", "--allow-none", "--server", url)
	if code != 0 || out != "target app: no run awaiting contract\n" || stub.confirmed != "" {
		t.Fatalf("code = %d, out = %q, confirmed = %q", code, out, stub.confirmed)
	}
	code, out, _ = runCLI("run", "confirm", "--latest", "--target", "app", "--allow-none", "--json", "--server", url)
	if code != 0 || len(decodeJSON(t, out)["runs"].([]any)) != 1 {
		t.Fatalf("code = %d, out = %q", code, out)
	}

	stub.err = connect.NewError(connect.CodeUnavailable, errors.New("store down"))
	if code, _, errOut := runCLI("run", "confirm", "--latest", "--target", "app", "--server", url); code != 1 || errOut != "godwit: store down\n" {
		t.Fatalf("code = %d, stderr = %q", code, errOut)
	}
}

func TestRuns(t *testing.T) {
	t.Parallel()
	created := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	stub := &stubService{runs: []*godwitv1.Run{
		{Id: "r1", Target: "app", Kind: "baseline", State: godwitv1.RunState_RUN_STATE_SUCCEEDED, Rollout: "direct", CreatedBy: "ops", CreatedAt: timestamppb.New(created)},
		{Id: "r2", Target: "app", Kind: "migrate", State: godwitv1.RunState_RUN_STATE_AWAITING_CONTRACT, Rollout: "expand-contract", Phase: "expand", CreatedBy: "ci", Source: "repo@sha:db"},
	}}
	url := startStub(t, stub)

	code, out, errOut := runCLI("runs", "--server", url, "--target", "app")
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
	want := "ID  TARGET  KIND      STATE              ROLLOUT          PHASE   BY   SOURCE       CREATED\n" +
		"r1  app     baseline  succeeded          direct                   ops               2026-09-01T12:00:00Z\n" +
		"r2  app     migrate   awaiting_contract  expand-contract  expand  ci   repo@sha:db  \n"
	if out != want || stub.listed.Target != "app" {
		t.Fatalf("out = %q, target = %q", out, stub.listed.Target)
	}

	code, out, _ = runCLI("runs", "--server", url, "--json")
	if code != 0 || len(decodeJSON(t, out)["runs"].([]any)) != 2 || stub.listed.Target != "" {
		t.Fatalf("code = %d, out = %q", code, out)
	}

	stub.err = connect.NewError(connect.CodeUnauthenticated, errors.New("missing bearer token"))
	if code, _, errOut := runCLI("runs", "--server", url); code != 1 || errOut != "godwit: missing bearer token\n" {
		t.Fatalf("code = %d, stderr = %q", code, errOut)
	}
}

func TestDrift(t *testing.T) {
	t.Parallel()
	stub := &stubService{drift: &godwitv1.CheckDriftResponse{Drifted: true, Diff: "+table rogue"}}
	url := startStub(t, stub)

	code, out, _ := runCLI("drift", "check", "app", "--server", url)
	if code != 0 || out != "target app: drifted\n+table rogue\n" || stub.checked != "app" {
		t.Fatalf("code = %d, out = %q, checked = %q", code, out, stub.checked)
	}

	stub.drift = &godwitv1.CheckDriftResponse{}
	code, out, _ = runCLI("drift", "check", "app", "--server", url)
	if code != 0 || out != "target app: no drift\n" {
		t.Fatalf("code = %d, out = %q", code, out)
	}

	code, out, _ = runCLI("drift", "check", "app", "--server", url, "--json")
	if code != 0 || len(decodeJSON(t, out)) != 0 {
		t.Fatalf("code = %d, out = %q", code, out)
	}

	code, out, _ = runCLI("drift", "accept", "app", "--server", url)
	if code != 0 || out != "target app: baseline accepted\n" || stub.accepted != "app" {
		t.Fatalf("code = %d, out = %q, accepted = %q", code, out, stub.accepted)
	}

	stub.err = connect.NewError(connect.CodeNotFound, errors.New("target app: not found"))
	if code, _, errOut := runCLI("drift", "check", "app", "--server", url); code != 1 || errOut != "godwit: target app: not found\n" {
		t.Fatalf("code = %d, stderr = %q", code, errOut)
	}
	if code, _, errOut := runCLI("drift", "accept", "app", "--server", url); code != 1 || errOut != "godwit: target app: not found\n" {
		t.Fatalf("code = %d, stderr = %q", code, errOut)
	}
}

func TestAudit(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	stub := &stubService{entries: []*godwitv1.AuditEntry{
		{Id: 2, At: timestamppb.New(at), Actor: "ci", Action: "run.create", Target: "app", RunId: "r1", Detail: "rollout=direct migrations=2"},
		{Id: 1, At: timestamppb.New(at), Actor: "ops", Action: "target.register", Target: "app"},
	}}
	url := startStub(t, stub)

	code, out, errOut := runCLI("audit", "--server", url, "--token", "tok", "--target", "app", "--run", "r1", "--limit", "5")
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
	want := "AT                    ACTOR  ACTION           TARGET  RUN  DETAIL\n" +
		"2026-09-01T12:00:00Z  ci     run.create       app     r1   rollout=direct migrations=2\n" +
		"2026-09-01T12:00:00Z  ops    target.register  app          \n"
	if out != want || stub.audited.Target != "app" || stub.audited.RunId != "r1" || stub.audited.Limit != 5 || stub.auth != "Bearer tok" {
		t.Fatalf("out = %q, request = %v, auth = %q", out, stub.audited, stub.auth)
	}

	code, out, _ = runCLI("audit", "--server", url, "--json")
	if code != 0 || len(decodeJSON(t, out)["entries"].([]any)) != 2 {
		t.Fatalf("code = %d, out = %q", code, out)
	}

	stub.err = connect.NewError(connect.CodeUnauthenticated, errors.New("invalid or missing bearer token"))
	if code, _, errOut := runCLI("audit", "--server", url); code != 1 || errOut != "godwit: invalid or missing bearer token\n" {
		t.Fatalf("code = %d, stderr = %q", code, errOut)
	}
}

func storedPlanStub() *stubService {
	stub := dryRunStub()
	stub.plan.PlanId = "p1"
	stub.plan.PlanKey = "k1"
	stub.plan.Drift = "+ table public.orders\n- index public.idx_old"
	stub.plan.Observed = &godwitv1.PlanObservation{
		HistoryHash: "h1", SchemaFingerprint: "f1", AppliedCount: 1, NewestApplied: 20260901120000,
		At: timestamppb.New(time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)),
	}

	return stub
}

func TestPlan_RemotePersists(t *testing.T) {
	t.Parallel()
	stub := storedPlanStub()
	url := startStub(t, stub)

	code, out, errOut := runCLI("plan", "--server", url, "--token", "tok", "--target", "app", "--dir", goodMigs(t),
		"--rollout", "expand-contract", "--ack", "H003", "--skip-validation", "--allow-out-of-order", "--source", "repo@sha:db")
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
	want := "plan p1 on app (rollout expand-contract, validated on a scratch database)\n" +
		"key: k1\n" +
		"observed: 1 applied, newest 20260901120000, history h1, schema f1, at 2026-09-01T10:00:00Z\n" +
		"drift since baseline:\n" +
		"  + table public.orders\n" +
		"  - index public.idx_old\n" +
		"20260901120000_users (up): 2 statement(s) [expand, applied]\n"
	if !strings.HasPrefix(out, want) {
		t.Fatalf("out = %q, want prefix %q", out, want)
	}
	p := stub.planned
	if !p.Persist || p.Target != "app" || p.Rollout != "expand-contract" || !p.SkipValidation || !p.AllowOutOfOrder ||
		strings.Join(p.AcknowledgeHazards, ",") != "H003" || p.Source != "repo@sha:db" || len(p.Files) != 2 || stub.auth != "Bearer tok" {
		t.Fatalf("request = %v, auth = %q", p, stub.auth)
	}
}

func TestPlan_RemoteFormats(t *testing.T) {
	t.Parallel()
	url := startStub(t, storedPlanStub())

	_, out, _ := runCLI("plan", "--server", url, "--target", "app", "--dir", goodMigs(t), "--format", "markdown")
	for _, want := range []string{"## godwit plan p1\n", "\nkey: k1\n", "\nobserved: 1 applied", "\n### Changes outside migrations\n\n```diff\n+ table public.orders\n- index public.idx_old\n```\n\n| Migration"} {
		if !strings.Contains(out, want) {
			t.Fatalf("markdown lacks %q:\n%s", want, out)
		}
	}
	_, out, _ = runCLI("plan", "--server", url, "--target", "app", "--dir", goodMigs(t), "--format", "json")
	var got dryRunJSON
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if got.PlanID != "p1" || got.PlanKey != "k1" || got.Observed == nil || got.Observed.NewestApplied != 20260901120000 ||
		got.Drift != "+ table public.orders\n- index public.idx_old" {
		t.Fatalf("json = %+v", got)
	}
	_, out, _ = runCLI("plan", "--server", url, "--target", "app", "--dir", goodMigs(t), "--json")
	if m := decodeJSON(t, out); m["planId"] != "p1" {
		t.Fatalf("raw json = %s", out)
	}
}

func TestPlan_RemoteErrors(t *testing.T) {
	t.Parallel()
	if code, _, errOut := runCLI("plan", "--target", "app", "--dir", goodMigs(t)); code != 1 || !strings.Contains(errOut, "--server") {
		t.Fatalf("code = %d, stderr = %q", code, errOut)
	}
	url := startStub(t, &stubService{err: connect.NewError(connect.CodeInvalidArgument, errors.New("20260901120000_users applied with different content"))})
	code, _, errOut := runCLI("plan", "--server", url, "--target", "app", "--dir", goodMigs(t))
	if code != 1 || errOut != "godwit: 20260901120000_users applied with different content\n" {
		t.Fatalf("code = %d, stderr = %q", code, errOut)
	}
	if code, _, errOut := runCLI("plan", "--server", url, "--target", "app", "--dir", t.TempDir()+"/missing"); code != 1 ||
		!strings.Contains(errOut, "read migration dir") {
		t.Fatalf("code = %d, stderr = %q", code, errOut)
	}
}

func alreadyAppliedStub() *stubService {
	return &stubService{plan: &godwitv1.PlanRunResponse{
		Target: "app", Rollout: "direct", Validated: true, PlanId: "p1", PlanKey: "k1",
		Migrations: []*godwitv1.PlannedMigration{
			{Version: 20260901120000, Name: "users", Checksum: "c1", Applied: true, Phase: "expand", Statements: []*godwitv1.PlannedStatement{
				{Sql: "CREATE TABLE users (id int)"},
			}},
			{
				Version: 20260901120001, Name: "email", Checksum: "c2", Phase: "expand", AlreadyApplied: true,
				Effect:     "+ column public.users.email text null=YES default=<none>",
				Statements: []*godwitv1.PlannedStatement{{Sql: "ALTER TABLE users ADD COLUMN email text"}},
			},
			{Version: 20260901120002, Name: "seed", Checksum: "c3", Phase: "expand", Note: "has DML, must execute", Statements: []*godwitv1.PlannedStatement{
				{Sql: "INSERT INTO users (id) VALUES (1)", Hazards: []*godwitv1.PlannedHazard{{Code: "H011", Detail: "seed rows in a migration"}}},
			}},
		},
	}}
}

func TestMigrateDryRunAlreadyApplied(t *testing.T) {
	t.Parallel()
	url := startStub(t, alreadyAppliedStub())

	code, out, errOut := runCLI("migrate", "--dry-run", "--server", url, "--target", "app", "--dir", goodMigs(t))
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
	if !strings.Contains(out, "20260901120001_email (up): 1 statement(s) [expand, already applied]\n") ||
		!strings.Contains(out, "20260901120002_seed (up): 1 statement(s) [expand, pending (has DML, must execute)]\n") {
		t.Fatalf("out = %q", out)
	}

	code, out, _ = runCLI("migrate", "--dry-run", "--format", "markdown", "--server", url, "--target", "app", "--dir", goodMigs(t))
	if code != 0 || !strings.Contains(out, "| `20260901120001_email` | up | 0 | tx | `ALTER TABLE users ADD COLUMN email text` |  | expand | already applied |\n") ||
		!strings.Contains(out, "| H011: seed rows in a migration | expand | pending (has DML, must execute) |\n") ||
		!strings.Contains(out, "\n`20260901120001_email` is already applied by hand; migrate records it without executing:\n\n```diff\n+ column public.users.email text null=YES default=<none>\n```\n\n⚠️ 1 hazard(s); acknowledge them with `--ack`\n") ||
		strings.Contains(out, "<details>") {
		t.Fatalf("code = %d, out = %q", code, out)
	}

	code, out, _ = runCLI("migrate", "--dry-run", "--format", "json", "--server", url, "--target", "app", "--dir", goodMigs(t))
	var got dryRunJSON
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	m := got.Migrations
	if code != 0 || len(m) != 3 || m[0].AlreadyApplied || !m[1].AlreadyApplied || !strings.HasPrefix(m[1].Effect, "+ column") ||
		m[2].AlreadyApplied || m[2].Note != "has DML, must execute" {
		t.Fatalf("code = %d, migrations = %+v", code, m)
	}
}
