package githubapp

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/SamuelMolling/godwit/internal/config"
)

type project struct {
	root       string
	dir        string
	target     string
	rollout    string
	format     string
	outOfOrder bool
}

func (p project) String() string {
	if p.root == "" {
		return p.target
	}

	return p.target + " (" + p.root + ")"
}

func (p project) path() string {
	return path.Join(p.root, p.dir)
}

func defaultWhenModified(dir string) []string {
	return []string{path.Join(dir, "**", "*.sql"), config.FileName}
}

type resolution struct {
	planned []project
	skipped []string
}

var errTruncated = errors.New("truncated")

func resolve(ctx context.Context, repo repoView, bound bindings, req *request, head string, files int) (resolution, error) {
	changed, err := repo.changed(ctx, req.number)
	if err != nil {
		return resolution{}, err
	}
	if !changed.whole(files) {
		return resolution{}, fmt.Errorf("%w: github listed %d of the %d files pull request #%d changes",
			errTruncated, changed.listed, max(files, changed.listed), req.number)
	}
	var out resolution
	for _, root := range roots(bound) {
		under := within(root, changed.paths)
		if len(under) == 0 {
			continue
		}
		p, why := load(ctx, repo, bound, req.repository, root, head, under, req.name == "plan", req.branch, "", false)
		switch {
		case why != "":
			out.skipped = append(out.skipped, why)
		case p != nil:
			out.planned = append(out.planned, *p)
		}
	}
	slices.SortFunc(out.planned, func(a, b project) int {
		return strings.Compare(a.String(), b.String())
	})

	return out, nil
}

func resolvePush(ctx context.Context, repo repoView, bound bindings, req *request) resolution {
	var out resolution
	for _, root := range roots(bound) {
		under := within(root, req.paths)
		if !req.all && len(under) == 0 {
			continue
		}
		if req.all {
			under = []string{config.FileName}
		}
		p, why := load(ctx, repo, bound, req.repository, root, req.headSHA, under, false, req.branch, req.defaultBranch, true)
		switch {
		case why != "":
			out.skipped = append(out.skipped, why)
		case p != nil && (req.all || matchAny(defaultWhenModified(p.dir), under)):
			out.planned = append(out.planned, *p)
		}
	}
	slices.SortFunc(out.planned, func(a, b project) int {
		return strings.Compare(a.String(), b.String())
	})

	return out
}

func roots(bound bindings) []string {
	var out []string
	for _, b := range bound {
		if !slices.Contains(out, b.dir) {
			out = append(out, b.dir)
		}
	}
	slices.Sort(out)

	return out
}

func within(root string, changed []string) []string {
	if root == "" {
		return changed
	}
	prefix := root + "/"
	var out []string
	for _, name := range changed {
		if rest, ok := strings.CutPrefix(name, prefix); ok {
			out = append(out, rest)
		}
	}

	return out
}

func load(ctx context.Context, repo repoView, bound bindings, repository, root, head string, under []string, auto bool, branch, defaultBranch string, push bool) (*project, string) {
	raw, err := repo.file(ctx, path.Join(root, config.FileName), head)
	if errors.Is(err, errAbsent) {
		return pushSkip(push, branch, defaultBranch, fmt.Sprintf("%s carries no %s, so godwit does not know what it is", orRoot(root), config.FileName))
	}
	if err != nil {
		return pushSkip(push, branch, defaultBranch, fmt.Sprintf("%s: %s", path.Join(root, config.FileName), err))
	}
	cfg, err := config.Decode(raw)
	if err != nil {
		return pushSkip(push, branch, defaultBranch, fmt.Sprintf("%s does not parse: %s", path.Join(root, config.FileName), err))
	}
	file := path.Join(root, config.FileName)
	target, skip := targeted(cfg, branch, defaultBranch, push)
	if target == "" && skip == "" {
		return nil, ""
	}
	if skip != "" {
		return nil, file + " " + skip
	}
	if err := contained(cfg.Dir); err != nil {
		return nil, fmt.Sprintf("%s: dir %s", file, err)
	}
	if err := bound.grant(repository, root, target); err != nil {
		return nil, err.Error()
	}
	when, err := trigger(cfg, auto)
	if err != nil {
		return nil, fmt.Sprintf("%s: %s", path.Join(root, config.FileName), err)
	}
	if when == nil || !matchAny(when, under) {
		return nil, ""
	}

	return &project{
		root: root, dir: cfg.Dir, target: target, rollout: cfg.Rollout,
		format: cfg.PlanFormat(), outOfOrder: cfg.AllowOutOfOrder,
	}, ""
}

// pushSkip drops a failure on a non-default push. The file has not shown that this branch names a target, and a check there would be noise.
func pushSkip(push bool, branch, defaultBranch, why string) (*project, string) {
	if push && branch != defaultBranch {
		return nil, ""
	}

	return nil, why
}

func targeted(cfg config.Config, branch, defaultBranch string, push bool) (string, string) {
	if cfg.Target == "" && len(cfg.Targets) == 0 {
		if push && branch != defaultBranch {
			return "", ""
		}

		return "", "names no target, so godwit has nothing to plan it against"
	}
	if len(cfg.Targets) > 0 && branch == "" && !push {
		return "", "names no base branch, so godwit cannot choose a target"
	}
	name, ok := cfg.TargetFor(branch)
	if ok {
		if push && len(cfg.Targets) == 0 && branch != defaultBranch {
			return "", ""
		}

		return name, ""
	}
	if push {
		return "", ""
	}

	return "", fmt.Sprintf("has no target for branch %s, so godwit has nothing to plan it against", branch)
}

// trigger widens the default rather than replacing it, so a project cannot configure its own migrations out of being planned.
func trigger(cfg config.Config, auto bool) ([]string, error) {
	when := defaultWhenModified(cfg.Dir)
	if a := cfg.Autoplan; a != nil {
		if auto && a.Enabled != nil && !*a.Enabled {
			return nil, nil
		}
		when = append(when, a.WhenModified...)
	}
	for _, pattern := range when {
		if err := validGlob(pattern); err != nil {
			return nil, err
		}
	}

	return when, nil
}

func matchAny(when, under []string) bool {
	for _, pattern := range when {
		for _, name := range under {
			if matchGlob(pattern, name) {
				return true
			}
		}
	}

	return false
}

// pathChars is what a directory may carry, because godwit puts it in a github url unescaped.
const pathChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-/"

func contained(dir string) error {
	if dir == "" {
		return errors.New("is empty")
	}
	if strings.HasPrefix(dir, "/") {
		return errors.New("is absolute; it is read relative to " + config.FileName)
	}
	if strings.Trim(dir, pathChars) != "" {
		return errors.New("carries a character godwit will not read a directory at")
	}
	for _, segment := range strings.Split(dir, "/") {
		if segment == ".." {
			return errors.New("leaves the project directory")
		}
	}

	return nil
}

func orRoot(root string) string {
	if root == "" {
		return "the repository root"
	}

	return root
}
