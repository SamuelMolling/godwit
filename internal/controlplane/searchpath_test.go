package controlplane

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/SamuelMolling/godwit/internal/creds"
)

func TestParseSearchPath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, in, want, wantErr string
	}{
		{name: "empty", in: ""},
		{name: "blank", in: "  "},
		{name: "single", in: "app", want: "app"},
		{name: "list with spaces", in: " app , public ", want: "app,public"},
		{name: "folded like postgres", in: "App", want: "app"},
		{name: "dollar and underscore", in: "_tenant$1", want: "_tenant$1"},
		{name: "quoted refused", in: `"my schema"`, wantErr: `search_path: "\"my schema\"" is not a schema name; give unquoted identifiers separated by commas`},
		{name: "user refused", in: "$user,public", wantErr: `search_path: "$user" is not a schema name; give unquoted identifiers separated by commas`},
		{name: "empty element refused", in: "app,,public", wantErr: `search_path: "" is not a schema name; give unquoted identifiers separated by commas`},
		{name: "journal schema refused", in: "app,godwit", wantErr: `search_path: "godwit" holds godwit's journal and must not be on a target's search path`},
		{name: "journal schema refused whatever the case", in: "GODWIT", wantErr: `search_path: "godwit" holds godwit's journal and must not be on a target's search path`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseSearchPath(tc.in)
			if tc.wantErr != "" {
				if err == nil || err.Error() != tc.wantErr {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}

				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got = %q, err = %v", got, err)
			}
		})
	}
}

func TestDSNWithSearchPath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, dsn, path, want string
	}{
		{name: "unset leaves the dsn alone", dsn: "postgres://h/db", want: "postgres://h/db"},
		{name: "url without query", dsn: "postgres://u:p@h:5432/db", path: "app", want: "postgres://u:p@h:5432/db?search_path=app"},
		{name: "url with query", dsn: "postgresql://h/db?sslmode=disable", path: "app,public", want: "postgresql://h/db?search_path=app%2Cpublic&sslmode=disable"},
		{name: "url search path wins over the dsn", dsn: "postgres://h/db?search_path=old", path: "app", want: "postgres://h/db?search_path=app"},
		{name: "keyword form", dsn: "host=h dbname=db", path: "app,public", want: "host=h dbname=db search_path=app,public"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := dsnWithSearchPath(tc.dsn, tc.path); got != tc.want {
				t.Fatalf("dsn = %q, want %q", got, tc.want)
			}
		})
	}
}

const journalShadowUp = `CREATE SCHEMA IF NOT EXISTS app;
CREATE TABLE migrations (id bigint PRIMARY KEY, note text);`

func searchPathFiles() map[string]string {
	return map[string]string{
		"20260901120000_shadow.up.sql":   journalShadowUp,
		"20260901120000_shadow.down.sql": "DROP TABLE migrations;\nDROP SCHEMA app;",
	}
}

func registerSearchPathTarget(t *testing.T, s *Store, searchPath string) string {
	t.Helper()
	targetDSN := newDatabase(t, "sp")
	config := map[string]string{"dsn": targetDSN, ConfigSearchPath: searchPath}
	if err := s.RegisterTarget(context.Background(), "app", "plain", config); err != nil {
		t.Fatal(err)
	}

	return targetDSN
}

func TestSchedulerAppliesTargetSearchPath(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newStore(t)
	targetDSN := registerSearchPathTarget(t, s, "app,public")
	sched := NewScheduler(s, map[string]creds.Provider{"plain": plainProvider{}}, PGEngine{}, Policies(), Config{Holder: "h"}, testLog)
	id := "77777777-0000-0000-0000-000000000001"
	if err := s.CreateRun(ctx, id, "app", RolloutDirect, searchPathFiles(), Timeouts{}, Provenance{}, ""); err != nil {
		t.Fatal(err)
	}
	sched.Tick(ctx)
	waitState(t, s, id, StateSucceeded)

	conn, err := pgx.Connect(ctx, targetDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	var landed, shadowed string
	if err := conn.QueryRow(ctx, `SELECT table_schema FROM information_schema.tables WHERE table_name = 'migrations' AND table_schema = 'app'`).
		Scan(&landed); err != nil {
		t.Fatalf("unqualified table did not land in app: %v", err)
	}
	if err := conn.QueryRow(ctx, `SELECT string_agg(name, ',') FROM godwit.migrations`).Scan(&shadowed); err != nil {
		t.Fatal(err)
	}
	if shadowed != "shadow" {
		t.Fatalf("godwit.migrations = %q", shadowed)
	}
	var columns int
	if err := conn.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.columns WHERE table_schema = 'godwit' AND table_name = 'migrations' AND column_name = 'note'`).
		Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if columns != 0 {
		t.Fatal("the migration reached godwit.migrations")
	}
}

func TestInspectorReportsSearchPath(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newStore(t)
	registerSearchPathTarget(t, s, "public")
	sched := NewScheduler(s, map[string]creds.Provider{"plain": plainProvider{}}, PGEngine{}, Policies(), Config{Holder: "h"}, testLog)
	insp := NewInspector(sched)
	st, err := insp.Status(ctx, "app")
	if err != nil || st.SearchPath != "public" {
		t.Fatalf("status = %+v, err = %v", st, err)
	}
	obs, err := insp.Observe(ctx, "app")
	if err != nil || obs.SearchPath != "public" {
		t.Fatalf("observation = %+v, err = %v", obs, err)
	}
}

func TestSchedulerRejectsBadStoredSearchPath(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newStore(t)
	registerSearchPathTarget(t, s, "public")
	sched := NewScheduler(s, map[string]creds.Provider{"plain": plainProvider{}}, PGEngine{}, Policies(), Config{Holder: "h"}, testLog)
	if _, err := s.pool.Exec(ctx, `UPDATE cp_targets SET config = jsonb_set(config, '{search_path}', '"godwit"')`); err != nil {
		t.Fatal(err)
	}
	if _, err := sched.target(ctx, "app"); err == nil || !strings.Contains(err.Error(), "holds godwit's journal") {
		t.Fatalf("err = %v", err)
	}
}

func TestPlanSearchPathStaleness(t *testing.T) {
	t.Parallel()
	obs := Observation{SearchPath: "app,public", Fingerprint: "fp", Definition: "d"}
	stored := Plan{SearchPath: "public", SchemaFingerprint: "fp", SchemaDefinition: "d"}
	d := StaleDiff(stored, obs)
	if d.Path != "public -> app,public" {
		t.Fatalf("path = %q", d.Path)
	}
	if len(d.Schema) != 2 || d.Schema[0] != "- search_path public" || d.Schema[1] != "+ search_path app,public" {
		t.Fatalf("schema = %q", d.Schema)
	}
	if d.Reason() != StaleSchema || d.Explained("fp", "fp") {
		t.Fatalf("reason = %q, explained = %t", d.Reason(), d.Explained("fp", "fp"))
	}
	same := StaleDiff(Plan{SearchPath: "app,public", SchemaFingerprint: "fp", SchemaDefinition: "d"}, obs)
	if same.Path != "" || len(same.Schema) != 0 || !same.Explained("fp", "fp") {
		t.Fatalf("unchanged path = %+v", same)
	}
	old := StaleDiff(Plan{SchemaFingerprint: "fp", SchemaDefinition: "d"}, obs)
	if old.Path != "" || !old.Explained("fp", "fp") {
		t.Fatalf("plan stored before the path was recorded = %+v", old)
	}
}
