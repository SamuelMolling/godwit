package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/pashagolub/pgxmock/v4"
)

const golangMigrateTable = `
	CREATE TABLE public.schema_migrations (version bigint NOT NULL PRIMARY KEY, dirty boolean NOT NULL);
	INSERT INTO public.schema_migrations VALUES (20260902165420, false)`

func TestSnapshotLeavesTheAdoptedToolsBookkeepingOut(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn := newTestDB(t)()

	if _, err := conn.Exec(ctx, `CREATE TABLE users (id bigint PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	clean, err := Snapshot(ctx, conn, IgnoreAdopted)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := conn.Exec(ctx, golangMigrateTable); err != nil {
		t.Fatal(err)
	}
	adopted, err := Snapshot(ctx, conn, IgnoreAdopted)
	if err != nil {
		t.Fatal(err)
	}
	if diff := DiffSchemas(clean.Definition, adopted.Definition); len(diff) > 0 {
		t.Fatalf("adopted bookkeeping reached the snapshot: %v", diff)
	}
	if adopted.Fingerprint != clean.Fingerprint {
		t.Fatalf("fingerprint moved: %s vs %s", clean.Fingerprint, adopted.Fingerprint)
	}
	if len(adopted.Ignored) != 1 || adopted.Ignored[0].String() != "public.schema_migrations (golang-migrate)" {
		t.Fatalf("ignored = %+v", adopted.Ignored)
	}

	kept, err := Snapshot(ctx, conn, KeepAdopted)
	if err != nil {
		t.Fatal(err)
	}
	if len(kept.Ignored) != 0 {
		t.Fatalf("KeepAdopted must ignore nothing: %+v", kept.Ignored)
	}
	for _, want := range []string{
		"table public.schema_migrations",
		"column public.schema_migrations.dirty",
		"column public.schema_migrations.version",
		"constraint public.schema_migrations.schema_migrations_pkey",
	} {
		if !strings.Contains(kept.Definition, want) {
			t.Fatalf("KeepAdopted missing %q:\n%s", want, kept.Definition)
		}
	}
}

func TestDetectAdoptedMatchesOnColumnsNotOnName(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn := newTestDB(t)()

	if _, err := conn.Exec(ctx, `
		CREATE SCHEMA app;
		CREATE TABLE public.schema_migrations (version bigint NOT NULL, dirty boolean NOT NULL, applied_by text);
		CREATE TABLE public.alembic_version (revision text NOT NULL);
		CREATE TABLE app.alembic_version (version_num varchar(32) NOT NULL);
		CREATE TABLE public.flyway_schema_history (
			installed_rank int NOT NULL, version text, description text, type text, script text,
			checksum int, installed_by text, installed_on timestamp, execution_time int, success boolean NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	got, err := DetectAdopted(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].String() != "app.alembic_version (alembic)" ||
		got[1].String() != "public.flyway_schema_history (flyway)" {
		t.Fatalf("detected = %+v", got)
	}
}

func TestDetectAdoptedQueryErrors(t *testing.T) {
	t.Parallel()

	mock, _ := newMockExec(t)
	expectNoExtensionObjects(mock)
	mock.ExpectQuery("array_agg").WithArgs(pgxmock.AnyArg()).WillReturnError(errBoom)
	if _, err := Snapshot(context.Background(), mock, IgnoreAdopted); err == nil ||
		!strings.Contains(err.Error(), "inspect adopted tables") {
		t.Fatalf("err = %v", err)
	}

	mock2, _ := newMockExec(t)
	mock2.ExpectQuery("array_agg").WithArgs(pgxmock.AnyArg()).WillReturnRows(
		pgxmock.NewRows([]string{"table_schema", "table_name", "columns"}).
			AddRow("public", "alembic_version", []string{"version_num"}).RowError(0, errBoom))
	if _, err := DetectAdopted(context.Background(), mock2); err == nil ||
		!strings.Contains(err.Error(), "read adopted tables") {
		t.Fatalf("err = %v", err)
	}
}

func TestAdoptedLines(t *testing.T) {
	t.Parallel()

	got := AdoptedLines([]Adopted{{Tool: "flyway", Schema: "public", Table: "flyway_schema_history"}})
	if len(got) != 1 || got[0] != "public.flyway_schema_history (flyway)" {
		t.Fatalf("lines = %v", got)
	}
	if len(AdoptedLines(nil)) != 0 {
		t.Fatal("no tables must render no lines")
	}
}
