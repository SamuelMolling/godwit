package cli

import (
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
)

var reportStart = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

func reportRun() *godwitv1.Run {
	return &godwitv1.Run{
		Id: "r1", Target: "app", State: godwitv1.RunState_RUN_STATE_SUCCEEDED, Kind: "migrate", PlanId: "p1", Rollout: "direct",
		Source:    "github.com/acme/app@0419cdd1c2f3a4b5c6d7e8f90123456789abcdef:db/migrations",
		CreatedAt: timestamppb.New(reportStart), FinishedAt: timestamppb.New(reportStart.Add(2 * time.Second)),
	}
}

func ledgerRow(id string) *godwitv1.RunMigration {
	return &godwitv1.RunMigration{Migration: id, AppliedAt: timestamppb.New(reportStart)}
}

func TestRunReportCommand(t *testing.T) {
	stub := &stubService{
		run:     reportRun(),
		applied: []*godwitv1.RunMigration{ledgerRow("20260901120000_users")},
		stored:  storedPlanProto(),
	}
	url := startStub(t, stub)
	t.Setenv("GODWIT_PUBLIC_URL", "https://godwit.example.com")

	code, out, errOut := runCLI("run", "report", "r1", "--server", url, "--command", "apply", "--format", "markdown")
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
	if stub.got != "r1" || stub.planGot != "p1" {
		t.Fatalf("the report reads the run and the plan it was bound to: run %q, plan %q", stub.got, stub.planGot)
	}
	if !strings.HasPrefix(out, "## godwit apply\n") ||
		!strings.Contains(out, "run [`r1`](https://godwit.example.com/ui/runs/r1)") {
		t.Fatalf("unexpected report:\n%s", out)
	}

	code, out, errOut = runCLI("run", "report", "r1", "--server", url, "--json")
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
	if !strings.Contains(out, `"id":"r1"`) {
		t.Fatalf("--json prints the response:\n%s", out)
	}
}

func TestRunReportWithNoStoredPlan(t *testing.T) {
	t.Parallel()

	run := reportRun()
	run.PlanId = ""
	stub := &stubService{run: run, applied: []*godwitv1.RunMigration{ledgerRow("20260901120000_users")}}
	url := startStub(t, stub)

	code, out, errOut := runCLI("run", "report", "r1", "--server", url, "--format", "markdown")
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
	if stub.planGot != "" {
		t.Fatalf("an implicit run has no plan to read: %q", stub.planGot)
	}
	if !strings.Contains(out, "✅ **1 migration applied to `app`.** It took 2s.") {
		t.Fatalf("without a plan the ledger is the whole report:\n%s", out)
	}
}

func TestRunReportRefusals(t *testing.T) {
	t.Parallel()

	url := startStub(t, &stubService{run: reportRun(), stored: storedPlanProto()})
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--format", "json"}, `unknown format "json" (want text or markdown)`},
		{[]string{"--plan-format", "prose"}, `unknown plan format "prose" (want schema or statements)`},
	} {
		code, _, errOut := runCLI(append([]string{"run", "report", "r1", "--server", url}, tc.args...)...)
		if code == 0 || !strings.Contains(errOut, tc.want) {
			t.Fatalf("code = %d, stderr = %s, want %q", code, errOut, tc.want)
		}
	}

	refused := errors.New("refused")
	for _, stub := range []*stubService{
		{err: refused},
		{run: reportRun(), stored: storedPlanProto(), planErr: refused},
	} {
		if code, _, _ := runCLI("run", "report", "r1", "--server", startStub(t, stub)); code == 0 {
			t.Fatalf("a service that refuses must not produce a report")
		}
	}
}
