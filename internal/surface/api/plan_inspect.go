package api

import (
	"context"
	"fmt"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/internal/controlplane"
)

// GetPlan returns one stored plan by id.
func (s *Server) GetPlan(ctx context.Context, req *connect.Request[godwitv1.GetPlanRequest]) (*connect.Response[godwitv1.GetPlanResponse], error) {
	if req.Msg.PlanId == "" {
		return nil, invalid("plan_id is required")
	}
	p, err := s.store.Plan(ctx, req.Msg.PlanId)
	if err != nil {
		return nil, rpcErr(err)
	}
	out := &godwitv1.GetPlanResponse{Plan: planToProto(p)}
	if req.Msg.IncludeFiles {
		files, err := s.store.PlanFiles(ctx, p.ID)
		if err != nil {
			return nil, rpcErr(err)
		}
		out.Files = filesToProto(files)
	}

	return connect.NewResponse(out), nil
}

// ListPlans returns a target's stored plans, newest first.
func (s *Server) ListPlans(ctx context.Context, req *connect.Request[godwitv1.ListPlansRequest]) (*connect.Response[godwitv1.ListPlansResponse], error) {
	if req.Msg.Target == "" {
		return nil, invalid("target is required")
	}
	plans, err := s.store.ListPlans(ctx, req.Msg.Target, int(req.Msg.Limit))
	if err != nil {
		return nil, rpcErr(err)
	}
	out := &godwitv1.ListPlansResponse{Plans: make([]*godwitv1.Plan, 0, len(plans))}
	for _, p := range plans {
		out.Plans = append(out.Plans, planToProto(p))
	}

	return connect.NewResponse(out), nil
}

func planToProto(p controlplane.Plan) *godwitv1.Plan {
	obs := &godwitv1.PlanObservation{
		HistoryHash: p.HistoryHash, SchemaFingerprint: p.SchemaFingerprint, AppliedCount: int32(len(p.Applied)),
		At: timestamppb.New(p.CreatedAt), SearchPath: p.SearchPath,
	}
	for _, a := range p.Applied {
		obs.NewestApplied = max(obs.NewestApplied, a.Version)
	}

	return &godwitv1.Plan{
		Id: p.ID, Target: p.Target, Key: p.Key, Rollout: p.Rollout, State: p.State, Observed: obs, Drift: p.Drift,
		Migrations: migrationsToProto(p.Migrations), Validated: p.Validated, AcknowledgedHazards: p.Acked,
		AllowOutOfOrder: p.AllowOutOfOrder, CreatedBy: p.CreatedBy, Source: p.Source, CreatedAt: timestamppb.New(p.CreatedAt),
		RunId: p.RunID, SupersededBy: p.SupersededBy,
	}
}

func (s *Server) explicitPlan(ctx context.Context, m *godwitv1.CreateRunRequest) error {
	if s.Inspector == nil {
		return errPlanDisabled
	}
	p, err := s.store.Plan(ctx, m.PlanId)
	if err != nil {
		return rpcErr(err)
	}
	if m.Target != "" && m.Target != p.Target {
		return invalid(fmt.Sprintf("plan %s belongs to target %s, not %s", p.ID, p.Target, m.Target))
	}
	if m.Rollout != "" && m.Rollout != p.Rollout {
		return invalid(fmt.Sprintf("plan %s uses rollout %s, not %s", p.ID, p.Rollout, m.Rollout))
	}
	m.Target, m.Rollout = p.Target, p.Rollout
	if len(m.Files) > 0 {
		return nil
	}
	files, err := s.store.PlanFiles(ctx, p.ID)
	if err != nil {
		return rpcErr(err)
	}
	m.Files = filesToProto(files)

	return nil
}
