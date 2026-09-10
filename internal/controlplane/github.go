package controlplane

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const configGitHubRepositories = "github_repositories"

// SetGitHubRepositories records the repositories a GitHub App delivery may reach this target from.
func SetGitHubRepositories(config map[string]string, entries []string) error {
	spec, err := joinRepositories(entries)
	if err != nil || spec == "" {
		return err
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
