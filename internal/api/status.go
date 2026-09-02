package api

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/internal/controlplane"
	"github.com/SamuelMolling/godwit/internal/engine"
)

var errStatusDisabled = connect.NewError(connect.CodeUnimplemented, errors.New("target status is not enabled"))

// GetTargetStatus reports what a target has applied, what the given files would still apply, its last run and drift baseline.
func (s *Server) GetTargetStatus(ctx context.Context, req *connect.Request[godwitv1.GetTargetStatusRequest]) (*connect.Response[godwitv1.GetTargetStatusResponse], error) {
	if s.Inspector == nil {
		return nil, errStatusDisabled
	}
	m := req.Msg
	if m.Target == "" {
		return nil, invalid("target is required")
	}
	files := map[string]string{}
	for _, f := range m.Files {
		files[f.Name] = f.Body
	}
	migs, err := controlplane.MigrationsFromFiles(files)
	if err != nil {
		return nil, invalid(err.Error())
	}
	st, err := s.Inspector.Status(ctx, m.Target)
	if err != nil {
		return nil, rpcErr(err)
	}
	ready, err := s.store.ReadyPlanCount(ctx, m.Target, s.planSince())
	if err != nil {
		return nil, rpcErr(err)
	}
	out := statusToProto(st, migs)
	out.ReadyPlans = int32(ready)

	return connect.NewResponse(out), nil
}

func statusToProto(st controlplane.TargetStatus, migs []engine.Migration) *godwitv1.GetTargetStatusResponse {
	out := &godwitv1.GetTargetStatusResponse{
		Target: st.Target, Provider: st.Provider,
		LockTimeout: st.Timeouts.Lock, StatementTimeout: st.Timeouts.Statement,
	}
	byVersion := make(map[int64]*godwitv1.AppliedMigration, len(st.Applied))
	for _, a := range st.Applied {
		pb := &godwitv1.AppliedMigration{Version: a.Version, Name: a.Name, Checksum: a.Checksum, AppliedAt: timestamppb.New(a.AppliedAt)}
		byVersion[a.Version] = pb
		out.Applied = append(out.Applied, pb)
	}
	for _, m := range migs {
		if pb, ok := byVersion[m.Version]; ok {
			pb.ChecksumMismatch = pb.Checksum != m.Checksum

			continue
		}
		out.Pending = append(out.Pending, &godwitv1.PendingMigration{Version: m.Version, Name: m.Name})
	}
	if st.LastRun != nil {
		out.LastRun = toProto(*st.LastRun)
	}
	if st.Snapshot != nil {
		out.DriftBaseline = &godwitv1.DriftBaseline{
			TakenAt: timestamppb.New(st.Snapshot.TakenAt), RunId: st.Snapshot.RunID, UnresolvedDrift: st.OpenDrift,
		}
	}

	return out
}
