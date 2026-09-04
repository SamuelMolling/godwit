package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/pashagolub/pgxmock/v4"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/internal/controlplane"
	"github.com/SamuelMolling/godwit/internal/creds"
	"github.com/SamuelMolling/godwit/internal/engine"
)

func expectFleetRows(mock pgxmock.PgxPoolIface) {
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	mock.ExpectQuery("FROM cp_targets").WithArgs([]string{}).WillReturnRows(
		pgxmock.NewRows([]string{"name"}).AddRow("production").AddRow("staging"))
	mock.ExpectQuery("FROM cp_run_applied").
		WithArgs([]string{"production", "staging"}, engine.DirectiveMarker+" "+engine.DirectiveCheckpoint).
		WillReturnRows(pgxmock.NewRows([]string{"target", "migration", "applied_at", "run_id", "checksum", "body"}).
			AddRow("production", "20260901120000_users", at, "r1", "c1", "").
			AddRow("staging", "20260901120000_users", at, "r2", "c2", "").
			AddRow("staging", "20260902090000_status", at, "r3", "c3", "").
			AddRow("production", "20260903000000_later", at, "r4", "c4", ""))
}

func TestListMigrations(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mock.Close)
	s := NewServer(controlplane.NewStore(mock), nil, nil, creds.Keyring{})

	expectFleetRows(mock)
	res, err := s.ListMigrations(ctx, connect.NewRequest(&godwitv1.ListMigrationsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Msg.Targets; len(got) != 2 || got[0] != "production" {
		t.Fatalf("targets = %v", got)
	}
	if len(res.Msg.Migrations) != 4 {
		t.Fatalf("migrations = %+v", res.Msg.Migrations)
	}
	users := res.Msg.Migrations[0]
	if users.Migration != "20260901120000_users" || users.Version != 20260901120000 || users.Name != "users" ||
		users.Checksum != "c1" || !users.Divergent || users.Repeatable || users.Checkpoint {
		t.Fatalf("users = %+v", users)
	}
	if len(users.AppliedOn) != 1 || users.AppliedOn[0].Target != "production" ||
		users.AppliedOn[0].RunId != "r1" || users.AppliedOn[0].AppliedAt == nil {
		t.Fatalf("applied on = %+v", users.AppliedOn)
	}
	if len(users.MissingFrom) != 1 {
		t.Fatalf("missing from = %+v", users.MissingFrom)
	}
	if g := users.MissingFrom[0]; g.Target != "staging" || !g.Holds || g.OtherChecksum != "c2" || g.Behind ||
		g.NewestVersion != 20260902090000 {
		t.Fatalf("gap = %+v", g)
	}
	if g := res.Msg.Migrations[2].MissingFrom[0]; g.Target != "production" || g.Behind || g.Holds ||
		g.NewestVersion != 20260903000000 {
		t.Fatalf("production is past it and skipped it: %+v", g)
	}
	if g := res.Msg.Migrations[3].MissingFrom[0]; g.Target != "staging" || !g.Behind {
		t.Fatalf("staging has not reached it: %+v", g)
	}
}

func TestListMigrationsRefusals(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mock.Close)
	s := NewServer(controlplane.NewStore(mock), nil, nil, creds.Keyring{})

	for _, req := range []*godwitv1.ListMigrationsRequest{
		{FromVersion: -1},
		{ToVersion: -2},
		{FromVersion: 20260902000000, ToVersion: 20260901000000},
	} {
		if _, err := s.ListMigrations(ctx, connect.NewRequest(req)); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Fatalf("%+v: err = %v", req, err)
		}
	}

	mock.ExpectQuery("FROM cp_targets").WithArgs([]string{}).WillReturnError(errors.New("store down"))
	if _, err := s.ListMigrations(ctx, connect.NewRequest(&godwitv1.ListMigrationsRequest{})); connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("store error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
