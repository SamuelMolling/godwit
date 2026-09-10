package githubapp

import (
	"context"
	"slices"
	"strings"
)

var forbiddenAssociations = []string{"CONTRIBUTOR", "FIRST_TIME_CONTRIBUTOR", "FIRST_TIMER", "MANNEQUIN", "NONE"}

// DefaultAssociations narrows who may command before the permission lookup authorises them.
var DefaultAssociations = []string{"OWNER", "MEMBER", "COLLABORATOR"}

func parseAssociations(values []string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, raw := range values {
		v := strings.ToUpper(strings.TrimSpace(raw))
		switch {
		case v == "":
			continue
		case slices.Contains(forbiddenAssociations, v):
			return nil, &configError{"author association " + v + " is not access to a repository: anyone who opened a pull request carries it"}
		case !slices.Contains(DefaultAssociations, v):
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

var needsApproval = map[string]bool{"apply": true, "confirm": true}

var wantsOpen = map[string]bool{"plan": true, "apply": true, "confirm": true}

type authorizer struct {
	// open mints the installation token only once the association has narrowed, so a comment nobody could
	// have commanded with spends no GitHub budget.
	open    func(ctx context.Context) (Repo, error)
	allowed map[string]bool
}

// authorize reproduces the Action's guards server-side and returns the head the command would run at.
func (a authorizer) authorize(ctx context.Context, req *request) (string, *outcome, error) {
	if !a.allowed[req.association] {
		return "", refused("godwit %s by %s refused: author association %s is not allowed",
			req.name, req.commander, orNone(req.association)), nil
	}
	repo, err := a.open(ctx)
	if err != nil {
		return "", nil, err
	}
	if out, err := permitted(ctx, repo, req.commander, "commander"); out != nil || err != nil {
		return "", out, err
	}
	pr, err := repo.PullRequest(ctx, req.number)
	if err != nil {
		return "", nil, err
	}
	if !validSHA(pr.Head) {
		return "", refused("could not read the head of pull request #%d", req.number), nil
	}
	if pr.HeadRepo != req.repository {
		return "", refused("godwit %s on pull request #%d refused: its head is in %s, not %s; a fork may not reach "+
			"the targets of %s", req.name, req.number, orNone(pr.HeadRepo), req.repository, req.repository), nil
	}
	if out := anchored(req, pr); out != nil {
		return "", out, nil
	}
	if needsApproval[req.name] {
		if out, err := approved(ctx, repo, req, pr); out != nil || err != nil {
			return "", out, err
		}
	}

	return pr.Head, nil, nil
}

func anchored(req *request, pr PullRequest) *outcome {
	if req.name == "revert" {
		if pr.Merged {
			return refused("pull request #%d was merged: its migrations belong to the base branch now, "+
				"revert them from a new pull request", req.number)
		}

		return nil
	}
	switch {
	case wantsOpen[req.name] && pr.State != "open":
		return refused("pull request #%d is %s: nothing to %s", req.number, orNone(pr.State), req.name)
	case req.reviewSHA != "" && req.reviewSHA != pr.Head:
		return refused("the review is on %s but pull request #%d is at %s: the head moved after the review, "+
			"so godwit %s would run commits nobody reviewed", short(req.reviewSHA), req.number, short(pr.Head), req.name)
	case req.cmd != nil && req.cmd.Sha != "" && !strings.HasPrefix(pr.Head, req.cmd.Sha):
		return refused("godwit %s names %s but pull request #%d is at %s: the head moved after the comment",
			req.name, req.cmd.Sha, req.number, short(pr.Head))
	}

	return nil
}

func approved(ctx context.Context, repo Repo, req *request, pr PullRequest) (*outcome, error) {
	reviews, err := repo.Reviews(ctx, req.number)
	if err != nil {
		return nil, err
	}
	approver := standingApproval(reviews, pr.Head, pr.Author)
	if approver == "" {
		return refused("godwit %s on pull request #%d refused: no approving review by anyone other than %s "+
			"stands on %s", req.name, req.number, orNone(pr.Author), short(pr.Head)), nil
	}
	if !validLogin(approver) {
		return refused("%q is not a github login", approver), nil
	}

	return permitted(ctx, repo, approver, "approver")
}

func standingApproval(reviews []Review, head, author string) string {
	last := map[string]string{}
	var order []string
	for _, r := range reviews {
		switch r.State {
		case "APPROVED":
			last[r.Login] = r.CommitID
		case "CHANGES_REQUESTED", "DISMISSED":
			last[r.Login] = ""
		default:
			continue
		}
		if !slices.Contains(order, r.Login) {
			order = append(order, r.Login)
		}
	}
	for _, login := range order {
		if last[login] == head && login != author {
			return login
		}
	}

	return ""
}

func permitted(ctx context.Context, repo Repo, login, role string) (*outcome, error) {
	perm, err := repo.Permission(ctx, login)
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
