package api

import (
	"context"
	"errors"
	"strings"

	"connectrpc.com/connect"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/internal/controlplane"
	"github.com/SamuelMolling/godwit/internal/engine"
)

var errDiffDisabled = connect.NewError(connect.CodeUnimplemented, errors.New("schema diff is not enabled"))

// Diff generates the migration between the target's live schema and the desired DDL, in both directions.
func (s *Server) Diff(ctx context.Context, req *connect.Request[godwitv1.DiffRequest]) (*connect.Response[godwitv1.DiffResponse], error) {
	m := req.Msg
	if m.Target == "" {
		return nil, invalid("target is required")
	}
	if strings.TrimSpace(m.Schema) == "" {
		return nil, invalid("schema is required")
	}
	if s.Differ == nil {
		return nil, errDiffDisabled
	}
	d, err := s.Differ.Diff(ctx, m.Target, m.Schema)
	if err != nil {
		if errors.Is(err, controlplane.ErrDesiredSchema) {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}

		return nil, rpcErr(err)
	}
	out := &godwitv1.DiffResponse{
		Target: m.Target, UpSql: d.UpSQL, DownSql: d.DownSQL, Observed: observationToProto(d.Observed), Drift: strings.Join(d.Drift, "\n"),
	}
	if d.UpSQL != "" {
		plan, err := engine.BuildPlan(engine.Migration{Name: "diff", UpSQL: d.UpSQL}, engine.DirectionUp)
		if err != nil {
			return nil, rpcErr(err)
		}
		out.Statements = statementsToProto(controlplane.PlanStatements(plan.Statements))
	}
	s.Log.Info("schema diffed", "target", m.Target, "statements", len(out.Statements), "drift", len(d.Drift))

	return connect.NewResponse(out), nil
}
