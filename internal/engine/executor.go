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
	// LockWait bounds the wait for godwit's own advisory lock on the target; zero takes DefaultLockWait.
	LockWait time.Duration
}

// DefaultLockWait bounds how long a run waits for another session's advisory lock on the target: long
// enough to ride out a peer executor's statement, short enough to hand the run slot back and let the
// scheduler retry with backoff rather than park a replica on a lock nobody is going to release.
const DefaultLockWait = 30 * time.Second

func (o Options) withDefaults() Options {
	if o.LockTimeout <= 0 {
		o.LockTimeout = 5 * time.Second
	}
	if o.LockWait <= 0 {
		o.LockWait = DefaultLockWait
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
	HookAfterBatch      HookPoint = "after_batch"
)

// StatementEvent reports one executed statement.
type StatementEvent struct {
	Migration string
	Index     int
	Statement Statement
	Duration  time.Duration
	Err       error
	RowsDone  int64
	RowsTotal int64
	Batches   int
	// Partial marks a report from inside a still-running statement: counts only, no duration and no outcome.
	Partial bool
}

// Executor applies plans over one database session.
type Executor struct {
	db          DB
	opts        Options
	hook        func(HookPoint, int)
	observe     func(StatementEvent)
	newID       func() string
	assertProbe bool
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

// WithAssertProbe runs assertions without enforcing them, for the scratch database that mirrors the
// target's schema but holds none of its rows.
func WithAssertProbe() Option {
	return func(e *Executor) { e.assertProbe = true }
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

// Result reports what one apply did; Held marks a plan stopped at its contract boundary.
type Result struct {
	Migration string
	Skipped   bool
	Held      bool
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
	release, err := acquireLock(ctx, e.db, e.opts.LockWait)
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
	var held string
	if p.Direction == DirectionDown && !recorded {
		if held, err = heldRun(ctx, e.db, p.Migration); err != nil {
			return res, err
		}
		if held == "" {
			res.Skipped = true

			return res, nil
		}
	}
	if p.MarkOnly {
		return res, e.mark(ctx, p)
	}
	// After the skips: a migration that is recorded, or only being recorded, never runs its body.
	if len(p.Statements) == 0 && awaitsExpansion(p.Migration, p.Direction) {
		return res, fmt.Errorf("%s (%s): its godwit directives were never expanded", res.Migration, p.Direction)
	}

	prog, err := openRun(ctx, e.db, p, e.newID())
	if err != nil {
		return res, err
	}

	last := len(p.Statements)
	if p.Held() {
		last = p.HoldFrom
	}
	for i := range last {
		// An assertion is re-evaluated whenever the executor walks past it: a condition that held before
		// the crash, or before the confirm, is not a condition that holds now.
		if i <= prog.lastDone && p.Statements[i].Assert == nil {
			continue
		}
		if err := e.execStatement(ctx, prog, res.Migration, i, p.Statements[i]); err != nil {
			err = fmt.Errorf("statement %d of %s (%s): %w", i, res.Migration, p.Direction, err)
			markFailed(ctx, e.db, prog.runID, err)

			return res, err
		}
		res.Applied++
	}
	if p.Held() {
		res.Held = true

		return res, nil
	}

	return res, e.finalize(ctx, p, prog.runID, held)
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

	return e.finalize(ctx, p, runID, "")
}

// heldRun is the id of an unfinished up run for m: the contract phase never ran, so the migration has
// statements to undo even though it has no history row yet.
func heldRun(ctx context.Context, db DB, m Migration) (string, error) {
	k := keyOf(m)
	var id string
	err := db.QueryRow(ctx,
		`SELECT id FROM godwit.runs
		 WHERE direction = 'up' AND state = 'running'
		   AND version IS NOT DISTINCT FROM $1 AND repeatable IS NOT DISTINCT FROM $2
		   AND ($1 IS NOT NULL OR checksum = $3)
		 ORDER BY started_at DESC LIMIT 1`,
		k.version, k.repeatable, k.checksum).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("find held run for %s: %w", m.ID(), err)
	}

	return id, nil
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
	ev := StatementEvent{Migration: migration, Index: idx, Statement: st}
	var err error
	switch {
	case st.Assert != nil:
		err = e.execAssert(ctx, prog, idx, st)
	case st.Batch != nil:
		err = e.execBatch(ctx, prog, idx, st, &ev)
	case st.NoTx:
		err = e.execNoTx(ctx, prog, idx, st)
	default:
		err = e.execTx(ctx, prog, idx, st)
	}
	ev.Duration, ev.Err = time.Since(start), err
	e.observe(ev)

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

func (e *Executor) finalize(ctx context.Context, p Plan, runID, held string) error {
	tx, err := e.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin finalize: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := record(ctx, tx, p); err != nil {
		return err
	}
	if err := discard(ctx, tx, held); err != nil {
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

// discard drops the journal of an up run this down just undid, so a later apply starts from scratch
// instead of resuming past statements that no longer took effect.
func discard(ctx context.Context, tx DB, runID string) error {
	if runID == "" {
		return nil
	}
	if _, err := tx.Exec(ctx, `DELETE FROM godwit.journal WHERE run_id = $1`, runID); err != nil {
		return fmt.Errorf("discard held journal: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM godwit.runs WHERE id = $1`, runID); err != nil {
		return fmt.Errorf("discard held run: %w", err)
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
