package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/SamuelMolling/godwit/internal/engine"
)

// Pool is the connection-pool surface the store needs; *pgxpool.Pool satisfies it.
type Pool interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Run states.
const (
	StateQueued           = "queued"
	StateRunning          = "running"
	StateSucceeded        = "succeeded"
	StateFailed           = "failed"
	StateNeedsAttention   = "needs_attention"
	StateAwaitingContract = "awaiting_contract"
)

// Run phases under a rollout policy.
const (
	PhaseExpand   = "expand"
	PhaseContract = "contract"
)

// Sentinel errors callers can match on.
var (
	ErrNotFound            = errors.New("not found")
	ErrLeaseLost           = errors.New("lease lost")
	ErrNotResumable        = errors.New("run is not failed or parked")
	ErrNotAwaitingContract = errors.New("run is not awaiting contract")
)

// Run is one migration run tracked by the control plane.
type Run struct {
	ID         string
	Target     string
	State      string
	Error      string
	Attempts   int
	Rollout    string
	Phase      string
	CreatedAt  time.Time
	FinishedAt *time.Time
}

// Store persists targets, runs, files and leases in the control-plane database.
type Store struct {
	pool Pool
}

// NewStore wraps a connection pool.
func NewStore(pool Pool) *Store {
	return &Store{pool: pool}
}

// Migrate applies the control-plane schema using godwit's own engine.
// It needs a dedicated session (advisory lock), not the pool.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire store connection: %w", err)
	}
	defer conn.Release()

	return applyMigrations(ctx, conn.Conn(), storeMigrations)
}

func applyMigrations(ctx context.Context, db engine.DB, migs []engine.Migration) error {
	plans, err := buildPlans(migs)
	if err != nil {
		return err
	}

	return applyPlans(ctx, db, engine.Options{}, plans)
}

// RegisterTarget upserts a target and its credential config.
func (s *Store) RegisterTarget(ctx context.Context, name, provider string, config map[string]string) error {
	cfg, _ := json.Marshal(config) // map[string]string cannot fail
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO cp_targets (name, provider, config) VALUES ($1, $2, $3)
		 ON CONFLICT (name) DO UPDATE SET provider = EXCLUDED.provider, config = EXCLUDED.config`,
		name, provider, cfg); err != nil {
		return fmt.Errorf("register target: %w", err)
	}

	return nil
}

// Target returns a target's provider name and config.
func (s *Store) Target(ctx context.Context, name string) (string, map[string]string, error) {
	var provider string
	var cfg []byte
	err := s.pool.QueryRow(ctx,
		`SELECT provider, config FROM cp_targets WHERE name = $1`, name).Scan(&provider, &cfg)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, fmt.Errorf("target %q: %w", name, ErrNotFound)
	}
	if err != nil {
		return "", nil, fmt.Errorf("load target: %w", err)
	}
	var config map[string]string
	if err := json.Unmarshal(cfg, &config); err != nil {
		return "", nil, fmt.Errorf("target %q config: %w", name, err)
	}

	return provider, config, nil
}

// CreateRun queues a run with its migration files (single atomic statement).
func (s *Store) CreateRun(ctx context.Context, id, target, rollout string, files map[string]string) error {
	names := make([]string, 0, len(files))
	bodies := make([]string, 0, len(files))
	for name, body := range files {
		names = append(names, name)
		bodies = append(bodies, body)
	}
	if _, err := s.pool.Exec(ctx, `
		WITH r AS (INSERT INTO cp_runs (id, target, state, rollout) VALUES ($1, $2, 'queued', $5))
		INSERT INTO cp_run_files (run_id, name, body)
		SELECT $1, n, b FROM unnest($3::text[], $4::text[]) AS f (n, b)`,
		id, target, names, bodies, rollout); err != nil {
		return fmt.Errorf("create run: %w", err)
	}

	return nil
}

const runColumns = `id, target, state, coalesce(error, ''), attempts, rollout, phase, created_at, finished_at`

func (r *Run) fields() []any {
	return []any{&r.ID, &r.Target, &r.State, &r.Error, &r.Attempts, &r.Rollout, &r.Phase, &r.CreatedAt, &r.FinishedAt}
}

func scanRun(row pgx.Row) (Run, error) {
	var r Run
	err := row.Scan(r.fields()...)

	return r, err
}

// Run returns one run by id.
func (s *Store) Run(ctx context.Context, id string) (Run, error) {
	r, err := scanRun(s.pool.QueryRow(ctx,
		`SELECT `+runColumns+` FROM cp_runs WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, fmt.Errorf("run %q: %w", id, ErrNotFound)
	}
	if err != nil {
		return Run{}, fmt.Errorf("load run: %w", err)
	}

	return r, nil
}

