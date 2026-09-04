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
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/SamuelMolling/godwit/internal/engine"
	"github.com/SamuelMolling/godwit/internal/metrics"
)

// Pool is the connection-pool surface the store needs; *pgxpool.Pool and pgx.Tx satisfy it.
type Pool interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Begin(ctx context.Context) (pgx.Tx, error)
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
	ErrNotRevertable       = errors.New("run is not revertable")
	ErrBaselineRun         = errors.New("baseline runs cannot be reverted")
)

// Run kinds that adopt what the target already holds instead of applying anything.
var adoptionKinds = []string{KindBaseline, KindReconcile}

// Reasons a run cannot be reverted, appended to ErrNotRevertable.
const (
	reasonState      = "its state is %q; a revertable run is succeeded, awaiting_contract, failed or needs_attention"
	reasonBusy       = "target %s has a queued or running run"
	reasonNewer      = "run %s is newer and still stands; revert it first, or force this one"
	reasonNothing    = "it applied no migration that still stands"
	reasonIsRevert   = "it is itself a revert"
	reasonCheckpoint = "it is a checkpoint, and a checkpoint has no inverse"
	reasonCollapsed  = "checkpoint %s collapsed it: the target's history below version %014d cannot be reverted"
)

// Run kinds.
const (
	KindMigrate   = "migrate"
	KindBaseline  = "baseline"
	KindReconcile = "reconcile"
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
	Provenance Provenance
	PlanID     string
	Retries    int
	NotBefore  *time.Time
	CreatedAt  time.Time
	FinishedAt *time.Time
	Progress   *RunProgress
	// Expansions is the SQL godwit generated for the run's directives, keyed by migration id.
	Expansions map[string]Expansion
}

// RunProgress is the newest statement a running run reported, so a long backfill is visible while it runs.
type RunProgress struct {
	Migration string `json:"migration"`
	Statement int    `json:"statement"`
	Phase     string `json:"phase,omitempty"`
	RowsDone  int64  `json:"rows_done,omitempty"`
	RowsTotal int64  `json:"rows_total,omitempty"`
	Batches   int    `json:"batches,omitempty"`
}

// Provenance records who created a run and where its files came from.
type Provenance struct {
	CreatedBy string
	Source    string
}

// Store persists targets, runs, files and leases in the control-plane database.
type Store struct {
	pool Pool
}

// NewStore wraps a connection pool.
func NewStore(pool Pool) *Store {
	return &Store{pool: pool}
}

