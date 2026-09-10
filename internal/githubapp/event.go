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
)

var planActions = map[string]bool{"opened": true, "synchronize": true, "reopened": true, "ready_for_review": true}

type user struct {
	Login string `json:"login"`
}

type payload struct {
	Action     string `json:"action"`
	Repository struct {
		ID       int64  `json:"id"`
		FullName string `json:"full_name"`
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
		Head         struct {
			SHA  string `json:"sha"`
			Repo struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"head"`
	} `json:"pull_request"`
}

type request struct {
	event       string
	repository  string
	number      int
	name        string
	cmd         *comment.Command
	commander   string
	association string
	at          time.Time
	reviewSHA   string
	headSHA     string
	headRepo    string
	files       int
	comment     int64
}

type outcome struct {
	result  string
	message string
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
	}

	return nil, ignored("godwit does not act on %s deliveries", event)
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
