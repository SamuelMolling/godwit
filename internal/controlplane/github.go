package controlplane

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const configGitHubRepositories = "github_repositories"

// SetGitHubRepositories records the repositories a GitHub App delivery may reach this target from, and
// an empty list leaves none able to.
func SetGitHubRepositories(config map[string]string, entries []string) error {
	spec, err := joinRepositories(entries)
	if err != nil {
		return err
	}
	if spec == "" {
		delete(config, configGitHubRepositories)

		return nil
	}
	config[configGitHubRepositories] = spec

	return nil
}

func joinRepositories(entries []string) (string, error) {
	out := make([]string, 0, len(entries))
	seen := map[string]bool{}
	for _, raw := range entries {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		if err := validRepository(entry); err != nil {
			return "", err
		}
		if seen[entry] {
			continue
		}
		seen[entry] = true
		out = append(out, entry)
	}

	return strings.Join(out, ","), nil
}

func splitRepositories(spec string) []string {
	if spec == "" {
		return nil
	}

	return strings.Split(spec, ",")
}

func validRepository(entry string) error {
	repo, dir, hasDir := strings.Cut(entry, ":")
	owner, name, ok := strings.Cut(repo, "/")
	switch {
	case !ok || owner == "" || name == "" || strings.ContainsAny(name, "/ "):
		return fmt.Errorf("github repository %q is not owner/repo or owner/repo:dir", entry)
	case hasDir && dir == "":
		return fmt.Errorf("github repository %q names no directory after its colon", entry)
	}

	return nil
}

// GitHubBindings returns the stored repository binding of every target that carries one, by target name.
func (s *Store) GitHubBindings(ctx context.Context) (map[string]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT name, config ->> $1 FROM cp_targets WHERE config ? $1`, configGitHubRepositories)
	if err != nil {
		return nil, fmt.Errorf("list github bindings: %w", err)
	}
	out := map[string]string{}
	var name, spec string
	if _, err := pgx.ForEachRow(rows, []any{&name, &spec}, func() error {
		out[name] = spec

		return nil
	}); err != nil {
		return nil, fmt.Errorf("read github bindings: %w", err)
	}

	return out, nil
}

// RecordDelivery records a webhook delivery id and reports whether it is the first sight of it.
func (s *Store) RecordDelivery(ctx context.Context, id, event, repository string) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`INSERT INTO cp_webhook_deliveries (id, event, repository) VALUES ($1, $2, $3) ON CONFLICT (id) DO NOTHING`,
		id, event, repository)
	if err != nil {
		return false, fmt.Errorf("record delivery: %w", err)
	}

	return tag.RowsAffected() == 1, nil
}

// SweepDeliveries deletes recorded delivery ids received before t and reports how many went.
func (s *Store) SweepDeliveries(ctx context.Context, before time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM cp_webhook_deliveries WHERE received_at < $1`, before)
	if err != nil {
		return 0, fmt.Errorf("sweep deliveries: %w", err)
	}

	return tag.RowsAffected(), nil
}

// GitHubRun binds a run to the pull request whose command created it, so a replica that did not accept
// that command can still report the run back when it settles.
type GitHubRun struct {
	RunID        string
	Repository   string
	RepositoryID int64
	Installation int64
	PullRequest  int
	Head         string
	Command      string
	Target       string
	Marker       string
	Format       string
	CheckRun     int64
	// State is the run's live state, read from cp_runs rather than from this row.
	State string
	// Reverts is the run this one undoes, empty for every other kind.
	Reverts string
}

const githubRunColumns = `g.run_id, g.repository, g.repository_id, g.installation, g.pull_request, g.head,
	g.command, g.target, g.marker, g.format, g.check_run`

func (g *GitHubRun) fields() []any {
	return []any{
		&g.RunID, &g.Repository, &g.RepositoryID, &g.Installation, &g.PullRequest, &g.Head,
		&g.Command, &g.Target, &g.Marker, &g.Format, &g.CheckRun,
	}
}

