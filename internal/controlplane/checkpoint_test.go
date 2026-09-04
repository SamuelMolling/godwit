package controlplane

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"
	"github.com/stripe/pg-schema-diff/pkg/diff"

	"github.com/SamuelMolling/godwit/internal/engine"
)

// longHistory is a directory of n versioned migrations, each adding a table and an index; it is what a
// checkpoint is for, and what the replay of a checkpointed target must stop replaying.
func longHistory(n int) map[string]string {
	files := map[string]string{}
	for i := 1; i <= n; i++ {
		id := fmt.Sprintf("2026010100%04d_t%d", i, i)
		files[id+".up.sql"] = fmt.Sprintf(
			"CREATE TABLE public.t%d (id bigint PRIMARY KEY, note text);\nCREATE INDEX t%d_note_idx ON public.t%d (note);", i, i, i)
		files[id+".down.sql"] = fmt.Sprintf("DROP TABLE public.t%d;", i)
	}

	return files
}

// churningHistory is a directory that builds and discards as it goes: what is left at the end is one
// table, and a checkpoint over it is a fraction of the statements the history ran to get there.
func churningHistory(n int) map[string]string {
	files := map[string]string{}
	for i := 1; i <= n; i++ {
		id := fmt.Sprintf("2026010100%04d_c%d", i, i)
		body := fmt.Sprintf(
			"CREATE TABLE public.c%d (id bigint PRIMARY KEY, note text);\nCREATE INDEX c%d_note_idx ON public.c%d (note);", i, i, i)
		if i > 1 {
			body += fmt.Sprintf("\nDROP TABLE public.c%d;", i-1)
		}
		files[id+".up.sql"] = body
		files[id+".down.sql"] = fmt.Sprintf("DROP TABLE public.c%d;", i)
	}

	return files
}

func newCheckpointer(t *testing.T) (*Checkpointer, *Store) {
	t.Helper()
	s, pool := newStore(t)
	if err := s.RegisterTarget(context.Background(), "app", "plain", map[string]string{"dsn": "x"}); err != nil {
		t.Fatal(err)
	}
	var seq int
	newID := func() string {
		seq++

		return fmt.Sprintf("%s%d", strings.ToLower(strings.ReplaceAll(t.Name(), "/", "")), seq)
	}

	return NewCheckpointer(NewScratch(pool, ""), newID), s
}

