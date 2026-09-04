package controlplane

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pashagolub/pgxmock/v4"

	"github.com/SamuelMolling/godwit/internal/engine"
)

const (
	usersBody   = "CREATE TABLE users (id int);"
	ordersBody  = "CREATE TABLE orders (id int);"
	statusBody  = "ALTER TABLE users ADD COLUMN status text;"
	squashBody  = "-- godwit: checkpoint through=20260901130000\nCREATE TABLE users (id int);\nCREATE TABLE orders (id int);"
	viewOneBody = "CREATE OR REPLACE VIEW v AS SELECT 1;"
	viewTwoBody = "CREATE OR REPLACE VIEW v AS SELECT 2;"
)

func pair(id, body string) map[string]string {
	return map[string]string{id + ".up.sql": body, id + ".down.sql": "SELECT 1;"}
}

func files(pairs ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, p := range pairs {
		for name, body := range p {
			out[name] = body
		}
	}

	return out
}

func fleetRun(t *testing.T, s *Store, id, target string, all map[string]string, applied ...string) {
	t.Helper()
	ctx := context.Background()
	if err := s.CreateRun(ctx, id, target, RolloutDirect, all, Timeouts{}, Provenance{}, "", nil); err != nil {
		t.Fatal(err)
	}
	for _, m := range applied {
		if err := s.RecordApplied(ctx, id, m, false, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Finish(ctx, id, StateSucceeded, ""); err != nil {
		t.Fatal(err)
	}
}

func runID(n int) string {
	return "fffffff0-0000-0000-0000-00000000000" + string(rune('0'+n))
}

func entry(f Fleet, migration string) FleetMigration {
	for _, m := range f.Migrations {
		if m.Migration == migration {
			return m
		}
	}

	return FleetMigration{}
}

func on(m FleetMigration, target string) FleetOn {
	for _, o := range m.On {
		if o.Target == target {
			return o
		}
	}

	return FleetOn{}
}

func gap(m FleetMigration, target string) (FleetGap, bool) {
	for _, g := range m.Missing {
		if g.Target == target {
			return g, true
		}
	}

	return FleetGap{}, false
}

// fleetStore stages the fleet every test below reads: production and staging both carry users and orders,
// staging alone carries add_status (from a run that then failed), the two disagree on the content of x and
// of the repeatable, and newbie was built from a checkpoint that collapsed users and orders.
func fleetStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	s, _ := newStore(t)
	for _, name := range []string{"production", "staging", "newbie", "fresh"} {
		if err := s.RegisterTarget(ctx, name, "static", map[string]string{}); err != nil {
			t.Fatal(err)
		}
	}
	base := files(pair("20260901120000_users", usersBody), pair("20260901130000_orders", ordersBody))

	fleetRun(t, s, runID(1), "production", base, "20260901120000_users", "20260901130000_orders")
	fleetRun(t, s, runID(2), "staging", base, "20260901120000_users", "20260901130000_orders")
	fleetRun(t, s, runID(3), "staging", files(pair("20260902090000_add_status", statusBody)), "20260902090000_add_status")
	if err := s.Finish(ctx, runID(3), StateFailed, "boom"); err != nil {
		t.Fatal(err)
	}
	fleetRun(t, s, runID(4), "production", files(pair("20260903100000_x", "SELECT 'prod';")), "20260903100000_x")
	fleetRun(t, s, runID(5), "staging", files(pair("20260903100000_x", "SELECT 'stag';")), "20260903100000_x")
	fleetRun(t, s, runID(6), "production", files(pair("R__v", viewOneBody)), "R__v")
	fleetRun(t, s, runID(7), "staging", files(pair("R__v", viewTwoBody)), "R__v")
	fleetRun(t, s, runID(8), "newbie",
		files(base, map[string]string{"20260904000000_squash.up.sql": squashBody}),
		"20260901120000_users", "20260901130000_orders", "20260904000000_squash")

	return s
}

func TestFleetMigrations(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := fleetStore(t)

	f, err := s.FleetMigrations(ctx, FleetFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"fresh", "newbie", "production", "staging"}; strings.Join(f.Targets, ",") != strings.Join(want, ",") {
		t.Fatalf("targets = %v", f.Targets)
	}
	order := make([]string, 0, len(f.Migrations))
	for _, m := range f.Migrations {
		order = append(order, m.Migration)
	}
	want := "20260901120000_users,20260901130000_orders,20260902090000_add_status,20260903100000_x,20260903100000_x,20260904000000_squash,R__v,R__v"
	if strings.Join(order, ",") != want {
		t.Fatalf("order = %v", order)
	}

	users := entry(f, "20260901120000_users")
	if users.Version != 20260901120000 || users.Name != "users" || users.Repeatable || users.Checkpoint || users.Divergent {
		t.Fatalf("users = %+v", users)
	}
	if len(users.On) != 3 {
		t.Fatalf("users on %d targets", len(users.On))
	}
	if o := on(users, "newbie"); o.CollapsedBy != "20260904000000_squash" {
		t.Fatalf("newbie ran the collapsed migration: %+v", o)
	}
	if o := on(users, "production"); o.CollapsedBy != "" || o.RunID != runID(1) || o.AppliedAt.IsZero() {
		t.Fatalf("production users = %+v", o)
	}
	if g, _ := gap(users, "fresh"); !g.Behind || g.Holds || g.NewestVersion != 0 {
		t.Fatalf("fresh gap = %+v", g)
	}

	squash := entry(f, "20260904000000_squash")
	if !squash.Checkpoint || len(squash.On) != 1 {
		t.Fatalf("squash = %+v", squash)
	}
	if o := on(squash, "newbie"); o.CollapsedBy != "" {
		t.Fatalf("a checkpoint does not collapse itself: %+v", o)
	}

	// A run that failed after applying it leaves the migration standing, and production is simply past it.
	status := entry(f, "20260902090000_add_status")
	if len(status.On) != 1 || status.On[0].Target != "staging" {
		t.Fatalf("add_status = %+v", status)
	}
	if g, _ := gap(status, "production"); g.Behind || g.Holds || g.NewestVersion != 20260903100000 {
		t.Fatalf("production gap = %+v", g)
	}
	if g, _ := gap(status, "newbie"); g.Behind || g.NewestVersion != 20260904000000 {
		t.Fatalf("newbie is past it, not behind it: %+v", g)
	}
}

func TestFleetMigrationsDivergence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := fleetStore(t)

	f, err := s.FleetMigrations(ctx, FleetFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var xs []FleetMigration
	for _, m := range f.Migrations {
		if m.Migration == "20260903100000_x" {
			xs = append(xs, m)
		}
	}
	if len(xs) != 2 {
		t.Fatalf("x entries = %+v", xs)
	}
	for _, m := range xs {
		if !m.Divergent || len(m.On) != 1 {
			t.Fatalf("x = %+v", m)
		}
		other := "production"
		if m.On[0].Target == "production" {
			other = "staging"
		}
		g, ok := gap(m, other)
		if !ok || !g.Holds || g.OtherChecksum == "" || g.OtherChecksum == m.Checksum || g.Behind {
			t.Fatalf("%s gap on %s = %+v", m.Checksum, other, g)
		}
	}

	// A repeatable is keyed by name and content, so an edited one diverges the same way a version does.
	var reps []FleetMigration
	for _, m := range f.Migrations {
		if m.Repeatable {
			reps = append(reps, m)
		}
	}
	if len(reps) != 2 {
		t.Fatalf("repeatables = %+v", reps)
	}
	if reps[0].Name != "v" || reps[0].Version != 0 || !reps[0].Divergent {
		t.Fatalf("repeatable = %+v", reps[0])
	}
	if g, _ := gap(reps[0], "fresh"); !g.Behind || g.Holds {
		t.Fatalf("a target with no history is behind a repeatable too: %+v", g)
	}
	if g, _ := gap(reps[0], "newbie"); g.Behind || g.Holds {
		t.Fatalf("a migrated target simply does not have it: %+v", g)
	}
}

func TestFleetMigrationsFilters(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := fleetStore(t)

	ids := func(f FleetFilter) []string {
		t.Helper()
		got, err := s.FleetMigrations(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		out := make([]string, 0, len(got.Migrations))
		for _, m := range got.Migrations {
			out = append(out, m.Migration)
		}

		return out
	}

	if got := ids(FleetFilter{Targets: []string{"production", "staging"}}); strings.Join(got, ",") !=
		"20260901120000_users,20260901130000_orders,20260902090000_add_status,20260903100000_x,20260903100000_x,R__v,R__v" {
		t.Fatalf("two targets = %v", got)
	}
	if got := ids(FleetFilter{FromVersion: 20260901130000, ToVersion: 20260903100000}); strings.Join(got, ",") !=
		"20260901130000_orders,20260902090000_add_status,20260903100000_x,20260903100000_x" {
		t.Fatalf("range = %v", got)
	}
	if got := ids(FleetFilter{ToVersion: 20260901120000}); strings.Join(got, ",") != "20260901120000_users" {
		t.Fatalf("open lower bound = %v", got)
	}
	if got := ids(FleetFilter{Targets: []string{"production", "staging"}, NotEverywhere: true}); strings.Join(got, ",") !=
		"20260902090000_add_status,20260903100000_x,20260903100000_x,R__v,R__v" {
		t.Fatalf("not everywhere = %v", got)
	}
	if got := ids(FleetFilter{Targets: []string{"production", "staging"}, In: "staging", NotIn: "production"}); strings.Join(got, ",") !=
		"20260902090000_add_status,20260903100000_x,R__v" {
		t.Fatalf("in staging not in production = %v", got)
	}
	if got := ids(FleetFilter{In: "fresh"}); len(got) != 0 {
		t.Fatalf("a target with no history holds nothing: %v", got)
	}
}

func TestFleetMigrationsUnknownTarget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := fleetStore(t)

	for _, f := range []FleetFilter{{Targets: []string{"nope"}}, {In: "nope"}, {NotIn: "nope"}} {
		if _, err := s.FleetMigrations(ctx, f); !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), `"nope"`) {
			t.Fatalf("%+v: err = %v", f, err)
		}
	}
}

