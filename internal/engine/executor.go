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

// HookPoint marks a crash-injection point.
type HookPoint string

// Crash-injection points.
const (
	HookBeforeStatement HookPoint = "before_statement"
	HookInsideTx        HookPoint = "inside_tx"
	HookAfterIntent     HookPoint = "after_intent"
	HookAfterExec       HookPoint = "after_exec"
)

// StatementEvent reports one executed statement.
type StatementEvent struct {
	Migration string
	Index     int
	Statement Statement
	Duration  time.Duration
	Err       error
}

// Executor applies plans over one database session.
type Executor struct {
	db      DB
	opts    Options
	hook    func(HookPoint, int)
	observe func(StatementEvent)
	newID   func() string
}

// Option customizes an Executor.
type Option func(*Executor)

// WithHook installs a crash-injection callback.
func WithHook(fn func(HookPoint, int)) Option {
	return func(e *Executor) { e.hook = fn }
}

// WithObserver installs a callback that sees every executed statement.
func WithObserver(fn func(StatementEvent)) Option {
	return func(e *Executor) { e.observe = fn }
}

// WithIDGenerator overrides run ID generation.
func WithIDGenerator(fn func() string) Option {
	return func(e *Executor) { e.newID = fn }
}

// New builds an Executor over db.
func New(db DB, opts Options, extra ...Option) *Executor {
	e := &Executor{
		db:      db,
		opts:    opts.withDefaults(),
		hook:    func(HookPoint, int) {},
		observe: func(StatementEvent) {},
		newID:   uuid.NewString,
	}
	for _, fn := range extra {
		fn(e)
	}

	return e
}

// Result reports what one apply did.
type Result struct {
	Migration string
	Skipped   bool
	Applied   int
}

// Up applies an up plan; applied versions are checksum-checked and skipped.
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
	res := Result{Migration: p.Migration.ID()}

	release, err := acquireLock(ctx, e.db)
	if err != nil {
		return res, err
	}
	defer release()

	if err := bootstrap(ctx, e.db); err != nil {
		return res, err
	}

	recorded, checksum, err := e.recorded(ctx, p.Migration)
	if err != nil {
		return res, err
	}
	if p.Direction == DirectionUp && recorded {
		if checksum == p.Migration.Checksum {
			res.Skipped = true

			return res, nil
		}
		if !p.Migration.Repeatable {
			return res, fmt.Errorf("version %d already applied with different content", p.Migration.Version)
		}
	}
	if p.Direction == DirectionDown && !recorded {
		res.Skipped = true

		return res, nil
	}
	if p.MarkOnly {
		return res, e.mark(ctx, p)
	}

	prog, err := openRun(ctx, e.db, p, e.newID())
	if err != nil {
		return res, err
	}

	for i := prog.lastDone + 1; i < len(p.Statements); i++ {
		if err := e.execStatement(ctx, prog, res.Migration, i, p.Statements[i]); err != nil {
			err = fmt.Errorf("statement %d of %s (%s): %w", i, res.Migration, p.Direction, err)
			markFailed(ctx, e.db, prog.runID, err)

			return res, err
		}
		res.Applied++
	}

	return res, e.finalize(ctx, p, prog.runID)
}

func (e *Executor) mark(ctx context.Context, p Plan) error {
	if p.Direction != DirectionUp {
		return fmt.Errorf("mark requires an up plan, got %q", p.Direction)
	}
	invalid, err := InvalidIndexes(ctx, e.db)
	if err != nil {
		return err
	}
	if len(invalid) > 0 {
		return fmt.Errorf("index %s exists but is INVALID; drop it and let the migration build it", invalid[0])
	}
	runID := e.newID()
	k := keyOf(p.Migration)
	if _, err := e.db.Exec(ctx,
		`INSERT INTO godwit.runs (id, version, repeatable, checksum, direction, state, stmt_count)
		 VALUES ($1, $2, $3, $4, 'up', 'running', 0)`,
		runID, k.version, k.repeatable, k.checksum); err != nil {
		return fmt.Errorf("insert run: %w", err)
	}

	return e.finalize(ctx, p, runID)
}