func mustCheckpoint(t *testing.T, c *Checkpointer, files map[string]string, at int64) Checkpoint {
	t.Helper()
	cp, err := c.Generate(context.Background(), files, at, "squash", time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	return cp
}

func TestCheckpointGeneratesAVerifiedSchema(t *testing.T) {
	t.Parallel()
	c, _ := newCheckpointer(t)
	files := longHistory(4)
	cp := mustCheckpoint(t, c, files, 0)

	if cp.Through != 20260101000004 || len(cp.Covers) != 4 || cp.Version != 20260601000000 {
		t.Fatalf("checkpoint = %+v", cp)
	}
	if !strings.HasPrefix(cp.Body, "-- godwit: checkpoint through=20260101000004\n") {
		t.Fatalf("body header = %q", firstLineOf(cp.Body))
	}
	migs, err := MigrationsFromFiles(map[string]string{cp.UpFile(): cp.Body})
	if err != nil || len(migs) != 1 || !migs[0].Checkpoint || migs[0].Through != cp.Through {
		t.Fatalf("the generated file must load as a checkpoint: %+v, err = %v", migs, err)
	}
	for i := 1; i <= 4; i++ {
		if !strings.Contains(cp.Body, fmt.Sprintf("t%d", i)) {
			t.Fatalf("the checkpoint must carry every collapsed table, missing t%d:\n%s", i, cp.Body)
		}
	}
}

// The checkpoint's body is generated from the scratch replay, so a directive below it lands as the SQL
// godwit expanded, not as the directive: nothing under a checkpoint is ever expanded again.
func TestCheckpointExpandsDirectivesBelowIt(t *testing.T) {
	t.Parallel()
	c, _ := newCheckpointer(t)
	files := directiveFiles("add-column public.people.nick text", revertDown)
	cp := mustCheckpoint(t, c, files, 0)
	if !strings.Contains(cp.Body, "nick") {
		t.Fatalf("the expansion must be baked into the checkpoint:\n%s", cp.Body)
	}
	if strings.Contains(cp.Body, "add-column") {
		t.Fatalf("the directive itself must not survive:\n%s", cp.Body)
	}
}

func TestCheckpointRefusals(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c, _ := newCheckpointer(t)

	if _, err := c.Generate(ctx, map[string]string{"nope.sql": "x"}, 0, "squash", time.Now()); !errors.Is(err, ErrMigrationFiles) {
		t.Fatalf("bad files = %v", err)
	}
	if _, err := c.Generate(ctx, longHistory(1), 0, "squash", time.Now()); !errors.Is(err, ErrCheckpoint) ||
		!strings.Contains(err.Error(), "at least two") {
		t.Fatalf("one migration = %v", err)
	}
	broken := longHistory(2)
	broken["20260101000002_t2.up.sql"] = "CREATE TABLE public.t1 (id int);"
	if _, err := c.Generate(ctx, broken, 0, "squash", time.Now()); !errors.Is(err, ErrMigrationFiles) {
		t.Fatalf("failing replay = %v", err)
	}

	cp := mustCheckpoint(t, c, longHistory(3), 0)
	with := longHistory(3)
	with[cp.UpFile()] = cp.Body
	if _, err := c.Generate(ctx, with, 0, "again", time.Now()); !errors.Is(err, ErrCheckpoint) ||
		!strings.Contains(err.Error(), "already collapses history") {
		t.Fatalf("second checkpoint = %v", err)
	}
}

// A directory stamped in the future must not produce a checkpoint that sorts below what it collapses.
func TestCheckpointVersionStaysAboveTheDirectory(t *testing.T) {
	t.Parallel()
	c, _ := newCheckpointer(t)
	files := map[string]string{
		"29990101000000_a.up.sql":   "CREATE TABLE public.a (id int);",
		"29990101000000_a.down.sql": "DROP TABLE public.a;",
		"29990101000001_b.up.sql":   "CREATE TABLE public.b (id int);",
		"29990101000001_b.down.sql": "DROP TABLE public.b;",
	}
	cp := mustCheckpoint(t, c, files, 0)
	if cp.Version != 29990101000002 {
		t.Fatalf("version = %d", cp.Version)
	}
}

func TestCheckpointAtAVersion(t *testing.T) {
	t.Parallel()
	c, _ := newCheckpointer(t)
	cp := mustCheckpoint(t, c, longHistory(4), 20260101000002)
	if cp.Through != 20260101000002 || len(cp.Covers) != 2 {
		t.Fatalf("checkpoint = %+v", cp)
	}
	if strings.Contains(cp.Body, "t3") {
		t.Fatalf("a checkpoint at a version must stop there:\n%s", cp.Body)
	}
}

func TestCheckpointVerificationRefusesAWrongSchema(t *testing.T) {
	t.Parallel()
	c, _ := newCheckpointer(t)
	factory := &scratchFactory{scratch: c.scratch, newID: c.newID}
	err := c.verify(context.Background(), factory, "CREATE TABLE public.other (id int);", "column public.t1.id bigint null=NO default=<none>")
	if !errors.Is(err, ErrCheckpoint) || !strings.Contains(err.Error(), "not the one the migrations produce") {
		t.Fatalf("err = %v", err)
	}
	err = c.verify(context.Background(), factory, "NOT SQL", "")
	if !errors.Is(err, ErrCheckpoint) || !strings.Contains(err.Error(), "does not plan") {
		t.Fatalf("err = %v", err)
	}
	err = c.verify(context.Background(), factory, "DROP TABLE public.gone;", "")
	if !errors.Is(err, ErrCheckpoint) || !strings.Contains(err.Error(), "does not apply") {
		t.Fatalf("err = %v", err)
	}
}

func firstLineOf(s string) string {
	line, _, _ := strings.Cut(s, "\n")

	return line
}

// checkpointedTarget records the history of a target that ran the whole directory and then took the
// checkpoint on top, which is what an existing target does the moment a checkpoint is merged.
func checkpointedTarget(t *testing.T, s *Store, files map[string]string, cp Checkpoint) {
	t.Helper()
	succeededRun(t, s, "app", files, nil)
	with := map[string]string{cp.UpFile(): cp.Body}
	for name, body := range files {
		with[name] = body
	}
	succeededRun(t, s, "app", with, nil)
}

func TestReplayStartsAtTheCheckpoint(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c, s := newCheckpointer(t)
	files := longHistory(6)
	cp := mustCheckpoint(t, c, files, 0)
	checkpointedTarget(t, s, files, cp)

	v := NewValidator(c.scratch, s, uuid.NewString)
	val, err := v.Validate(ctx, "app", nil, "")
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	plain, err := NewValidator(c.scratch, s, uuid.NewString).Validate(ctx, "app", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if val.Base != plain.Base {
		t.Fatal("the replay must be deterministic")
	}
	for i := 1; i <= 6; i++ {
		if !strings.Contains(val.Base, fmt.Sprintf("public.t%d.", i)) {
			t.Fatalf("the checkpoint must rebuild table t%d:\n%s", i, val.Base)
		}
	}
}

// The whole point: the replay of a checkpointed target executes the checkpoint and nothing below it.
func TestReplayCollapsesTheHistory(t *testing.T) {
	t.Parallel()
	steps := func(files map[string]string, cp *Checkpoint) ([]historyStep, []historyStep) {
		t.Helper()
		all := map[string]string{}
		for name, body := range files {
			all[name] = body
		}
		if cp != nil {
			all[cp.UpFile()] = cp.Body
		}
		migs, err := MigrationsFromFiles(all)
		if err != nil {
			t.Fatal(err)
		}
		var hist []HistoryMigration
		for _, m := range migs {
			hist = append(hist, HistoryMigration{ID: m.ID(), UpSQL: m.UpSQL, DownSQL: m.DownSQL})
		}
		hs, err := historySteps([]HistoryRun{{Migrations: hist}})
		if err != nil {
			t.Fatal(err)
		}

		return collapseAtCheckpoint(hs)
	}

	c, _ := newCheckpointer(t)
	files := longHistory(6)
	cp := mustCheckpoint(t, c, files, 0)

	ordered, collapsed := steps(files, &cp)
	if len(collapsed) != 6 || len(ordered) != 1 {
		t.Fatalf("ordered = %d, collapsed = %d; the replay must run the checkpoint alone", len(ordered), len(collapsed))
	}
	if !ordered[0].plan.Migration.Checkpoint {
		t.Fatal("the checkpoint must lead the replay")
	}
	if ordered, collapsed = steps(files, nil); len(collapsed) != 0 || len(ordered) != 6 {
		t.Fatalf("without a checkpoint nothing is collapsed: ordered = %d, collapsed = %d", len(ordered), len(collapsed))
	}
}

// The measured half of the same claim: a checkpointed target replays one migration where a plain one
// replays all of them, for the same schema. The wall clock is logged, not asserted: it is the reason
// the feature exists, but under -race on a shared container it is not a stable number.
func TestReplayGetsShorterWithACheckpoint(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c, s := newCheckpointer(t)
	files := churningHistory(24)
	cp := mustCheckpoint(t, c, files, 0)

	if err := s.RegisterTarget(ctx, "plain", "plain", map[string]string{"dsn": "x"}); err != nil {
		t.Fatal(err)
	}
	succeededRun(t, s, "plain", files, nil)
	checkpointedTarget(t, s, files, cp)

	v := NewValidator(c.scratch, s, uuid.NewString)
	whole, wholeAt := timeValidate(t, v, "plain")
	short, shortAt := timeValidate(t, v, "app")
	t.Logf("replay of 24 migrations: whole %d executed in %s, checkpointed %d executed (%d collapsed) in %s",
		whole.Replayed, wholeAt, short.Replayed, short.Collapsed, shortAt)
	if whole.Replayed != 24 || whole.Collapsed != 0 {
		t.Fatalf("a plain history replays whole: %d executed, %d collapsed", whole.Replayed, whole.Collapsed)
	}
	if short.Replayed != 1 || short.Collapsed != 24 {
		t.Fatalf("a checkpointed history replays the checkpoint alone: %d executed, %d collapsed", short.Replayed, short.Collapsed)
	}
	if whole.Base != short.Base {
		t.Fatalf("the two replays must produce the same schema:\n%s", strings.Join(engine.DiffSchemas(whole.Base, short.Base), "\n"))
	}
}

func timeValidate(t *testing.T, v *Validator, target string) (Validation, time.Duration) {
	t.Helper()
	start := time.Now()
	val, err := v.Validate(context.Background(), target, nil, "")
	if err != nil {
		t.Fatalf("validate %s: %v", target, err)
	}

	return val, time.Since(start)
}

// A repeatable is not collapsed: the checkpoint carries none of them, so every one still replays on top.
func TestReplayKeepsRepeatablesAboveTheCheckpoint(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c, s := newCheckpointer(t)
	files := longHistory(3)
	cp := mustCheckpoint(t, c, files, 0)
	files["R__view.up.sql"] = "CREATE OR REPLACE VIEW public.v AS SELECT id FROM public.t1;"
	files["R__view.down.sql"] = "DROP VIEW IF EXISTS public.v;"
	checkpointedTarget(t, s, files, cp)

	val, err := NewValidator(c.scratch, s, uuid.NewString).Validate(ctx, "app", nil, "")
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if !strings.Contains(val.Base, "public.v") {
		t.Fatalf("the repeatable must still build on top of the checkpoint:\n%s", val.Base)
	}
}

func TestValidateRefusesACheckpointGap(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c, s := newCheckpointer(t)
	files := longHistory(3)
	cp := mustCheckpoint(t, c, files, 0)
	succeededRun(t, s, "app", files, nil)
	revertRun(t, s, mostRecentRun(t, s, "app"))
	succeededRun(t, s, "app", map[string]string{
		"20260101000001_t1.up.sql":   files["20260101000001_t1.up.sql"],
		"20260101000001_t1.down.sql": files["20260101000001_t1.down.sql"],
	}, nil)

	_, err := NewValidator(c.scratch, s, uuid.NewString).Validate(ctx, "app",
		upPlans(t, map[string]string{cp.UpFile(): cp.Body}), "")
	if !errors.Is(err, engine.ErrCheckpointGap) {
		t.Fatalf("err = %v", err)
	}
}

func mostRecentRun(t *testing.T, s *Store, target string) string {
	t.Helper()
	run, err := s.RevertTarget(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}

	return run.ID
}

func TestRevertRefusedBelowACheckpoint(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c, s := newCheckpointer(t)
	files := longHistory(3)
	cp := mustCheckpoint(t, c, files, 0)
	checkpointedTarget(t, s, files, cp)

	cps, err := s.Checkpoints(ctx, "app")
	if err != nil || len(cps) != 1 || cps[0].Through != cp.Through {
		t.Fatalf("checkpoints = %+v, err = %v", cps, err)
	}
	last := mostRecentRun(t, s, "app")
	if _, err := s.PlanRevert(ctx, last); !errors.Is(err, ErrNotRevertable) ||
		!strings.Contains(err.Error(), "a checkpoint has no inverse") {
		t.Fatalf("reverting the checkpoint = %v", err)
	}
}

func TestRevertRefusedForACollapsedMigration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c, s := newCheckpointer(t)
	files := longHistory(3)
	cp := mustCheckpoint(t, c, files, 0)
	first := succeededRun(t, s, "app", files, nil)
	succeededRun(t, s, "app", withCheckpoint(files, cp), nil)

	if _, err := s.PlanRevert(ctx, first); !errors.Is(err, ErrNotRevertable) ||
		!strings.Contains(err.Error(), "cannot be reverted") {
		t.Fatalf("reverting below the checkpoint = %v", err)
	}
}

func withCheckpoint(files map[string]string, cp Checkpoint) map[string]string {
	out := map[string]string{cp.UpFile(): cp.Body}
	for name, body := range files {
		out[name] = body
	}

	return out
}

func TestCheckpointsStoreErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mock, s := newMockStore(t)

	mock.ExpectQuery("ORDER BY a.migration DESC").WithArgs(anyArgs(2)...).WillReturnError(errBoom)
	if _, err := s.Checkpoints(ctx, "app"); err == nil || !strings.Contains(err.Error(), "list checkpoints") {
		t.Fatalf("err = %v", err)
	}
	mock.ExpectQuery("ORDER BY a.migration DESC").WithArgs(anyArgs(2)...).WillReturnRows(
		pgxmock.NewRows([]string{"migration", "body"}).AddRow("20260101000000_a", "SELECT 1;").RowError(0, errBoom))
	if _, err := s.Checkpoints(ctx, "app"); err == nil || !strings.Contains(err.Error(), "read checkpoints") {
		t.Fatalf("err = %v", err)
	}
	// A body that mentions the marker without being a checkpoint is not one.
	mock.ExpectQuery("ORDER BY a.migration DESC").WithArgs(anyArgs(2)...).WillReturnRows(
		pgxmock.NewRows([]string{"migration", "body"}).AddRow("20260101000000_a", "SELECT 'godwit: checkpoint';"))
	if cps, err := s.Checkpoints(ctx, "app"); err != nil || len(cps) != 0 {
		t.Fatalf("cps = %+v, err = %v", cps, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// forced makes every scratch database of a Checkpointer take the same name, so the second create collides.
func forced(c *Checkpointer, name string) {
	c.newID = func() string { return name }
}

func TestCheckpointScratchFailures(t *testing.T) {
	ctx := context.Background()
	files := longHistory(2)

	t.Run("replay database", func(t *testing.T) {
		c, _ := newCheckpointer(t)
		forced(c, "clash")
		if _, err := c.scratch.pool.Exec(ctx, "CREATE DATABASE godwit_diff_clash"); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Generate(ctx, files, 0, "squash", time.Now()); err == nil ||
			!strings.Contains(err.Error(), "create scratch database") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("connect", func(t *testing.T) {
		c, _ := newCheckpointer(t)
		orig := connectScratch
		connectScratch = func(context.Context, *pgx.ConnConfig) (*pgx.Conn, error) { return nil, errBoom }
		defer func() { connectScratch = orig }()
		if _, err := c.Generate(ctx, files, 0, "squash", time.Now()); err == nil ||
			!strings.Contains(err.Error(), "connect scratch database") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("snapshot", func(t *testing.T) {
		c, _ := newCheckpointer(t)
		orig := snapshotScratch
		snapshotScratch = func(context.Context, engine.DB) (string, string, error) { return "", "", errBoom }
		defer func() { snapshotScratch = orig }()
		if _, err := c.Generate(ctx, files, 0, "squash", time.Now()); err == nil ||
			!strings.Contains(err.Error(), "snapshot scratch database") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("render", func(t *testing.T) {
		c, _ := newCheckpointer(t)
		orig := generatePlan
		generatePlan = func(context.Context, diff.SchemaSource, diff.SchemaSource, ...diff.PlanOpt) (diff.Plan, error) {
			return diff.Plan{}, errBoom
		}
		defer func() { generatePlan = orig }()
		if _, err := c.Generate(ctx, files, 0, "squash", time.Now()); !errors.Is(err, ErrCheckpoint) ||
			!strings.Contains(err.Error(), "render the schema as DDL") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("verify", func(t *testing.T) {
		c, _ := newCheckpointer(t)
		orig := generatePlan
		generatePlan = func(context.Context, diff.SchemaSource, diff.SchemaSource, ...diff.PlanOpt) (diff.Plan, error) {
			return diff.Plan{Statements: []diff.Statement{{DDL: "CREATE TABLE public.wrong (id int)"}}}, nil
		}
		defer func() { generatePlan = orig }()
		if _, err := c.Generate(ctx, files, 0, "squash", time.Now()); !errors.Is(err, ErrCheckpoint) ||
			!strings.Contains(err.Error(), "not the one the migrations produce") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestCheckpointVerifyScratchFailures(t *testing.T) {
	ctx := context.Background()
	c, _ := newCheckpointer(t)
	forced(c, "vclash")
	factory := &scratchFactory{scratch: c.scratch, newID: c.newID}
	if _, err := c.scratch.pool.Exec(ctx, "CREATE DATABASE godwit_diff_vclash"); err != nil {
		t.Fatal(err)
	}
	if err := c.verify(ctx, factory, "SELECT 1;", ""); err == nil || !strings.Contains(err.Error(), "create scratch database") {
		t.Fatalf("create = %v", err)
	}
	if _, err := c.scratch.pool.Exec(ctx, "DROP DATABASE godwit_diff_vclash"); err != nil {
		t.Fatal(err)
	}

	orig := connectScratch
	connectScratch = func(context.Context, *pgx.ConnConfig) (*pgx.Conn, error) { return nil, errBoom }
	if err := c.verify(ctx, factory, "SELECT 1;", ""); err == nil || !strings.Contains(err.Error(), "connect scratch database") {
		t.Fatalf("connect = %v", err)
	}
	connectScratch = orig

	snap := snapshotScratch
	snapshotScratch = func(context.Context, engine.DB) (string, string, error) { return "", "", errBoom }
	defer func() { snapshotScratch = snap }()
	if err := c.verify(ctx, factory, "SELECT 1;", ""); err == nil || !strings.Contains(err.Error(), "snapshot scratch database") {
		t.Fatalf("snapshot = %v", err)
	}
}

// A directive under the checkpoint that cannot be expanded stops the generation with its own error.
func TestCheckpointRefusesAnUnexpandableDirective(t *testing.T) {
	t.Parallel()
	c, _ := newCheckpointer(t)
	files := longHistory(2)
	files["20260101000003_d.up.sql"] = "-- godwit: change-type public.gone.age bigint\n"
	files["20260101000003_d.down.sql"] = revertDown
	if _, err := c.Generate(context.Background(), files, 0, "squash", time.Now()); err == nil ||
		!strings.Contains(err.Error(), "gone") {
		t.Fatalf("err = %v", err)
	}
}

// A file that loads but does not parse is refused before anything reaches the scratch database.
func TestCheckpointRefusesAnUnparseableMigration(t *testing.T) {
	t.Parallel()
	c, _ := newCheckpointer(t)
	files := longHistory(2)
	files["20260101000002_t2.up.sql"] = "NOT SQL;"
	if _, err := c.Generate(context.Background(), files, 0, "squash", time.Now()); !errors.Is(err, ErrMigrationFiles) {
		t.Fatalf("err = %v", err)
	}
}

type appliedFails struct{ Engine }

func (appliedFails) Applied(context.Context, string) ([]engine.Applied, []engine.Repeatable, error) {
	return nil, nil, errBoom
}

// The scheduler decides what the checkpoint does against the target's own history, so a run it cannot
// place fails there rather than half-applying.
func TestSchedulerRefusesACheckpointGap(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, pool := newStore(t)
	sched, targetDSN := newScheduler(t, s, Config{Holder: "h", MaxAttempts: 1})
	var seq int
	c := NewCheckpointer(NewScratch(pool, ""), func() string {
		seq++

		return fmt.Sprintf("%s%d", strings.ToLower(t.Name()), seq)
	})
	files := longHistory(3)
	cp := mustCheckpoint(t, c, files, 0)

	first := "cccccccc-0000-0000-0000-000000000001"
	queueRun(t, s, first, map[string]string{
		"20260101000001_t1.up.sql":   files["20260101000001_t1.up.sql"],
		"20260101000001_t1.down.sql": files["20260101000001_t1.down.sql"],
	})
	sched.Tick(ctx)
	waitState(t, s, first, StateSucceeded)

	second := "cccccccc-0000-0000-0000-000000000002"
	queueRun(t, s, second, map[string]string{cp.UpFile(): cp.Body})
	sched.Tick(ctx)
	if run := waitState(t, s, second, StateFailed); !strings.Contains(run.Error, "not in the migration directory") {
		t.Fatalf("error = %q", run.Error)
	}
	_ = targetDSN

	sched.engine = appliedFails{sched.engine}
	if _, err := sched.shapeCheckpoint(ctx, upPlans(t, map[string]string{cp.UpFile(): cp.Body}), "x"); !errors.Is(err, errBoom) {
		t.Fatalf("err = %v", err)
	}
}

// A repeatable has no version, so a checkpoint never bars its revert.
func TestCheckpointBarSkipsRepeatables(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c, s := newCheckpointer(t)
	files := longHistory(3)
	cp := mustCheckpoint(t, c, files, 0)
	checkpointedTarget(t, s, files, cp)

	if err := s.checkpointBar(ctx, "app", []RunMigration{{Migration: "R__view"}}); err != nil {
		t.Fatalf("a repeatable has no version to compare: %v", err)
	}
	if err := s.checkpointBar(ctx, "app", []RunMigration{{Migration: "20260101000001_t1"}}); !errors.Is(err, ErrNotRevertable) {
		t.Fatalf("err = %v", err)
	}
}

// Replay is the diff's files base; a set it cannot place under the checkpoint is refused there too.
func TestReplayRefusesACheckpointGap(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c, s := newCheckpointer(t)
	files := longHistory(3)
	cp := mustCheckpoint(t, c, files, 0)
	succeededRun(t, s, "app", map[string]string{
		"20260101000001_t1.up.sql":   files["20260101000001_t1.up.sql"],
		"20260101000001_t1.down.sql": files["20260101000001_t1.down.sql"],
	}, nil)

	conn, err := connectScratch(ctx, c.scratch.connConfig(c.scratch.pool.Config().ConnConfig.Database, ""))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	err = NewValidator(c.scratch, s, uuid.NewString).Replay(ctx, conn, "app", "",
		upPlans(t, map[string]string{cp.UpFile(): cp.Body}))
	if !errors.Is(err, engine.ErrCheckpointGap) {
		t.Fatalf("err = %v", err)
	}
}

// The replay records what a checkpoint collapsed on the scratch database; a connection that cannot take
// those rows stops the replay instead of leaving a history the plans would contradict.
func TestReplayReportsAFailedCollapse(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c, s := newCheckpointer(t)
	files := longHistory(3)
	cp := mustCheckpoint(t, c, files, 0)
	checkpointedTarget(t, s, files, cp)

	conn, err := connectScratch(ctx, c.scratch.connConfig(c.scratch.pool.Config().ConnConfig.Database, ""))
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close(ctx)
	if err := NewValidator(c.scratch, s, uuid.NewString).Replay(ctx, conn, "app", "", nil); err == nil ||
		!strings.Contains(err.Error(), "bootstrap godwit schema") {
		t.Fatalf("err = %v", err)
	}
}