// RecordGitHubRun binds run to a pull request; a second command over the same run replaces the binding,
// which is how `godwit confirm` takes over the check its own comment opened.
func (s *Store) RecordGitHubRun(ctx context.Context, g GitHubRun) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO cp_github_runs (run_id, repository, repository_id, installation, pull_request, head,
			command, target, marker, format, check_run)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (run_id) DO UPDATE SET
			head = EXCLUDED.head, command = EXCLUDED.command, marker = EXCLUDED.marker,
			format = EXCLUDED.format, check_run = EXCLUDED.check_run,
			reported_state = NULL, claimed_at = NULL`,
		g.RunID, g.Repository, g.RepositoryID, g.Installation, g.PullRequest, g.Head,
		g.Command, g.Target, g.Marker, g.Format, g.CheckRun)
	if err != nil {
		return fmt.Errorf("record github run: %w", err)
	}

	return nil
}

// reportable are the states worth telling a pull request about. `reverted` is not one: the run that did
// the reverting reports itself, and the original turning reverted would overwrite that with older news.
var reportable = []string{StateSucceeded, StateFailed, StateNeedsAttention, StateAwaitingContract}

// ClaimGitHubReports takes the bindings whose run has reached a state nobody has reported yet, and holds
// them for lease. A claim that is not marked reported within it is taken again, so a replica dying mid
// report costs a repeat rather than silence.
func (s *Store) ClaimGitHubReports(ctx context.Context, lease time.Duration, limit int) ([]GitHubRun, error) {
	rows, err := s.pool.Query(ctx, `
		UPDATE cp_github_runs g SET claimed_at = now()
		FROM cp_runs r
		WHERE g.run_id = r.id AND g.run_id IN (
			SELECT g2.run_id FROM cp_github_runs g2 JOIN cp_runs r2 ON r2.id = g2.run_id
			WHERE r2.state = ANY($1)
			  AND g2.reported_state IS DISTINCT FROM r2.state
			  AND (g2.claimed_at IS NULL OR g2.claimed_at < now() - $2::interval)
			ORDER BY g2.created_at
			LIMIT $3)
		RETURNING `+githubRunColumns+`, r.state, coalesce(r.reverts::text, '')`, reportable, lease, limit)
	if err != nil {
		return nil, fmt.Errorf("claim github reports: %w", err)
	}

	return collectGitHubRuns(rows)
}

// MarkGitHubReported records the state the pull request was told about and releases the claim.
func (s *Store) MarkGitHubReported(ctx context.Context, runID, state string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE cp_github_runs SET reported_state = $2, claimed_at = NULL WHERE run_id = $1`, runID, state)
	if err != nil {
		return fmt.Errorf("mark github reported: %w", err)
	}

	return nil
}

// GitHubRunsOf lists the runs one pull request created on one target, newest first, with their live state.
func (s *Store) GitHubRunsOf(ctx context.Context, repository string, pull int, target string) ([]GitHubRun, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+githubRunColumns+`, r.state, coalesce(r.reverts::text, '')
		FROM cp_github_runs g JOIN cp_runs r ON r.id = g.run_id
		WHERE g.repository = $1 AND g.pull_request = $2 AND g.target = $3
		ORDER BY r.seq DESC`, repository, pull, target)
	if err != nil {
		return nil, fmt.Errorf("list github runs: %w", err)
	}

	return collectGitHubRuns(rows)
}

func collectGitHubRuns(rows pgx.Rows) ([]GitHubRun, error) {
	var out []GitHubRun
	var g GitHubRun
	if _, err := pgx.ForEachRow(rows, append(g.fields(), &g.State, &g.Reverts), func() error {
		out = append(out, g)

		return nil
	}); err != nil {
		return nil, fmt.Errorf("read github runs: %w", err)
	}

	return out, nil
}

// SweepGitHubRuns deletes reported bindings created before t and reports how many went.
func (s *Store) SweepGitHubRuns(ctx context.Context, before time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM cp_github_runs WHERE created_at < $1 AND reported_state IS NOT NULL`, before)
	if err != nil {
		return 0, fmt.Errorf("sweep github runs: %w", err)
	}

	return tag.RowsAffected(), nil
}
