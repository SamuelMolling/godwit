package controlplane

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/SamuelMolling/godwit/internal/engine"
)

// appliedOutside puts migrations into a target's own journal the way another tool, another godwit
// instance, or `godwit apply` does: the target records them and this control plane never saw it.
func appliedOutside(t *testing.T, dsn string, files map[string]string) []engine.Migration {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	plans := upPlans(t, files)
	if _, err := applyPlans(ctx, conn, engine.Options{}, plans, nil); err != nil {
		t.Fatal(err)
	}

	return migrationsOf(plans)
}

func migrationsOf(plans []engine.Plan) []engine.Migration {
	out := make([]engine.Migration, 0, len(plans))
	for _, p := range plans {
		out = append(out, p.Migration)
	}

	return out
}

func journalIDs(t *testing.T, dsn string) []string {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	journal, err := engine.Journal(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(journal))
	for id := range journal {
		ids = append(ids, id)
	}
	slices.Sort(ids)

	return ids
}

// A run that meets a migration the target's own journal already records must put it on the control
// plane's books. Before this, the run succeeded and the ledger stayed empty for ever.
func TestRunAdoptsWhatTheTargetAlreadyRecords(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newStore(t)
	sched, targetDSN := newScheduler(t, s, Config{Holder: "h1", MaxAttempts: 1})

	files := twoTableFiles()
	appliedOutside(t, targetDSN, files)
	if applied, err := s.Applied(ctx, "app"); err != nil || len(applied.Versions) != 0 {
		t.Fatalf("the ledger starts blind to an out-of-band apply: %+v, %v", applied, err)
	}

	id := "aaaaaaaa-0000-0000-0000-000000000001"
	queueRun(t, s, id, files)
	sched.Tick(ctx)
	waitState(t, s, id, StateSucceeded)

	applied, err := s.Applied(ctx, "app")
	if err != nil || !slices.Equal(applied.Versions, []int64{20260101000000, 20260101000001}) {
		t.Fatalf("applied = %+v, err = %v", applied, err)
	}
	targets, err := s.ListTargets(ctx, time.Time{})
	if err != nil || targets[0].AppliedCount != 2 {
		t.Fatalf("applied count = %+v, err = %v", targets, err)
	}
	rows, err := s.AppliedMigrations(ctx, id)
	if err != nil || len(rows) != 2 || !rows[0].Adopted || !rows[1].Adopted {
		t.Fatalf("ledger = %+v, err = %v", rows, err)
	}

	// An adopted row is what the run found, not what it applied, so it is out of the revert's scope.
	if _, err := s.PlanRevert(ctx, id); !errors.Is(err, ErrNotRevertable) ||
		!strings.Contains(err.Error(), "applied no migration that still stands") {
		t.Fatalf("revert of an adoption-only run: %v", err)
	}

	// A second run over the same directory adopts nothing: the rows already stand.
	second := "aaaaaaaa-0000-0000-0000-000000000002"
	queueRun(t, s, second, files)
	sched.Tick(ctx)
	waitState(t, s, second, StateSucceeded)
	if rows, err := s.AppliedMigrations(ctx, second); err != nil || len(rows) != 0 {
		t.Fatalf("second run's ledger = %+v, err = %v", rows, err)
	}
}

// The revert undoes what the run applied and leaves what it merely found.
func TestRevertLeavesAdoptedMigrations(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newStore(t)
	sched, targetDSN := newScheduler(t, s, Config{Holder: "h1", MaxAttempts: 1})

	first := map[string]string{
		"20260101000000_a.up.sql":   "CREATE TABLE public.a (id int);",
		"20260101000000_a.down.sql": "DROP TABLE public.a;",
	}
	appliedOutside(t, targetDSN, first)

	id := "cccccccc-0000-0000-0000-000000000001"
	queueRun(t, s, id, twoTableFiles())
	sched.Tick(ctx)
	waitState(t, s, id, StateSucceeded)

	rp, err := s.PlanRevert(ctx, id)
	if err != nil || len(rp.Applied) != 1 || rp.Applied[0].Migration != "20260101000001_b" {
		t.Fatalf("revert plan = %+v, err = %v", rp, err)
	}
}

