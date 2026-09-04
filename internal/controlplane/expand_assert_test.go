package controlplane

import (
	"context"
	"strings"
	"testing"

	"github.com/SamuelMolling/godwit/internal/engine"
)

const ordersDDL = `CREATE TABLE public.orders (id bigint PRIMARY KEY, total numeric)`

const downNoop = "SELECT 1;\n"

const assertTotals = "-- godwit: assert 'SELECT count(*) FROM orders WHERE total IS NULL' = 0\n"

func TestExpandAssertBecomesOneCheckedStatement(t *testing.T) {
	t.Parallel()
	conn := newScratch(t, ordersDDL)
	exp, err := expandOne(t, conn, assertTotals, downNoop)
	if err != nil {
		t.Fatal(err)
	}
	if exp.Contract() != -1 || len(exp.Phase) != 1 || exp.Phase[0] != engine.PhaseExpand {
		t.Fatalf("phases = %v", exp.Phase)
	}
	if len(exp.Asserts) != 1 || exp.Asserts[0] == nil || exp.Asserts[0].String() != "= 0" {
		t.Fatalf("asserts = %+v", exp.Asserts)
	}
	for _, want := range []string{
		"-- godwit expanded: assert 'SELECT count(*) FROM orders WHERE total IS NULL' = 0",
		"SELECT count(*) FROM orders WHERE total IS NULL;",
	} {
		if !strings.Contains(exp.UpSQL, want) {
			t.Fatalf("expansion is missing %q:\n%s", want, exp.UpSQL)
		}
	}
	p, err := ExpandPlan(engine.Plan{Migration: directiveMigration(t, assertTotals, downNoop), Direction: engine.DirectionUp}, exp)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Statements) != 1 || p.Statements[0].Assert == nil {
		t.Fatalf("statements = %+v", p.Statements)
	}
	if p.Statements[0].Opaque == "" {
		t.Fatal("an assertion must keep the plan out of already-applied detection")
	}
}

// An assertion produces no contract block, so it may sit in a migration whose own SQL is destructive:
// that is the precondition shape, and it runs before the statement it guards.
func TestExpandAssertGuardsADestructiveStatement(t *testing.T) {
	t.Parallel()
	conn := newScratch(t, ordersDDL)
	up := "-- godwit: assert 'SELECT count(*) FROM orders' = 0\nDROP TABLE orders;\n"
	exp, err := expandOne(t, conn, up, downNoop)
	if err != nil {
		t.Fatal(err)
	}
	p, err := ExpandPlan(engine.Plan{Migration: directiveMigration(t, up, downNoop), Direction: engine.DirectionUp}, exp)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Statements) != 2 || p.Statements[0].Assert == nil || !strings.HasPrefix(p.Statements[1].SQL, "DROP TABLE") {
		t.Fatalf("statements = %+v", p.Statements)
	}
	if len(p.Statements[1].Hazards) != 1 {
		t.Fatal("the author's own DROP TABLE keeps its hazard")
	}
}

// A directive that does produce a contract block still refuses the destructive statement beside it.
func TestExpandAssertBesideChangeTypeKeepsTheDestructiveRefusal(t *testing.T) {
	t.Parallel()
	conn := newScratch(t, usersDDL)
	up := "-- godwit: assert 'SELECT count(*) FROM users' > 0\n" + changeAge + "DROP TABLE users;\n"
	_, err := expandOne(t, conn, up, downNoop)
	if err == nil || !strings.Contains(err.Error(), "H002") {
		t.Fatalf("err = %v", err)
	}
}

func TestExpandAssertRunsAtTheEndOfTheExpandPhase(t *testing.T) {
	t.Parallel()
	conn := newScratch(t, usersDDL)
	up := changeAge + "-- godwit: assert 'SELECT count(*) FROM users WHERE age_new IS NULL' = 0\n"
	exp, err := expandOne(t, conn, up, downNoop)
	if err != nil {
		t.Fatal(err)
	}
	contract := exp.Contract()
	if contract < 0 {
		t.Fatalf("phases = %v", exp.Phase)
	}
	at := -1
	for i, a := range exp.Asserts {
		if a != nil {
			at = i
		}
	}
	if at != contract-1 {
		t.Fatalf("assertion at %d, contract starts at %d", at, contract)
	}
}

