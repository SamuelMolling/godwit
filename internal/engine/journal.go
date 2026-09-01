package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

var bootstrapDDL = []string{
	`CREATE SCHEMA IF NOT EXISTS godwit`,
	`CREATE TABLE IF NOT EXISTS godwit.migrations (
		version    bigint PRIMARY KEY,
		name       text NOT NULL,
		checksum   text NOT NULL,
		applied_at timestamptz NOT NULL DEFAULT now()
	)`,
	`CREATE TABLE IF NOT EXISTS godwit.runs (
		id          uuid PRIMARY KEY,
		version     bigint NOT NULL,
		direction   text NOT NULL CHECK (direction IN ('up', 'down')),
		state       text NOT NULL CHECK (state IN ('running', 'succeeded', 'failed')),
		stmt_count  int NOT NULL,
		error       text,
		started_at  timestamptz NOT NULL DEFAULT now(),
		finished_at timestamptz
	)`,
	`CREATE TABLE IF NOT EXISTS godwit.journal (
		run_id      uuid NOT NULL REFERENCES godwit.runs (id),
		stmt_idx    int NOT NULL,
		state       text NOT NULL CHECK (state IN ('intent', 'done')),
		sql_hash    text NOT NULL,
		recorded_at timestamptz NOT NULL DEFAULT now(),
		PRIMARY KEY (run_id, stmt_idx, state)
	)`,
}

func bootstrap(ctx context.Context, db DB) error {
	for _, ddl := range bootstrapDDL {
		if _, err := db.Exec(ctx, ddl); err != nil {
			return fmt.Errorf("bootstrap godwit schema: %w", err)
		}
	}

	return nil
}

// runProgress is what the journal says about an unfinished run.
type runProgress struct {
	runID         string
	lastDone      int
	pendingIntent int
}

func newProgress(runID string) runProgress {
	return runProgress{runID: runID, lastDone: -1, pendingIntent: -1}
}

// openRun resumes the latest unfinished run for the plan or starts a new one.
func openRun(ctx context.Context, db DB, p Plan, newID string) (runProgress, error) {
	var id string
	err := db.QueryRow(ctx,
		`SELECT id FROM godwit.runs
		 WHERE version = $1 AND direction = $2 AND state IN ('running', 'failed')
		 ORDER BY started_at DESC LIMIT 1`,
		p.Migration.Version, string(p.Direction)).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		if _, err := db.Exec(ctx,
			`INSERT INTO godwit.runs (id, version, direction, state, stmt_count) VALUES ($1, $2, $3, 'running', $4)`,
			newID, p.Migration.Version, string(p.Direction), len(p.Statements)); err != nil {
			return runProgress{}, fmt.Errorf("insert run: %w", err)
		}

		return newProgress(newID), nil
	}
	if err != nil {
		return runProgress{}, fmt.Errorf("find open run: %w", err)
	}

	return loadProgress(ctx, db, p, id)
}

func loadProgress(ctx context.Context, db DB, p Plan, runID string) (runProgress, error) {
	rows, err := db.Query(ctx,
		`SELECT stmt_idx, state, sql_hash FROM godwit.journal WHERE run_id = $1 ORDER BY stmt_idx`, runID)
	if err != nil {
		return runProgress{}, fmt.Errorf("load journal: %w", err)
	}

	prog := newProgress(runID)
	intents := map[int]bool{}
	var idx int
	var state, hash string
	_, err = pgx.ForEachRow(rows, []any{&idx, &state, &hash}, func() error {
		if hash != p.Statements[idx].Hash {
			return fmt.Errorf("statement %d changed since run %s started; refusing to resume", idx, runID)
		}
		if state == "done" {
			if idx > prog.lastDone {
				prog.lastDone = idx
			}
			delete(intents, idx)
		} else {
			intents[idx] = true
		}

		return nil
	})
	if err != nil {
		return runProgress{}, fmt.Errorf("read journal: %w", err)
	}
	for idx := range intents {
		if idx > prog.lastDone {
			prog.pendingIntent = idx
		}
	}
	if _, err := db.Exec(ctx,
		`UPDATE godwit.runs SET state = 'running', error = NULL WHERE id = $1`, runID); err != nil {
		return runProgress{}, fmt.Errorf("reopen run: %w", err)
	}

	return prog, nil
}

func recordJournal(ctx context.Context, db DB, runID string, idx int, state, hash string) error {
	_, err := db.Exec(ctx,
		`INSERT INTO godwit.journal (run_id, stmt_idx, state, sql_hash) VALUES ($1, $2, $3, $4)
		 ON CONFLICT DO NOTHING`,
		runID, idx, state, hash)
	if err != nil {
		return fmt.Errorf("journal %s for statement %d: %w", state, idx, err)
	}

	return nil
}

func markFailed(ctx context.Context, db DB, runID string, cause error) {
	// Best effort: after a crash-like failure the connection may be gone.
	_, _ = db.Exec(ctx,
		`UPDATE godwit.runs SET state = 'failed', error = $2, finished_at = now() WHERE id = $1`,
		runID, cause.Error())
}