// ListRuns returns recent runs, optionally filtered by target.
func (s *Store) ListRuns(ctx context.Context, target string) ([]Run, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+runColumns+` FROM cp_runs
		 WHERE $1 = '' OR target = $1
		 ORDER BY created_at DESC LIMIT 100`, target)
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}

	var out []Run
	var r Run
	if _, err := pgx.ForEachRow(rows, r.fields(), func() error {
		out = append(out, r)

		return nil
	}); err != nil {
		return nil, fmt.Errorf("read runs: %w", err)
	}

	return out, nil
}

// RunFiles returns the migration files attached to a run.
func (s *Store) RunFiles(ctx context.Context, id string) (map[string]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT name, body FROM cp_run_files WHERE run_id = $1`, id)
	if err != nil {
		return nil, fmt.Errorf("list run files: %w", err)
	}

	files := map[string]string{}
	var name, body string
	if _, err := pgx.ForEachRow(rows, []any{&name, &body}, func() error {
		files[name] = body

		return nil
	}); err != nil {
		return nil, fmt.Errorf("read run files: %w", err)
	}

	return files, nil
}

// Claim atomically leases the next runnable run: a queued run, or a running
// run whose executor's lease expired (the crash-recovery path). One live
// lease per target at a time.
func (s *Store) Claim(ctx context.Context, holder string, ttl time.Duration) (Run, bool, error) {
	run, err := scanRun(s.pool.QueryRow(ctx, `
		WITH candidate AS (
			SELECT r.id FROM cp_runs r
			LEFT JOIN cp_leases l ON l.run_id = r.id
			WHERE r.state IN ('queued', 'running')
			  AND (l.run_id IS NULL OR l.expires_at <= now())
			  AND NOT EXISTS (
				SELECT 1 FROM cp_runs r2
				JOIN cp_leases l2 ON l2.run_id = r2.id
				WHERE r2.target = r.target AND r2.id <> r.id AND l2.expires_at > now())
			ORDER BY r.created_at
			FOR UPDATE OF r SKIP LOCKED
			LIMIT 1
		), lease AS (
			INSERT INTO cp_leases (run_id, holder, expires_at)
			SELECT id, $1, now() + $2 FROM candidate
			ON CONFLICT (run_id) DO UPDATE SET holder = EXCLUDED.holder, expires_at = EXCLUDED.expires_at
		)
		UPDATE cp_runs SET state = 'running', attempts = attempts + 1, updated_at = now()
		WHERE id IN (SELECT id FROM candidate)
		RETURNING `+runColumns, holder, ttl))
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, false, nil
	}
	if err != nil {
		return Run{}, false, fmt.Errorf("claim run: %w", err)
	}

	return run, true, nil
}

// Heartbeat extends the holder's lease; ErrLeaseLost means another holder took it.
func (s *Store) Heartbeat(ctx context.Context, runID, holder string, ttl time.Duration) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE cp_leases SET expires_at = now() + $3 WHERE run_id = $1 AND holder = $2`,
		runID, holder, ttl)
	if err != nil {
		return fmt.Errorf("heartbeat: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrLeaseLost
	}

	return nil
}

// Finish records a terminal state and releases the lease.
func (s *Store) Finish(ctx context.Context, id, state, errText string) error {
	tag, err := s.pool.Exec(ctx, `
		WITH del AS (DELETE FROM cp_leases WHERE run_id = $1)
		UPDATE cp_runs SET state = $2, error = NULLIF($3, ''), finished_at = now(), updated_at = now()
		WHERE id = $1`, id, state, errText)
	if err != nil {
		return fmt.Errorf("finish run: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("run %q: %w", id, ErrNotFound)
	}

	return nil
}

// Resume requeues a failed or parked run.
func (s *Store) Resume(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `
		WITH del AS (DELETE FROM cp_leases WHERE run_id = $1)
		UPDATE cp_runs SET state = 'queued', attempts = 0, error = NULL, finished_at = NULL, updated_at = now()
		WHERE id = $1 AND state IN ('failed', 'needs_attention')`, id)
	if err != nil {
		return fmt.Errorf("resume run: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("run %q: %w", id, ErrNotResumable)
	}

	return nil
}

// Confirm releases the contract phase of a run that finished its expand phase.
func (s *Store) Confirm(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE cp_runs SET state = 'queued', phase = 'contract', attempts = 0, finished_at = NULL, updated_at = now()
		WHERE id = $1 AND state = 'awaiting_contract'`, id)
	if err != nil {
		return fmt.Errorf("confirm rollout: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("run %q: %w", id, ErrNotAwaitingContract)
	}

	return nil
}
