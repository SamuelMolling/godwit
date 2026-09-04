package ui

import (
	"context"
	"net/http"
	"testing"
	"time"

	"connectrpc.com/connect"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/internal/api"
)

func (s *stub) ListMigrations(ctx context.Context, req *connect.Request[godwitv1.ListMigrationsRequest]) (*connect.Response[godwitv1.ListMigrationsResponse], error) {
	if err := s.call(ctx, "ListMigrations:"+req.Msg.InTarget); err != nil {
		return nil, err
	}
	if s.fleetErr != nil {
		return nil, s.fleetErr
	}
	if s.fleet == nil {
		return connect.NewResponse(&godwitv1.ListMigrationsResponse{}), nil
	}
	out := &godwitv1.ListMigrationsResponse{Targets: s.fleet.Targets}
	for _, m := range s.fleet.Migrations {
		if req.Msg.NotEverywhere && len(m.MissingFrom) == 0 {
			continue
		}
		out.Migrations = append(out.Migrations, m)
	}

	return connect.NewResponse(out), nil
}

func fleetFixture() *stub {
	s := fixture()
	day := func(d int) *godwitv1.MigrationOn {
		return &godwitv1.MigrationOn{Target: "app", AppliedAt: at(time.Duration(d) * time.Hour)}
	}
	s.fleet = &godwitv1.ListMigrationsResponse{
		Targets: []string{"app", "billing"},
		Migrations: []*godwitv1.FleetMigration{
			{
				Migration: "20260901120000_users", Version: 20260901120000, Name: "users", Checksum: "aabbccdd11223344",
				AppliedOn: []*godwitv1.MigrationOn{
					day(3),
					{Target: "billing", AppliedAt: at(time.Hour), CollapsedBy: "20260904000000_squash"},
				},
			},
			{
				Migration: "20260902090000_add_status", Version: 20260902090000, Name: "add_status", Checksum: "ffeeddcc99887766",
				AppliedOn:   []*godwitv1.MigrationOn{day(2)},
				MissingFrom: []*godwitv1.MigrationGap{{Target: "billing", Behind: true}},
			},
			{
				Migration: "20260903100000_x", Version: 20260903100000, Name: "x", Checksum: "1111222233334444", Divergent: true,
				AppliedOn:   []*godwitv1.MigrationOn{day(1)},
				MissingFrom: []*godwitv1.MigrationGap{{Target: "billing", Holds: true, OtherChecksum: "5555666677778888"}},
			},
			{
				Migration: "R__active_users", Name: "active_users", Repeatable: true,
				AppliedOn:   []*godwitv1.MigrationOn{day(1)},
				MissingFrom: []*godwitv1.MigrationGap{{Target: "billing", NewestVersion: 20260904000000}},
			},
			{
				Migration: "20260904000000_squash", Version: 20260904000000, Name: "squash", Checkpoint: true, Checksum: "",
				AppliedOn:   []*godwitv1.MigrationOn{{Target: "billing", AppliedAt: at(time.Hour)}},
				MissingFrom: []*godwitv1.MigrationGap{{Target: "app", NewestVersion: 20260903100000}},
			},
		},
	}

	return s
}

func TestFleetPage(t *testing.T) {
	t.Parallel()
	s := fleetFixture()
	h := newUI(s, Config{Replica: "godwit-0"})

	rec := do(h, http.MethodGet, "/ui/migrations", nil)
	want(t, rec, http.StatusOK, "Every migration", "20260901120000_users", "aabbccdd", "unknown",
		"recorded by 20260904000000_squash, not run", "not yet", "differs", "applied here as 55556666", "missing",
		"stands under more than one checksum", "repeatable, keyed by content", "checkpoint",
		"4 not on every target · 1 under more than one checksum",
		`href="/ui/migrations?gaps=1"`, "Only what is not everywhere")
	if s.calls[1] != "ListMigrations:" {
		t.Fatalf("calls = %v", s.calls)
	}

	rec = do(h, http.MethodGet, "/ui/migrations?target=app&gaps=1", nil)
	want(t, rec, http.StatusOK, "Standing on app", `href="/ui/migrations?target=app&amp;gaps=0"`, "Show every migration")
	absent(t, rec, "20260901120000_users")
}

func TestFleetPageEmptyAndErrors(t *testing.T) {
	t.Parallel()

	rec := do(newUI(&stub{}, Config{}), http.MethodGet, "/ui/migrations", nil)
	want(t, rec, http.StatusOK, "Nothing to compare", "godwit migrations")

	s := fleetFixture()
	s.err = connect.NewError(connect.CodeUnavailable, errBoom)
	want(t, do(newUI(s, Config{}), http.MethodGet, "/ui/migrations", nil), http.StatusBadGateway, "boom")

	s = fleetFixture()
	s.fleetErr = connect.NewError(connect.CodeUnavailable, errBoom)
	want(t, do(newUI(s, Config{}), http.MethodGet, "/ui/migrations", nil), http.StatusBadGateway, "boom")
}

func TestFleetPageScope(t *testing.T) {
	t.Parallel()
	h := newUI(fleetFixture(), Config{AnonymousScope: api.ScopeRead})

	want(t, do(h, http.MethodGet, "/ui/migrations", nil), http.StatusOK, "Every migration")
}

func TestFleetAllOnEveryTarget(t *testing.T) {
	t.Parallel()
	s := fixture()
	s.fleet = &godwitv1.ListMigrationsResponse{
		Targets: []string{"app"},
		Migrations: []*godwitv1.FleetMigration{{
			Migration: "20260901120000_users", Checksum: "aabbccdd11223344",
			AppliedOn: []*godwitv1.MigrationOn{{Target: "app", AppliedAt: at(time.Hour)}},
		}},
	}

	want(t, do(newUI(s, Config{}), http.MethodGet, "/ui/migrations", nil), http.StatusOK, "All on every target")
}
