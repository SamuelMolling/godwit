package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

const planColumns = `id, target, key, rollout, state, history_hash, applied, repeatables, schema_fingerprint, schema_definition, search_path, drift, plan,
	validated, acked, allow_out_of_order, created_by, source, created_at, coalesce(run_id::text, ''), coalesce(superseded_by::text, ''), expansions`

// SavePlan stores a ready plan with its files; a ready plan with the same key on the target is replaced.
func (s *Store) SavePlan(ctx context.Context, p Plan, files map[string]string) error {
	if _, err := s.pool.Exec(ctx, `
		DELETE FROM cp_plan_files f USING cp_plans p
		WHERE f.plan_id = p.id AND p.target = $1 AND p.key = $2 AND p.state = 'ready'`, p.Target, p.Key); err != nil {
		return fmt.Errorf("replace plan files: %w", err)
	}
	names := make([]string, 0, len(files))
	bodies := make([]string, 0, len(files))
	for name, body := range files {
		names = append(names, name)
		bodies = append(bodies, body)
	}
	if _, err := s.pool.Exec(ctx, `
		WITH p AS (
			INSERT INTO cp_plans (id, target, key, rollout, state, history_hash, applied, schema_fingerprint, schema_definition,
				drift, plan, validated, acked, allow_out_of_order, created_by, source, files_hash, search_path, repeatables, expansions)
			VALUES ($1, $2, $3, $4, 'ready', $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $18, $19, $20, $21)
			ON CONFLICT (target, key) WHERE state = 'ready' DO UPDATE SET
				id = EXCLUDED.id, rollout = EXCLUDED.rollout, history_hash = EXCLUDED.history_hash, applied = EXCLUDED.applied,
				repeatables = EXCLUDED.repeatables, expansions = EXCLUDED.expansions,
				schema_fingerprint = EXCLUDED.schema_fingerprint, schema_definition = EXCLUDED.schema_definition,
				drift = EXCLUDED.drift, plan = EXCLUDED.plan, validated = EXCLUDED.validated, acked = EXCLUDED.acked,
				allow_out_of_order = EXCLUDED.allow_out_of_order, created_by = EXCLUDED.created_by, source = EXCLUDED.source,
				files_hash = EXCLUDED.files_hash, search_path = EXCLUDED.search_path, created_at = now()
			RETURNING id)
		INSERT INTO cp_plan_files (plan_id, name, body)
		SELECT (SELECT id FROM p), n, b FROM unnest($16::text[], $17::text[]) AS f (n, b)`,
		p.ID, p.Target, p.Key, p.Rollout, p.HistoryHash, jsonOf(p.Applied), p.SchemaFingerprint, p.SchemaDefinition,
		p.Drift, jsonOf(p.Migrations), p.Validated, append([]string{}, p.Acked...), p.AllowOutOfOrder, p.CreatedBy, p.Source, names, bodies,
		FilesHash(files), p.SearchPath, jsonOf(p.Repeatables), jsonOf(orEmpty(p.Expansions))); err != nil {
		return fmt.Errorf("save plan: %w", err)
	}

	return nil
}

// ReadyPlan returns the ready plan for a key on a target, created at or after since.
func (s *Store) ReadyPlan(ctx context.Context, target, key string, since time.Time) (Plan, error) {
	p, err := scanPlan(s.pool.QueryRow(ctx, `SELECT `+planColumns+` FROM cp_plans
		WHERE target = $1 AND key = $2 AND state = 'ready' AND created_at >= $3`, target, key, since))
	if errors.Is(err, pgx.ErrNoRows) {
		return Plan{}, fmt.Errorf("plan for %s: %w", target, ErrNotFound)
	}
	if err != nil {
		return Plan{}, fmt.Errorf("load plan: %w", err)
	}

	return p, nil
}

// BoundPlan returns the newest bound plan on a target whose rollout and submitted files match.
func (s *Store) BoundPlan(ctx context.Context, target, rollout, filesHash string) (Plan, error) {
	p, err := scanPlan(s.pool.QueryRow(ctx, `SELECT `+planColumns+` FROM cp_plans
		WHERE target = $1 AND rollout = $2 AND files_hash = $3 AND state = 'bound'
		ORDER BY created_at DESC, id LIMIT 1`, target, rollout, filesHash))
	if errors.Is(err, pgx.ErrNoRows) {
		return Plan{}, fmt.Errorf("bound plan for %s: %w", target, ErrNotFound)
	}
	if err != nil {
		return Plan{}, fmt.Errorf("load bound plan: %w", err)
	}

	return p, nil
}

// RetirePlan marks a bound plan superseded so a later job with the same files plans afresh.
func (s *Store) RetirePlan(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE cp_plans SET state = 'superseded' WHERE id = $1 AND state = 'bound'`, id)
	if err != nil {
		return fmt.Errorf("retire plan: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("bound plan %q: %w", id, ErrNotFound)
	}

	return nil
}

// Plan returns one plan by id.
func (s *Store) Plan(ctx context.Context, id string) (Plan, error) {
	p, err := scanPlan(s.pool.QueryRow(ctx, `SELECT `+planColumns+` FROM cp_plans WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Plan{}, fmt.Errorf("plan %q: %w", id, ErrNotFound)
	}
	if err != nil {
		return Plan{}, fmt.Errorf("load plan: %w", err)
	}

	return p, nil
}

