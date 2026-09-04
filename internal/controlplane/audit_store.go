package controlplane

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// MaxPageSize is the ceiling every listing clamps its caller's limit to, so one call cannot
// materialise a whole table into a single response.
const MaxPageSize = 1000

const defaultPageSize = 100

func pageSize(limit int) int {
	if limit <= 0 {
		return defaultPageSize
	}

	return min(limit, MaxPageSize)
}

// Audit actions.
const (
	AuditTargetRegister  = "target.register"
	AuditTargetBaseline  = "target.baseline"
	AuditTargetReconcile = "target.reconcile"
	AuditRunCreate       = "run.create"
	AuditRunRevert       = "run.revert"
	AuditRunResume       = "run.resume"
	AuditRunPark         = "run.park"
	AuditRunConfirm      = "run.confirm"
	AuditDriftAccept     = "drift.accept"
	AuditPlanCreate      = "plan.create"
	AuditPlanSupersede   = "plan.supersede"
	AuditRunReattach     = "run.reattach"
	AuditCheckpoint      = "checkpoint.generate"
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

// ListAudit returns the newest entries first, optionally filtered by target and run; limit caps the page
// (100 when zero, MaxPageSize at most).
func (s *Store) ListAudit(ctx context.Context, target, runID string, limit int) ([]AuditEntry, error) {
	limit = pageSize(limit)
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
