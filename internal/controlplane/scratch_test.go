package controlplane

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func newRestrictedScratch(t *testing.T, attrs string) *Scratch {
	t.Helper()
	ctx := context.Background()
	role := fmt.Sprintf("scratch%d", dbSeq.Add(1))
	admin, err := pgx.Connect(ctx, testDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admin.Close(ctx) }()
	if attrs == "" {
		attrs = "NOCREATEROLE NOREPLICATION NOBYPASSRLS"
	}
	if _, err := admin.Exec(ctx, "CREATE ROLE "+role+
		" LOGIN PASSWORD 'scratch' CREATEDB NOSUPERUSER "+attrs); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, strings.Replace(testDSN, "godwit:godwit@", role+":scratch@", 1))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	return NewScratch(pool, "")
}

func storeDatabase(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()

	return pool.Config().ConnConfig.Database
}

func TestScratchCheckFlagsTheStoreRole(t *testing.T) {
	t.Parallel()
	_, pool := newStore(t)

	found, err := NewScratch(pool, "").Check(context.Background(), storeDatabase(t, pool))
	if err != nil {
		t.Fatal(err)
	}
	joined := ""
	fatal := 0
	for _, f := range found {
		joined += f.Detail + "\n"
		if f.Fatal {
			fatal++
		}
	}
	if fatal < 2 {
		t.Fatalf("store role must be refused as a scratch role:\n%s", joined)
	}
	for _, want := range []string{"is a superuser", "owns the store database"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q:\n%s", want, joined)
		}
	}
}

func TestScratchCheckAcceptsRestrictedRole(t *testing.T) {
	t.Parallel()
	_, pool := newStore(t)
	scratch := newRestrictedScratch(t, "")

	found, err := scratch.Check(context.Background(), storeDatabase(t, pool))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range found {
		if f.Fatal {
			t.Fatalf("restricted role refused: %s", f.Detail)
		}
	}
	if len(found) != 1 || !strings.Contains(found[0].Detail, "may CONNECT to the store database") {
		t.Fatalf("found = %+v", found)
	}
}

func TestScratchCheckFlagsGrantedRole(t *testing.T) {
	t.Parallel()
	_, pool := newStore(t)
	scratch := newRestrictedScratch(t, "CREATEROLE REPLICATION BYPASSRLS"+
		" IN ROLE pg_execute_server_program, pg_read_server_files, pg_write_server_files")

	found, err := scratch.Check(context.Background(), storeDatabase(t, pool))
	if err != nil {
		t.Fatal(err)
	}
	joined := ""
	for _, f := range found {
		joined += f.Detail + "\n"
	}
	for _, want := range []string{
		"pg_execute_server_program", "pg_read_server_files", "pg_write_server_files",
		"has CREATEROLE", "has REPLICATION", "has BYPASSRLS",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q:\n%s", want, joined)
		}
	}
}

func TestScratchCheckUnreachable(t *testing.T) {
	t.Parallel()
	_, pool := newStore(t)
	pool.Close()

	if _, err := NewScratch(pool, "").Check(context.Background(), "x"); err == nil ||
		!strings.Contains(err.Error(), "inspect scratch role") {
		t.Fatalf("err = %v", err)
	}
}

func TestScratchRefusesDangerousDDL(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d, _, _ := newDiffer(t, nil)
	store := storeDatabase(t, d.scratch.pool)
	d.scratch = newRestrictedScratch(t, "")

	for _, tc := range []struct{ name, ddl, want string }{
		{"drop the store database", "DROP DATABASE " + store + " WITH (FORCE)", "must be owner of database"},
		{"run a command on the host", "CREATE TABLE t (x text);\nCOPY t FROM PROGRAM 'id'", "permission denied to COPY to or from an external program"},
		{"read a file on the host", "CREATE TABLE t AS SELECT pg_read_file('/etc/passwd')", "permission denied for function pg_read_file"},
		{"reach the store over dblink", "CREATE EXTENSION dblink", "permission denied to create extension"},
		{"reach the store over postgres_fdw", "CREATE EXTENSION postgres_fdw", "permission denied to create extension"},
		{"grant itself a role", "ALTER ROLE godwit NOSUPERUSER", "permission denied to alter role"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := d.Diff(ctx, "app", tc.ddl, DiffBaseLive, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestScratchTemplate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, pool := newStore(t)
	tmpl := fmt.Sprintf("godwit_tmpl%d", dbSeq.Add(1))
	if _, err := pool.Exec(ctx, "CREATE DATABASE "+tmpl); err != nil {
		t.Fatal(err)
	}
	execDSN(t, strings.Replace(testDSN, "/godwit?", "/"+tmpl+"?", 1), "CREATE EXTENSION dblink")

	for _, tc := range []struct {
		template string
		want     int
	}{{"", 0}, {tmpl, 1}} {
		name := fmt.Sprintf("godwit_diff_tmpl%d", dbSeq.Add(1))
		scratch := NewScratch(pool, tc.template)
		if err := scratch.create(ctx, name); err != nil {
			t.Fatal(err)
		}
		conn, err := connectScratch(ctx, scratch.connConfig(name, ""))
		if err != nil {
			t.Fatal(err)
		}
		var n int
		err = conn.QueryRow(ctx, "SELECT count(*) FROM pg_extension WHERE extname <> 'plpgsql'").Scan(&n)
		_ = conn.Close(ctx)
		scratch.drop(ctx, name)
		if err != nil || n != tc.want {
			t.Fatalf("template %q: extensions = %d, want %d, err = %v", tc.template, n, tc.want, err)
		}
	}
}
