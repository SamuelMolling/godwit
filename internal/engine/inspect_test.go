package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/pashagolub/pgxmock/v4"
)

func TestSnapshotAndDiff(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	connect := newTestDB(t)
	conn := connect()

	if _, err := conn.Exec(ctx, `
		CREATE TABLE users (id bigint PRIMARY KEY, email text NOT NULL DEFAULT '', code varchar(20), rate numeric(10,2));
		CREATE INDEX idx_users_email ON users (email);
		CREATE VIEW active AS SELECT id FROM users;
		CREATE SCHEMA godwit;
		CREATE TABLE godwit.migrations (v int)`); err != nil {
		t.Fatal(err)
	}

	first, err := Snapshot(ctx, conn, IgnoreAdopted)
	if err != nil {
		t.Fatal(err)
	}
	def, fp := first.Definition, first.Fingerprint
	for _, want := range []string{
		"column public.users.id bigint null=NO",
		"column public.users.email text null=NO default=''::text",
		"column public.users.code character varying(20) null=YES default=<none>",
		"column public.users.rate numeric(10,2) null=YES default=<none>",
		"constraint public.users.users_pkey PRIMARY KEY (id)",
		"index public.idx_users_email",
		"view public.active",
	} {
		if !strings.Contains(def, want) {
			t.Fatalf("snapshot missing %q:\n%s", want, def)
		}
	}
	if strings.Contains(def, "godwit.migrations") {
		t.Fatal("godwit's own schema must be excluded")
	}

	again, err := Snapshot(ctx, conn, IgnoreAdopted)
	if err != nil || again.Fingerprint != fp {
		t.Fatalf("fingerprint unstable: %s vs %s (err %v)", fp, again.Fingerprint, err)
	}

	// A manual change flips the fingerprint and shows up in the diff.
	if _, err := conn.Exec(ctx, `ALTER TABLE users ADD COLUMN sneaky text`); err != nil {
		t.Fatal(err)
	}
	changed, err := Snapshot(ctx, conn, IgnoreAdopted)
	if err != nil || changed.Fingerprint == fp {
		t.Fatalf("fingerprint must change (err %v)", err)
	}
	diff := DiffSchemas(def, changed.Definition)
	if len(diff) != 1 || !strings.HasPrefix(diff[0], "+ column public.users.sneaky") {
		t.Fatalf("diff = %v", diff)
	}

	if _, err := conn.Exec(ctx, `ALTER TABLE users DROP COLUMN sneaky; DROP INDEX idx_users_email`); err != nil {
		t.Fatal(err)
	}
	live2, err := Snapshot(ctx, conn, IgnoreAdopted)
	if err != nil {
		t.Fatal(err)
	}
	diff = DiffSchemas(def, live2.Definition)
	if len(diff) != 1 || !strings.HasPrefix(diff[0], "- index public.idx_users_email") {
		t.Fatalf("diff = %v", diff)
	}
}

func TestDiffSchemasIgnoresEmptyLines(t *testing.T) {
	t.Parallel()

	if diff := DiffSchemas("", "column public.rogue.id integer"); len(diff) != 1 || diff[0] != "+ column public.rogue.id integer" {
		t.Fatalf("empty expected: %v", diff)
	}
	if diff := DiffSchemas("column public.rogue.id integer", ""); len(diff) != 1 || diff[0] != "- column public.rogue.id integer" {
		t.Fatalf("empty live: %v", diff)
	}
	if diff := DiffSchemas("", ""); len(diff) != 0 {
		t.Fatalf("both empty: %v", diff)
	}
}

func TestSnapshotQueryErrors(t *testing.T) {
	t.Parallel()

	mock, _ := newMockExec(t)
	expectNoExtensionObjects(mock)
	mock.ExpectQuery("pg_class").WillReturnError(errBoom)
	if _, err := Snapshot(context.Background(), mock, KeepAdopted); err == nil ||
		!strings.Contains(err.Error(), "inspect tables") {
		t.Fatalf("err = %v", err)
	}

	mock2, _ := newMockExec(t)
	expectNoExtensionObjects(mock2)
	mock2.ExpectQuery("pg_class").
		WillReturnRows(pgxmock.NewRows([]string{"owner", "line"}).AddRow("x", "y").RowError(0, errBoom))
	if _, err := Snapshot(context.Background(), mock2, KeepAdopted); err == nil ||
		!strings.Contains(err.Error(), "read tables") {
		t.Fatalf("err = %v", err)
	}

	mock3, _ := newMockExec(t)
	mock3.ExpectQuery("pg_depend").WillReturnRows(
		pgxmock.NewRows([]string{"name"}).AddRow("public.x").RowError(0, errBoom))
	if _, err := Snapshot(context.Background(), mock3, KeepAdopted); err == nil ||
		!strings.Contains(err.Error(), "read extension objects") {
		t.Fatalf("err = %v", err)
	}
}

