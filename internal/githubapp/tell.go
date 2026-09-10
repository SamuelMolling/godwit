package githubapp

import (
	"context"
	"strings"
	"time"

	"connectrpc.com/connect"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/internal/api"
	"github.com/SamuelMolling/godwit/internal/controlplane"
	"github.com/SamuelMolling/godwit/internal/report"
	"github.com/SamuelMolling/godwit/internal/ui/link"
)

func (w *Worker) tell(ctx context.Context) {
	defer close(w.teller)
	tick := time.NewTicker(w.cfg.Interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			w.told(ctx)
		}
	}
}

func (w *Worker) told(ctx context.Context) {
	claimed, err := w.cfg.Runs.ClaimGitHubReports(ctx, defaultLease, reportBatch)
	if err != nil {
		w.cfg.Log.Error("could not look for runs the pull request has not been told about", "error", err)

		return
	}
	for _, g := range claimed {
		w.outcome(ctx, g)
	}
}

// outcome marks the run told only once GitHub has it: a replica dying part way leaves the claim to
// lapse, and the repeat replaces its own comment rather than adding a second.
func (w *Worker) outcome(ctx context.Context, g controlplane.GitHubRun) {
	log := w.cfg.Log.With("repository", g.Repository, "pull_request", g.PullRequest,
		"run", g.RunID, "command", g.Command, "state", g.State)
	body, conclusion, url, err := w.rendered(ctx, g)
	if err != nil {
		log.Error("could not read the run the pull request is waiting on", "error", err)

		return
	}
	repo, err := w.cfg.API.repository(ctx, g.Installation, g.RepositoryID, g.Repository)
	if err != nil {
		log.Error("could not reach the repository the run came from", "error", err)

		return
	}
	if err := repo.speak(ctx, g.PullRequest, g.Marker, body); err != nil {
		log.Error("could not post the outcome on the pull request", "error", err)

		return
	}
	if g.CheckRun != 0 {
		if err := repo.endCheck(ctx, g.CheckRun, checkRun{
			url: url, title: outcomeTitle(g), summary: body, conclusion: conclusion,
		}); err != nil {
			log.Warn("could not conclude the check", "error", err)
		}
	}
	if err := w.cfg.Runs.MarkGitHubReported(ctx, g.RunID, g.State); err != nil {
		log.Error("the pull request was told but the binding still says otherwise", "error", err)
	}
}

func (w *Worker) rendered(ctx context.Context, g controlplane.GitHubRun) (body, conclusion, url string, err error) {
	// The reporter reads; it never reaches a procedure a command's own scope had to be checked against.
	ctx = api.WithPrincipal(ctx, api.Principal{Name: "github:" + g.Repository, Scope: api.ScopeRead})
	got, err := w.cfg.Service.GetRun(ctx, connect.NewRequest(&godwitv1.GetRunRequest{RunId: g.RunID}))
	if err != nil {
		return "", "", "", err
	}
	var plan *godwitv1.Plan
	if id := got.Msg.GetRun().GetPlanId(); id != "" {
		p, err := w.cfg.Service.GetPlan(ctx, connect.NewRequest(&godwitv1.GetPlanRequest{PlanId: id}))
		if err != nil {
			return "", "", "", err
		}
		plan = p.Msg.GetPlan()
	}
	write, err := report.RunWriter("markdown", g.Format)
	if err != nil {
		return "", "", "", err
	}
	var b strings.Builder
	write(&b, report.RunFrom(got.Msg.GetRun(), got.Msg.GetApplied(), plan, g.Command, w.cfg.PublicURL))

	return b.String(), outcomeConclusion(g.State), link.Run(w.cfg.PublicURL, g.RunID), nil
}

func outcomeConclusion(state string) string {
	switch state {
	case controlplane.StateSucceeded:
		return conclusionSuccess
	case controlplane.StateAwaitingContract:
		return conclusionAction
	default:
		return conclusionFailure
	}
}

func outcomeTitle(g controlplane.GitHubRun) string {
	switch g.State {
	case controlplane.StateSucceeded:
		return "godwit " + g.Command + " applied"
	case controlplane.StateAwaitingContract:
		return "expand applied; comment godwit confirm to run the contract phase"
	default:
		return "godwit " + g.Command + " stopped on " + g.Target
	}
}
