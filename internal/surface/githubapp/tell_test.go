package githubapp

import (
	"context"
	"strings"
	"testing"
	"time"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/internal/controlplane"
)

func settledRun(state godwitv1.RunState) *godwitv1.Run {
	return &godwitv1.Run{
		Id: "run-7", Target: "orders", State: state, Kind: "migrate",
		Source: "github.com/" + testRepo + "@" + testHead + ":db/migrations",
	}
}

func TestARunThatSettledIsReportedOnItsPullRequest(t *testing.T) {
	t.Parallel()

	h := newHarness(t, planningRepo(), WorkerConfig{})
	h.svc.run = settledRun(godwitv1.RunState_RUN_STATE_SUCCEEDED)
	h.svc.applied = []*godwitv1.RunMigration{{Migration: "20260101000000_add_orders"}}
	h.runs.put(boundRun("run-7", controlplane.StateSucceeded))
	h.worker.told(context.Background())

	if len(h.repo.notices) != 1 {
		t.Fatalf("comments = %v", h.repo.notices)
	}
	body := h.repo.notices[0]
	if !strings.HasPrefix(body, "<!-- godwit:migrate -->") {
		t.Fatalf("comment carries the wrong marker:\n%s", body)
	}
	if !strings.Contains(body, "1 migration applied to `orders`") {
		t.Fatalf("comment = %s", body)
	}
	if len(h.repo.ended) != 1 || h.repo.ended[0].conclusion != conclusionSuccess {
		t.Fatalf("check = %+v", h.repo.ended)
	}
	if h.repo.ended[0].url != "https://godwit.test/ui/runs/run-7" {
		t.Fatalf("details_url = %q", h.repo.ended[0].url)
	}
	if h.runs.told() != controlplane.StateSucceeded {
		t.Fatalf("reported state = %q", h.runs.told())
	}
}

func TestExpandAndContractAreEachReportedOnce(t *testing.T) {
	t.Parallel()

	h := newHarness(t, planningRepo(), WorkerConfig{})
	h.svc.run = settledRun(godwitv1.RunState_RUN_STATE_AWAITING_CONTRACT)
	h.runs.put(boundRun("run-7", controlplane.StateAwaitingContract))

	h.worker.told(context.Background())
	h.worker.told(context.Background())
	if len(h.repo.notices) != 1 {
		t.Fatalf("the held phase was reported %d times", len(h.repo.notices))
	}
	if h.repo.ended[0].conclusion != conclusionAction {
		t.Fatalf("a held contract phase concluded %q", h.repo.ended[0].conclusion)
	}
	if !strings.Contains(h.repo.ended[0].title, "godwit confirm") {
		t.Fatalf("title = %q", h.repo.ended[0].title)
	}

	h.runs.setState("run-7", controlplane.StateSucceeded)
	h.svc.run = settledRun(godwitv1.RunState_RUN_STATE_SUCCEEDED)
	h.worker.told(context.Background())
	h.worker.told(context.Background())
	if len(h.repo.notices) != 2 {
		t.Fatalf("comments = %d, want the held phase and the contract one", len(h.repo.notices))
	}
	if h.repo.ended[1].conclusion != conclusionSuccess {
		t.Fatalf("the contract phase concluded %q", h.repo.ended[1].conclusion)
	}
}

func TestAStoppedRunTurnsTheCheckRed(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		state string
		proto godwitv1.RunState
	}{
		{"failed", controlplane.StateFailed, godwitv1.RunState_RUN_STATE_FAILED},
		{"needs attention", controlplane.StateNeedsAttention, godwitv1.RunState_RUN_STATE_NEEDS_ATTENTION},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t, planningRepo(), WorkerConfig{})
			run := settledRun(tc.proto)
			run.Error = "statement 0 of 20260101000000_add_orders (up): boom"
			h.svc.run = run
			h.runs.put(boundRun("run-7", tc.state))
			h.worker.told(context.Background())

			if h.repo.ended[0].conclusion != conclusionFailure {
				t.Fatalf("conclusion = %q", h.repo.ended[0].conclusion)
			}
			if !strings.Contains(h.repo.notices[0], "boom") {
				t.Fatalf("comment = %s", h.repo.notices[0])
			}
		})
	}
}

func TestAReportReadsThePlanTheRunWasBoundTo(t *testing.T) {
	t.Parallel()

	h := newHarness(t, planningRepo(), WorkerConfig{})
	run := settledRun(godwitv1.RunState_RUN_STATE_SUCCEEDED)
	run.PlanId = "plan-1"
	h.svc.run = run
	h.svc.plan = &godwitv1.Plan{
		Id: "plan-1", Target: "orders", Rollout: controlplane.RolloutDirect,
		Migrations: []*godwitv1.PlannedMigration{{
			Version: 20260101000000, Name: "add_orders",
			Statements: []*godwitv1.PlannedStatement{{Sql: "CREATE TABLE orders ()"}},
		}},
	}
	h.svc.applied = []*godwitv1.RunMigration{{Migration: "20260101000000_add_orders"}}
	h.runs.put(boundRun("run-7", controlplane.StateSucceeded))
	h.worker.told(context.Background())

	if !strings.Contains(h.repo.notices[0], "20260101000000_add_orders") {
		t.Fatalf("comment = %s", h.repo.notices[0])
	}
	if !strings.Contains(h.repo.notices[0], "plan [`plan-1`](https://godwit.test/ui/plans/plan-1)") {
		t.Fatalf("the report does not link the plan:\n%s", h.repo.notices[0])
	}
	if !strings.Contains(h.repo.notices[0], "CREATE TABLE orders ()") {
		t.Fatalf("the report does not describe what the plan carried:\n%s", h.repo.notices[0])
	}
}

