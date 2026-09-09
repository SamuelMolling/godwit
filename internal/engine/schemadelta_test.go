package engine

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func snapshotAround(t *testing.T, conn *pgx.Conn, before, after string) []ObjectChange {
	t.Helper()
	ctx := context.Background()
	if _, err := conn.Exec(ctx, before); err != nil {
		t.Fatal(err)
	}
	from, err := Snapshot(ctx, conn, KeepAdopted)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, after); err != nil {
		t.Fatal(err)
	}
	to, err := Snapshot(ctx, conn, KeepAdopted)
	if err != nil {
		t.Fatal(err)
	}

	return SchemaChanges(from.Definition, to.Definition)
}

func changeOf(t *testing.T, changes []ObjectChange, kind, name string) ObjectChange {
	t.Helper()
	for _, c := range changes {
		if c.Kind == kind && c.Name == name {
			return c
		}
	}
	body, _ := json.Marshal(changes)
	t.Fatalf("no %s %q in %s", kind, name, body)

	return ObjectChange{}
}

func attrOf(t *testing.T, c ObjectChange, name string) AttrChange {
	t.Helper()
	for _, a := range c.Attrs {
		if a.Name == name {
			return a
		}
	}
	body, _ := json.Marshal(c)
	t.Fatalf("no attribute %q in %s", name, body)

	return AttrChange{}
}

func TestSchemaChangesReadsARealCreate(t *testing.T) {
	t.Parallel()
	conn := newTestDB(t)()

	changes := snapshotAround(t, conn, "SELECT 1", `
		CREATE TABLE widget_events (id bigint PRIMARY KEY, widget_id bigint NOT NULL, kind text NOT NULL);
		CREATE INDEX widget_events_kind_idx ON widget_events (kind);`)

	tbl := changeOf(t, changes, KindTable, "widget_events")
	if tbl.Op != OpCreate || tbl.Schema != "public" || tbl.Unchanged != 0 {
		t.Fatalf("table = %+v", tbl)
	}
	if got := attrOf(t, tbl, "widget_id"); got.Op != OpCreate || len(got.New) != 2 || got.New[0] != "bigint" || got.New[1] != "NOT NULL" {
		t.Fatalf("widget_id = %+v", got)
	}
	if got := attrOf(t, tbl, "widget_events_pkey"); got.New[0] != "PRIMARY KEY (id)" {
		t.Fatalf("primary key = %+v", got)
	}
	idx := changeOf(t, changes, KindIndex, "widget_events_kind_idx")
	if idx.Op != OpCreate || len(idx.Attrs) != 1 || idx.Attrs[0].Name != "definition" {
		t.Fatalf("index = %+v", idx)
	}
	if idx.Ref() != "public.widget_events_kind_idx" {
		t.Fatalf("ref = %s", idx.Ref())
	}
}

func TestSchemaChangesReadsATypeChange(t *testing.T) {
	t.Parallel()
	conn := newTestDB(t)()

	changes := snapshotAround(t, conn, `
		CREATE TABLE widgets (id bigint PRIMARY KEY, name text, age integer, created_at timestamptz);`,
		`ALTER TABLE widgets ALTER COLUMN age TYPE varchar(20);
		 ALTER TABLE widgets ADD COLUMN age_old integer;`)

	if len(changes) != 1 {
		t.Fatalf("changes = %+v", changes)
	}
	tbl := changes[0]
	if tbl.Op != OpUpdate || tbl.Unchanged != 4 {
		t.Fatalf("table = %+v", tbl)
	}
	age := attrOf(t, tbl, "age")
	if age.Op != OpUpdate || len(age.Old) != 1 || age.Old[0] != "integer" || age.New[0] != "character varying(20)" {
		t.Fatalf("age = %+v", age)
	}
	if got := attrOf(t, tbl, "age_old"); got.Op != OpCreate || got.New[1] != "NULL" {
		t.Fatalf("age_old = %+v", got)
	}
}

func TestSchemaChangesReadsDestroysAndDefaults(t *testing.T) {
	t.Parallel()
	conn := newTestDB(t)()

	changes := snapshotAround(t, conn, `
		CREATE TABLE a (id bigint);
		CREATE TABLE b (id bigint, note text);
		CREATE VIEW v AS SELECT id FROM b;`,
		`DROP VIEW v;
		 DROP TABLE a;
		 ALTER TABLE b DROP COLUMN note;
		 ALTER TABLE b ALTER COLUMN id SET DEFAULT 1;`)

	gone := changeOf(t, changes, KindTable, "a")
	if gone.Op != OpDestroy || attrOf(t, gone, "id").Op != OpDestroy {
		t.Fatalf("a = %+v", gone)
	}
	b := changeOf(t, changes, KindTable, "b")
	if got := attrOf(t, b, "note"); got.Op != OpDestroy || got.Old[0] != "text" {
		t.Fatalf("note = %+v", got)
	}
	if got := attrOf(t, b, "id"); got.Op != OpUpdate || len(got.Old) != 2 || len(got.New) != 3 || got.New[2] != "DEFAULT 1" {
		t.Fatalf("id = %+v", got)
	}
	if v := changeOf(t, changes, KindView, "v"); v.Op != OpDestroy {
		t.Fatalf("v = %+v", v)
	}
}

