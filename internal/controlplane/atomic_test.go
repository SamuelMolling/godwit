package controlplane

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/SamuelMolling/godwit/internal/engine"
)

func TestEveryStoreMigrationRunsInOneTransaction(t *testing.T) {
	t.Parallel()

	for _, m := range storeMigrations {
		p, err := engine.BuildPlan(m, engine.DirectionUp)
		if err != nil {
			t.Fatal(err)
		}
		if !p.Transactional() {
			t.Errorf("%s applies statement by statement at start-up, and a failure part way through it "+
				"leaves the store half-migrated for every replica that boots after", m.ID())
		}
	}
}

func TestStoreMigrationRollsBackWhenAStatementFails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn := newScratch(t)

	i := slices.IndexFunc(storeMigrations, func(m engine.Migration) bool { return m.Name == "run_order" })
	if i < 0 {
		t.Fatal("the run_order migration is what this test is about")
	}
	runOrder := storeMigrations[i : i+1]
	p, err := engine.BuildPlan(storeMigrations[i], engine.DirectionUp)
	if err != nil {
		t.Fatal(err)
	}
	setNotNull := slices.IndexFunc(p.Statements, func(st engine.Statement) bool {
		return strings.Contains(st.SQL, "SET NOT NULL")
	})
	if setNotNull <= 0 {
		t.Fatalf("run_order no longer backfills before a SET NOT NULL: %+v", p.Statements)
	}
	if _, err := applyMigrations(ctx, conn, storeMigrations[:i]); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx,
		`INSERT INTO cp_targets (name, provider, config) VALUES ('app', 'static', '{}')`); err != nil {
		t.Fatal(err)
	}
	queue := func(state string) {
		t.Helper()
		if _, err := conn.Exec(ctx,
			`INSERT INTO cp_runs (id, target, state) VALUES ($1, 'app', $2)`, uuid.NewString(), state); err != nil {
			t.Error(err)
		}
	}
	queue(StateSucceeded)

	var raced bool
	hook := func(point engine.HookPoint, idx int) {
		if point != engine.HookBeforeStatement || idx != setNotNull || raced {
			return
		}
		raced = true
		queue(StateQueued)
	}
	if _, err := applyMigrations(ctx, conn, runOrder, engine.WithHook(hook)); err == nil {
		t.Fatal("a run written after the backfill carries no seq and the SET NOT NULL has to refuse it")
	}
	if !raced {
		t.Fatal("the race was never injected")
	}

	var column, recorded, journalled bool
	if err := conn.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM pg_attribute
			WHERE attrelid = 'cp_runs'::regclass AND attname = 'seq' AND NOT attisdropped),
		       EXISTS (SELECT 1 FROM godwit.migrations WHERE version = $1),
		       EXISTS (SELECT 1 FROM godwit.journal)`,
		storeMigrations[i].Version).Scan(&column, &recorded, &journalled); err != nil {
		t.Fatal(err)
	}
	if column || recorded || journalled {
		t.Fatalf("column = %t, recorded = %t, journalled = %t; the failed migration left work behind",
			column, recorded, journalled)
	}

	if n, err := applyMigrations(ctx, conn, runOrder); err != nil || n != 1 {
		t.Fatalf("applied = %d, err = %v; the next start has to apply it whole", n, err)
	}
	var notNull, unique bool
	if err := conn.QueryRow(ctx, `
		SELECT bool_or(a.attnotnull), EXISTS (SELECT 1 FROM pg_index x
			WHERE x.indrelid = 'cp_runs'::regclass AND x.indisunique AND x.indkey::text = a.attnum::text)
		FROM pg_attribute a WHERE a.attrelid = 'cp_runs'::regclass AND a.attname = 'seq'
		GROUP BY a.attnum`).Scan(&notNull, &unique); err != nil {
		t.Fatal(err)
	}
	if !notNull || !unique {
		t.Fatalf("seq not null = %t, unique = %t", notNull, unique)
	}
}
