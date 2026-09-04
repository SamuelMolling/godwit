package cli

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/gen/godwit/v1/godwitv1connect"
)

type fleetStub struct {
	godwitv1connect.UnimplementedGodwitServiceHandler
	req  *godwitv1.ListMigrationsRequest
	resp *godwitv1.ListMigrationsResponse
	err  error
}

func (s *fleetStub) ListMigrations(_ context.Context, req *connect.Request[godwitv1.ListMigrationsRequest]) (*connect.Response[godwitv1.ListMigrationsResponse], error) {
	s.req = req.Msg
	if s.err != nil {
		return nil, s.err
	}

	return connect.NewResponse(s.resp), nil
}

func startFleetStub(t *testing.T, svc *fleetStub) string {
	t.Helper()
	path, handler := godwitv1connect.NewGodwitServiceHandler(svc)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	srv := httptest.NewUnstartedServer(mux)
	srv.Config.Protocols = new(http.Protocols)
	srv.Config.Protocols.SetUnencryptedHTTP2(true)
	srv.Start()
	t.Cleanup(srv.Close)

	return srv.URL
}

func day(d int) *timestamppb.Timestamp {
	return timestamppb.New(time.Date(2026, 9, d, 10, 0, 0, 0, time.UTC))
}

func TestMigrations(t *testing.T) {
	t.Parallel()
	stub := &fleetStub{resp: &godwitv1.ListMigrationsResponse{
		Targets: []string{"production", "staging"},
		Migrations: []*godwitv1.FleetMigration{
			{
				Migration: "20260901120000_users", Version: 20260901120000, Name: "users", Checksum: "aabbccdd11223344",
				AppliedOn: []*godwitv1.MigrationOn{
					{Target: "production", AppliedAt: day(1), CollapsedBy: "20260904000000_squash"},
					{Target: "staging", AppliedAt: day(1)},
				},
			},
			{
				Migration: "20260902090000_add_status", Version: 20260902090000, Name: "add_status", Checksum: "ffeeddcc99887766",
				AppliedOn:   []*godwitv1.MigrationOn{{Target: "staging", AppliedAt: day(4)}},
				MissingFrom: []*godwitv1.MigrationGap{{Target: "production", Behind: true, NewestVersion: 20260901120000}},
			},
			{
				Migration: "20260903100000_x", Version: 20260903100000, Name: "x", Checksum: "1111222233334444", Divergent: true,
				AppliedOn:   []*godwitv1.MigrationOn{{Target: "production", AppliedAt: day(3)}},
				MissingFrom: []*godwitv1.MigrationGap{{Target: "staging", Holds: true, OtherChecksum: "5555666677778888"}},
			},
			{
				Migration: "R__active_users", Name: "active_users", Repeatable: true,
				AppliedOn:   []*godwitv1.MigrationOn{{Target: "staging", AppliedAt: day(4)}},
				MissingFrom: []*godwitv1.MigrationGap{{Target: "production", NewestVersion: 20260903100000}},
			},
		},
	}}
	url := startFleetStub(t, stub)

	code, out, errOut := runCLI("migrations", "--server", url)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
	want := "MIGRATION                  CHECKSUM  PRODUCTION   STAGING\n" +
		"20260901120000_users       aabbccdd  2026-09-01*  2026-09-01\n" +
		"20260902090000_add_status  ffeeddcc  -            2026-09-04\n" +
		"20260903100000_x           11112222  2026-09-03   differs\n" +
		"R__active_users            unknown   missing      2026-09-04\n" +
		"\n4 migrations, 2 targets: 3 not on every target, 1 under more than one checksum\n" +
		"key: - not there yet · missing the target is past it · differs another checksum · * recorded by a checkpoint\n"
	if out != want {
		t.Fatalf("out = %q\nwant %q", out, want)
	}

	code, out, _ = runCLI("migrations", "--server", url, "--json")
	if code != 0 || len(decodeJSON(t, out)["migrations"].([]any)) != 4 {
		t.Fatalf("code = %d, out = %q", code, out)
	}

	code, _, errOut = runCLI("migrations", "--server", url,
		"--target", "production,staging", "--from", "20260901000000", "--to", "20260904000000",
		"--not-everywhere", "--in", "staging", "--not-in", "production")
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
	if got := stub.req; len(got.Targets) != 2 || got.FromVersion != 20260901000000 || got.ToVersion != 20260904000000 ||
		!got.NotEverywhere || got.InTarget != "staging" || got.NotInTarget != "production" {
		t.Fatalf("request = %+v", stub.req)
	}

	stub.err = connect.NewError(connect.CodeUnavailable, errors.New("store down"))
	if code, _, errOut := runCLI("migrations", "--server", url); code != 1 || errOut != "godwit: store down\n" {
		t.Fatalf("code = %d, stderr = %q", code, errOut)
	}
}

func TestMigrationsEmpty(t *testing.T) {
	t.Parallel()
	stub := &fleetStub{resp: &godwitv1.ListMigrationsResponse{}}
	url := startFleetStub(t, stub)

	if code, out, _ := runCLI("migrations", "--server", url); code != 0 || out != "no targets registered\n" {
		t.Fatalf("code = %d, out = %q", code, out)
	}

	stub.resp = &godwitv1.ListMigrationsResponse{Targets: []string{"production", "staging"}}
	if code, out, _ := runCLI("migrations", "--server", url); code != 0 || out != "nothing stands on production, staging\n" {
		t.Fatalf("code = %d, out = %q", code, out)
	}

	stub.resp = &godwitv1.ListMigrationsResponse{
		Targets: []string{"production"},
		Migrations: []*godwitv1.FleetMigration{{
			Migration: "20260901120000_users", Checksum: "aabbccdd11223344",
			AppliedOn: []*godwitv1.MigrationOn{{Target: "production", AppliedAt: day(1)}},
		}},
	}
	want := "MIGRATION             CHECKSUM  PRODUCTION\n" +
		"20260901120000_users  aabbccdd  2026-09-01\n" +
		"\n1 migration, 1 target: all on every target\n"
	if code, out, _ := runCLI("migrations", "--server", url); code != 0 || out != want {
		t.Fatalf("code = %d, out = %q", code, out)
	}
}
