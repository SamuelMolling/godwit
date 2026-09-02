package api

import (
	"context"
	"slices"

	"connectrpc.com/connect"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/internal/controlplane"
	"github.com/SamuelMolling/godwit/internal/engine"
)

// PlanRun runs CreateRun's admission on the files and returns what the run would do without queueing it.
func (s *Server) PlanRun(ctx context.Context, req *connect.Request[godwitv1.PlanRunRequest]) (*connect.Response[godwitv1.PlanRunResponse], error) {
	m := req.Msg
	spec, err := upSpec(m.Target, m.Rollout, m.Files)
	if err != nil {
		return nil, err
	}
	adm, err := s.admit(ctx, m.Target, spec.plans, m.AcknowledgeHazards, m.SkipValidation, m.AllowOutOfOrder)
	if err != nil {
		return nil, err
	}
	s.Log.Info("run planned", "target", m.Target, "rollout", spec.rollout, "files", len(spec.files),
		"acked", m.AcknowledgeHazards, "validated", adm.validated, "allow_out_of_order", m.AllowOutOfOrder)

	return connect.NewResponse(planToProto(m.Target, spec.rollout, spec.plans, adm)), nil
}

func planToProto(target, rollout string, plans []engine.Plan, adm admission) *godwitv1.PlanRunResponse {
	expand, _ := controlplane.Policies()[rollout].Split(plans)
	out := &godwitv1.PlanRunResponse{Target: target, Rollout: rollout, Validated: adm.validated}
	for i, p := range plans {
		phase := controlplane.PhaseContract
		if i < len(expand) {
			phase = controlplane.PhaseExpand
		}
		pm := &godwitv1.PlannedMigration{
			Version: p.Migration.Version, Name: p.Migration.Name, Checksum: p.Migration.Checksum,
			Applied: slices.Contains(adm.applied, p.Migration.Version), Phase: phase,
		}
		for _, st := range p.Statements {
			ps := &godwitv1.PlannedStatement{Sql: st.SQL, NoTx: st.NoTx}
			for _, h := range st.Hazards {
				ps.Hazards = append(ps.Hazards, &godwitv1.PlannedHazard{Code: h.Code, Detail: h.Detail})
			}
			pm.Statements = append(pm.Statements, ps)
		}
		out.Migrations = append(out.Migrations, pm)
	}

	return out
}
