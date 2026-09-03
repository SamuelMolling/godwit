package ui

import (
	"context"
	"errors"
	"net/http"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/gen/godwit/v1/godwitv1connect"
	"github.com/SamuelMolling/godwit/internal/controlplane"
	"github.com/SamuelMolling/godwit/internal/engine"
)

type migRow struct {
	ID         string
	Repeatable bool
	Checksum   string
	At         *timestamppb.Timestamp
	Mismatch   bool
	Plan       string
	Statements int
}

func (h *Handler) targets(w http.ResponseWriter, r *http.Request) {
	p, err := h.frame(r.Context(), r, "targets")
	if err != nil {
		h.fail(w, p, err)

		return
	}
	h.render(w, http.StatusOK, "targets.html", p)
}

func (h *Handler) target(w http.ResponseWriter, r *http.Request) {
	ctx, name := r.Context(), r.PathValue("name")
	p, err := h.frame(ctx, r, "targets")
	if err != nil {
		h.fail(w, p, err)

		return
	}
	p.Target = name
	for _, s := range p.Summaries {
		if s.Name == name {
			p.Summary = s
		}
	}
	if p.Summary == nil {
		h.fail(w, p, connect.NewError(connect.CodeNotFound, errors.New("target "+name+" is not registered")))

		return
	}
	if err := h.detail(ctx, &p, name); err != nil {
		h.fail(w, p, err)

		return
	}
	p.Locked = !p.Can["check"]
	h.render(w, http.StatusOK, "target.html", p)
}

func (h *Handler) detail(ctx context.Context, p *page, name string) error {
	status, err := call(ctx, godwitv1connect.GodwitServiceGetTargetStatusProcedure,
		&godwitv1.GetTargetStatusRequest{Target: name}, h.svc.GetTargetStatus)
	if err != nil {
		return err
	}
	plans, err := call(ctx, godwitv1connect.GodwitServiceListPlansProcedure,
		&godwitv1.ListPlansRequest{Target: name}, h.svc.ListPlans)
	if err != nil {
		return err
	}
	events, err := call(ctx, godwitv1connect.GodwitServiceListDriftEventsProcedure,
		&godwitv1.ListDriftEventsRequest{Target: name}, h.svc.ListDriftEvents)
	if err != nil {
		return err
	}
	p.Status, p.Applied = status, appliedRows(status.Applied)
	p.Ready = readyPlans(plans.Plans)
	p.Pending = pendingRows(p.Ready)
	p.Events = events.Events
	for _, e := range events.Events {
		if e.ResolvedAt == nil && p.Open == nil {
			p.Open = e
		}
	}

	return nil
}

func appliedRows(applied []*godwitv1.AppliedMigration) []migRow {
	out := make([]migRow, 0, len(applied))
	for _, a := range applied {
		out = append(out, migRow{
			ID: engine.MigrationID(a.Version, a.Name, a.Repeatable), Repeatable: a.Repeatable,
			Checksum: a.Checksum, At: a.AppliedAt, Mismatch: a.ChecksumMismatch,
		})
	}

	return out
}

func readyPlans(plans []*godwitv1.Plan) []*godwitv1.Plan {
	var out []*godwitv1.Plan
	for _, p := range plans {
		if p.State == controlplane.PlanReady {
			out = append(out, p)
		}
	}

	return out
}

// pendingRows takes the pending set from the newest ready plan: the service holds no migration directory of its own.
func pendingRows(ready []*godwitv1.Plan) []migRow {
	if len(ready) == 0 {
		return nil
	}
	plan := ready[0]
	var out []migRow
	for _, m := range plan.Migrations {
		if m.Applied {
			continue
		}
		out = append(out, migRow{
			ID: engine.MigrationID(m.Version, m.Name, m.Repeatable), Repeatable: m.Repeatable,
			Plan: plan.Id, Statements: len(m.Statements),
		})
	}

	return out
}
