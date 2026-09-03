package controlplane

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/SamuelMolling/godwit/internal/engine"
)

const peopleUp = "CREATE TABLE public.teams (id bigint PRIMARY KEY);\n" +
	"CREATE TABLE public.people (id bigserial PRIMARY KEY, age integer NOT NULL, team_id bigint);\n" +
	"CREATE INDEX people_team_id_idx ON public.people (team_id);"

func peopleFiles() map[string]string {
	return map[string]string{
		"20260101000000_people.up.sql":   peopleUp,
		"20260101000000_people.down.sql": "DROP TABLE public.people, public.teams;",
	}
}

func directiveFiles(directive, down string) map[string]string {
	files := peopleFiles()
	files["20260102000000_d.up.sql"] = "-- godwit: " + directive + "\n"
	files["20260102000000_d.down.sql"] = down

	return files
}

func succeededRun(t *testing.T, s *Store, target string, files map[string]string, exps map[string]Expansion) {
	t.Helper()
	ctx := context.Background()
	id := uuid.NewString()
	if err := s.CreateRun(ctx, id, target, RolloutExpandContract, files, Timeouts{}, Provenance{}, "", exps); err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(ctx, id, StateSucceeded, ""); err != nil {
		t.Fatal(err)
	}
}

func upPlans(t *testing.T, files map[string]string) []engine.Plan {
	t.Helper()
	plans, err := PlansFromFiles(files, engine.DirectionUp)
	if err != nil {
		t.Fatal(err)
	}

	return plans
}

func replayFixture(t *testing.T, targets ...string) (*Store, *Validator) {
	t.Helper()
	s, pool := newStore(t)
	for _, target := range targets {
		if err := s.RegisterTarget(context.Background(), target, "plain", map[string]string{"dsn": "x"}); err != nil {
			t.Fatal(err)
		}
		succeededRun(t, s, target, peopleFiles(), nil)
	}

	return s, NewValidator(pool, s, uuid.NewString)
}

// applyDirective validates the directive while it is still pending, then records it as a succeeded run
// carrying the expansion that run froze — the history a later validation replays.
func applyDirective(t *testing.T, s *Store, v *Validator, target, directive, down string) []engine.Plan {
	t.Helper()
	files := directiveFiles(directive, down)
	plans := upPlans(t, files)
	val, err := v.Validate(context.Background(), target, plans, "")
	if err != nil {
		t.Fatalf("%s must expand while pending: %v", directive, err)
	}
	if len(val.Expansions) != 1 {
		t.Fatalf("%s expansions = %+v", directive, val.Expansions)
	}
	succeededRun(t, s, target, files, val.Expansions)

	return plans
}

const revertDown = "-- godwit: revert\n"

func TestValidateDoesNotReexpandAppliedDirectives(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, tc := range []struct{ directive, down string }{
		{"change-type public.people.age bigint batch=500", revertDown},
		{"add-column public.people.nick text", revertDown},
		{"add-not-null public.people.team_id", revertDown},
		{"add-index public.people (age)", revertDown},
		{"add-fk public.people.team_id -> public.teams.id", revertDown},
		{"add-check public.people people_age_check 'age > 0'", revertDown},
		{"drop-index public.people_team_id_idx", "CREATE INDEX people_team_id_idx ON public.people (team_id);"},
		{"drop-column public.people.age", "ALTER TABLE public.people ADD COLUMN age integer;"},
	} {
		t.Run(strings.Fields(tc.directive)[0], func(t *testing.T) {
			t.Parallel()
			s, v := replayFixture(t, "app")
			plans := applyDirective(t, s, v, "app", tc.directive, tc.down)

			val, err := v.Validate(ctx, "app", plans, "")
			if err != nil {
				t.Fatalf("re-validating an applied directive: %v", err)
			}
			if len(val.Expansions) != 0 {
				t.Fatalf("an applied directive must not be expanded again: %+v", val.Expansions)
			}
			if len(val.Plans[1].Statements) != 0 {
				t.Fatalf("statements = %+v", val.Plans[1].Statements)
			}
			if len(val.Effects[1]) != 0 {
				t.Fatalf("an applied migration must leave the scratch alone: %v", val.Effects[1])
			}
		})
	}
}

func TestValidateExpandsWhereTheTargetIsBehind(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, v := replayFixture(t, "app", "other")
	plans := applyDirective(t, s, v, "app", "change-type public.people.age bigint batch=500", revertDown)

	val, err := v.Validate(ctx, "other", plans, "")
	if err != nil {
		t.Fatalf("a target still to apply the directive must expand it: %v", err)
	}
	if len(val.Expansions) != 1 || len(val.Plans[1].Statements) == 0 {
		t.Fatalf("expansions = %+v, plan = %+v", val.Expansions, val.Plans[1])
	}
}

// A baseline records the files with no expansion, so the replay leaves the directive unrendered; it is
// still history, and expanding it again against the live catalog is what this guards.
func TestValidateDoesNotReexpandBaselinedDirectives(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, v := replayFixture(t, "app")
	files := directiveFiles("add-not-null public.people.team_id", revertDown)
	if err := s.CreateBaseline(ctx, uuid.NewString(), "app", files, Provenance{}); err != nil {
		t.Fatal(err)
	}
	val, err := v.Validate(ctx, "app", upPlans(t, files), "")
	if err != nil {
		t.Fatalf("re-validating a baselined directive: %v", err)
	}
	if len(val.Expansions) != 0 {
		t.Fatalf("a baselined directive must not be expanded: %+v", val.Expansions)
	}
}

func TestDiffReplaysAppliedDirectives(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d, s, targetDSN := newDiffer(t, nil)
	v := NewValidator(d.pool, s, uuid.NewString)
	d.history = v
	execDSN(t, targetDSN, peopleUp)
	succeededRun(t, s, "app", peopleFiles(), nil)
	applyDirective(t, s, v, "app", "change-type public.people.age bigint batch=500", revertDown)

	out, err := d.Diff(ctx, "app", peopleUp, DiffBaseLive, nil)
	if err != nil {
		t.Fatalf("diff after a change-type: %v", err)
	}
	if !strings.Contains(strings.Join(out.Drift, "\n"), "age_old") {
		t.Fatalf("the replay must carry the frozen expansion, drift = %q", out.Drift)
	}
}