// ListPlans returns a target's plans, newest first; limit caps the page (100 when zero, MaxPageSize at most).
func (s *Store) ListPlans(ctx context.Context, target string, limit int) ([]Plan, error) {
	limit = pageSize(limit)
	rows, err := s.pool.Query(ctx, `SELECT `+planColumns+` FROM cp_plans
		WHERE target = $1 ORDER BY created_at DESC, id LIMIT $2`, target, limit)
	if err != nil {
		return nil, fmt.Errorf("list plans: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Plan, error) { return scanPlan(row) })
	if err != nil {
		return nil, fmt.Errorf("list plans: %w", err)
	}

	return out, nil
}

// PlanFiles returns the migration files stored with a plan.
func (s *Store) PlanFiles(ctx context.Context, id string) (map[string]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT name, body FROM cp_plan_files WHERE plan_id = $1`, id)
	if err != nil {
		return nil, fmt.Errorf("list plan files: %w", err)
	}
	files := map[string]string{}
	var name, body string
	if _, err := pgx.ForEachRow(rows, []any{&name, &body}, func() error {
		files[name] = body

		return nil
	}); err != nil {
		return nil, fmt.Errorf("read plan files: %w", err)
	}

	return files, nil
}

// ReadyPlanCount counts a target's ready plans created at or after since.
func (s *Store) ReadyPlanCount(ctx context.Context, target string, since time.Time) (int, error) {
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM cp_plans WHERE target = $1 AND state = 'ready' AND created_at >= $2`,
		target, since).Scan(&n); err != nil {
		return 0, fmt.Errorf("count ready plans: %w", err)
	}

	return n, nil
}

// SweepPlans deletes bound and superseded plans created before olderThan, keeping those an unfinished run still applies.
func (s *Store) SweepPlans(ctx context.Context, olderThan time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM cp_plans p WHERE p.state IN ('bound', 'superseded') AND p.created_at < $1
		AND NOT EXISTS (SELECT 1 FROM cp_runs r WHERE r.plan_id = p.id AND r.finished_at IS NULL)`, olderThan)
	if err != nil {
		return 0, fmt.Errorf("sweep plans: %w", err)
	}

	return tag.RowsAffected(), nil
}

// BindPlan marks a ready plan as applied by a run.
func (s *Store) BindPlan(ctx context.Context, id, runID string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE cp_plans SET state = 'bound', run_id = $2 WHERE id = $1 AND state = 'ready'`, id, runID)
	if err != nil {
		return fmt.Errorf("bind plan: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("ready plan %q: %w", id, ErrNotFound)
	}

	return nil
}

// SupersedePlan retires a ready plan and stores next in its place, linking the two.
func (s *Store) SupersedePlan(ctx context.Context, id string, next Plan, files map[string]string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE cp_plans SET state = 'superseded' WHERE id = $1 AND state = 'ready'`, id)
	if err != nil {
		return fmt.Errorf("supersede plan: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("ready plan %q: %w", id, ErrNotFound)
	}
	if err := s.SavePlan(ctx, next, files); err != nil {
		return err
	}
	if _, err := s.pool.Exec(ctx, `UPDATE cp_plans SET superseded_by = $2 WHERE id = $1`, id, next.ID); err != nil {
		return fmt.Errorf("link superseded plan: %w", err)
	}

	return nil
}

// RunsApplying maps each migration held by a succeeded run of the target created after since to that run's id,
// keyed the way engine.Migration.ID renders it.
func (s *Store) RunsApplying(ctx context.Context, target string, since time.Time) (map[string]string, error) {
	const migrationID = `regexp_replace(f.name, '\.(up|down)\.sql$', '')`
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT ON (`+migrationID+`) `+migrationID+`, r.id
		FROM cp_runs r JOIN cp_run_files f ON f.run_id = r.id
		WHERE r.target = $1 AND r.state = 'succeeded' AND r.created_at > $2
		ORDER BY `+migrationID+`, r.created_at`, target, since)
	if err != nil {
		return nil, fmt.Errorf("list applying runs: %w", err)
	}
	out := map[string]string{}
	var mid, id string
	if _, err := pgx.ForEachRow(rows, []any{&mid, &id}, func() error {
		out[mid] = id

		return nil
	}); err != nil {
		return nil, fmt.Errorf("list applying runs: %w", err)
	}

	return out, nil
}

func scanPlan(row pgx.Row) (Plan, error) {
	var p Plan
	err := row.Scan(&p.ID, &p.Target, &p.Key, &p.Rollout, &p.State, &p.HistoryHash, &p.Applied, &p.Repeatables, &p.SchemaFingerprint,
		&p.SchemaDefinition, &p.SearchPath, &p.Drift, &p.Migrations, &p.Validated, &p.Acked, &p.AllowOutOfOrder, &p.CreatedBy,
		&p.Source, &p.CreatedAt, &p.RunID, &p.SupersededBy, &p.Expansions)

	return p, err
}

// orEmpty keeps a nil map out of the column: jsonb is NOT NULL and 'null' is not an object.
func orEmpty(m map[string]Expansion) map[string]Expansion {
	if m == nil {
		return map[string]Expansion{}
	}

	return m
}

// FilesHash identifies a set of submitted files independent of what is pending; the retry migration computes the same value in SQL.
func FilesHash(files map[string]string) string {
	names := slices.Sorted(maps.Keys(files))
	h := sha256.New()
	for _, name := range names {
		h.Write([]byte(name + "\n" + files[name] + "\n"))
	}

	return hex.EncodeToString(h.Sum(nil))
}

func jsonOf(v any) []byte {
	b, _ := json.Marshal(v)

	return b
}
