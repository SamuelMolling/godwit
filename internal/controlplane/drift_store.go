package controlplane

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Snapshot is the expected schema state recorded for a target.
type Snapshot struct {
	Target      string
	Fingerprint string
	Definition  string
	RunID       string
	TakenAt     time.Time
}

// DriftEvent is one detected divergence between expected and live schema.
type DriftEvent struct {
	ID         int64
	Target     string
	Diff       string
	DetectedAt time.Time
	ResolvedAt *time.Time
}

// SaveSnapshot upserts the expected schema for a target and resolves open drift.
func (s *Store) SaveSnapshot(ctx context.Context, target, fingerprint, definition, runID string) error {
	if _, err := s.pool.Exec(ctx, `
		WITH up AS (
			INSERT INTO cp_snapshots (target, fingerprint, definition, run_id, taken_at)
			VALUES ($1, $2, $3, NULLIF($4, '')::uuid, now())
			ON CONFLICT (target) DO UPDATE
			SET fingerprint = EXCLUDED.fingerprint, definition = EXCLUDED.definition,
			    run_id = EXCLUDED.run_id, taken_at = now()
		)
		UPDATE cp_drift_events SET resolved_at = now()
		WHERE target = $1 AND resolved_at IS NULL`,
		target, fingerprint, definition, runID); err != nil {
		return fmt.Errorf("save snapshot: %w", err)
	}

	return nil
}

// SnapshotFor returns the expected schema recorded for a target.
func (s *Store) SnapshotFor(ctx context.Context, target string) (Snapshot, error) {
	var snap Snapshot
	err := s.pool.QueryRow(ctx,
		`SELECT target, fingerprint, definition, coalesce(run_id::text, ''), taken_at FROM cp_snapshots WHERE target = $1`,
		target).Scan(&snap.Target, &snap.Fingerprint, &snap.Definition, &snap.RunID, &snap.TakenAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Snapshot{}, fmt.Errorf("snapshot for %q: %w", target, ErrNotFound)
	}
	if err != nil {
		return Snapshot{}, fmt.Errorf("load snapshot: %w", err)
	}

	return snap, nil
}

// OpenDrift reports whether target has a drift event nobody has resolved or re-baselined.
func (s *Store) OpenDrift(ctx context.Context, target string) (bool, error) {
	var open bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM cp_drift_events WHERE target = $1 AND resolved_at IS NULL)`,
		target).Scan(&open); err != nil {
		return false, fmt.Errorf("check open drift: %w", err)
	}

	return open, nil
}

// SnapshotTargets lists targets that have an expected schema recorded.
func (s *Store) SnapshotTargets(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT target FROM cp_snapshots ORDER BY target`)
	if err != nil {
		return nil, fmt.Errorf("list snapshot targets: %w", err)
	}
	var out []string
	var t string
	if _, err := pgx.ForEachRow(rows, []any{&t}, func() error {
		out = append(out, t)

		return nil
	}); err != nil {
		return nil, fmt.Errorf("read snapshot targets: %w", err)
	}

	return out, nil
}

// RecordDrift opens a drift event while baseline is still the target's fingerprint and no identical one is open; reports whether it did.
func (s *Store) RecordDrift(ctx context.Context, target, baseline, diff string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO cp_drift_events (target, diff)
		SELECT target, $3 FROM (
			SELECT target FROM cp_snapshots WHERE target = $1 AND fingerprint = $2 FOR UPDATE) baseline
		ON CONFLICT (target, md5(diff)) WHERE resolved_at IS NULL DO NOTHING`,
		target, baseline, diff)
	if err != nil {
		return false, fmt.Errorf("record drift: %w", err)
	}

	return tag.RowsAffected() > 0, nil
}

// ResolveDrift closes every open drift event for a target and reports whether any was open.
func (s *Store) ResolveDrift(ctx context.Context, target string) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE cp_drift_events SET resolved_at = now() WHERE target = $1 AND resolved_at IS NULL`,
		target)
	if err != nil {
		return false, fmt.Errorf("resolve drift: %w", err)
	}

	return tag.RowsAffected() > 0, nil
}

// HistoryRun is one run's contribution to a target's history: the migrations it applied that still
// stand, in the order it applied them.
type HistoryRun struct {
	Migrations []HistoryMigration
}

// HistoryMigration is one standing ledger row: the pair the run carried for it and the expansion it froze.
type HistoryMigration struct {
	ID      string
	UpSQL   string
	DownSQL string
	// Expansion is what the run ran in place of the migration's directives, nil when it had none.
	Expansion *Expansion
}

// History returns what every run of a target applied and no revert undid, oldest first. It is the same
// row set Applied is derived from, so the replay and the applied set can never disagree; in particular a
// held migration is in neither, which is why a replayed plan never has to reproduce half of one.
func (s *Store) History(ctx context.Context, target string) ([]HistoryRun, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT a.run_id, a.migration, u.body, d.body, a.expansion
		FROM cp_run_applied a
		JOIN cp_runs r ON r.id = a.run_id
		JOIN cp_run_files u ON u.run_id = a.run_id AND u.name = a.migration || '.up.sql'
		JOIN cp_run_files d ON d.run_id = a.run_id AND d.name = a.migration || '.down.sql'
		WHERE r.target = $1 AND `+standingRow+`
		ORDER BY r.created_at, a.run_id, a.seq`, target)
	if err != nil {
		return nil, fmt.Errorf("list history: %w", err)
	}

	var out []HistoryRun
	var lastRun, runID string
	var m HistoryMigration
	if _, err := pgx.ForEachRow(rows, []any{&runID, &m.ID, &m.UpSQL, &m.DownSQL, &m.Expansion}, func() error {
		if runID != lastRun {
			out = append(out, HistoryRun{})
			lastRun = runID
		}
		last := &out[len(out)-1]
		last.Migrations = append(last.Migrations, m)

		return nil
	}); err != nil {
		return nil, fmt.Errorf("read history: %w", err)
	}

	return out, nil
}

// ListDriftEvents returns recent drift events, optionally filtered by target.
func (s *Store) ListDriftEvents(ctx context.Context, target string) ([]DriftEvent, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, target, diff, detected_at, resolved_at FROM cp_drift_events
		WHERE $1 = '' OR target = $1
		ORDER BY detected_at DESC LIMIT 100`, target)
	if err != nil {
		return nil, fmt.Errorf("list drift events: %w", err)
	}
	var out []DriftEvent
	var e DriftEvent
	if _, err := pgx.ForEachRow(rows,
		[]any{&e.ID, &e.Target, &e.Diff, &e.DetectedAt, &e.ResolvedAt}, func() error {
			out = append(out, e)

			return nil
		}); err != nil {
		return nil, fmt.Errorf("read drift events: %w", err)
	}

	return out, nil
}
