package controlplane

import (
	"testing"

	"github.com/SamuelMolling/godwit/internal/engine"
)

func repeatable(name, up string) engine.Migration {
	m := engine.Migration{Name: name, Repeatable: true, UpSQL: up, DownSQL: "DROP VIEW v;"}
	m.Checksum = sum(up)

	return m
}

func ids(plans []engine.Plan) []string {
	out := make([]string, 0, len(plans))
	for _, p := range plans {
		out = append(out, p.Migration.ID())
	}

	return out
}

func TestTruncateHoldsBackVersionsAboveTheTarget(t *testing.T) {
	t.Parallel()
	a, b, c := mig(1, "a", "CREATE TABLE a (id int);"), mig(2, "b", "CREATE TABLE b (id int);"), mig(3, "c", "CREATE TABLE c (id int);")
	r := repeatable("v", "CREATE OR REPLACE VIEW v AS SELECT 1;")

	keep, withheld := Truncate(planFor(t, a, b, c, r), 2, AppliedSet{})
	if got := ids(keep); len(got) != 2 || got[0] != a.ID() || got[1] != b.ID() {
		t.Fatalf("keep = %v", got)
	}
	if got := ids(withheld); len(got) != 2 || got[0] != c.ID() || got[1] != r.ID() {
		t.Fatalf("withheld = %v; a repeatable follows the versioned migrations it ships with", got)
	}
}

func TestTruncateRunsRepeatablesWhenNothingPendingIsHeldBack(t *testing.T) {
	t.Parallel()
	a, b := mig(1, "a", "CREATE TABLE a (id int);"), mig(2, "b", "CREATE TABLE b (id int);")
	r := repeatable("v", "CREATE OR REPLACE VIEW v AS SELECT 1;")
	plans := planFor(t, a, b, r)

	keep, withheld := Truncate(plans, 2, AppliedSet{})
	if got := ids(keep); len(got) != 3 || got[2] != r.ID() || len(withheld) != 0 {
		t.Fatalf("keep = %v, withheld = %v", got, ids(withheld))
	}

	keep, withheld = Truncate(plans, 1, AppliedSet{Versions: []int64{2}})
	if got := ids(keep); len(got) != 2 || got[1] != r.ID() {
		t.Fatalf("keep = %v; an applied version above the target holds nothing back", got)
	}
	if got := ids(withheld); len(got) != 1 || got[0] != b.ID() {
		t.Fatalf("withheld = %v", got)
	}
}

func TestWithheldMigrationsAreNotPending(t *testing.T) {
	t.Parallel()
	a, b := mig(1, "a", "CREATE TABLE a (id int);"), mig(2, "b", "CREATE TABLE b (id int);")
	keep, withheld := Truncate(planFor(t, a, b), 1, AppliedSet{})

	migs := append(BuildPlanMigrations(RolloutDirect, keep, AppliedSet{}, nil), WithheldMigrations(withheld, AppliedSet{Versions: []int64{2}})...)
	if len(migs) != 2 || migs[1].Version != 2 || !migs[1].Withheld || !migs[1].Applied || migs[1].Checksum != b.Checksum {
		t.Fatalf("migrations = %+v", migs)
	}
	if pending := (Plan{Migrations: migs}).Pending(); len(pending) != 1 || pending[0].Version != 1 {
		t.Fatalf("pending = %+v; a withheld migration is not what the plan would run", pending)
	}
}
