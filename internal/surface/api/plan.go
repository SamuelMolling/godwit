package api

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/internal/admission"
	"github.com/SamuelMolling/godwit/internal/controlplane"
	"github.com/SamuelMolling/godwit/internal/engine"
)

var errPlanDisabled = connect.NewError(connect.CodeUnimplemented, errors.New("stored plans are not enabled"))

// PlanRun runs CreateRun's admission on the files and returns what the run would do without queueing it;
// with persist the plan is stored together with an observation of the target so a later CreateRun binds to it.
func (s *Server) PlanRun(ctx context.Context, req *connect.Request[godwitv1.PlanRunRequest]) (*connect.Response[godwitv1.PlanRunResponse], error) {
	m := req.Msg
	set, err := s.upSet(m.Target, m.Rollout, m.Files)
	if err != nil {
		return nil, err
	}
	g := s.gate()
	if set, err = g.StopAt(ctx, m.Target, set, m.ToVersion); err != nil {
		return nil, admitErr(err)
	}
	if m.Persist && s.Inspector == nil {
		return nil, errPlanDisabled
	}
	var obs controlplane.Observation
	if m.Persist {
		if err := g.CheckIdle(ctx, m.Target); err != nil {
			return nil, admitErr(err)
		}
		if obs, err = s.Inspector.Observe(ctx, m.Target); err != nil {
			return nil, rpcErr(err)
		}
		if err := g.CheckReconciled(ctx, m.Target, obs); err != nil {
			return nil, admitErr(err)
		}
	}
	adm, err := g.Admit(ctx, m.Target, set.Plans, m.AcknowledgeHazards, m.SkipValidation, m.AllowOutOfOrder, obs.SearchPath)
	if err != nil {
		return nil, admitErr(err)
	}
	if err := admission.CheckRollout(set.Rollout, adm.Expanded(set)); err != nil {
		return nil, admitErr(err)
	}
	out := &godwitv1.PlanRunResponse{
		Target: m.Target, Rollout: set.Rollout, Validated: adm.Validated,
		Migrations: migrationsToProto(adm.Migrations(set)),
	}
	if !m.Persist {
		s.Log.Info("run planned", "target", m.Target, "rollout", set.Rollout, "files", len(set.Files),
			"acked", m.AcknowledgeHazards, "validated", adm.Validated, "allow_out_of_order", m.AllowOutOfOrder,
			"to_version", m.ToVersion, "withheld", len(set.Withheld))

		return connect.NewResponse(out), nil
	}
	p, pending, err := g.Save(ctx, planRequest(m), set, adm, obs)
	if err != nil {
		return nil, admitErr(err)
	}
	out.Migrations = migrationsToProto(p.Migrations)
	s.audit(ctx, controlplane.AuditPlanCreate, "", m.Target,
		fmt.Sprintf("plan=%s key=%s rollout=%s pending=%d acked=%s source=%s", p.ID, p.Key, set.Rollout, pending,
			strings.Join(m.AcknowledgeHazards, ","), m.Source))
	out.PlanId, out.PlanKey, out.Drift, out.Observed = p.ID, p.Key, p.Drift, observationToProto(obs)

	return connect.NewResponse(out), nil
}

func planRequest(m *godwitv1.PlanRunRequest) admission.Request {
	return admission.Request{
		Target: m.Target, Source: m.Source, Acked: m.AcknowledgeHazards,
		SkipValidation: m.SkipValidation, AllowOutOfOrder: m.AllowOutOfOrder,
	}
}

func migrationsToProto(migs []controlplane.PlanMigration) []*godwitv1.PlannedMigration {
	out := make([]*godwitv1.PlannedMigration, 0, len(migs))
	for _, m := range migs {
		pm := &godwitv1.PlannedMigration{
			Version: m.Version, Name: m.Name, Repeatable: m.Repeatable, Checksum: m.Checksum, Applied: m.Applied,
			Phase: m.Phase, AlreadyApplied: m.AlreadyApplied, Effect: m.Effect, Note: m.Note,
			Directives: m.Directives, Expanded: m.Expanded, Notes: m.Notes, Withheld: m.Withheld,
			Checkpoint: m.Checkpoint, CollapsesThrough: m.Through, Skipped: m.Skipped,
		}
		pm.Statements = statementsToProto(m.Statements)
		pm.Changes = changesToProto(m.Changes)
		out = append(out, pm)
	}

	return out
}

func statementsToProto(sts []controlplane.PlanStatement) []*godwitv1.PlannedStatement {
	out := make([]*godwitv1.PlannedStatement, 0, len(sts))
	for _, st := range sts {
		ps := &godwitv1.PlannedStatement{Sql: st.SQL, NoTx: st.NoTx, Phase: st.Phase}
		if st.Batch != nil {
			ps.Batch = &godwitv1.PlannedBatch{Key: st.Batch.Key, Kind: st.Batch.Kind, Size: int32(st.Batch.Size), Pause: st.Batch.Pause}
		}
		if st.Assert != nil {
			ps.Assert = &godwitv1.PlannedAssert{Op: st.Assert.Op, Kind: st.Assert.Kind, Value: st.Assert.Value}
		}
		for _, h := range st.Hazards {
			ps.Hazards = append(ps.Hazards, &godwitv1.PlannedHazard{
				Code: h.Code, Detail: h.Detail, Recipe: h.Recipe, Object: h.Object, Attribute: h.Attribute,
			})
		}
		out = append(out, ps)
	}

	return out
}

func changesToProto(changes []engine.ObjectChange) []*godwitv1.SchemaChange {
	out := make([]*godwitv1.SchemaChange, 0, len(changes))
	for _, c := range changes {
		sc := &godwitv1.SchemaChange{
			Op: c.Op, Kind: c.Kind, Schema: c.Schema, Name: c.Name, Unchanged: int32(c.Unchanged),
		}
		for _, a := range c.Attrs {
			sc.Attributes = append(sc.Attributes, &godwitv1.SchemaAttribute{Op: a.Op, Name: a.Name, Old: a.Old, New: a.New})
		}
		out = append(out, sc)
	}

	return out
}

func observationToProto(obs controlplane.Observation) *godwitv1.PlanObservation {
	out := &godwitv1.PlanObservation{
		HistoryHash: obs.HistoryHash(), SchemaFingerprint: obs.Fingerprint, AppliedCount: int32(len(obs.Applied)),
		At: timestamppb.New(obs.At), SearchPath: obs.SearchPath, IgnoredTables: engine.AdoptedLines(obs.Ignored),
	}
	for _, a := range obs.Applied {
		out.NewestApplied = max(out.NewestApplied, a.Version)
	}

	return out
}

func planDetail(reason error) proto.Message {
	var stale *controlplane.PlanStale
	if errors.As(reason, &stale) {
		out := &godwitv1.PlanStale{PlanId: stale.Plan.ID, Reason: stale.Reason, SchemaDiff: strings.Join(stale.Diff.Schema, "\n"), Hint: stale.Hint}
		for _, c := range stale.Diff.Added {
			out.HistoryAdded = append(out.HistoryAdded, c.String())
		}
		for _, c := range stale.Diff.Removed {
			out.HistoryRemoved = append(out.HistoryRemoved, c.String())
		}

		return out
	}
	var required *controlplane.PlanRequired
	_ = errors.As(reason, &required)
	out := &godwitv1.PlanRequired{Target: required.Target, Key: required.Key, FilesDiff: required.FilesDiff()}
	for _, p := range required.Nearest {
		out.NearestPlanIds = append(out.NearestPlanIds, p.ID)
	}

	return out
}
