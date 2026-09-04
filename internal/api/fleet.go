package api

import (
	"context"
	"fmt"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/internal/controlplane"
)

// ListMigrations reports which targets hold each migration and which do not, keyed by migration and by the
// content applied under it, from the control plane's ledger alone: no target is connected to.
func (s *Server) ListMigrations(ctx context.Context, req *connect.Request[godwitv1.ListMigrationsRequest]) (*connect.Response[godwitv1.ListMigrationsResponse], error) {
	m := req.Msg
	if m.FromVersion < 0 || m.ToVersion < 0 {
		return nil, invalid("version bounds cannot be negative")
	}
	if m.FromVersion != 0 && m.ToVersion != 0 && m.FromVersion > m.ToVersion {
		return nil, invalid(fmt.Sprintf("from_version %d is above to_version %d", m.FromVersion, m.ToVersion))
	}
	fleet, err := s.store.FleetMigrations(ctx, controlplane.FleetFilter{
		Targets: m.Targets, FromVersion: m.FromVersion, ToVersion: m.ToVersion,
		NotEverywhere: m.NotEverywhere, In: m.InTarget, NotIn: m.NotInTarget,
	})
	if err != nil {
		return nil, rpcErr(err)
	}
	out := &godwitv1.ListMigrationsResponse{
		Targets:    fleet.Targets,
		Migrations: make([]*godwitv1.FleetMigration, 0, len(fleet.Migrations)),
	}
	for _, e := range fleet.Migrations {
		out.Migrations = append(out.Migrations, fleetToProto(e))
	}

	return connect.NewResponse(out), nil
}

func fleetToProto(e controlplane.FleetMigration) *godwitv1.FleetMigration {
	pb := &godwitv1.FleetMigration{
		Migration: e.Migration, Version: e.Version, Name: e.Name, Repeatable: e.Repeatable,
		Checkpoint: e.Checkpoint, Checksum: e.Checksum, Divergent: e.Divergent,
	}
	for _, o := range e.On {
		pb.AppliedOn = append(pb.AppliedOn, &godwitv1.MigrationOn{
			Target: o.Target, AppliedAt: timestamppb.New(o.AppliedAt), RunId: o.RunID, CollapsedBy: o.CollapsedBy,
		})
	}
	for _, g := range e.Missing {
		pb.MissingFrom = append(pb.MissingFrom, &godwitv1.MigrationGap{
			Target: g.Target, NewestVersion: g.NewestVersion, Behind: g.Behind,
			Holds: g.Holds, OtherChecksum: g.OtherChecksum,
		})
	}

	return pb
}
