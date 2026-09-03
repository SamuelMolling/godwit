package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
)

func TestTargetStatus(t *testing.T) {
	t.Parallel()
	at := timestamppb.New(time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC))
	stub := &stubService{status: &godwitv1.GetTargetStatusResponse{
		Target: "app", Provider: "static", LockTimeout: "3s", SearchPath: "app,public",
		Applied: []*godwitv1.AppliedMigration{
			{Version: 1, Name: "init", Checksum: "c", AppliedAt: at},
			{Version: 2, Name: "users", Checksum: "d", AppliedAt: at, ChecksumMismatch: true},
		},
		Pending:       []*godwitv1.PendingMigration{{Version: 3, Name: "next"}},
		LastRun:       &godwitv1.Run{Id: "r1", Kind: "migrate", State: godwitv1.RunState_RUN_STATE_SUCCEEDED, FinishedAt: at},
		DriftBaseline: &godwitv1.DriftBaseline{TakenAt: at, RunId: "r1", UnresolvedDrift: true},
		ReadyPlans:    2,
	}}
	url := startStub(t, stub)

	code, out, errOut := runCLI("target", "status", "app", "--server", url, "--dir", goodMigs(t))
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
	want := strings.Join([]string{
		"target app: provider static, lock timeout 3s, statement timeout none, search path app,public",
		"applied (2):",
		"  00000000000001_init   2026-09-01T12:00:00Z  ",
		"  00000000000002_users  2026-09-01T12:00:00Z  checksum mismatch",
		"pending (1):",
		"  00000000000003_next",
		"last run: r1 migrate succeeded finished 2026-09-01T12:00:00Z",
		"ready plans: 2",
		"drift baseline: taken 2026-09-01T12:00:00Z by run r1, unresolved drift",
		"",
	}, "\n")
	if out != want {
		t.Fatalf("out = %q\nwant %q", out, want)
	}
	if stub.statused.Target != "app" || len(stub.statused.Files) != 2 {
		t.Fatalf("request = %v", stub.statused)
	}

	stub.status = &godwitv1.GetTargetStatusResponse{Target: "app", Provider: "static"}
	code, out, _ = runCLI("target", "status", "app", "--server", url)
	if code != 0 || !strings.Contains(out, "applied (0):\nlast run: none\nready plans: 0\ndrift baseline: none\n") || len(stub.statused.Files) != 0 {
		t.Fatalf("code = %d, out = %q, files = %v", code, out, stub.statused.Files)
	}
	code, out, _ = runCLI("target", "status", "app", "--server", url, "--json")
	if code != 0 || decodeJSON(t, out)["provider"] != "static" {
		t.Fatalf("code = %d, out = %q", code, out)
	}

	if code, _, errOut := runCLI("target", "status", "app", "--server", url, "--dir", "/nope"); code != 1 ||
		!strings.Contains(errOut, "read migration dir") {
		t.Fatalf("code = %d, stderr = %q", code, errOut)
	}
	stub.err = connect.NewError(connect.CodeNotFound, errors.New("target \"app\": not found"))
	if code, _, errOut := runCLI("target", "status", "app", "--server", url, "--dir", goodMigs(t)); code != 1 ||
		errOut != "godwit: target \"app\": not found\n" {
		t.Fatalf("code = %d, stderr = %q", code, errOut)
	}
}

func (s *stubService) ListTargets(_ context.Context, req *connect.Request[godwitv1.ListTargetsRequest]) (*connect.Response[godwitv1.ListTargetsResponse], error) {
	if err := s.record(req.Header()); err != nil {
		return nil, err
	}

	return connect.NewResponse(&godwitv1.ListTargetsResponse{Targets: s.summaries}), nil
}

func TestTargets(t *testing.T) {
	t.Parallel()
	stub := &stubService{summaries: []*godwitv1.TargetSummary{
		{
			Name: "app", Provider: "static", SearchPath: "app,public", LockTimeout: "3s", StatementTimeout: "1m",
			RequirePlan: true, KeepOld: false, AppliedCount: 7, ReadyPlans: 2, AttentionRuns: 1, UnresolvedDrift: true,
			LastRun: &godwitv1.Run{Id: "r1", State: godwitv1.RunState_RUN_STATE_NEEDS_ATTENTION},
		},
		{Name: "billing", Provider: "vault", KeepOld: true},
	}}
	url := startStub(t, stub)

	code, out, errOut := runCLI("targets", "--server", url)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
	want := "NAME     PROVIDER  APPLIED  READY PLANS  NEEDS YOU  DRIFT    SEARCH PATH  LOCK  STATEMENT  REQUIRE PLAN  LAST RUN\n" +
		"app      static    7        2            1          drifted  app,public   3s    1m         true          r1 needs_attention\n" +
		"billing  vault     0        0            0          clean    none         none  none       false         none\n"
	if out != want {
		t.Fatalf("out = %q\nwant %q", out, want)
	}

	code, out, _ = runCLI("targets", "--server", url, "--json")
	if code != 0 || len(decodeJSON(t, out)["targets"].([]any)) != 2 {
		t.Fatalf("code = %d, out = %q", code, out)
	}

	stub.err = connect.NewError(connect.CodeUnavailable, errors.New("store down"))
	if code, _, errOut := runCLI("targets", "--server", url); code != 1 || errOut != "godwit: store down\n" {
		t.Fatalf("code = %d, stderr = %q", code, errOut)
	}
}
