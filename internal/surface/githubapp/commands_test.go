package githubapp

import (
	"context"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/internal/comment"
	"github.com/SamuelMolling/godwit/internal/controlplane"
)

func commandNamed(name string, with func(*comment.Command)) command {
	cmd := planCommand()
	cmd.name = name
	cmd.cmd = &comment.Command{Name: name}
	cmd.principal.Scope = scopes[name]
	if with != nil {
		with(cmd.cmd)
	}

	return cmd
}

func boundRun(runID, state string) controlplane.GitHubRun {
	return controlplane.GitHubRun{
		RunID: runID, Repository: testRepo, RepositoryID: 42, Installation: 7, PullRequest: 3,
		Head: testHead, Command: "apply", Target: "orders", Marker: "<!-- godwit:migrate -->",
		Format: "schema", CheckRun: 77, State: state,
	}
}

func TestApplyQueuesARunAndLeavesTheCheckOpen(t *testing.T) {
	t.Parallel()

	h := newHarness(t, planningRepo(), WorkerConfig{})
	h.worker.carry(context.Background(), commandNamed("apply", func(c *comment.Command) {
		c.Ack = []string{"H001", "H003"}
	}))

	if len(h.svc.created) != 1 {
		t.Fatalf("CreateRun calls = %d", len(h.svc.created))
	}
	req := h.svc.created[0]
	switch {
	case req.Target != "orders":
		t.Fatalf("target = %q", req.Target)
	case strings.Join(req.AcknowledgeHazards, ",") != "H001,H003":
		t.Fatalf("acked = %v", req.AcknowledgeHazards)
	case req.Source != "github.com/"+testRepo+"@"+testHead+":db/migrations":
		t.Fatalf("source = %q", req.Source)
	}
	if len(h.repo.ended) != 0 {
		t.Fatalf("concluded a check over a run that has not run: %+v", h.repo.ended)
	}
	bound := h.runs.bound()
	if len(bound) != 1 || bound[0].RunID != "run-1" || bound[0].CheckRun != 1 {
		t.Fatalf("binding = %+v", bound)
	}
	if !strings.Contains(h.repo.notices[0], "Run `run-1` is queued") {
		t.Fatalf("comment = %s", h.repo.notices[0])
	}
}

func TestAStalePlanSaysWhatMovedOnThePullRequest(t *testing.T) {
	t.Parallel()

	stale := &controlplane.PlanStale{
		Plan:   controlplane.Plan{ID: "plan-9", Target: "orders", Key: "k"},
		Reason: controlplane.StaleHistory,
		Hint:   "push to the pull request (re-plan)",
	}
	h := newHarness(t, planningRepo(), WorkerConfig{})
	h.svc.createErr = connect.NewError(connect.CodeFailedPrecondition, stale)
	h.worker.carry(context.Background(), commandNamed("apply", nil))

	body := h.repo.notices[0]
	for _, want := range []string{"is stale", "reason : history", "push to the pull request"} {
		if !strings.Contains(body, want) {
			t.Fatalf("comment missing %q:\n%s", want, body)
		}
	}
	if len(h.runs.bound()) != 0 {
		t.Fatal("bound a run that was never created")
	}
	if h.repo.ended[0].conclusion != conclusionFailure {
		t.Fatalf("conclusion = %q", h.repo.ended[0].conclusion)
	}
}

func TestConfirmReleasesTheRunThisPullRequestHeld(t *testing.T) {
	t.Parallel()

	h := newHarness(t, planningRepo(), WorkerConfig{})
	h.runs.put(boundRun("run-7", controlplane.StateAwaitingContract))
	h.worker.carry(context.Background(), commandNamed("confirm", nil))

	if strings.Join(h.svc.confirmed, ",") != "run-7" {
		t.Fatalf("confirmed = %v", h.svc.confirmed)
	}
	bound := h.runs.bound()
	if len(bound) != 1 || bound[0].RunID != "run-7" || bound[0].Command != "confirm" {
		t.Fatalf("binding = %+v", bound)
	}
}

