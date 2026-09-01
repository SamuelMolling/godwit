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
		CREATE TABLE users (id bigint PRIMARY KEY, email text NOT NULL DEFAULT '');
		CREATE INDEX idx_users_email ON users (email);
		CREATE VIEW active AS SELECT id FROM users;
		CREATE SCHEMA godwit;
		CREATE TABLE godwit.migrations (v int)`); err != nil {
		t.Fatal(err)
	}

	def, fp, err := Snapshot(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"column public.users.id bigint null=NO",
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

	// Deterministic: same schema, same fingerprint.
	_, fp2, err := Snapshot(ctx, conn)
	if err != nil || fp2 != fp {
		t.Fatalf("fingerprint unstable: %s vs %s (err %v)", fp, fp2, err)
	}

	// A manual change flips the fingerprint and shows up in the diff.
	if _, err := conn.Exec(ctx, `ALTER TABLE users ADD COLUMN sneaky text`); err != nil {
		t.Fatal(err)
	}
	live, fp3, err := Snapshot(ctx, conn)
	if err != nil || fp3 == fp {
		t.Fatalf("fingerprint must change (err %v)", err)
	}
	diff := DiffSchemas(def, live)
	if len(diff) != 1 || !strings.HasPrefix(diff[0], "+ column public.users.sneaky") {
		t.Fatalf("diff = %v", diff)
	}

	// Dropping shows as missing.
	if _, err := conn.Exec(ctx, `ALTER TABLE users DROP COLUMN sneaky; DROP INDEX idx_users_email`); err != nil {
		t.Fatal(err)
	}
	live2, _, err := Snapshot(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	diff = DiffSchemas(def, live2)
	if len(diff) != 1 || !strings.HasPrefix(diff[0], "- index public.idx_users_email") {
		t.Fatalf("diff = %v", diff)
	}
}

func TestSnapshotQueryErrors(t *testing.T) {
	t.Parallel()

	mock, _ := newMockExec(t)
	mock.ExpectQuery("information_schema.columns").WillReturnError(errBoom)
	if _, _, err := Snapshot(context.Background(), mock); err == nil ||
		!strings.Contains(err.Error(), "inspect columns") {
		t.Fatalf("err = %v", err)
	}

	mock2, _ := newMockExec(t)
	mock2.ExpectQuery("information_schema.columns").
		WillReturnRows(pgxmock.NewRows([]string{"line"}).AddRow("x").RowError(0, errBoom))
	if _, _, err := Snapshot(context.Background(), mock2); err == nil ||
		!strings.Contains(err.Error(), "read columns") {
		t.Fatalf("err = %v", err)
	}
}
