package githubapp

import (
	"context"
	"log/slog"

	"github.com/SamuelMolling/godwit/internal/authz"
)

type policyView struct{ repoView }

// Permission implements authz.Repository.
func (v policyView) Permission(ctx context.Context, login string) (string, error) {
	return v.permission(ctx, login)
}

// PullRequest implements authz.Repository.
func (v policyView) PullRequest(ctx context.Context, number int) (authz.PullRequest, error) {
	return v.pullRequest(ctx, number)
}

// Reviews implements authz.Repository.
func (v policyView) Reviews(ctx context.Context, number int) ([]authz.Review, bool, error) {
	return v.reviews(ctx, number)
}

type authorizer struct {
	open     func(ctx context.Context) (repoView, error)
	forge    authz.Forge
	reaction string
	log      *slog.Logger
}

func (a authorizer) authorize(ctx context.Context, req *request) (at, *outcome, error) {
	var view repoView
	open := func(ctx context.Context) (authz.Repository, error) {
		repo, err := a.open(ctx)
		if err != nil {
			return nil, err
		}
		view = repo
		a.seen(ctx, repo, req)

		return policyView{repo}, nil
	}
	pr, ref, err := a.forge.Authorize(ctx, open, commandOf(req))
	if ref != "" || err != nil {
		return at{}, outcomeOf(ref), err
	}

	return at{head: pr.Head, repo: view, files: pr.Files}, nil, nil
}

func eventOf(req *request) authz.Event {
	return authz.Event{Repository: req.repository, HeadRepo: req.headRepo, Number: req.number}
}

func commandOf(req *request) authz.Command {
	c := authz.Command{
		Name: req.name, Repository: req.repository, Commander: req.commander,
		Association: req.association, Number: req.number, ReviewSHA: req.reviewSHA,
	}
	if req.cmd != nil {
		c.CommentSHA = req.cmd.Sha
	}

	return c
}

func outcomeOf(ref string) *outcome {
	if ref == "" {
		return nil
	}

	return refused("%s", ref)
}

func (a authorizer) seen(ctx context.Context, repo repoView, req *request) {
	if a.reaction == unsetReaction || a.reaction == noReaction || req.comment == 0 {
		return
	}
	if err := repo.react(ctx, req.comment, a.reaction); err != nil {
		a.log.Warn("could not react to the commanding comment", "repository", req.repository,
			"comment", req.comment, "error", err)
	}
}
