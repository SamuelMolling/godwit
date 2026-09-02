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
	"github.com/SamuelMolling/godwit/internal/metrics"
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
	StateReverted         = "reverted"
)

// Run phases.
const (
	PhaseExpand   = "expand"
	PhaseContract = "contract"
)

// Sentinel errors.
var (
	ErrNotFound            = errors.New("not found")
	ErrLeaseLost           = errors.New("lease lost")
	ErrNotResumable        = errors.New("run is not failed or parked")
	ErrNotAwaitingContract = errors.New("run is not awaiting contract")
	ErrNotRevertable       = errors.New("run is not the latest on its target or the target is busy")
	ErrBaselineRun         = errors.New("baseline runs cannot be reverted")
)

// Run kinds.
const (
	KindMigrate  = "migrate"
	KindBaseline = "baseline"
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
	Reverts    string
	Kind       string
	Timeouts   Timeouts
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

// Migrate applies the control-plane schema and reports how many migrations ran; the advisory lock needs a dedicated session.
func Migrate(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return 0, fmt.Errorf("acquire store connection: %w", err)
	}
	defer conn.Release()

	return applyMigrations(ctx, conn.Conn(), storeMigrations)
}

func applyMigrations(ctx context.Context, db engine.DB, migs []engine.Migration) (int, error) {
	plans, err := buildPlans(migs, engine.DirectionUp)
	if err != nil {
		return 0, err
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

// CreateRun queues a run with its migration files and per-run timeout overrides.
func (s *Store) CreateRun(ctx context.Context, id, target, rollout string, files map[string]string, t Timeouts) error {
	names := make([]string, 0, len(files))
	bodies := make([]string, 0, len(files))
	for name, body := range files {
		names = append(names, name)
		bodies = append(bodies, body)
	}
	if _, err := s.pool.Exec(ctx, `
		WITH r AS (INSERT INTO cp_runs (id, target, state, rollout, lock_timeout, statement_timeout)
			VALUES ($1, $2, 'queued', $5, nullif($6, ''), nullif($7, '')))
		INSERT INTO cp_run_files (run_id, name, body)
		SELECT $1, n, b FROM unnest($3::text[], $4::text[]) AS f (n, b)`,
		id, target, names, bodies, rollout, t.Lock, t.Statement); err != nil {
		return fmt.Errorf("create run: %w", err)
	}

	return nil
}

// CreateBaseline records an already-succeeded baseline run holding the files that describe the target's current schema.
func (s *Store) CreateBaseline(ctx context.Context, id, target string, files map[string]string) error {
	names := make([]string, 0, len(files))
	bodies := make([]string, 0, len(files))
	for name, body := range files {
		names = append(names, name)
		bodies = append(bodies, body)
	}
	if _, err := s.pool.Exec(ctx, `
		WITH r AS (INSERT INTO cp_runs (id, target, state, kind, finished_at)
			VALUES ($1, $2, 'succeeded', 'baseline', now()))
		INSERT INTO cp_run_files (run_id, name, body)
		SELECT $1, n, b FROM unnest($3::text[], $4::text[]) AS f (n, b)`,
		id, target, names, bodies); err != nil {
		return fmt.Errorf("create baseline: %w", err)
	}

	return nil
}

// CreateRevert queues a run that applies the down side of another run's files.
// Only the latest non-reverted run on an idle target can be reverted.
func (s *Store) CreateRevert(ctx context.Context, id, original string, t Timeouts) error {
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO cp_runs (id, target, state, reverts, lock_timeout, statement_timeout)
		SELECT $1, o.target, 'queued', o.id, nullif($3, ''), nullif($4, '') FROM cp_runs o
		WHERE o.id = $2 AND o.state IN ('succeeded', 'awaiting_contract', 'failed', 'needs_attention')
		  AND NOT EXISTS (
			SELECT 1 FROM cp_runs r WHERE r.target = o.target AND r.id <> o.id
			  AND (r.state IN ('queued', 'running')
			    OR (r.reverts IS NULL AND r.state <> 'reverted' AND r.created_at > o.created_at)))`,
		id, original, t.Lock, t.Statement)
	if err != nil {
		return fmt.Errorf("create revert: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("run %q: %w", original, ErrNotRevertable)
	}

	return nil
}

const runColumns = `id, target, state, coalesce(error, ''), attempts, rollout, phase, coalesce(reverts::text, ''), kind,
	coalesce(lock_timeout, ''), coalesce(statement_timeout, ''), created_at, finished_at`

func (r *Run) fields() []any {
	return []any{
		&r.ID, &r.Target, &r.State, &r.Error, &r.Attempts, &r.Rollout, &r.Phase, &r.Reverts, &r.Kind,
		&r.Timeouts.Lock, &r.Timeouts.Statement, &r.CreatedAt, &r.FinishedAt,
	}
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

// Claim leases the next queued run, or a running one whose lease expired; one lease per target.
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

// Finish records a terminal state and releases the lease; a succeeded revert marks its original reverted.
func (s *Store) Finish(ctx context.Context, id, state, errText string) error {
	tag, err := s.pool.Exec(ctx, `
		WITH del AS (DELETE FROM cp_leases WHERE run_id = $1),
		orig AS (
			UPDATE cp_runs SET state = 'reverted', updated_at = now()
			WHERE $2 = 'succeeded' AND id = (SELECT reverts FROM cp_runs WHERE id = $1))
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

// Resume requeues a failed or parked run and returns it.
func (s *Store) Resume(ctx context.Context, id string) (Run, error) {
	run, err := scanRun(s.pool.QueryRow(ctx, `
		WITH del AS (DELETE FROM cp_leases WHERE run_id = $1)
		UPDATE cp_runs SET state = 'queued', attempts = 0, error = NULL, finished_at = NULL, updated_at = now()
		WHERE id = $1 AND state IN ('failed', 'needs_attention')
		RETURNING `+runColumns, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, fmt.Errorf("run %q: %w", id, ErrNotResumable)
	}
	if err != nil {
		return Run{}, fmt.Errorf("resume run: %w", err)
	}

	return run, nil
}

// Ping reports whether the store answers a trivial query.
func (s *Store) Ping(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx, "SELECT 1"); err != nil {
		return fmt.Errorf("ping store: %w", err)
	}

	return nil
}

// RunStats counts runs per target and state with the age of the oldest one.
func (s *Store) RunStats(ctx context.Context) ([]metrics.RunStat, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT target, state, count(*), extract(epoch FROM now() - min(updated_at))
		FROM cp_runs GROUP BY target, state ORDER BY target, state`)
	if err != nil {
		return nil, fmt.Errorf("run stats: %w", err)
	}
	var out []metrics.RunStat
	var st metrics.RunStat
	var age float64
	if _, err := pgx.ForEachRow(rows, []any{&st.Target, &st.State, &st.Count, &age}, func() error {
		st.OldestAge = time.Duration(age * float64(time.Second))
		out = append(out, st)

		return nil
	}); err != nil {
		return nil, fmt.Errorf("read run stats: %w", err)
	}

	return out, nil
}

// Confirm requeues an awaiting_contract run for its contract phase.
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