// recorded reports what the target holds for a migration: a version row, or a repeatable row keyed by name.
func (e *Executor) recorded(ctx context.Context, m Migration) (bool, string, error) {
	query, arg := `SELECT checksum FROM godwit.migrations WHERE version = $1`, any(m.Version)
	if m.Repeatable {
		query, arg = `SELECT checksum FROM godwit.repeatables WHERE name = $1`, any(m.Name)
	}
	var checksum string
	err := e.db.QueryRow(ctx, query, arg).Scan(&checksum)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, "", nil
	}
	if err != nil {
		return false, "", fmt.Errorf("check applied %s: %w", m.ID(), err)
	}

	return true, checksum, nil
}

func (e *Executor) execStatement(ctx context.Context, prog runProgress, migration string, idx int, st Statement) error {
	e.hook(HookBeforeStatement, idx)
	start := time.Now()
	var err error
	if st.NoTx {
		err = e.execNoTx(ctx, prog, idx, st)
	} else {
		err = e.execTx(ctx, prog, idx, st)
	}
	e.observe(StatementEvent{Migration: migration, Index: idx, Statement: st, Duration: time.Since(start), Err: err})

	return err
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

	if err := record(ctx, tx, p); err != nil {
		return err
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

func record(ctx context.Context, tx DB, p Plan) error {
	m := p.Migration
	var err error
	switch {
	case p.Direction == DirectionUp && m.Repeatable:
		_, err = tx.Exec(ctx,
			`INSERT INTO godwit.repeatables (name, checksum) VALUES ($1, $2)
			 ON CONFLICT (name) DO UPDATE SET checksum = EXCLUDED.checksum, applied_at = now()`,
			m.Name, m.Checksum)
	case p.Direction == DirectionUp:
		_, err = tx.Exec(ctx,
			`INSERT INTO godwit.migrations (version, name, checksum) VALUES ($1, $2, $3) ON CONFLICT (version) DO NOTHING`,
			m.Version, m.Name, m.Checksum)
	case m.Repeatable:
		_, err = tx.Exec(ctx, `DELETE FROM godwit.repeatables WHERE name = $1`, m.Name)
	default:
		_, err = tx.Exec(ctx, `DELETE FROM godwit.migrations WHERE version = $1`, m.Version)
	}
	if err != nil {
		return fmt.Errorf("record migration: %w", err)
	}

	return nil
}

// StatusRow is the applied state of one loaded migration.
type StatusRow struct {
	Migration Migration
	Applied   bool
	Drifted   bool
	AppliedAt time.Time
}

// Status reports the applied state of migs against the target; a repeatable whose content changed reads as pending.
func (e *Executor) Status(ctx context.Context, migs []Migration) ([]StatusRow, error) {
	if err := bootstrap(ctx, e.db); err != nil {
		return nil, err
	}
	applied, err := readApplied(ctx, e.db)
	if err != nil {
		return nil, err
	}
	reps, err := readRepeatables(ctx, e.db)
	if err != nil {
		return nil, err
	}
	byVersion := make(map[int64]Applied, len(applied))
	for _, a := range applied {
		byVersion[a.Version] = a
	}
	byName := make(map[string]Repeatable, len(reps))
	for _, r := range reps {
		byName[r.Name] = r
	}

	out := make([]StatusRow, 0, len(migs))
	for _, m := range migs {
		row := StatusRow{Migration: m}
		switch r, ok := byName[m.Name]; {
		case m.Repeatable && ok:
			row.Applied, row.AppliedAt = r.Checksum == m.Checksum, r.AppliedAt
		case m.Repeatable:
		default:
			if a, ok := byVersion[m.Version]; ok {
				row.Applied, row.Drifted, row.AppliedAt = true, a.Checksum != m.Checksum, a.AppliedAt
			}
		}
		out = append(out, row)
	}

	return out, nil
}
