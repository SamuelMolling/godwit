package engine

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/pashagolub/pgxmock/v4"
)

const checkpointBody = "-- godwit: checkpoint through=20260102000000\nCREATE TABLE a (id int);\nCREATE TABLE b (id int);"

func loadFiles(t *testing.T, files map[string]string) []Migration {
	t.Helper()
	fsys := fstest.MapFS{}
	for name, body := range files {
		fsys[name] = &fstest.MapFile{Data: []byte(body)}
	}
	migs, err := LoadFS(fsys)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	return migs
}

func loadError(t *testing.T, files map[string]string) string {
	t.Helper()
	fsys := fstest.MapFS{}
	for name, body := range files {
		fsys[name] = &fstest.MapFile{Data: []byte(body)}
	}
	if _, err := LoadFS(fsys); err != nil {
		return err.Error()
	}
	t.Fatal("want a load error")

	return ""
}

func checkpointDir() map[string]string {
	return map[string]string{
		"20260101000000_a.up.sql":      "CREATE TABLE a (id int);",
		"20260101000000_a.down.sql":    "DROP TABLE a;",
		"20260102000000_b.up.sql":      "CREATE TABLE b (id int);",
		"20260102000000_b.down.sql":    "DROP TABLE b;",
		"20260103000000_squash.up.sql": checkpointBody,
		"20260104000000_c.up.sql":      "CREATE TABLE c (id int);",
		"20260104000000_c.down.sql":    "DROP TABLE c;",
		"R__view.up.sql":               "CREATE OR REPLACE VIEW v AS SELECT 1 AS one;",
		"R__view.down.sql":             "DROP VIEW IF EXISTS v;",
	}
}

func plansOf(t *testing.T, files map[string]string) []Plan {
	t.Helper()
	var out []Plan
	for _, m := range loadFiles(t, files) {
		p, err := BuildPlan(m, DirectionUp)
		if err != nil {
			t.Fatalf("build %s: %v", m.ID(), err)
		}
		out = append(out, p)
	}

	return out
}

func marked(plans []Plan) []string {
	var out []string
	for _, p := range plans {
		if p.MarkOnly {
			out = append(out, p.Migration.ID())
		}
	}

	return out
}

func TestLoadCheckpoint(t *testing.T) {
	t.Parallel()
	migs := loadFiles(t, checkpointDir())
	cp, ok := NewestCheckpoint(migs)
	if !ok || cp.Name != "squash" || cp.Through != 20260102000000 || !cp.Checkpoint {
		t.Fatalf("checkpoint = %+v, ok = %t", cp, ok)
	}
	if len(cp.Directives) != 0 {
		t.Fatalf("a checkpoint carries no directive to expand: %+v", cp.Directives)
	}
	if !migs[0].Collapses(cp) || !migs[1].Collapses(cp) || migs[3].Collapses(cp) {
		t.Fatal("a checkpoint collapses every version below it and nothing above")
	}
	if migs[len(migs)-1].Collapses(cp) {
		t.Fatal("a repeatable is never collapsed")
	}
	if _, ok := NewestCheckpoint(migs[:2]); ok {
		t.Fatal("no checkpoint in a plain directory")
	}
}