func TestARunIsNotMarkedToldUntilGitHubHasIt(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		fail func(*harness)
	}{
		{"the run cannot be read", func(h *harness) { h.svc.runErr = errBroken }},
		{"the plan cannot be read", func(h *harness) {
			run := settledRun(godwitv1.RunState_RUN_STATE_SUCCEEDED)
			run.PlanId = "plan-1"
			h.svc.run, h.svc.planErr = run, errBroken
		}},
		{"the repository cannot be reached", func(h *harness) { h.api.err = errBroken }},
		{"the comment will not post", func(h *harness) { h.repo.noticeErr = errBroken }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t, planningRepo(), WorkerConfig{})
			h.svc.run = settledRun(godwitv1.RunState_RUN_STATE_SUCCEEDED)
			tc.fail(h)
			h.runs.put(boundRun("run-7", controlplane.StateSucceeded))
			h.worker.told(context.Background())

			if h.runs.told() != "" {
				t.Fatalf("marked told after a failure: %q", h.runs.told())
			}
		})
	}
}

func TestAReportWhoseCheckOrBookkeepingFails(t *testing.T) {
	t.Parallel()

	h := newHarness(t, planningRepo(), WorkerConfig{})
	h.svc.run = settledRun(godwitv1.RunState_RUN_STATE_SUCCEEDED)
	h.repo.endErr, h.runs.markErr = errBroken, errBroken
	h.runs.put(boundRun("run-7", controlplane.StateSucceeded))
	h.worker.told(context.Background())

	if len(h.repo.notices) != 1 {
		t.Fatalf("the pull request was not told: %v", h.repo.notices)
	}
}

func TestAReportOfARunWithNoCheckOfItsOwn(t *testing.T) {
	t.Parallel()

	h := newHarness(t, planningRepo(), WorkerConfig{})
	h.svc.run = settledRun(godwitv1.RunState_RUN_STATE_SUCCEEDED)
	g := boundRun("run-7", controlplane.StateSucceeded)
	g.CheckRun = 0
	h.runs.put(g)
	h.worker.told(context.Background())

	if len(h.repo.ended) != 0 {
		t.Fatalf("concluded a check that was never opened: %+v", h.repo.ended)
	}
	if len(h.repo.notices) != 1 {
		t.Fatalf("comments = %v", h.repo.notices)
	}
}

func TestAReportFormatTheProjectAskedForThatDoesNotExist(t *testing.T) {
	t.Parallel()

	h := newHarness(t, planningRepo(), WorkerConfig{})
	h.svc.run = settledRun(godwitv1.RunState_RUN_STATE_SUCCEEDED)
	g := boundRun("run-7", controlplane.StateSucceeded)
	g.Format = "prose"
	h.runs.put(g)
	h.worker.told(context.Background())

	if len(h.repo.notices) != 0 {
		t.Fatalf("posted a report it could not render: %v", h.repo.notices)
	}
}

func TestAStoreThatCannotBeAskedWhatToReport(t *testing.T) {
	t.Parallel()

	h := newHarness(t, planningRepo(), WorkerConfig{})
	h.runs.claimErr = errBroken
	h.worker.told(context.Background())

	if len(h.repo.notices) != 0 {
		t.Fatalf("comments = %v", h.repo.notices)
	}
}

func TestARunStillInFlightIsNotReported(t *testing.T) {
	t.Parallel()

	h := newHarness(t, planningRepo(), WorkerConfig{})
	h.runs.put(boundRun("run-7", controlplane.StateRunning))
	h.worker.told(context.Background())

	if len(h.repo.notices) != 0 {
		t.Fatalf("reported a run that has not settled: %v", h.repo.notices)
	}
}

func TestTheReporterRunsOnItsOwnUntilTheWorkerStops(t *testing.T) {
	t.Parallel()

	h := newHarness(t, planningRepo(), WorkerConfig{Interval: time.Millisecond})
	h.svc.run = settledRun(godwitv1.RunState_RUN_STATE_SUCCEEDED)
	h.runs.put(boundRun("run-7", controlplane.StateSucceeded))
	deadline := time.After(10 * time.Second)
	for h.runs.told() == "" {
		select {
		case <-deadline:
			t.Fatal("the reporter never picked the run up")
		case <-time.After(time.Millisecond):
		}
	}
	h.worker.Stop(context.Background())
}
