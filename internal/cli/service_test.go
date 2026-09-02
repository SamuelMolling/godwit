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
	"google.golang.org/protobuf/types/known/timestamppb"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/gen/godwit/v1/godwitv1connect"
)

type stubService struct {
	godwitv1connect.UnimplementedGodwitServiceHandler
	mu         sync.Mutex
	auth       string
	registered *godwitv1.RegisterTargetRequest
	created    *godwitv1.CreateRunRequest
	reverted   *godwitv1.RevertRunRequest
	listed     *godwitv1.ListRunsRequest
	got        string
	watched    string
	resumed    string
	confirmed  string
	checked    string
	accepted   string
	run        *godwitv1.Run
	runs       []*godwitv1.Run
	events     []*godwitv1.Run
	drift      *godwitv1.CheckDriftResponse
	revertID   string
	err        error
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

func (s *stubService) CreateRun(_ context.Context, req *connect.Request[godwitv1.CreateRunRequest]) (*connect.Response[godwitv1.CreateRunResponse], error) {
	s.created = req.Msg
	if err := s.record(req.Header()); err != nil {
		return nil, err
	}

	return connect.NewResponse(&godwitv1.CreateRunResponse{RunId: "r1"}), nil
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
		"--provider", "vault", "--dsn", "d", "--secret-path", "s", "--vault-path", "v", "--vault-template", "tpl")
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
	if r.Name != "app" || r.Provider != "vault" || r.Dsn != "d" || r.SecretPath != "s" || r.VaultPath != "v" || r.VaultTemplate != "tpl" {
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

func TestClientMissingServer(t *testing.T) {
	t.Parallel()

	code, _, errOut := runCLI("runs")
	if code != 1 || !strings.Contains(errOut, "--server (or GODWIT_SERVER) is required") {
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
		"--rollout", "expand-contract", "--ack", "H001,H003", "--skip-validation")
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
	want := "run r1: queued\nrun r1: running (attempt 1)\nrun r1: succeeded (attempt 1)\n"
	if out != want {
		t.Fatalf("out = %q, want %q", out, want)
	}
	c := stub.created
	if c.Target != "app" || c.Rollout != "expand-contract" || !c.SkipValidation || strings.Join(c.AcknowledgeHazards, ",") != "H001,H003" {
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
	if out != "run r1: failed (attempt 2): boom\n" {
		t.Fatalf("out = %q", out)
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

	code, out, errOut := runCLI("revert", "r1", "--server", url, "--ack", "H003", "--skip-validation")
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
	if out != "run r2: running (attempt 1)\nrun r2: succeeded (attempt 1)\n" {
		t.Fatalf("out = %q", out)
	}
	rv := stub.reverted
	if rv.RunId != "r1" || !rv.SkipValidation || strings.Join(rv.AcknowledgeHazards, ",") != "H003" || stub.watched != "r2" {
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
		Rollout: "expand-contract", Phase: "contract", Reverts: "r0",
		CreatedAt: timestamppb.New(created), FinishedAt: timestamppb.New(created.Add(time.Minute)),
	}}
	url := startStub(t, stub)

	code, out, errOut := runCLI("run", "get", "r1", "--server", url)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
	want := "run r1: succeeded (attempt 1)\n  target: app\n  rollout: expand-contract\n  phase: contract\n  reverts: r0\n" +
		"  created: 2026-09-01T12:00:00Z\n  finished: 2026-09-01T12:01:00Z\n"
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

func TestRuns(t *testing.T) {
	t.Parallel()
	created := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	stub := &stubService{runs: []*godwitv1.Run{
		{Id: "r1", Target: "app", State: godwitv1.RunState_RUN_STATE_SUCCEEDED, Rollout: "direct", CreatedAt: timestamppb.New(created)},
		{Id: "r2", Target: "app", State: godwitv1.RunState_RUN_STATE_AWAITING_CONTRACT, Rollout: "expand-contract", Phase: "expand"},
	}}
	url := startStub(t, stub)

	code, out, errOut := runCLI("runs", "--server", url, "--target", "app")
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
	want := "ID  TARGET  STATE              ROLLOUT          PHASE   CREATED\n" +
		"r1  app     succeeded          direct                   2026-09-01T12:00:00Z\n" +
		"r2  app     awaiting_contract  expand-contract  expand  \n"
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