func TestConfirmWithNothingHeldIsRefusedRatherThanSilent(t *testing.T) {
	t.Parallel()

	h := newHarness(t, planningRepo(), WorkerConfig{})
	h.runs.put(boundRun("run-7", controlplane.StateSucceeded))
	h.worker.carry(context.Background(), commandNamed("confirm", nil))

	if len(h.svc.confirmed) != 0 {
		t.Fatalf("confirmed = %v", h.svc.confirmed)
	}
	if !strings.Contains(h.repo.notices[0], "awaiting its contract phase") {
		t.Fatalf("comment = %s", h.repo.notices[0])
	}
	if h.repo.ended[0].conclusion != conclusionFailure {
		t.Fatalf("conclusion = %q", h.repo.ended[0].conclusion)
	}
}

func TestRevertUndoesTheNewestRunOfThisPullRequest(t *testing.T) {
	t.Parallel()

	h := newHarness(t, planningRepo(), WorkerConfig{})
	h.runs.put(boundRun("run-old", controlplane.StateSucceeded))
	h.runs.put(boundRun("run-new", controlplane.StateSucceeded))
	h.worker.carry(context.Background(), commandNamed("revert", func(c *comment.Command) {
		c.Ack, c.Force, c.AllowDataLoss = []string{"H002"}, true, true
	}))

	if len(h.svc.reverted) != 1 {
		t.Fatalf("RevertRun calls = %d", len(h.svc.reverted))
	}
	req := h.svc.reverted[0]
	switch {
	case req.RunId != "run-new":
		t.Fatalf("reverted = %q, want the newest", req.RunId)
	case req.Target != "orders":
		t.Fatalf("target = %q", req.Target)
	case strings.Join(req.AcknowledgeHazards, ",") != "H002":
		t.Fatalf("acked = %v", req.AcknowledgeHazards)
	case !req.Force || !req.AllowDataLoss:
		t.Fatalf("force = %v, allow_data_loss = %v", req.Force, req.AllowDataLoss)
	}
	bound := h.runs.bound()
	if len(bound) != 1 || bound[0].RunID != "revert-1" {
		t.Fatalf("binding = %+v", bound)
	}
}

func TestRevertSkipsWhatIsAlreadyUndone(t *testing.T) {
	t.Parallel()

	h := newHarness(t, planningRepo(), WorkerConfig{})
	undone := boundRun("run-1", controlplane.StateSucceeded)
	undone.Reverts = "run-0"
	h.runs.put(undone)
	h.worker.carry(context.Background(), commandNamed("revert", nil))

	if len(h.svc.reverted) != 0 {
		t.Fatalf("reverted %v", h.svc.reverted)
	}
	if !strings.Contains(h.repo.notices[0], "No run of pull request #3") {
		t.Fatalf("comment = %s", h.repo.notices[0])
	}
	if h.repo.ended[0].conclusion != conclusionSuccess {
		t.Fatalf("conclusion = %q, nothing to revert is not a failure", h.repo.ended[0].conclusion)
	}
}

func TestRevertOnAPullRequestThatCreatedNoRun(t *testing.T) {
	t.Parallel()

	h := newHarness(t, planningRepo(), WorkerConfig{})
	h.worker.carry(context.Background(), commandNamed("revert", nil))

	if len(h.svc.reverted) != 0 {
		t.Fatalf("reverted %v", h.svc.reverted)
	}
	if !strings.Contains(h.repo.notices[0], "left to revert") {
		t.Fatalf("comment = %s", h.repo.notices[0])
	}
}

func TestAStoreThatCannotBeAskedRefusesTheCommand(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"confirm", "revert"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t, planningRepo(), WorkerConfig{})
			h.runs.listErr = errBroken
			h.worker.carry(context.Background(), commandNamed(name, nil))

			if !strings.Contains(h.repo.notices[0], "broken") {
				t.Fatalf("comment = %s", h.repo.notices[0])
			}
		})
	}
}

func TestACommandRefusedByTheService(t *testing.T) {
	t.Parallel()

	h := newHarness(t, planningRepo(), WorkerConfig{})
	h.runs.put(boundRun("run-7", controlplane.StateAwaitingContract))
	h.svc.confirmErr = connect.NewError(connect.CodeFailedPrecondition, errors.New("run run-7 is not awaiting contract"))
	h.worker.carry(context.Background(), commandNamed("confirm", nil))

	if !strings.Contains(h.repo.notices[0], "not awaiting contract") {
		t.Fatalf("comment = %s", h.repo.notices[0])
	}
	if len(h.runs.bound()) != 0 {
		t.Fatal("bound a run the service refused to release")
	}
}

