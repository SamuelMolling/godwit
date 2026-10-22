package githubapp

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/SamuelMolling/godwit/internal/comment"
)

const (
	eventIssueComment = "issue_comment"
	eventReview       = "pull_request_review"
	eventPullRequest  = "pull_request"
	eventPush         = "push"
)

var planActions = map[string]bool{"opened": true, "synchronize": true, "reopened": true, "ready_for_review": true}

const (
	pushCommitCap = 2048
	pushFilesCap  = 3000
)

type user struct {
	Login string `json:"login"`
}

type pushCommit struct {
	Added    []string `json:"added"`
	Modified []string `json:"modified"`
	Removed  []string `json:"removed"`
}

type payload struct {
	Action     string `json:"action"`
	Repository struct {
		ID            int64  `json:"id"`
		FullName      string `json:"full_name"`
		DefaultBranch string `json:"default_branch"`
	} `json:"repository"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
	Issue struct {
		Number      int             `json:"number"`
		PullRequest json.RawMessage `json:"pull_request"`
	} `json:"issue"`
	Comment struct {
		ID          int64     `json:"id"`
		Body        string    `json:"body"`
		CreatedAt   time.Time `json:"created_at"`
		User        user      `json:"user"`
		Association string    `json:"author_association"`
	} `json:"comment"`
	Review struct {
		Body        string    `json:"body"`
		CommitID    string    `json:"commit_id"`
		SubmittedAt time.Time `json:"submitted_at"`
		User        user      `json:"user"`
		Association string    `json:"author_association"`
	} `json:"review"`
	PullRequest struct {
		Number       int `json:"number"`
		ChangedFiles int `json:"changed_files"`
		Base         struct {
			Ref string `json:"ref"`
		} `json:"base"`
		Head struct {
			SHA  string `json:"sha"`
			Repo struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"head"`
	} `json:"pull_request"`
	Ref     string       `json:"ref"`
	After   string       `json:"after"`
	Created bool         `json:"created"`
	Deleted bool         `json:"deleted"`
	Forced  bool         `json:"forced"`
	Commits []pushCommit `json:"commits"`
}

type request struct {
	event         string
	repository    string
	number        int
	name          string
	cmd           *comment.Command
	commander     string
	association   string
	at            time.Time
	reviewSHA     string
	headSHA       string
	headRepo      string
	branch        string
	defaultBranch string
	files         int
	comment       int64
	paths         []string
	all           bool
}

type outcome struct {
	result  string
	message string
	check   bool
	head    string
}

func checked(head string, out *outcome) *outcome {
	out.check, out.head = true, head

	return out
}

func ignored(format string, a ...any) *outcome {
	return &outcome{result: resultIgnored, message: fmt.Sprintf(format, a...)}
}

func refused(format string, a ...any) *outcome {
	return &outcome{result: resultRefused, message: fmt.Sprintf(format, a...)}
}

func parse(event string, p *payload) (*request, *outcome) {
	if !validRepository(p.Repository.FullName) {
		return nil, refused("the payload names no repository")
	}
	if p.Installation.ID <= 0 {
		return nil, refused("the payload names no installation")
	}
	req := &request{event: event, repository: p.Repository.FullName}
	switch event {
	case eventIssueComment:
		return commentRequest(req, p)
	case eventReview:
		return reviewRequest(req, p)
	case eventPullRequest:
		return pullRequest(req, p)
	case eventPush:
		return pushRequest(req, p)
	}

	return nil, ignored("godwit does not act on %s deliveries", event)
}

func pushRequest(req *request, p *payload) (*request, *outcome) {
	if p.Deleted {
		return nil, ignored("deleted branch ignored")
	}
	if p.Repository.DefaultBranch == "" {
		return req, refused("the push payload names no default branch")
	}
	branch, ok := strings.CutPrefix(p.Ref, "refs/heads/")
	if !ok || branch == "" {
		return nil, ignored("push to %s ignored; godwit verifies branch pushes", orNone(p.Ref))
	}
	if !validSHA(p.After) {
		return req, refused("the push payload carries no head commit")
	}
	req.name, req.headSHA = "verify", p.After
	req.branch, req.defaultBranch = branch, p.Repository.DefaultBranch
	req.all = p.Created || p.Forced || len(p.Commits) == 0 || len(p.Commits) >= pushCommitCap
	seen := map[string]bool{}
	for _, commit := range p.Commits {
		if len(commit.Added)+len(commit.Modified)+len(commit.Removed) >= pushFilesCap {
			req.all = true
		}
		for _, names := range [][]string{commit.Added, commit.Modified, commit.Removed} {
			for _, name := range names {
				if !seen[name] {
					req.paths = append(req.paths, name)
					seen[name] = true
				}
			}
		}
	}

	return req, nil
}

func commentRequest(req *request, p *payload) (*request, *outcome) {
	if p.Action != "created" {
		return nil, ignored("a command is what someone posted, not what a comment now says; %s ignored", p.Action)
	}
	if len(p.Issue.PullRequest) == 0 || string(p.Issue.PullRequest) == "null" {
		return nil, ignored("comment on an issue, not a pull request")
	}
	req.number, req.commander, req.association = p.Issue.Number, p.Comment.User.Login, p.Comment.Association
	req.at, req.comment = p.Comment.CreatedAt, p.Comment.ID

	return commanded(req, p.Comment.Body, "comment")
}

func reviewRequest(req *request, p *payload) (*request, *outcome) {
	if p.Action != "submitted" {
		return nil, ignored("review %s ignored", p.Action)
	}
	req.number, req.commander, req.association = p.PullRequest.Number, p.Review.User.Login, p.Review.Association
	req.at, req.reviewSHA = p.Review.SubmittedAt, p.Review.CommitID

	return commanded(req, p.Review.Body, "review")
}

func pullRequest(req *request, p *payload) (*request, *outcome) {
	if !planActions[p.Action] {
		return nil, ignored("pull request %s ignored", p.Action)
	}
	req.number, req.name = p.PullRequest.Number, "plan"
	req.headSHA, req.headRepo = p.PullRequest.Head.SHA, p.PullRequest.Head.Repo.FullName
	req.branch = p.PullRequest.Base.Ref
	req.files = p.PullRequest.ChangedFiles
	if !validSHA(req.headSHA) {
		return req, refused("the pull_request payload carries no head commit")
	}

	return validated(req)
}

func commanded(req *request, body, what string) (*request, *outcome) {
	cmd, err := comment.Parse(body, "")
	if err != nil {
		return req, refused("%s", err)
	}
	if cmd == nil {
		return nil, ignored("the %s names no godwit command", what)
	}
	if !validLogin(req.commander) {
		return req, refused("%q is not a github login", req.commander)
	}
	req.name, req.cmd = cmd.Name, cmd

	return validated(req)
}

func validated(req *request) (*request, *outcome) {
	if req.number <= 0 {
		return nil, refused("the %s payload carries no pull request number", req.event)
	}

	return req, nil
}

func validLogin(login string) bool {
	if login == "" {
		return false
	}

	return strings.Trim(login, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-") == ""
}

func validRepository(full string) bool {
	owner, name, ok := strings.Cut(full, "/")

	return ok && validLogin(owner) && validName(name)
}

func validName(name string) bool {
	if name == "" {
		return false
	}

	return strings.Trim(name, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._") == ""
}

func validSHA(sha string) bool {
	return len(sha) == 40 && strings.Trim(sha, "0123456789abcdef") == ""
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