// Transact runs fn on a store bound to one transaction; its writes stay invisible to other sessions until fn returns nil.
func (s *Store) Transact(ctx context.Context, fn func(tx *Store) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	if err := fn(&Store{pool: tx}); err != nil {
		_ = tx.Rollback(ctx)

		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}

	return nil
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

	return applyPlans(ctx, db, engine.Options{}, plans, nil)
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

// TargetSummary is the control plane's own view of a registered target, assembled without connecting to it.
type TargetSummary struct {
	Name            string
	Provider        string
	Timeouts        Timeouts
	SearchPath      string
	RequirePlan     bool
	KeepOld         bool
	LastRun         *Run
	AttentionRuns   int
	UnresolvedDrift bool
	ReadyPlans      int
	AppliedCount    int
}

// ListTargets summarises every target by name, counting ready plans created at or after since.
func (s *Store) ListTargets(ctx context.Context, since time.Time) ([]TargetSummary, error) {
	last, err := s.lastRuns(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT t.name, t.provider, t.config,
			(SELECT count(DISTINCT left(a.migration, 14)) FROM cp_run_applied a JOIN cp_runs r ON r.id = a.run_id
			 WHERE r.target = t.name AND `+standingRow+`),
			(SELECT count(*) FROM cp_runs r WHERE r.target = t.name AND r.state IN ('needs_attention', 'awaiting_contract')),
			(SELECT count(*) FROM cp_plans p WHERE p.target = t.name AND p.state = 'ready' AND p.created_at >= $1),
			EXISTS (SELECT 1 FROM cp_drift_events d WHERE d.target = t.name AND d.resolved_at IS NULL)
		FROM cp_targets t ORDER BY t.name`, since)
	if err != nil {
		return nil, fmt.Errorf("list targets: %w", err)
	}
	var out []TargetSummary
	var sum TargetSummary
	var cfg []byte
	fields := []any{&sum.Name, &sum.Provider, &cfg, &sum.AppliedCount, &sum.AttentionRuns, &sum.ReadyPlans, &sum.UnresolvedDrift}
	if _, err := pgx.ForEachRow(rows, fields, func() error {
		var config map[string]string
		if err := json.Unmarshal(cfg, &config); err != nil {
			return fmt.Errorf("target %q config: %w", sum.Name, err)
		}
		sum.Timeouts, sum.SearchPath = TargetTimeouts(config), config[ConfigSearchPath]
		sum.RequirePlan, sum.KeepOld = config[ConfigRequirePlan] == "true", config[ConfigKeepOld] != "false"
		sum.LastRun = last[sum.Name]
		out = append(out, sum)

		return nil
	}); err != nil {
		return nil, fmt.Errorf("read targets: %w", err)
	}

	return out, nil
}

func (s *Store) lastRuns(ctx context.Context) (map[string]*Run, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+runColumns+` FROM (SELECT DISTINCT ON (target) * FROM cp_runs ORDER BY target, created_at DESC) r`)
	if err != nil {
		return nil, fmt.Errorf("list last runs: %w", err)
	}
	out := map[string]*Run{}
	var r Run
	if _, err := pgx.ForEachRow(rows, r.fields(), func() error {
		run := r
		out[run.Target] = &run

		return nil
	}); err != nil {
		return nil, fmt.Errorf("read last runs: %w", err)
	}

	return out, nil
}

// CreateRun queues a run with its migration files, per-run timeout overrides, provenance, bound plan
// (empty when implicit) and the directive expansions the run applies in place of the file bodies.
func (s *Store) CreateRun(ctx context.Context, id, target, rollout string, files map[string]string, t Timeouts, p Provenance, planID string, exps map[string]Expansion) error {
	names := make([]string, 0, len(files))
	bodies := make([]string, 0, len(files))
	for name, body := range files {
		names = append(names, name)
		bodies = append(bodies, body)
	}
	if _, err := s.pool.Exec(ctx, `
		WITH r AS (INSERT INTO cp_runs (id, target, state, rollout, lock_timeout, statement_timeout, created_by, source, plan_id, expansions)
			VALUES ($1, $2, 'queued', $5, nullif($6, ''), nullif($7, ''), $8, $9, nullif($10, '')::uuid, $11))
		INSERT INTO cp_run_files (run_id, name, body)
		SELECT $1, n, b FROM unnest($3::text[], $4::text[]) AS f (n, b)`,
		id, target, names, bodies, rollout, t.Lock, t.Statement, p.CreatedBy, p.Source, planID, jsonOf(orEmpty(exps))); err != nil {
		return fmt.Errorf("create run: %w", err)
	}

	return nil
}

// SaveProgress records what the newest statement of a running run reported. Every transition that starts
// or ends an attempt clears it, so a progress row always describes work in flight and never a leftover.
func (s *Store) SaveProgress(ctx context.Context, id string, p RunProgress) error {
	if _, err := s.pool.Exec(ctx, `UPDATE cp_runs SET progress = $2 WHERE id = $1`, id, jsonOf(p)); err != nil {
		return fmt.Errorf("save run progress: %w", err)
	}

	return nil
}

// RetireColumns records the columns a run left behind so a desired-schema diff stops proposing their drop.
func (s *Store) RetireColumns(ctx context.Context, target, runID, migration string, cols []RetiredColumn) error {
	for _, c := range cols {
		if _, err := s.pool.Exec(ctx, `
			INSERT INTO cp_retired_columns (target, schema, rel, col, retires, migration, run_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (target, schema, rel, col) DO UPDATE SET
				retires = EXCLUDED.retires, migration = EXCLUDED.migration, run_id = EXCLUDED.run_id, retired_at = now()`,
			target, c.Schema, c.Table, c.Column, c.Retires, migration, runID); err != nil {
			return fmt.Errorf("retire column %s: %w", c, err)
		}
	}

	return nil
}

// UnretireColumns forgets columns a revert renamed back into place.
func (s *Store) UnretireColumns(ctx context.Context, target string, cols []RetiredColumn) error {
	for _, c := range cols {
		if _, err := s.pool.Exec(ctx,
			`DELETE FROM cp_retired_columns WHERE target = $1 AND schema = $2 AND rel = $3 AND col = $4`,
			target, c.Schema, c.Table, c.Column); err != nil {
			return fmt.Errorf("unretire column %s: %w", c, err)
		}
	}

	return nil
}

// RetiredColumns lists the columns a target keeps only as the rollback of a completed change-type.
func (s *Store) RetiredColumns(ctx context.Context, target string) ([]RetiredColumn, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT schema, rel, col, retires FROM cp_retired_columns WHERE target = $1 ORDER BY schema, rel, col`, target)
	if err != nil {
		return nil, fmt.Errorf("list retired columns: %w", err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[RetiredColumn])
	if err != nil {
		return nil, fmt.Errorf("read retired columns: %w", err)
	}

	return out, nil
}

// AwaitingContract returns the target's run stopped between its phases, if it has one.
func (s *Store) AwaitingContract(ctx context.Context, target string) (Run, bool, error) {
	r, err := scanRun(s.pool.QueryRow(ctx,
		`SELECT `+runColumns+` FROM cp_runs WHERE target = $1 AND state = 'awaiting_contract' ORDER BY created_at LIMIT 1`, target))
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, false, nil
	}
	if err != nil {
		return Run{}, false, fmt.Errorf("load awaiting run: %w", err)
	}

	return r, true, nil
}

// CreateAdoption records an already-succeeded run of kind that carries migs' files and one adopted
// ledger row per migration: what the target already held, put on the control plane's books.
func (s *Store) CreateAdoption(ctx context.Context, id, target, kind string, migs []engine.Migration, p Provenance) error {
	files := adoptionFiles(migs)
	names := make([]string, 0, len(files))
	bodies := make([]string, 0, len(files))
	for name, body := range files {
		names = append(names, name)
		bodies = append(bodies, body)
	}
	ids := make([]string, 0, len(migs))
	for _, m := range migs {
		ids = append(ids, m.ID())
	}
	if _, err := s.pool.Exec(ctx, `
		WITH r AS (INSERT INTO cp_runs (id, target, state, kind, finished_at, created_by, source)
			VALUES ($1, $2, 'succeeded', $3, now(), $6, $7)),
		f AS (INSERT INTO cp_run_files (run_id, name, body)
			SELECT $1, n, b FROM unnest($4::text[], $5::text[]) AS f (n, b))
		INSERT INTO cp_run_applied (run_id, migration, seq, adopted)
		SELECT $1, m, ord, true FROM unnest($8::text[]) WITH ORDINALITY AS u (m, ord)`,
		id, target, kind, names, bodies, p.CreatedBy, p.Source, ids); err != nil {
		return fmt.Errorf("create %s run: %w", kind, err)
	}

	return nil
}

func adoptionFiles(migs []engine.Migration) map[string]string {
	files := make(map[string]string, 2*len(migs))
	for _, m := range migs {
		maps.Copy(files, pairOf(m.ID(), m.UpSQL, m.DownSQL))
	}

	return files
}

// CreateRevert queues a run that undoes what another run applied; force allows a run that is not the
// newest un-reverted one on its target.
func (s *Store) CreateRevert(ctx context.Context, id string, orig Run, force bool, t Timeouts, p Provenance) error {
	if err := s.checkRevertable(ctx, orig, force); err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO cp_runs (id, target, state, reverts, lock_timeout, statement_timeout, created_by, source)
		SELECT $1, o.target, 'queued', o.id, nullif($3, ''), nullif($4, ''), $5, $6 FROM cp_runs o
		WHERE o.id = $2 AND o.state = $7`,
		id, orig.ID, t.Lock, t.Statement, p.CreatedBy, p.Source, orig.State)
	if err != nil {
		return fmt.Errorf("create revert: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return notRevertable(orig.ID, reasonState, orig.State)
	}

	return nil
}

func notRevertable(id, reason string, args ...any) error {
	return fmt.Errorf("run %q: %w: %s", id, ErrNotRevertable, fmt.Sprintf(reason, args...))
}

var revertableStates = []string{StateSucceeded, StateAwaitingContract, StateFailed, StateNeedsAttention}

// checkRevertable reports why orig cannot be reverted right now, or nil.
func (s *Store) checkRevertable(ctx context.Context, orig Run, force bool) error {
	if slices.Contains(adoptionKinds, orig.Kind) {
		return fmt.Errorf("run %q: %w", orig.ID, ErrBaselineRun)
	}
	if orig.Reverts != "" {
		return notRevertable(orig.ID, reasonIsRevert)
	}
	if !slices.Contains(revertableStates, orig.State) {
		return notRevertable(orig.ID, reasonState, orig.State)
	}
	var busy bool
	var newer *string
	if err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM cp_runs r WHERE r.target = $1 AND r.id <> $2 AND r.state IN ('queued', 'running')),
		       (SELECT r.id::text FROM cp_runs r WHERE r.target = $1 AND r.id <> $2 AND r.reverts IS NULL
			         AND r.state <> 'reverted' AND r.created_at > $3 ORDER BY r.created_at DESC LIMIT 1)`,
		orig.Target, orig.ID, orig.CreatedAt).Scan(&busy, &newer); err != nil {
		return fmt.Errorf("check revertable: %w", err)
	}
	if busy {
		return notRevertable(orig.ID, reasonBusy, orig.Target)
	}
	if newer != nil && !force {
		return notRevertable(orig.ID, reasonNewer, *newer)
	}

	return nil
}

// Newer reports whether a run created after orig still stands on its target.
func (s *Store) Newer(ctx context.Context, orig Run) (bool, error) {
	var newer bool
	if err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM cp_runs r WHERE r.target = $1 AND r.id <> $2 AND r.reverts IS NULL
		                AND r.state <> 'reverted' AND r.created_at > $3)`,
		orig.Target, orig.ID, orig.CreatedAt).Scan(&newer); err != nil {
		return false, fmt.Errorf("check newer runs: %w", err)
	}

	return newer, nil
}

// RevertTarget returns the newest un-reverted run on target, which is what a revert with no run id acts on.
func (s *Store) RevertTarget(ctx context.Context, target string) (Run, error) {
	r, err := scanRun(s.pool.QueryRow(ctx, `
		SELECT `+runColumns+` FROM cp_runs r
		WHERE r.target = $1 AND r.reverts IS NULL AND r.kind = 'migrate'
		  AND r.state IN ('succeeded', 'awaiting_contract', 'failed', 'needs_attention')
		ORDER BY r.created_at DESC LIMIT 1`, target))
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, fmt.Errorf("target %q has no revertable run: %w", target, ErrNotFound)
	}
	if err != nil {
		return Run{}, fmt.Errorf("load revertable run: %w", err)
	}

	return r, nil
}

const runColumns = `id, target, state, coalesce(error, ''), attempts, rollout, phase, coalesce(reverts::text, ''), kind,
	coalesce(lock_timeout, ''), coalesce(statement_timeout, ''), created_at, finished_at, created_by, source, coalesce(plan_id::text, ''),
	retries, not_before, progress, expansions`

func (r *Run) fields() []any {
	return []any{
		&r.ID, &r.Target, &r.State, &r.Error, &r.Attempts, &r.Rollout, &r.Phase, &r.Reverts, &r.Kind,
		&r.Timeouts.Lock, &r.Timeouts.Statement, &r.CreatedAt, &r.FinishedAt, &r.Provenance.CreatedBy, &r.Provenance.Source, &r.PlanID,
		&r.Retries, &r.NotBefore, &r.Progress, &r.Expansions,
	}
}

func scanRun(row pgx.Row) (Run, error) {
	var r Run
	err := row.Scan(r.fields()...)

	return r, err
}

// LastRun returns the most recently created run for target; ok is false when it never had one.
func (s *Store) LastRun(ctx context.Context, target string) (r Run, ok bool, err error) {
	r, err = scanRun(s.pool.QueryRow(ctx,
		`SELECT `+runColumns+` FROM cp_runs WHERE target = $1 ORDER BY created_at DESC LIMIT 1`, target))
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, false, nil
	}
	if err != nil {
		return Run{}, false, fmt.Errorf("load last run: %w", err)
	}

	return r, true, nil
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

// standingRow matches a ledger row the target records: a migration applied to completion that no revert
// undid. The run's own state is not part of it — a run that failed half way leaves what it did apply
// standing, and a held row is on the target's disk but not in its history until the contract phase lands.
const standingRow = `NOT a.held AND a.reverted_by IS NULL`

// versionedMigration matches a ledger migration id that carries a version; repeatables are named R__<name>.
const versionedMigration = `a.migration ~ '^[0-9]{14}_'`

// Applied returns what a target's runs actually applied and no revert undid, whether or not the run
// that applied it went on to succeed: their versions ascending, and the content last recorded under
// each repeatable name.
func (s *Store) Applied(ctx context.Context, target string) (AppliedSet, error) {
	versions, err := s.appliedVersions(ctx, target)
	if err != nil {
		return AppliedSet{}, err
	}
	reps, err := s.appliedRepeatables(ctx, target)
	if err != nil {
		return AppliedSet{}, err
	}

	return AppliedSet{Versions: versions, Repeatables: reps}, nil
}

func (s *Store) appliedVersions(ctx context.Context, target string) ([]int64, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT left(a.migration, 14)::bigint AS version
		FROM cp_run_applied a
		JOIN cp_runs r ON r.id = a.run_id
		WHERE r.target = $1 AND `+standingRow+` AND `+versionedMigration+`
		ORDER BY version`, target)
	if err != nil {
		return nil, fmt.Errorf("list applied versions: %w", err)
	}
	var out []int64
	var v int64
	if _, err := pgx.ForEachRow(rows, []any{&v}, func() error {
		out = append(out, v)

		return nil
	}); err != nil {
		return nil, fmt.Errorf("read applied versions: %w", err)
	}

	return out, nil
}