func TestARevertTheServiceRefuses(t *testing.T) {
	t.Parallel()

	h := newHarness(t, planningRepo(), WorkerConfig{})
	h.runs.put(boundRun("run-7", controlplane.StateSucceeded))
	h.svc.revertErr = connect.NewError(connect.CodeFailedPrecondition, errors.New("revert would destroy data"))
	h.worker.carry(context.Background(), commandNamed("revert", nil))

	if !strings.Contains(h.repo.notices[0], "destroy data") {
		t.Fatalf("comment = %s", h.repo.notices[0])
	}
}

func TestNoMutatingCommandReachesTheServiceWithoutItsScope(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"apply", "confirm", "revert"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t, planningRepo(), WorkerConfig{})
			h.runs.put(boundRun("run-7", controlplane.StateAwaitingContract))
			cmd := commandNamed(name, nil)
			cmd.principal.Scope = ""
			h.worker.carry(context.Background(), cmd)

			if len(h.svc.created)+len(h.svc.confirmed)+len(h.svc.reverted) != 0 {
				t.Fatal("reached the service with no scope")
			}
			if !strings.Contains(h.repo.notices[0], "requires scope pipeline") {
				t.Fatalf("comment = %s", h.repo.notices[0])
			}
		})
	}
}

func TestARunGodwitCouldNotBindIsSaidOutLoud(t *testing.T) {
	t.Parallel()

	h := newHarness(t, planningRepo(), WorkerConfig{})
	h.runs.recErr = errBroken
	h.worker.carry(context.Background(), commandNamed("apply", nil))

	if len(h.svc.created) != 1 {
		t.Fatalf("CreateRun calls = %d", len(h.svc.created))
	}
}

func TestACreateRunThatAnswersWithNoRun(t *testing.T) {
	t.Parallel()

	h := newHarness(t, planningRepo(), WorkerConfig{})
	h.svc.createRes = &godwitv1.CreateRunResponse{}
	h.worker.carry(context.Background(), commandNamed("apply", nil))

	if !strings.Contains(h.repo.notices[0], "created no run") {
		t.Fatalf("comment = %s", h.repo.notices[0])
	}
}

func TestTheCommentSaysWhatTheRunBoundTo(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		res  *godwitv1.CreateRunResponse
		want string
	}{
		{"a stored plan", &godwitv1.CreateRunResponse{RunId: "r", PlanId: "p1"}, "bound to plan `p1`"},
		{"no stored plan", &godwitv1.CreateRunResponse{RunId: "r"}, "the plan is implicit"},
		{"a run already carrying them", &godwitv1.CreateRunResponse{RunId: "r", Reattached: true}, "re-attached"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t, planningRepo(), WorkerConfig{})
			h.svc.createRes = tc.res
			h.worker.carry(context.Background(), commandNamed("apply", nil))

			if !strings.Contains(h.repo.notices[0], tc.want) {
				t.Fatalf("comment = %s", h.repo.notices[0])
			}
		})
	}
}

func TestAnEmptyDirectoryIsNothingToApply(t *testing.T) {
	t.Parallel()

	repo := planningRepo()
	repo.listing = map[string]contents{}
	h := newHarness(t, repo, WorkerConfig{})
	h.worker.carry(context.Background(), commandNamed("apply", nil))

	if len(h.svc.created) != 0 {
		t.Fatal("created a run over an empty directory")
	}
	if h.repo.ended[0].title != "nothing to apply" {
		t.Fatalf("title = %q", h.repo.ended[0].title)
	}
}

func TestACommandWithNoCommentAcknowledgesNothing(t *testing.T) {
	t.Parallel()

	h := newHarness(t, planningRepo(), WorkerConfig{})
	cmd := commandNamed("apply", nil)
	cmd.cmd = nil
	h.worker.carry(context.Background(), cmd)

	if got := h.svc.created[0].AcknowledgeHazards; len(got) != 0 {
		t.Fatalf("acked = %v", got)
	}
}
