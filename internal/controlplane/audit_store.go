package controlplane

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Audit actions.
const (
	AuditTargetRegister = "target.register"
	AuditTargetBaseline = "target.baseline"
	AuditRunCreate      = "run.create"
	AuditRunRevert      = "run.revert"
	AuditRunResume      = "run.resume"
	AuditRunPark        = "run.park"
	AuditRunConfirm     = "run.confirm"
	AuditDriftAccept    = "drift.accept"
	AuditPlanCreate     = "plan.create"
	AuditPlanSupersede  = "plan.supersede"
	AuditRunReattach    = "run.reattach"
)

// AuditEntry is one recorded mutation: who did what to which target or run.
type AuditEntry struct {
	ID     int64
	At     time.Time
	Actor  string
	Action string
	RunID  string
	Target string
	Detail string
}

// Audit appends an entry; at and id are assigned by the store.
func (s *Store) Audit(ctx context.Context, e AuditEntry) error {
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO cp_audit (actor, action, run_id, target, detail) VALUES ($1, $2, NULLIF($3, '')::uuid, $4, $5)`,
		e.Actor, e.Action, e.RunID, e.Target, e.Detail); err != nil {
		return fmt.Errorf("write audit: %w", err)
	}

	return nil
}

// ListAudit returns the newest entries first, optionally filtered by target and run; limit caps the page (100 when zero).
func (s *Store) ListAudit(ctx context.Context, target, runID string, limit int) ([]AuditEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, at, actor, action, coalesce(run_id::text, ''), target, detail FROM cp_audit
		WHERE ($1 = '' OR target = $1) AND ($2 = '' OR run_id = NULLIF($2, '')::uuid)
		ORDER BY id DESC LIMIT $3`, target, runID, limit)
	if err != nil {
		return nil, fmt.Errorf("list audit: %w", err)
	}

	var out []AuditEntry
	var e AuditEntry
	if _, err := pgx.ForEachRow(rows, []any{&e.ID, &e.At, &e.Actor, &e.Action, &e.RunID, &e.Target, &e.Detail}, func() error {
		out = append(out, e)

		return nil
	}); err != nil {
		return nil, fmt.Errorf("read audit: %w", err)
	}

	return out, nil
}