func (s *Store) appliedRepeatables(ctx context.Context, target string) (map[string]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT ON (a.migration) a.migration, f.body
		FROM cp_run_applied a
		JOIN cp_runs r ON r.id = a.run_id
		JOIN cp_run_files f ON f.run_id = a.run_id AND f.name = a.migration || '.up.sql'
		WHERE r.target = $1 AND `+standingRow+` AND a.migration LIKE 'R\_\_%'
		ORDER BY a.migration, r.created_at DESC`, target)
	if err != nil {
		return nil, fmt.Errorf("list applied repeatables: %w", err)
	}
	out := map[string]string{}
	var name, body string
	if _, err := pgx.ForEachRow(rows, []any{&name, &body}, func() error {
		out[strings.TrimPrefix(name, engine.RepeatablePrefix)] = checksum(body)

		return nil
	}); err != nil {
		return nil, fmt.Errorf("read applied repeatables: %w", err)
	}

	return out, nil
}

func checksum(body string) string {
	h := sha256.Sum256([]byte(body))

	return hex.EncodeToString(h[:])
}

// Claim leases the next queued run, or a running one whose lease expired; one lease per target.
func (s *Store) Claim(ctx context.Context, holder string, ttl time.Duration) (Run, bool, error) {
	run, err := scanRun(s.pool.QueryRow(ctx, `
		WITH candidate AS (
			SELECT r.id FROM cp_runs r
			LEFT JOIN cp_leases l ON l.run_id = r.id
			WHERE r.state IN ('queued', 'running')
			  AND (r.not_before IS NULL OR r.not_before <= now())
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
		UPDATE cp_runs SET state = 'running', attempts = attempts + 1, not_before = NULL, progress = NULL, updated_at = now()
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

// Heartbeat extends the holder's lease; ErrLeaseLost means another holder took it. holder is matched
// whole, so it has to identify the process rather than the machine it runs on (NewHolder).
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

// Finish records a terminal state and releases the lease by run id rather than by holder, because an
// operator parking a run holds none; the executing replica's guard is that it gives the run up the
// moment its own lease is gone. A succeeded revert marks its original reverted
// and withdraws every ledger row of it the revert had nothing left to undo, so the run and its rows say
// the same thing.
func (s *Store) Finish(ctx context.Context, id, state, errText string) error {
	tag, err := s.pool.Exec(ctx, `
		WITH del AS (DELETE FROM cp_leases WHERE run_id = $1),
		orig AS (
			UPDATE cp_runs SET state = 'reverted', updated_at = now()
			WHERE $2 = 'succeeded' AND id = (SELECT reverts FROM cp_runs WHERE id = $1)
			RETURNING id),
		undone AS (
			UPDATE cp_run_applied SET reverted_by = $1
			WHERE run_id IN (SELECT id FROM orig) AND reverted_by IS NULL AND NOT adopted)
		UPDATE cp_runs SET state = $2, error = NULLIF($3, ''), finished_at = now(), progress = NULL, updated_at = now()
		WHERE id = $1`, id, state, errText)
	if err != nil {
		return fmt.Errorf("finish run: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("run %q: %w", id, ErrNotFound)
	}

	return nil
}

// Retry requeues a running run after a transient failure, holding it back for wait and releasing its lease.
func (s *Store) Retry(ctx context.Context, id, errText string, wait time.Duration) error {
	tag, err := s.pool.Exec(ctx, `
		WITH del AS (DELETE FROM cp_leases WHERE run_id = $1)
		UPDATE cp_runs SET state = 'queued', error = $2, not_before = now() + $3, retries = retries + 1,
			progress = NULL, updated_at = now()
		WHERE id = $1 AND state = 'running'`, id, errText, wait)
	if err != nil {
		return fmt.Errorf("retry run: %w", err)
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
		UPDATE cp_runs SET state = 'queued', attempts = 0, error = NULL, finished_at = NULL, not_before = NULL,
			progress = NULL, updated_at = now()
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

// Confirm requeues an awaiting_contract run for its contract phase and returns it.
func (s *Store) Confirm(ctx context.Context, id string) (Run, error) {
	run, err := scanRun(s.pool.QueryRow(ctx, `
		UPDATE cp_runs SET state = 'queued', phase = 'contract', attempts = 0, finished_at = NULL, not_before = NULL,
			progress = NULL, updated_at = now()
		WHERE id = $1 AND state = 'awaiting_contract'
		RETURNING `+runColumns, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, fmt.Errorf("run %q: %w", id, ErrNotAwaitingContract)
	}
	if err != nil {
		return Run{}, fmt.Errorf("confirm rollout: %w", err)
	}

	return run, nil
}
