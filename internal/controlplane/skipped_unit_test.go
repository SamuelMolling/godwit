package controlplane

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SamuelMolling/godwit/internal/engine"
)

// Every report of a plan reads Skipped, and the hazard gate reads RunsBody; they are the same answer or
// the pull-request comment can tell someone to acknowledge a hazard nothing would run.
func TestPlanMigrationsCarryTheGatesVerdict(t *testing.T) {
	t.Parallel()
	a, b := mig(1, "a", "CREATE TABLE a (id int);"), mig(2, "b", "CREATE TABLE b (id int);")
	applied := AppliedSet{Versions: []int64{1}}

	ms := BuildPlanMigrations(RolloutDirect, planFor(t, a, b), applied, nil)
	if len(ms) != 2 || !ms[0].Skipped || ms[1].Skipped {
		t.Fatalf("migrations = %+v", ms)
	}
	for i, m := range ms {
		if m.Skipped == RunsBody(planFor(t, a, b)[i], applied) {
			t.Fatalf("%s disagrees with the gate", m.ID())
		}
	}

	marked := planFor(t, b)
	marked[0].MarkOnly = true
	if RunsBody(marked[0], applied) || !BuildPlanMigrations(RolloutDirect, marked, applied, nil)[0].Skipped {
		t.Fatal("a mark-only plan runs no body")
	}

	down, err := engine.BuildPlan(mig(1, "a", "CREATE TABLE a (id int);"), engine.DirectionDown)
	if err != nil {
		t.Fatal(err)
	}
	if !RunsBody(down, applied) {
		t.Fatal("a down plan runs precisely what the target holds")
	}

	if !WithheldMigrations(planFor(t, b), applied)[0].Skipped {
		t.Fatal("a withheld migration runs nothing")
	}
}

func TestSnapshotScopeOf(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		config map[string]string
		want   engine.SnapshotScope
	}{
		{nil, engine.IgnoreAdopted},
		{map[string]string{ConfigIgnoreAdopted: "true"}, engine.IgnoreAdopted},
		{map[string]string{ConfigIgnoreAdopted: "false"}, engine.KeepAdopted},
	} {
		if got := SnapshotScopeOf(tc.config); got != tc.want {
			t.Fatalf("config %v: scope = %v", tc.config, got)
		}
	}
}

// A baseline taken before the snapshot changed shape cannot be compared with one taken after. Reporting the
// difference would call the upgrade drift on every target at once; re-baselining here would swallow whatever
// drift the target really had. It says so instead.
func TestDriftRefusesABaselineFromAnOlderSchemaFormat(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newStore(t)
	mon, _ := newMonitor(t, s, &recordingNotifier{})

	if err := mon.AcceptBaseline(ctx, "app"); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveSnapshot(ctx, "app", "old-fingerprint", "column public.users.id bigint", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := mon.Check(ctx, "app"); !errors.Is(err, ErrBaselineFormat) ||
		!strings.Contains(err.Error(), "godwit drift accept app") {
		t.Fatalf("err = %v", err)
	}
	if events, err := s.ListDriftEvents(ctx, "app"); err != nil || len(events) != 0 {
		t.Fatalf("nothing may be recorded from a comparison godwit refused: %+v, err = %v", events, err)
	}

	if err := mon.AcceptBaseline(ctx, "app"); err != nil {
		t.Fatal(err)
	}
	if d, err := mon.Check(ctx, "app"); err != nil || d.Drifted {
		t.Fatalf("a fresh baseline compares again: %+v, err = %v", d, err)
	}
}
