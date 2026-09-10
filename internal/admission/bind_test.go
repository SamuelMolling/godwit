package admission

import (
	"context"
	"errors"
	"testing"

	"github.com/SamuelMolling/godwit/internal/controlplane"
)

func TestPlanMigrationsDetects(t *testing.T) {
	t.Parallel()
	set := toSet(t, map[string]string{
		"20260901120000_t.up.sql":   "CREATE TABLE b (id int);",
		"20260901120000_t.down.sql": "DROP TABLE b;",
	})
	obs := controlplane.Observation{Fingerprint: "f2", Definition: "table a\ntable b\n"}

	migs, drift, detected := planMigrations(set, Admitted{}, obs)
	if detected || drift != "" || migs[0].AlreadyApplied {
		t.Fatalf("no validation: %+v %q %t", migs, drift, detected)
	}

	val := controlplane.Validation{Base: "table a\n", Effects: [][]string{{"+ table b"}}, Fingerprints: []string{"f1", "f2"}}
	migs, drift, detected = planMigrations(set, Admitted{Validation: &val}, obs)
	if !detected || drift != "" || !migs[0].AlreadyApplied || migs[0].Effect != "+ table b" {
		t.Fatalf("validation: %+v %q %t", migs, drift, detected)
	}
}

func TestObservedSearchPath(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	g, _ := newGate(t)

	if path, err := g.ObservedSearchPath(ctx, "app"); path != "" || err != nil {
		t.Fatalf("no observer: %q, %v", path, err)
	}
	g.Observer = stubObserver{err: errors.New("target down")}
	if _, err := g.ObservedSearchPath(ctx, "app"); err == nil {
		t.Fatal("observe error must surface")
	}
	g.Observer = stubObserver{obs: controlplane.Observation{SearchPath: "app,public"}}
	if path, err := g.ObservedSearchPath(ctx, "app"); path != "app,public" || err != nil {
		t.Fatalf("path = %q, err = %v", path, err)
	}
}

func TestBindWithoutAnObserverBindsNoPlan(t *testing.T) {
	t.Parallel()
	g, _ := newGate(t)

	b, err := g.Bind(context.Background(), Request{Target: "app", Acked: []string{"H002"}}, toSet(t, toFiles()))
	if err != nil || b.PlanID != "" || b.Reattach != nil || len(b.Acked) != 1 {
		t.Fatalf("binding = %+v, err = %v", b, err)
	}
}

func TestReattachSkipsAnExplicitPlan(t *testing.T) {
	t.Parallel()
	g, _ := newGate(t)

	r, err := g.reattach(context.Background(), Request{Target: "app", PlanID: "p1"}, Set{}, controlplane.Observation{})
	if r != nil || err != nil {
		t.Fatalf("explicit plan: %+v, %v", r, err)
	}
}