func expectNoExtensionObjects(mock pgxmock.PgxConnIface) {
	mock.ExpectQuery("pg_depend").WillReturnRows(pgxmock.NewRows([]string{"name"}))
}

func TestListApplied(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn := newTestDB(t)()

	got, err := ListApplied(ctx, conn)
	if err != nil || got != nil {
		t.Fatalf("fresh database: %v, err = %v", got, err)
	}
	if err := bootstrap(ctx, conn); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO godwit.migrations (version, name, checksum) VALUES (2, 'b', 'y'), (1, 'a', 'x')`); err != nil {
		t.Fatal(err)
	}
	got, err = ListApplied(ctx, conn)
	if err != nil || len(got) != 2 || got[0].Version != 1 || got[0].Name != "a" || got[0].Checksum != "x" ||
		got[1].Version != 2 || got[0].AppliedAt.IsZero() {
		t.Fatalf("applied = %+v, err = %v", got, err)
	}

	mock, _ := newMockExec(t)
	mock.ExpectQuery("to_regclass").WillReturnError(errBoom)
	_, err = ListApplied(ctx, mock)
	wantErr(t, err, "probe godwit schema")
}

func TestSnapshotSeesTablesSequencesEnumsAndMatviews(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn := newTestDB(t)()

	if _, err := conn.Exec(ctx, `
		CREATE TABLE marker ();
		CREATE TABLE rows_here (id bigserial PRIMARY KEY, tag text);
		CREATE SEQUENCE ticket_no START 100 INCREMENT 5;
		CREATE TYPE mood AS ENUM ('ok', 'bad');
		CREATE VIEW plain AS SELECT id FROM rows_here;
		CREATE MATERIALIZED VIEW cached AS SELECT id FROM rows_here`); err != nil {
		t.Fatal(err)
	}
	before, err := Snapshot(ctx, conn, IgnoreAdopted)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"table public.marker",
		"sequence public.ticket_no bigint increment=5",
		"type public.mood enum (ok, bad)",
		"matview public.cached",
		"view public.plain",
	} {
		if !strings.Contains(before.Definition, want) {
			t.Fatalf("snapshot missing %q:\n%s", want, before.Definition)
		}
	}
	for _, unwanted := range []string{"sequence public.rows_here_id_seq", "index public.rows_here_pkey"} {
		if strings.Contains(before.Definition, unwanted) {
			t.Fatalf("snapshot carries %q twice:\n%s", unwanted, before.Definition)
		}
	}

	if _, err := conn.Exec(ctx, `ALTER TYPE mood ADD VALUE 'great'`); err != nil {
		t.Fatal(err)
	}
	after, err := Snapshot(ctx, conn, IgnoreAdopted)
	if err != nil {
		t.Fatal(err)
	}
	diff := DiffSchemas(before.Definition, after.Definition)
	if len(diff) != 2 || diff[0] != "- type public.mood enum (ok, bad)" ||
		diff[1] != "+ type public.mood enum (ok, bad, great)" {
		t.Fatalf("a hand-run ALTER TYPE must show: %v", diff)
	}
}

func TestSnapshotLeavesExtensionObjectsOut(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn := newTestDB(t)()

	if _, err := conn.Exec(ctx, `CREATE TABLE mine (id int)`); err != nil {
		t.Fatal(err)
	}
	before, err := Snapshot(ctx, conn, IgnoreAdopted)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `CREATE EXTENSION citext`); err != nil {
		t.Fatal(err)
	}
	after, err := Snapshot(ctx, conn, IgnoreAdopted)
	if err != nil {
		t.Fatal(err)
	}
	if diff := DiffSchemas(before.Definition, after.Definition); len(diff) != 0 {
		t.Fatalf("extension objects reached the snapshot: %v", diff)
	}
}

func TestSchemaFormat(t *testing.T) {
	t.Parallel()

	if !SameFormat(SchemaFormat+"\ntable public.a") || SameFormat("table public.a") || SameFormat("") {
		t.Fatal("the marker is what tells a snapshot's format apart")
	}
	if SameFormat("godwit-schema-v2\ncolumn public.a.b character varying null=YES default=<none>") {
		t.Fatal("a baseline that recorded a type without its modifier cannot be compared with one that does")
	}
}