// A held migration is on the target's disk but not in its history, and a reverted one is gone from it;
// neither stands, so neither is in the fleet view.
func TestFleetMigrationsSkipsHeldAndReverted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newStore(t)
	if err := s.RegisterTarget(ctx, "app", "static", map[string]string{}); err != nil {
		t.Fatal(err)
	}
	fleetRun(t, s, runID(1), "app", files(pair("20260901120000_users", usersBody)), "20260901120000_users")
	if err := s.CreateRun(ctx, runID(2), "app", RolloutDirect, files(pair("20260901130000_held", ordersBody)),
		Timeouts{}, Provenance{}, "", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordApplied(ctx, runID(2), "20260901130000_held", true, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkReverted(ctx, runID(1), runID(2), "20260901120000_users"); err != nil {
		t.Fatal(err)
	}

	f, err := s.FleetMigrations(ctx, FleetFilter{})
	if err != nil || len(f.Migrations) != 0 {
		t.Fatalf("migrations = %+v, err = %v", f.Migrations, err)
	}
}

// Retention that swept a run's file bodies leaves the migration in the answer with its content unknown,
// rather than dropping it and reporting the target as missing it.
func TestFleetMigrationsWithoutBodies(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newStore(t)
	if err := s.RegisterTarget(ctx, "app", "static", map[string]string{}); err != nil {
		t.Fatal(err)
	}
	fleetRun(t, s, runID(1), "app", files(pair("20260901120000_users", usersBody)), "20260901120000_users")
	if _, err := s.pool.Exec(ctx, `DELETE FROM cp_run_files WHERE run_id = $1`, runID(1)); err != nil {
		t.Fatal(err)
	}

	f, err := s.FleetMigrations(ctx, FleetFilter{})
	if err != nil || len(f.Migrations) != 1 {
		t.Fatalf("migrations = %+v, err = %v", f.Migrations, err)
	}
	if m := f.Migrations[0]; m.Checksum != "" || m.Divergent || len(m.On) != 1 {
		t.Fatalf("swept migration = %+v", m)
	}
}

func TestFleetMigrationsQueryErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mock.Close)
	s := NewStore(mock)
	boom := errors.New("boom")

	none, one := []string{}, []string{"app"}
	marker := engine.DirectiveMarker + " " + engine.DirectiveCheckpoint

	mock.ExpectQuery("FROM cp_targets").WithArgs(none).WillReturnError(boom)
	if _, err := s.FleetMigrations(ctx, FleetFilter{}); err == nil || !strings.Contains(err.Error(), "list fleet targets") {
		t.Fatalf("err = %v", err)
	}
	mock.ExpectQuery("FROM cp_targets").WithArgs(none).WillReturnRows(pgxmock.NewRows([]string{"name", "extra"}).AddRow("app", "x"))
	if _, err := s.FleetMigrations(ctx, FleetFilter{}); err == nil || !strings.Contains(err.Error(), "read fleet targets") {
		t.Fatalf("scan err = %v", err)
	}
	mock.ExpectQuery("FROM cp_targets").WithArgs(none).WillReturnRows(pgxmock.NewRows([]string{"name"}).AddRow("app"))
	mock.ExpectQuery("FROM cp_run_applied").WithArgs(one, marker).WillReturnError(boom)
	if _, err := s.FleetMigrations(ctx, FleetFilter{}); err == nil || !strings.Contains(err.Error(), "list standing migrations") {
		t.Fatalf("ledger err = %v", err)
	}
	mock.ExpectQuery("FROM cp_targets").WithArgs(none).WillReturnRows(pgxmock.NewRows([]string{"name"}).AddRow("app"))
	mock.ExpectQuery("FROM cp_run_applied").WithArgs(one, marker).WillReturnRows(
		pgxmock.NewRows([]string{"target", "migration"}).AddRow("app", "20260901120000_users"))
	if _, err := s.FleetMigrations(ctx, FleetFilter{}); err == nil || !strings.Contains(err.Error(), "read standing migrations") {
		t.Fatalf("ledger scan err = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
