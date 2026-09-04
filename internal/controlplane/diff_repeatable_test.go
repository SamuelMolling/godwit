package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/stdlib"

	"github.com/SamuelMolling/godwit/internal/engine"
)

const (
	totalsUp   = "CREATE OR REPLACE VIEW t_totals AS SELECT id FROM t;"
	totalsDown = "DROP VIEW IF EXISTS t_totals;"
	tableOnly  = "CREATE TABLE t (id int);"
)

func repeatableFiles(up string) map[string]string {
	files := goodFiles()
	files["R__t_totals.up.sql"] = up
	files["R__t_totals.down.sql"] = totalsDown

	return files
}

func differWithRepeatable(t *testing.T, id string, files map[string]string) *Differ {
	t.Helper()
	ctx := context.Background()
	d, s, _ := newDiffer(t, nil)
	d.history = NewValidator(d.scratch, s, d.newID)
	queueRun(t, s, id, files)
	d.sched.Tick(ctx)
	waitState(t, s, id, StateSucceeded)

	return d
}

func TestDifferKeepsWhatARepeatableDeclares(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	files := repeatableFiles(totalsUp)
	d := differWithRepeatable(t, "eeeeeeee-0000-0000-0000-000000000001", files)

	live, err := d.Diff(ctx, "app", tableOnly, DiffBaseLive, files)
	if err != nil {
		t.Fatal(err)
	}
	if live.UpSQL != "" || live.DownSQL != "" {
		t.Fatalf("live base proposed a change: up = %q, down = %q", live.UpSQL, live.DownSQL)
	}
	if len(live.RepeatableObjects) != 1 || live.RepeatableObjects[0] != "public.t_totals" {
		t.Fatalf("repeatable objects = %v", live.RepeatableObjects)
	}

	fromFiles, err := d.Diff(ctx, "app", tableOnly, DiffBaseFiles, files)
	if err != nil || fromFiles.UpSQL != "" {
		t.Fatalf("files base: up = %q, err = %v", fromFiles.UpSQL, err)
	}
}

func TestDifferDropsWhatAnEditedRepeatableLeft(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := differWithRepeatable(t, "eeeeeeee-0000-0000-0000-000000000002", repeatableFiles(totalsUp))

	renamed := repeatableFiles("CREATE OR REPLACE VIEW t_summary AS SELECT id FROM t;")
	out, err := d.Diff(ctx, "app", tableOnly, DiffBaseLive, renamed)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.UpSQL, `DROP VIEW "public"."t_totals"`) {
		t.Fatalf("the orphaned view must be dropped:\n%s", out.UpSQL)
	}
	if len(out.RepeatableObjects) != 1 || out.RepeatableObjects[0] != "public.t_summary" {
		t.Fatalf("repeatable objects = %v", out.RepeatableObjects)
	}
}

func TestDifferDropsWhatADeletedRepeatableLeft(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := differWithRepeatable(t, "eeeeeeee-0000-0000-0000-000000000003", repeatableFiles(totalsUp))

	out, err := d.Diff(ctx, "app", tableOnly, DiffBaseLive, goodFiles())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.UpSQL, `DROP VIEW "public"."t_totals"`) {
		t.Fatalf("a view no R__ file declares must be dropped:\n%s", out.UpSQL)
	}
	if len(out.RepeatableObjects) != 0 {
		t.Fatalf("repeatable objects = %v", out.RepeatableObjects)
	}
}

func TestDifferKeepsAnObjectARepeatableTookOver(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	versioned := goodFiles()
	versioned["20260901120001_totals.up.sql"] = "CREATE VIEW t_totals AS SELECT id FROM t;"
	versioned["20260901120001_totals.down.sql"] = totalsDown
	d := differWithRepeatable(t, "eeeeeeee-0000-0000-0000-000000000004", versioned)

	takenOver := versioned
	takenOver["R__t_totals.up.sql"] = totalsUp
	takenOver["R__t_totals.down.sql"] = totalsDown
	out, err := d.Diff(ctx, "app", tableOnly, DiffBaseLive, takenOver)
	if err != nil {
		t.Fatal(err)
	}
	if out.UpSQL != "" {
		t.Fatalf("a view now declared by an R__ file must be left alone:\n%s", out.UpSQL)
	}
	if len(out.RepeatableObjects) != 1 || out.RepeatableObjects[0] != "public.t_totals" {
		t.Fatalf("repeatable objects = %v", out.RepeatableObjects)
	}
}

func TestDifferRefusesWithoutTheMigrationDirectory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := differWithRepeatable(t, "eeeeeeee-0000-0000-0000-000000000005", repeatableFiles(totalsUp))

	_, err := d.Diff(ctx, "app", tableOnly, DiffBaseLive, nil)
	if !errors.Is(err, ErrRepeatablesUnknown) || !strings.Contains(err.Error(), "R__t_totals") {
		t.Fatalf("err = %v", err)
	}
}

func TestDifferRepeatableErrors(t *testing.T) {
	ctx := context.Background()
	d, _, _ := newDiffer(t, nil)

	broken := goodFiles()
	broken["R__t_totals.up.sql"] = totalsUp
	if _, err := d.Diff(ctx, "app", tableOnly, DiffBaseLive, broken); !errors.Is(err, ErrMigrationFiles) {
		t.Fatalf("unloadable repeatable err = %v", err)
	}

	files := repeatableFiles("CREATE OR REPLACE VIEW t_totals AS SELECT missing FROM t;")
	_, err := d.Diff(ctx, "app", tableOnly, DiffBaseLive, files)
	if !errors.Is(err, ErrRepeatableSchema) || !strings.Contains(err.Error(), "R__t_totals.up.sql") {
		t.Fatalf("unbuildable repeatable err = %v", err)
	}

	var calls int
	listObjects = func(context.Context, *sql.DB) ([]string, error) {
		calls++
		if calls == 2 {
			return nil, errBoom
		}

		return nil, nil
	}
	defer func() { listObjects = scratchObjects }()
	if _, err := d.Diff(ctx, "app", tableOnly, DiffBaseLive, repeatableFiles(totalsUp)); !errors.Is(err, errBoom) {
		t.Fatalf("after err = %v", err)
	}
	calls = 1
	if _, err := d.Diff(ctx, "app", tableOnly, DiffBaseLive, repeatableFiles(totalsUp)); !errors.Is(err, errBoom) {
		t.Fatalf("before err = %v", err)
	}
}

func TestScratchObjectsReportsAClosedPool(t *testing.T) {
	t.Parallel()
	cfg, err := parseDSN(newDatabase(t, "obj"))
	if err != nil {
		t.Fatal(err)
	}
	db := stdlib.OpenDB(*cfg)
	if _, err := scratchObjects(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := scratchObjects(context.Background(), db); err == nil ||
		!strings.Contains(err.Error(), "list scratch objects") {
		t.Fatalf("err = %v", err)
	}
}

func TestRepeatablesInSkipsAVersionedDirectoryTheLoaderRefuses(t *testing.T) {
	t.Parallel()
	files := map[string]string{"nope.sql": "SELECT 1;", "R__t_totals.up.sql": totalsUp, "R__t_totals.down.sql": totalsDown}
	migs, err := repeatablesIn(files)
	if err != nil || len(migs) != 1 || migs[0].ID() != engine.RepeatablePrefix+"t_totals" {
		t.Fatalf("migrations = %+v, err = %v", migs, err)
	}
}