// A baseline over a target another instance already journalled is the adoption case that mattered:
// it used to refuse outright.
func TestBaselineAdoptsAJournalledTarget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newStore(t)
	sched, targetDSN := newScheduler(t, s, Config{Holder: "h1", MaxAttempts: 1})

	files := twoTableFiles()
	migs := appliedOutside(t, targetDSN, files)

	b := NewBaseliner(sched)
	if err := b.Baseline(ctx, uuid.NewString(), "app", migs, Provenance{}); err != nil {
		t.Fatalf("baselining a journalled target: %v", err)
	}
	applied, err := s.Applied(ctx, "app")
	if err != nil || !slices.Equal(applied.Versions, []int64{20260101000000, 20260101000001}) {
		t.Fatalf("applied = %+v, err = %v", applied, err)
	}
	if err := b.Baseline(ctx, uuid.NewString(), "app", migs, Provenance{}); !errors.Is(err, engine.ErrAlreadyMigrated) {
		t.Fatalf("second baseline: %v", err)
	}

	edited := slices.Clone(migs)
	edited[0].Checksum = "not-what-the-target-recorded"
	if err := b.Baseline(ctx, uuid.NewString(), "app", edited, Provenance{}); !errors.Is(err, engine.ErrHistoryConflict) {
		t.Fatalf("baseline over edited content: %v", err)
	}
}

// The store is rebuilt or restored from a backup older than the target: the targets still know what
// they hold, and reconciling reads it back so the replay can rebuild it.
func TestReconcileRepairsALostLedger(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, pool := newStore(t)
	sched, targetDSN := newScheduler(t, s, Config{Holder: "h1", MaxAttempts: 1})

	files := twoTableFiles()
	migs := appliedOutside(t, targetDSN, files)
	r := NewReconciler(sched)

	d, err := r.Reconcile(ctx, uuid.NewString(), "app", migs, Provenance{})
	if err != nil || !slices.Equal(d.Adopt, []string{"20260101000000_a", "20260101000001_b"}) {
		t.Fatalf("divergence = %+v, err = %v", d, err)
	}
	applied, err := s.Applied(ctx, "app")
	if err != nil || !slices.Equal(applied.Versions, []int64{20260101000000, 20260101000001}) {
		t.Fatalf("applied = %+v, err = %v", applied, err)
	}
	if d, err := r.Reconcile(ctx, uuid.NewString(), "app", migs, Provenance{}); err != nil || len(d.Adopt) != 0 {
		t.Fatalf("second reconcile = %+v, err = %v", d, err)
	}

	// The point of the repair: the scratch replay can rebuild the target again, so a later migration
	// validates on top of what the target really holds.
	v := NewValidator(NewScratch(pool, ""), s, uuid.NewString)
	next := map[string]string{
		"20260101000002_c.up.sql":   "ALTER TABLE public.a ADD COLUMN name text;",
		"20260101000002_c.down.sql": "ALTER TABLE public.a DROP COLUMN name;",
	}
	val, err := v.Validate(ctx, "app", upPlans(t, next), "")
	if err != nil {
		t.Fatalf("validating on an adopted history: %v", err)
	}
	if !strings.Contains(val.Base, "public.a.id") || !strings.Contains(val.Base, "public.b.id") {
		t.Fatalf("the replay must rebuild what the target holds: %s", val.Base)
	}
}

func TestReconcileRefusesADivergence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newStore(t)
	sched, targetDSN := newScheduler(t, s, Config{Holder: "h1", MaxAttempts: 1})
	r := NewReconciler(sched)

	files := twoTableFiles()
	migs := appliedOutside(t, targetDSN, files)

	if _, err := r.Reconcile(ctx, uuid.NewString(), "app", migs[:1], Provenance{}); !errors.Is(err, ErrDiverged) ||
		!strings.Contains(err.Error(), "absent from the directory: 20260101000001_b") {
		t.Fatalf("directory missing a recorded migration: %v", err)
	}

	edited := slices.Clone(migs)
	edited[0].Checksum = "elsewhere"
	if _, err := r.Reconcile(ctx, uuid.NewString(), "app", edited, Provenance{}); !errors.Is(err, ErrDiverged) ||
		!strings.Contains(err.Error(), "different content than the directory carries: 20260101000000_a") {
		t.Fatalf("conflicting checksum: %v", err)
	}

	if _, err := r.Reconcile(ctx, uuid.NewString(), "app", migs, Provenance{}); err != nil {
		t.Fatal(err)
	}
	execTarget(t, targetDSN, "DELETE FROM godwit.migrations WHERE version = 20260101000001")
	if _, err := r.Reconcile(ctx, uuid.NewString(), "app", migs, Provenance{}); !errors.Is(err, ErrDiverged) ||
		!strings.Contains(err.Error(), "absent from the target: 20260101000001") {
		t.Fatalf("ledger ahead of the target: %v", err)
	}
	if ids := journalIDs(t, targetDSN); !slices.Equal(ids, []string{"20260101000000_a"}) {
		t.Fatalf("a reconcile never writes to the target: %v", ids)
	}
}