func TestExpandAssertRefusals(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, up, down, want string }{
		{
			name: "no inverse",
			up:   assertTotals,
			down: revertSentinel,
			want: "an assertion has no inverse",
		},
		{
			name: "same assertion twice",
			up:   assertTotals + assertTotals,
			down: downNoop,
			want: "is already the subject of the directive",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			conn := newScratch(t, ordersDDL)
			_, err := expandOne(t, conn, tc.up, tc.down)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
		})
	}
}

// The comparison is not in the statement, so the expansion hash has to carry it beside the SQL.
func TestExpandAssertHashCoversTheComparison(t *testing.T) {
	t.Parallel()
	base := Expansion{
		ID: "1_m", UpSQL: "SELECT count(*) FROM orders;", Phase: []string{engine.PhaseExpand},
		Asserts: []*engine.AssertSpec{{Op: "=", Kind: engine.AssertInt, Value: "0"}},
	}
	other := base
	other.Asserts = []*engine.AssertSpec{{Op: ">", Kind: engine.AssertInt, Value: "0"}}
	if expansionHash(base) == expansionHash(other) {
		t.Fatal("two conditions over the same SQL must not hash alike")
	}
	plain := Expansion{ID: "1_m", UpSQL: base.UpSQL, Phase: base.Phase, Asserts: []*engine.AssertSpec{nil}}
	bare := Expansion{ID: "1_m", UpSQL: base.UpSQL, Phase: base.Phase}
	if expansionHash(plain) != expansionHash(bare) {
		t.Fatal("an expansion with no assertion hashes as it always did")
	}
}

func TestExpandAssertUnexpandedSpecIsIgnored(t *testing.T) {
	t.Parallel()
	exp := Expansion{ID: "1_m", UpSQL: "SELECT 1;", Phase: []string{engine.PhaseExpand}, Batches: []*engine.BatchSpec{nil}}
	if exp.assertAt(0) != nil {
		t.Fatal("an expansion frozen before assertions existed carries none")
	}
	p, err := ExpandPlan(engine.Plan{
		Migration: engine.Migration{Version: 1, Name: "m", UpSQL: "-- godwit: assert 'SELECT 1' = 1\n"},
		Direction: engine.DirectionUp,
	}, exp)
	if err != nil {
		t.Fatal(err)
	}
	if p.Statements[0].Assert != nil {
		t.Fatal("nothing to attach")
	}
}

func TestExpandAssertProbeAcceptsAnEmptyScratch(t *testing.T) {
	t.Parallel()
	conn := newScratch(t, ordersDDL)
	up := "-- godwit: assert 'SELECT count(*) FROM orders' > 0\n"
	exp, err := expandOne(t, conn, up, downNoop)
	if err != nil {
		t.Fatal(err)
	}
	p, err := ExpandPlan(engine.Plan{Migration: directiveMigration(t, up, downNoop), Direction: engine.DirectionUp}, exp)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := applyPlans(context.Background(), conn, engine.Options{}, []engine.Plan{p}, nil, engine.WithAssertProbe()); err != nil {
		t.Fatalf("the scratch has the schema and not the rows: %v", err)
	}
}

func TestExpandAssertProbeRefusesAnUnknownColumn(t *testing.T) {
	t.Parallel()
	conn := newScratch(t, ordersDDL)
	up := "-- godwit: assert 'SELECT count(*) FROM orders WHERE nope IS NULL' = 0\n"
	exp, err := expandOne(t, conn, up, downNoop)
	if err != nil {
		t.Fatal(err)
	}
	p, err := ExpandPlan(engine.Plan{Migration: directiveMigration(t, up, downNoop), Direction: engine.DirectionUp}, exp)
	if err != nil {
		t.Fatal(err)
	}
	_, err = applyPlans(context.Background(), conn, engine.Options{}, []engine.Plan{p}, nil, engine.WithAssertProbe())
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("err = %v", err)
	}
}

func TestAssertionBuilderRefusesABrokenDirective(t *testing.T) {
	t.Parallel()
	_, err := assertion(engine.Directive{Op: engine.DirectiveAssert, Line: 1, Args: []string{"SELECT 1"}})
	if err == nil || !strings.Contains(err.Error(), "takes 3 argument(s)") {
		t.Fatalf("err = %v", err)
	}
}
