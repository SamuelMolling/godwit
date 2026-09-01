package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Options tunes execution.
type Options struct {
	LockTimeout      time.Duration
	StatementTimeout time.Duration
}

func (o Options) withDefaults() Options {
	if o.LockTimeout <= 0 {
		o.LockTimeout = 5 * time.Second
	}

	return o
}

// HookPoint marks a crash-injection point; tests use it to kill the session.
type HookPoint string

// Crash-injection points.
const (
	HookBeforeStatement HookPoint = "before_statement"
	HookInsideTx        HookPoint = "inside_tx"
	HookAfterIntent     HookPoint = "after_intent"
	HookAfterExec       HookPoint = "after_exec"
)

// Executor applies plans over one database session.
type Executor struct {
	db    DB
	opts  Options
	hook  func(HookPoint, int)
	newID func() string
}

// Option customizes an Executor.
type Option func(*Executor)

// WithHook installs a crash-injection callback.
func WithHook(fn func(HookPoint, int)) Option {
	return func(e *Executor) { e.hook = fn }
}

// WithIDGenerator overrides run ID generation.
func WithIDGenerator(fn func() string) Option {
	return func(e *Executor) { e.newID = fn }
}

// New builds an Executor over db.
func New(db DB, opts Options, extra ...Option) *Executor {
	e := &Executor{
		db:    db,
		opts:  opts.withDefaults(),
		hook:  func(HookPoint, int) {},
		newID: uuid.NewString,
	}
	for _, fn := range extra {
		fn(e)
	}

	return e
}

// Result reports what one apply did.
type Result struct {
	Version int64
	Skipped bool
	Applied int
}

// Up applies an up plan; already-applied versions are skipped after a checksum check.
func (e *Executor) Up(ctx context.Context, p Plan) (Result, error) {
	if p.Direction != DirectionUp {
		return Result{}, fmt.Errorf("up requires an up plan, got %q", p.Direction)
	}

	return e.apply(ctx, p)
}

// Down reverts an applied migration; unapplied versions are skipped.
func (e *Executor) Down(ctx context.Context, p Plan) (Result, error) {
	if p.Direction != DirectionDown {
		return Result{}, fmt.Errorf("down requires a down plan, got %q", p.Direction)
	}

	return e.apply(ctx, p)
}

func (e *Executor) apply(ctx context.Context, p Plan) (Result, error) {
	res := Result{Version: p.Migration.Version}

	release, err := acquireLock(ctx, e.db)
	if err != nil {
		return res, err
	}
	defer release()

	if err := bootstrap(ctx, e.db); err != nil {
		return res, err
	}

	applied, checksum, err := e.applied(ctx, p.Migration.Version)
	if err != nil {
		return res, err
	}
	if p.Direction == DirectionUp && applied {
		if checksum != p.Migration.Checksum {
			return res, fmt.Errorf("version %d already applied with different content", p.Migration.Version)
		}
		res.Skipped = true

		return res, nil
	}
	if p.Direction == DirectionDown && !applied {
		res.Skipped = true

		return res, nil
	}

	prog, err := openRun(ctx, e.db, p, e.newID())
	if err != nil {
		return res, err
	}

	for i := prog.lastDone + 1; i < len(p.Statements); i++ {
		if err := e.execStatement(ctx, prog, i, p.Statements[i]); err != nil {
			err = fmt.Errorf("statement %d of %d_%s (%s): %w", i, p.Migration.Version, p.Migration.Name, p.Direction, err)
			markFailed(ctx, e.db, prog.runID, err)

			return res, err
		}
		res.Applied++
	}

	return res, e.finalize(ctx, p, prog.runID)
}

func (e *Executor) applied(ctx context.Context, version int64) (bool, string, error) {
	var checksum string
	err := e.db.QueryRow(ctx, `SELECT checksum FROM godwit.migrations WHERE version = $1`, version).Scan(&checksum)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, "", nil
	}
	if err != nil {
		return false, "", fmt.Errorf("check applied version %d: %w", version, err)
	}

	return true, checksum, nil
}

func (e *Executor) execStatement(ctx context.Context, prog runProgress, idx int, st Statement) error {
	e.hook(HookBeforeStatement, idx)
	if st.NoTx {
		return e.execNoTx(ctx, prog, idx, st)
	}

	return e.execTx(ctx, prog, idx, st)
}