func TestLoadCheckpointRefusals(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, file, body, want string }{
		{"down file", "20260103000000_squash.down.sql", "DROP TABLE a;", "has no inverse"},
		{"through above", "20260103000000_squash.up.sql", "-- godwit: checkpoint through=20260104000000\nSELECT 1;", "must be below the checkpoint's own version"},
		{"second directive", "20260103000000_squash.up.sql", "-- godwit: checkpoint through=20260102000000\n-- godwit: add-not-null a.id\nSELECT 1;", "carries no other directive"},
		{"short version", "20260103000000_squash.up.sql", "-- godwit: checkpoint through=2026\nSELECT 1;", "14-digit migration version"},
		{"no through", "20260103000000_squash.up.sql", "-- godwit: checkpoint\nSELECT 1;", "requires through="},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			files := checkpointDir()
			files[tc.file] = tc.body
			if got := loadError(t, files); !strings.Contains(got, tc.want) {
				t.Fatalf("error = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLoadCheckpointRefusesARepeatable(t *testing.T) {
	t.Parallel()
	got := loadError(t, map[string]string{
		"R__squash.up.sql": "-- godwit: checkpoint through=20260102000000\nSELECT 1;",
	})
	if !strings.Contains(got, "must be a versioned migration") {
		t.Fatalf("error = %q", got)
	}
}

func TestLoadStillDemandsADownFile(t *testing.T) {
	t.Parallel()
	got := loadError(t, map[string]string{"20260101000000_a.up.sql": "SELECT 1;"})
	if !strings.Contains(got, "missing down file") {
		t.Fatalf("error = %q", got)
	}
}

func TestShapeCheckpointOnAFreshDatabase(t *testing.T) {
	t.Parallel()
	plans := plansOf(t, checkpointDir())
	out, err := ShapeCheckpoint(plans, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := marked(out)
	if len(got) != 2 || got[0] != "20260101000000_a" || got[1] != "20260102000000_b" {
		t.Fatalf("marked = %v; a fresh database runs the checkpoint and records what it collapses", got)
	}
	for _, p := range out {
		if p.Migration.Checkpoint && p.MarkOnly {
			t.Fatal("the checkpoint itself must run on a fresh database")
		}
	}
}

func TestShapeCheckpointOnAnAdvancedDatabase(t *testing.T) {
	t.Parallel()
	plans := plansOf(t, checkpointDir())
	out, err := ShapeCheckpoint(plans, 20260102000000)
	if err != nil {
		t.Fatal(err)
	}
	if got := marked(out); len(got) != 1 || got[0] != "20260103000000_squash" {
		t.Fatalf("marked = %v; a database past the checkpoint records it and runs nothing of it", got)
	}
}

// A database that stopped between two collapsed versions still moves forward file by file, and the
// checkpoint is recorded once it gets there.
func TestShapeCheckpointMidHistory(t *testing.T) {
	t.Parallel()
	plans := plansOf(t, checkpointDir())
	out, err := ShapeCheckpoint(plans, 20260101000000)
	if err != nil {
		t.Fatal(err)
	}
	if got := marked(out); len(got) != 1 || got[0] != "20260103000000_squash" {
		t.Fatalf("marked = %v", got)
	}
}

func TestShapeCheckpointRefusesAGap(t *testing.T) {
	t.Parallel()
	files := checkpointDir()
	delete(files, "20260102000000_b.up.sql")
	delete(files, "20260102000000_b.down.sql")
	_, err := ShapeCheckpoint(plansOf(t, files), 20260101000000)
	if !errors.Is(err, ErrCheckpointGap) || !strings.Contains(err.Error(), "20260102000000 is not in the") {
		t.Fatalf("err = %v", err)
	}
}

func TestShapeCheckpointWithoutOne(t *testing.T) {
	t.Parallel()
	files := checkpointDir()
	delete(files, "20260103000000_squash.up.sql")
	plans := plansOf(t, files)
	out, err := ShapeCheckpoint(plans, 0)
	if err != nil || len(marked(out)) != 0 {
		t.Fatalf("out = %v, err = %v", marked(out), err)
	}
}

func TestCheckpointNoteAndCount(t *testing.T) {
	t.Parallel()
	plans := plansOf(t, checkpointDir())
	cp, _ := NewestCheckpoint(loadFiles(t, checkpointDir()))
	if n := Collapsed(plans, cp); n != 2 {
		t.Fatalf("collapsed = %d", n)
	}
	run := CheckpointNote(Plan{Migration: cp}, 2)
	if !strings.Contains(run, "records the 2 migration(s)") {
		t.Fatalf("note = %q", run)
	}
	rec := CheckpointNote(Plan{Migration: cp, MarkOnly: true}, 2)
	if !strings.Contains(rec, "recorded, not run") {
		t.Fatalf("note = %q", rec)
	}
}

func TestRecordCollapsed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	migs := []Migration{{Version: 1, Name: "a", Checksum: "x"}}

	mock, _ := newMockExec(t)
	expectBootstrap(mock)
	mock.ExpectExec("INSERT INTO godwit.migrations").WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	if err := RecordCollapsed(ctx, mock, migs); err != nil {
		t.Fatal(err)
	}
	if err := RecordCollapsed(ctx, mock, nil); err != nil {
		t.Fatalf("nothing to record: %v", err)
	}

	failExec, _ := newMockExec(t)
	expectBootstrap(failExec)
	failExec.ExpectExec("INSERT INTO godwit.migrations").WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(errBoom)
	if err := RecordCollapsed(ctx, failExec, migs); err == nil || !strings.Contains(err.Error(), "record 1 collapsed migrations") {
		t.Fatalf("err = %v", err)
	}

	failBoot, _ := newMockExec(t)
	failBoot.ExpectExec(regexp.QuoteMeta(bootstrapDDL[0])).WillReturnError(errBoom)
	if err := RecordCollapsed(ctx, failBoot, migs); err == nil || !strings.Contains(err.Error(), "bootstrap") {
		t.Fatalf("err = %v", err)
	}
}
