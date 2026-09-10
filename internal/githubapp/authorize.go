package githubapp

import (
	"context"
	"log/slog"
	"slices"
	"strings"
)

var (
	forbiddenAssociations = []string{"CONTRIBUTOR", "FIRST_TIME_CONTRIBUTOR", "FIRST_TIMER", "MANNEQUIN", "NONE"}
	defaultAssociations   = []string{"OWNER", "MEMBER", "COLLABORATOR"}
)

func parseAssociations(values []string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, raw := range values {
		v := strings.ToUpper(strings.TrimSpace(raw))
		switch {
		case v == "":
			continue
		case slices.Contains(forbiddenAssociations, v):
			return nil, &configError{"author association " + v + " is not access to a repository: anyone who opened a pull request carries it"}
		case !slices.Contains(defaultAssociations, v):
			return nil, &configError{"unknown author association " + v + " (want OWNER, MEMBER or COLLABORATOR)"}
		}
		out[v] = true
	}
	if len(out) == 0 {
		return nil, &configError{"the author association allow-list is empty: no comment could ever command godwit"}
	}

	return out, nil
}

type configError struct{ msg string }

func (e *configError) Error() string { return e.msg }

var (
	needsApproval = map[string]bool{"apply": true, "confirm": true}
	wantsOpen     = map[string]bool{"plan": true, "apply": true, "confirm": true}
)

type authorizer struct {
	// open mints the token only after the association narrowed, so an idle comment spends no GitHub budget.
	open     func(ctx context.Context) (repoView, error)
	allowed  map[string]bool
	reaction string
	log      *slog.Logger
}

func (a authorizer) authorize(ctx context.Context, req *request) (at, *outcome, error) {
	if !a.allowed[req.association] {
		return at{}, refused("godwit %s by %s refused: author association %s is not allowed",
			req.name, req.commander, orNone(req.association)), nil
	}
	repo, err := a.open(ctx)
	if err != nil {
		return at{}, nil, err
	}
	a.seen(ctx, repo, req)
	if out, err := permitted(ctx, repo, req.commander, "commander"); out != nil || err != nil {
		return at{}, out, err
	}
	pr, err := repo.pullRequest(ctx, req.number)
	if err != nil {
		return at{}, nil, err
	}
	if !validSHA(pr.head) {
		return at{}, refused("could not read the head of pull request #%d", req.number), nil
	}
	if pr.headRepo != req.repository {
		return at{}, refused("godwit %s on pull request #%d refused: its head is in %s, not %s; a fork may not reach "+
			"the targets of %s", req.name, req.number, orNone(pr.headRepo), req.repository, req.repository), nil
	}
	if out := anchored(req, pr); out != nil {
		return at{}, out, nil
	}
	if needsApproval[req.name] {
		if out, err := approved(ctx, repo, req, pr); out != nil || err != nil {
			return at{}, out, err
		}
	}

	return at{head: pr.head, repo: repo, files: pr.files}, nil, nil
}

// seen marks the comment read before godwit knows whether it will obey it, which is the whole point of it.
func (a authorizer) seen(ctx context.Context, repo repoView, req *request) {
	if a.reaction == unsetReaction || a.reaction == NoReaction || req.comment == 0 {
		return
	}
	if err := repo.react(ctx, req.comment, a.reaction); err != nil {
		a.log.Warn("could not react to the commanding comment", "repository", req.repository,
			"comment", req.comment, "error", err)
	}
}

func anchored(req *request, pr pull) *outcome {
	if req.name == "revert" {
		if pr.merged {
			return refused("pull request #%d was merged: its migrations belong to the base branch now, "+
				"revert them from a new pull request", req.number)
		}

		return nil
	}
	switch {
	case wantsOpen[req.name] && pr.state != "open":
		return refused("pull request #%d is %s: nothing to %s", req.number, orNone(pr.state), req.name)
	case req.reviewSHA != "" && req.reviewSHA != pr.head:
		return refused("the review is on %s but pull request #%d is at %s: the head moved after the review, "+
			"so godwit %s would run commits nobody reviewed", short(req.reviewSHA), req.number, short(pr.head), req.name)
	case req.cmd != nil && req.cmd.Sha != "" && !strings.HasPrefix(pr.head, req.cmd.Sha):
		return refused("godwit %s names %s but pull request #%d is at %s: the head moved after the comment",
			req.name, req.cmd.Sha, req.number, short(pr.head))
	}

	return nil
}

func approved(ctx context.Context, repo repoView, req *request, pr pull) (*outcome, error) {
	all, whole, err := repo.reviews(ctx, req.number)
	if err != nil {
		return nil, err
	}
	if !whole {
		return refused("pull request #%d carries more reviews than godwit reads, and github lists the oldest "+
			"first, so the ones that withdraw an approval are the ones it would miss; godwit %s is refused rather "+
			"than decided on a part of the record", req.number, req.name), nil
	}
	approver := standingApproval(all, pr.head, pr.author)
	if approver == "" {
		return refused("godwit %s on pull request #%d refused: no approving review by anyone other than %s "+
			"stands on %s", req.name, req.number, orNone(pr.author), short(pr.head)), nil
	}
	if !validLogin(approver) {
		return refused("%q is not a github login", approver), nil
	}

	return permitted(ctx, repo, approver, "approver")
}

func standingApproval(all []review, head, author string) string {
	last := map[string]string{}
	var order []string
	for _, r := range all {
		switch r.state {
		case "APPROVED":
			last[r.login] = r.commitID
		case "CHANGES_REQUESTED", "DISMISSED":
			last[r.login] = ""
		default:
			continue
		}
		if !slices.Contains(order, r.login) {
			order = append(order, r.login)
		}
	}
	for _, login := range order {
		if last[login] == head && login != author {
			return login
		}
	}

	return ""
}

func permitted(ctx context.Context, repo repoView, login, role string) (*outcome, error) {
	perm, err := repo.permission(ctx, login)
	if err != nil {
		return nil, err
	}
	if perm != "admin" && perm != "write" {
		return refused("%s %s has permission %q on the repository, not write or admin", role, login, perm), nil
	}

	return nil, nil
}

func short(sha string) string {
	if len(sha) < 7 {
		return sha
	}

	return sha[:7]
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}

	return s
}