func (e *Executor) execTx(ctx context.Context, prog runProgress, idx int, st Statement) error {
	tx, err := e.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for _, set := range e.timeoutSQL("SET LOCAL") {
		if _, err := tx.Exec(ctx, set); err != nil {
			return fmt.Errorf("set timeouts: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, st.SQL); err != nil {
		return fmt.Errorf("exec: %w", err)
	}
	e.hook(HookInsideTx, idx)
	if _, err := tx.Exec(ctx,
		`INSERT INTO godwit.journal (run_id, stmt_idx, state, sql_hash) VALUES ($1, $2, 'done', $3)`,
		prog.runID, idx, st.Hash); err != nil {
		return fmt.Errorf("journal statement %d: %w", idx, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	return nil
}

func (e *Executor) execNoTx(ctx context.Context, prog runProgress, idx int, st Statement) error {
	if prog.pendingIntent == idx {
		done, err := reconcile(ctx, e.db, st)
		if err != nil {
			return err
		}
		if done {
			return recordJournal(ctx, e.db, prog.runID, idx, "done", st.Hash)
		}
	}
	if err := recordJournal(ctx, e.db, prog.runID, idx, "intent", st.Hash); err != nil {
		return err
	}
	e.hook(HookAfterIntent, idx)

	for _, set := range e.timeoutSQL("SET") {
		if _, err := e.db.Exec(ctx, set); err != nil {
			return fmt.Errorf("set timeouts: %w", err)
		}
	}
	if _, err := e.db.Exec(ctx, st.SQL); err != nil {
		return fmt.Errorf("exec: %w", err)
	}
	e.hook(HookAfterExec, idx)
	_, _ = e.db.Exec(ctx, `RESET lock_timeout`)
	_, _ = e.db.Exec(ctx, `RESET statement_timeout`)

	return recordJournal(ctx, e.db, prog.runID, idx, "done", st.Hash)
}

func (e *Executor) timeoutSQL(prefix string) []string {
	return []string{
		fmt.Sprintf("%s lock_timeout = %d", prefix, e.opts.LockTimeout.Milliseconds()),
		fmt.Sprintf("%s statement_timeout = %d", prefix, e.opts.StatementTimeout.Milliseconds()),
	}
}

func (e *Executor) finalize(ctx context.Context, p Plan, runID string) error {
	tx, err := e.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin finalize: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if p.Direction == DirectionUp {
		_, err = tx.Exec(ctx,
			`INSERT INTO godwit.migrations (version, name, checksum) VALUES ($1, $2, $3) ON CONFLICT (version) DO NOTHING`,
			p.Migration.Version, p.Migration.Name, p.Migration.Checksum)
	} else {
		_, err = tx.Exec(ctx, `DELETE FROM godwit.migrations WHERE version = $1`, p.Migration.Version)
	}
	if err != nil {
		return fmt.Errorf("record migration: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE godwit.runs SET state = 'succeeded', finished_at = now() WHERE id = $1`, runID); err != nil {
		return fmt.Errorf("close run: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit finalize: %w", err)
	}

	return nil
}

// StatusRow is the applied state of one loaded migration.
type StatusRow struct {
	Version   int64
	Name      string
	Applied   bool
	Drifted   bool
	AppliedAt time.Time
}

// Status reports the applied state of migs against the target.
func (e *Executor) Status(ctx context.Context, migs []Migration) ([]StatusRow, error) {
	if err := bootstrap(ctx, e.db); err != nil {
		return nil, err
	}
	rows, err := e.db.Query(ctx, `SELECT version, checksum, applied_at FROM godwit.migrations`)
	if err != nil {
		return nil, fmt.Errorf("list applied: %w", err)
	}

	type appliedRow struct {
		checksum  string
		appliedAt time.Time
	}
	appliedBy := map[int64]appliedRow{}
	var version int64
	var row appliedRow
	if _, err := pgx.ForEachRow(rows, []any{&version, &row.checksum, &row.appliedAt}, func() error {
		appliedBy[version] = row

		return nil
	}); err != nil {
		return nil, fmt.Errorf("read applied: %w", err)
	}

	out := make([]StatusRow, 0, len(migs))
	for _, m := range migs {
		row := StatusRow{Version: m.Version, Name: m.Name}
		if a, ok := appliedBy[m.Version]; ok {
			row.Applied = true
			row.Drifted = a.checksum != m.Checksum
			row.AppliedAt = a.appliedAt
		}
		out = append(out, row)
	}

	return out, nil
}
