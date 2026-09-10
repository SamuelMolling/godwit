package authz

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
)

var (
	forbiddenAssociations = []string{"CONTRIBUTOR", "FIRST_TIME_CONTRIBUTOR", "FIRST_TIMER", "MANNEQUIN", "NONE"}
	defaultAssociations   = []string{"OWNER", "MEMBER", "COLLABORATOR"}
)

var (
	needsApproval = map[string]bool{"apply": true, "confirm": true}
	wantsOpen     = map[string]bool{"plan": true, "apply": true, "confirm": true}
)

// PullRequest is what forge policy decides on.
type PullRequest struct {
	Head     string
	HeadRepo string
	State    string
	Merged   bool
	Files    int
}

// Review is one submitted review, as the forge reports it.
type Review struct {
	Login string
	State string
}

// Repository is what forge policy reads; the App's client supplies it.
type Repository interface {
	Permission(ctx context.Context, login string) (string, error)
	PullRequest(ctx context.Context, number int) (PullRequest, error)
	Reviews(ctx context.Context, number int) ([]Review, bool, error)
}

// Event is a pull request godwit noticed rather than one somebody commanded.
type Event struct {
	Repository string
	HeadRepo   string
	Number     int
}

// Command is a command someone posted on a pull request, as the delivery named it.
type Command struct {
	Name        string
	Repository  string
	Commander   string
	Association string
	Number      int
	ReviewSHA   string
	CommentSHA  string
}

func refuse(format string, a ...any) string { return fmt.Sprintf(format, a...) }

// forked is the one sentence both paths refuse a fork with; name is empty for an event nobody commanded.
func forked(name string, number int, headRepo, repository string) string {
	commanded := ""
	if name != "" {
		commanded = "godwit " + name + " on "
	}

	return refuse("%spull request #%d refused: its head is in %s, not %s; a fork may not reach the targets of %s",
		commanded, number, orNone(headRepo), repository, repository)
}

// AutoPlan decides an event nobody commanded: nothing is applied and there is nobody to hold to an association, a permission or an approval, so the head being in the bound repository — where only a writer could have pushed it — is the whole of it.
func (Forge) AutoPlan(ev Event) string {
	if ev.HeadRepo != ev.Repository {
		return forked("", ev.Number, ev.HeadRepo, ev.Repository)
	}

	return ""
}

// Forge is the policy for commands arriving through a forge: who may post them and on what.
type Forge struct{ associations map[string]bool }

// NewForge returns the policy allowing those author associations; an empty list takes the default set.
func NewForge(associations []string) (Forge, error) {
	if len(associations) == 0 {
		associations = defaultAssociations
	}
	allowed := map[string]bool{}
	for _, raw := range associations {
		v := strings.ToUpper(strings.TrimSpace(raw))
		switch {
		case v == "":
			continue
		case slices.Contains(forbiddenAssociations, v):
			return Forge{}, errors.New("author association " + v +
				" is not access to a repository: anyone who opened a pull request carries it")
		case !slices.Contains(defaultAssociations, v):
			return Forge{}, errors.New("unknown author association " + v + " (want OWNER, MEMBER or COLLABORATOR)")
		}
		allowed[v] = true
	}
	if len(allowed) == 0 {
		return Forge{}, errors.New("the author association allow-list is empty: no comment could ever command godwit")
	}

	return Forge{associations: allowed}, nil
}

