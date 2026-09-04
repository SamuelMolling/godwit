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

func succeededRun(t *testing.T, s *Store, target string, files map[string]string, exps map[string]Expansion) string {
	t.Helper()
	ctx := context.Background()
	id := uuid.NewString()
	if err := s.CreateRun(ctx, id, target, RolloutExpandContract, files, Timeouts{}, Provenance{}, "", exps); err != nil {
		t.Fatal(err)
	}
	migs, err := MigrationsFromFiles(files)
	if err != nil {
		t.Fatal(err)
	}
	applied, err := s.Applied(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migs {
		if applied.Has(m) {
			continue
		}
		var exp *Expansion
		if e, ok := exps[m.ID()]; ok {
			exp = &e
		}
		if err := s.RecordApplied(ctx, id, m.ID(), false, exp); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Finish(ctx, id, StateSucceeded, ""); err != nil {
		t.Fatal(err)
	}

	return id
}

// revertRun undoes every migration run id applied, the way a succeeded revert run leaves the store.
func revertRun(t *testing.T, s *Store, id string) {
	t.Helper()
	ctx := context.Background()
	orig, err := s.Run(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	applied, err := s.AppliedMigrations(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	rev := uuid.NewString()
	if err := s.CreateRevert(ctx, rev, orig, true, Timeouts{}, Provenance{}); err != nil {
		t.Fatal(err)
	}
	for _, m := range applied {
		if err := s.MarkReverted(ctx, id, rev, m.Migration); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Finish(ctx, rev, StateSucceeded, ""); err != nil {
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

	return s, NewValidator(NewScratch(pool, ""), s, uuid.NewString)
}

// applyDirective validates the directive while it is still pending, then records it as a succeeded run
// carrying the expansion that run froze — the history a later validation replays.
func applyDirective(t *testing.T, s *Store, v *Validator, directive, down string) (string, []engine.Plan) {
	t.Helper()
	const target = "app"
	files := directiveFiles(directive, down)
	plans := upPlans(t, files)
	val, err := v.Validate(context.Background(), target, plans, "")
	if err != nil {
		t.Fatalf("%s must expand while pending: %v", directive, err)
	}
	if len(val.Expansions) != 1 {
		t.Fatalf("%s expansions = %+v", directive, val.Expansions)
	}
	return succeededRun(t, s, target, files, val.Expansions), plans
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
			_, plans := applyDirective(t, s, v, tc.directive, tc.down)

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
	_, plans := applyDirective(t, s, v, "change-type public.people.age bigint batch=500", revertDown)

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

func withShops(files map[string]string) map[string]string {
	files["20260103000000_shops.up.sql"] = "CREATE TABLE public.shops (id bigint PRIMARY KEY);"
	files["20260103000000_shops.down.sql"] = "DROP TABLE public.shops;"

	return files
}

// Every run submits the whole directory, so a later run carries files it never applied. The replay must
// read what each run applied and still stands, or a reverted migration comes back on the scratch.
func TestReplayLeavesOutARevertedMigration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, pool := newStore(t)
	if err := s.RegisterTarget(ctx, "app", "plain", map[string]string{"dsn": "x"}); err != nil {
		t.Fatal(err)
	}
	v := NewValidator(NewScratch(pool, ""), s, uuid.NewString)
	first := succeededRun(t, s, "app", peopleFiles(), nil)
	succeededRun(t, s, "app", withShops(peopleFiles()), nil)
	revertRun(t, s, first)

	applied, err := s.Applied(ctx, "app")
	if err != nil || len(applied.Versions) != 1 {
		t.Fatalf("the ledger must hold only the shops migration: %+v, err = %v", applied, err)
	}
	val, err := v.Validate(ctx, "app", upPlans(t, peopleFiles()), "")
	if err != nil {
		t.Fatalf("validating the reverted migration again: %v", err)
	}
	if strings.Contains(val.Base, "people") {
		t.Fatalf("the replay rebuilt a reverted migration: %s", val.Base)
	}
	if len(val.Effects[0]) == 0 {
		t.Fatal("a reverted migration must replay as pending, with its effect")
	}
}

// #59 freezes an applied directive and never expands it again. A reverted one is not applied, and the
// later run that merely carried its file must not put it back into the replayed set.
func TestReplayReexpandsARevertedDirective(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, v := replayFixture(t, "app")
	id, plans := applyDirective(t, s, v, "change-type public.people.age bigint batch=500", revertDown)
	succeededRun(t, s, "app", withShops(directiveFiles("change-type public.people.age bigint batch=500", revertDown)), nil)
	revertRun(t, s, id)

	val, err := v.Validate(ctx, "app", plans, "")
	if err != nil {
		t.Fatalf("validating the reverted directive again: %v", err)
	}
	if len(val.Expansions) != 1 {
		t.Fatalf("a reverted directive must be expanded again: %+v", val.Expansions)
	}
	if len(val.Plans[1].Statements) == 0 {
		t.Fatal("a reverted directive must plan statements, not replay as history")
	}
}

func TestDiffReplaysAppliedDirectives(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d, s, targetDSN := newDiffer(t, nil)
	v := NewValidator(d.scratch, s, uuid.NewString)
	d.history = v
	execDSN(t, targetDSN, peopleUp)
	succeededRun(t, s, "app", peopleFiles(), nil)
	applyDirective(t, s, v, "change-type public.people.age bigint batch=500", revertDown)

	out, err := d.Diff(ctx, "app", peopleUp, DiffBaseLive, nil)
	if err != nil {
		t.Fatalf("diff after a change-type: %v", err)
	}
	if !strings.Contains(strings.Join(out.Drift, "\n"), "age_old") {
		t.Fatalf("the replay must carry the frozen expansion, drift = %q", out.Drift)
	}
}