func TestSchemaChangesCarriesTheTypeModifier(t *testing.T) {
	t.Parallel()
	conn := newTestDB(t)()

	changes := snapshotAround(t, conn, `
		CREATE TYPE mood AS ENUM ('sad', 'ok');
		CREATE TYPE feeling AS ENUM ('up', 'down');
		CREATE TABLE t (a numeric, b timestamptz, c text, d mood, e text);`,
		`ALTER TABLE t ALTER COLUMN a TYPE numeric(10,2);
		 ALTER TABLE t ALTER COLUMN b TYPE timestamptz(3);
		 ALTER TABLE t ALTER COLUMN c TYPE varchar(64);
		 ALTER TABLE t ALTER COLUMN d TYPE feeling USING 'up'::feeling;
		 ALTER TABLE t ALTER COLUMN e TYPE text[] USING ARRAY[e];`)

	tbl := changeOf(t, changes, KindTable, "t")
	for name, want := range map[string]string{
		"a": "numeric(10,2)", "b": "timestamp(3) with time zone", "c": "character varying(64)",
		"d": "feeling", "e": "text[]",
	} {
		got := attrOf(t, tbl, name)
		if got.Op != OpUpdate || len(got.New) != 1 || got.New[0] != want {
			t.Fatalf("%s = %+v, want %s", name, got, want)
		}
	}
}

func TestSchemaChangesReadsSequencesAndEnums(t *testing.T) {
	t.Parallel()
	conn := newTestDB(t)()

	changes := snapshotAround(t, conn, `
		CREATE SEQUENCE s INCREMENT 1;
		CREATE TYPE mood AS ENUM ('sad', 'ok');`,
		`ALTER SEQUENCE s INCREMENT 2;
		 ALTER TYPE mood ADD VALUE 'great';`)

	seq := changeOf(t, changes, KindSequence, "s")
	if got := attrOf(t, seq, "increment"); got.Op != OpUpdate || got.Old[0] != "1" || got.New[0] != "2" {
		t.Fatalf("increment = %+v", got)
	}
	if seq.Unchanged == 0 {
		t.Fatal("the properties the alter left alone are counted, not listed")
	}
	enum := changeOf(t, changes, KindEnum, "mood")
	if got := attrOf(t, enum, "values"); got.Old[0] != "(sad, ok)" || got.New[0] != "(sad, ok, great)" {
		t.Fatalf("values = %+v", got)
	}
}

func TestSchemaChangesIgnoresWhatItCannotRead(t *testing.T) {
	t.Parallel()

	if got := SchemaChanges("godwit-schema-v3", "godwit-schema-v3"); len(got) != 0 {
		t.Fatalf("two identical snapshots change nothing: %+v", got)
	}
	if got := SchemaChanges("", "godwit-schema-v3\nfuture public.thing something"); len(got) != 0 {
		t.Fatalf("a kind this version does not know is dropped: %+v", got)
	}
	got := SchemaChanges("", "godwit-schema-v3\ntable widgets\ncolumn widgets.id integer null=NO default=<none>")
	if len(got) != 1 || got[0].Schema != "" || got[0].Ref() != "widgets" {
		t.Fatalf("unqualified = %+v", got)
	}
}

func TestSchemaChangesOrdersWhatItReports(t *testing.T) {
	t.Parallel()

	got := SchemaChanges("", strings.Join([]string{
		"godwit-schema-v3",
		"matview b.mv deadbeef",
		"index z.i CREATE INDEX i ON z.t (a)",
		"index a.i CREATE INDEX i ON a.t (a)",
		"table b.t",
		"table a.t",
	}, "\n"))
	var order []string
	for _, c := range got {
		order = append(order, c.Kind+" "+c.Ref())
	}
	want := []string{"table a.t", "table b.t", "index a.i", "index z.i", "materialized view b.mv"}
	if !slices.Equal(order, want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
}

func TestSchemaChangesFallsBackToTheWholeValue(t *testing.T) {
	t.Parallel()

	before := "godwit-schema-v3\ntable public.t\ncolumn public.t.a integer null=YES default=<none>"
	after := "godwit-schema-v3\ntable public.t\ncolumn public.t.a bigint null=NO default=1"
	got := SchemaChanges(before, after)
	if len(got) != 1 {
		t.Fatalf("changes = %+v", got)
	}
	a := got[0].Attrs[0]
	if len(a.Old) != 2 || a.Old[0] != "integer" || len(a.New) != 3 || a.New[2] != "DEFAULT 1" {
		t.Fatalf("a = %+v", a)
	}
}