// Authorize decides c against the pull request open reaches; open runs only after the association narrows, so an idle comment spends no forge budget.
func (f Forge) Authorize(ctx context.Context, open func(context.Context) (Repository, error),
	c Command,
) (PullRequest, string, error) {
	if !f.associations[c.Association] {
		return PullRequest{}, refuse("godwit %s by %s refused: author association %s is not allowed",
			c.Name, c.Commander, orNone(c.Association)), nil
	}
	repo, err := open(ctx)
	if err != nil {
		return PullRequest{}, "", err
	}
	if ref, err := permitted(ctx, repo, c.Commander, "commander"); ref != "" || err != nil {
		return PullRequest{}, ref, err
	}
	pr, err := repo.PullRequest(ctx, c.Number)
	if err != nil {
		return PullRequest{}, "", err
	}
	if !validSHA(pr.Head) {
		return PullRequest{}, refuse("could not read the head of pull request #%d", c.Number), nil
	}
	if pr.HeadRepo != c.Repository {
		return PullRequest{}, forked(c.Name, c.Number, pr.HeadRepo, c.Repository), nil
	}
	if ref := anchored(c, pr); ref != "" {
		return PullRequest{}, ref, nil
	}
	if needsApproval[c.Name] {
		if ref, err := approved(ctx, repo, c); ref != "" || err != nil {
			return PullRequest{}, ref, err
		}
	}

	return pr, "", nil
}

func anchored(c Command, pr PullRequest) string {
	if c.Name == "revert" {
		if pr.Merged {
			return refuse("pull request #%d was merged: its migrations belong to the base branch now, "+
				"revert them from a new pull request", c.Number)
		}

		return ""
	}
	switch {
	case wantsOpen[c.Name] && pr.State != "open":
		return refuse("pull request #%d is %s: nothing to %s", c.Number, orNone(pr.State), c.Name)
	case c.ReviewSHA != "" && c.ReviewSHA != pr.Head:
		return refuse("the review is on %s but pull request #%d is at %s: the head moved after the review, "+
			"so godwit %s would run commits nobody reviewed", short(c.ReviewSHA), c.Number, short(pr.Head), c.Name)
	case c.CommentSHA != "" && !strings.HasPrefix(pr.Head, c.CommentSHA):
		return refuse("godwit %s names %s but pull request #%d is at %s: the head moved after the comment",
			c.Name, c.CommentSHA, c.Number, short(pr.Head))
	}

	return ""
}

func approved(ctx context.Context, repo Repository, c Command) (string, error) {
	all, whole, err := repo.Reviews(ctx, c.Number)
	if err != nil {
		return "", err
	}
	if !whole {
		return refuse("pull request #%d carries more reviews than godwit reads, and github lists the oldest "+
			"first, so the ones that withdraw an approval are the ones it would miss; godwit %s is refused rather "+
			"than decided on a part of the record", c.Number, c.Name), nil
	}
	approver := standingApproval(all)
	if approver == "" {
		return refuse("godwit %s on pull request #%d refused: github reports no approving review standing on it; "+
			"approve it and command godwit %s again", c.Name, c.Number, c.Name), nil
	}
	if !validLogin(approver) {
		return refuse("%q is not a github login", approver), nil
	}

	return permitted(ctx, repo, approver, "approver")
}

// A later CHANGES_REQUESTED supersedes that reviewer's approval (decision 0007), the one point godwit is stricter than Atlantis.
func standingApproval(all []Review) string {
	approved := map[string]bool{}
	var order []string
	for _, r := range all {
		switch r.State {
		case "APPROVED":
			approved[r.Login] = true
		case "CHANGES_REQUESTED", "DISMISSED":
			approved[r.Login] = false
		default:
			continue
		}
		if !slices.Contains(order, r.Login) {
			order = append(order, r.Login)
		}
	}
	for _, login := range order {
		if approved[login] {
			return login
		}
	}

	return ""
}

func permitted(ctx context.Context, repo Repository, login, role string) (string, error) {
	perm, err := repo.Permission(ctx, login)
	if err != nil {
		return "", err
	}
	if perm != "admin" && perm != "write" {
		return refuse("%s %s has permission %q on the repository, not write or admin", role, login, perm), nil
	}

	return "", nil
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

func validLogin(login string) bool {
	if login == "" {
		return false
	}

	return strings.Trim(login, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-") == ""
}

func validSHA(sha string) bool {
	return len(sha) == 40 && strings.Trim(sha, "0123456789abcdef") == ""
}
