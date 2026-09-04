package engine

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Batch cursor kinds.
const (
	BatchKeyInt  = "int"
	BatchKeyUUID = "uuid"
	BatchKeyText = "text"
)

// BatchSpec turns one statement into a resumable loop: the statement takes the cursor as $1, touches
// at most Size rows and returns the key of every row it touched.
type BatchSpec struct {
	Key      string
	KeyKind  string
	Size     int
	Pause    time.Duration
	Estimate string
}

type batchCursor struct {
	kind string
	num  int64
	uid  [16]byte
	str  string
}

func zeroCursor(kind string) (batchCursor, error) {
	switch kind {
	case BatchKeyInt:
		return batchCursor{kind: kind, num: math.MinInt64}, nil
	case BatchKeyUUID, BatchKeyText:
		return batchCursor{kind: kind}, nil
	default:
		return batchCursor{}, fmt.Errorf("unsupported batch key kind %q", kind)
	}
}

func parseCursor(kind, text string) (batchCursor, error) {
	c, err := zeroCursor(kind)
	if err != nil {
		return c, err
	}
	switch kind {
	case BatchKeyInt:
		n, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return c, fmt.Errorf("parse batch cursor %q: %w", text, err)
		}
		c.num = n
	case BatchKeyUUID:
		u, err := uuid.Parse(text)
		if err != nil {
			return c, fmt.Errorf("parse batch cursor %q: %w", text, err)
		}
		c.uid = u
	default:
		c.str = text
	}

	return c, nil
}

func (c batchCursor) arg() any {
	switch c.kind {
	case BatchKeyInt:
		return c.num
	case BatchKeyUUID:
		return pgtype.UUID{Bytes: c.uid, Valid: true}
	default:
		return c.str
	}
}

func (c batchCursor) text() string {
	switch c.kind {
	case BatchKeyInt:
		return strconv.FormatInt(c.num, 10)
	case BatchKeyUUID:
		return uuid.UUID(c.uid).String()
	default:
		return c.str
	}
}

func (c batchCursor) above(o batchCursor) bool {
	switch c.kind {
	case BatchKeyInt:
		return c.num > o.num
	case BatchKeyUUID:
		return bytes.Compare(c.uid[:], o.uid[:]) > 0
	default:
		return c.str > o.str
	}
}

func scanKey(rows pgx.Rows, kind string) (batchCursor, error) {
	next := batchCursor{kind: kind}
	var err error
	switch kind {
	case BatchKeyInt:
		err = rows.Scan(&next.num)
	case BatchKeyUUID:
		var u pgtype.UUID
		if err = rows.Scan(&u); err == nil {
			next.uid = u.Bytes
		}
	default:
		err = rows.Scan(&next.str)
	}
	if err != nil {
		return next, fmt.Errorf("scan batch key: %w", err)
	}

	return next, nil
}

// scanBatch lifts the cursor to the highest returned key; a key the database orders higher than the
// pick was in this batch too, so picking low repeats work but never skips it.
func scanBatch(rows pgx.Rows, cursor batchCursor) (int, batchCursor, error) {
	defer rows.Close()

	n := 0
	for rows.Next() {
		next, err := scanKey(rows, cursor.kind)
		if err != nil {
			return 0, cursor, err
		}
		if next.above(cursor) {
			cursor = next
		}
		n++
	}
	if err := rows.Err(); err != nil {
		return 0, cursor, fmt.Errorf("read batch: %w", err)
	}

	return n, cursor, nil
}

func (e *Executor) execBatch(ctx context.Context, prog runProgress, idx int, st Statement, ev *StatementEvent) error {
	b := st.Batch
	if b.Size <= 0 {
		return fmt.Errorf("batch size must be positive, got %d", b.Size)
	}
	cursor, err := e.openBatch(ctx, prog, idx, st, ev)
	if err != nil {
		return err
	}

	for {
		n, err := e.runBatch(ctx, prog.runID, idx, st, &cursor)
		ev.RowsDone += int64(n)
		ev.Batches++
		if err != nil {
			return err
		}
		partial := *ev
		partial.Partial = true
		e.observe(partial)
		e.hook(HookAfterBatch, idx)
		if n < b.Size {
			break
		}
		if err := pause(ctx, b.Pause); err != nil {
			return err
		}
	}

	return recordJournal(ctx, e.db, prog.runID, idx, "done", st.Hash)
}

func (e *Executor) openBatch(ctx context.Context, prog runProgress, idx int, st Statement, ev *StatementEvent) (batchCursor, error) {
	b := st.Batch
	cursor, err := zeroCursor(b.KeyKind)
	if err != nil {
		return cursor, err
	}
	if prog.pendingIntent == idx {
		ev.RowsDone, ev.RowsTotal = prog.rowsDone, prog.rowsTotal
		if prog.cursor == nil {
			return cursor, nil
		}

		return parseCursor(b.KeyKind, *prog.cursor)
	}

	total, err := e.estimate(ctx, b)
	if err != nil {
		return cursor, err
	}
	if total != nil {
		ev.RowsTotal = *total
	}
	if err := recordBatchIntent(ctx, e.db, prog.runID, idx, st.Hash, total); err != nil {
		return cursor, err
	}
	e.hook(HookAfterIntent, idx)

	return cursor, nil
}

func (e *Executor) estimate(ctx context.Context, b *BatchSpec) (*int64, error) {
	if b.Estimate == "" {
		return nil, nil
	}
	var n int64
	if err := e.db.QueryRow(ctx, b.Estimate).Scan(&n); err != nil {
		return nil, fmt.Errorf("estimate rows: %w", err)
	}

	return &n, nil
}

func (e *Executor) runBatch(ctx context.Context, runID string, idx int, st Statement, cursor *batchCursor) (int, error) {
	tx, err := e.db.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin batch: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for _, set := range e.timeoutSQL("SET LOCAL") {
		if _, err := tx.Exec(ctx, set); err != nil {
			return 0, fmt.Errorf("set timeouts: %w", err)
		}
	}
	rows, err := tx.Query(ctx, st.SQL, cursor.arg())
	if err != nil {
		return 0, fmt.Errorf("exec: %w", err)
	}
	n, next, err := scanBatch(rows, *cursor)
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE godwit.journal SET cursor = $1, rows_done = rows_done + $2
		 WHERE run_id = $3 AND stmt_idx = $4 AND state = 'intent'`,
		next.text(), n, runID, idx); err != nil {
		return 0, fmt.Errorf("journal batch of statement %d: %w", idx, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit batch: %w", err)
	}
	*cursor = next

	return n, nil
}

func pause(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-ctx.Done():
		return fmt.Errorf("pause between batches: %w", ctx.Err())
	case <-t.C:
		return nil
	}
}
