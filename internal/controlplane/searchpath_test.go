package controlplane

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/SamuelMolling/godwit/internal/creds"
	"github.com/SamuelMolling/godwit/internal/engine"
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

func TestJournalOnPath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, effective, setting, role, want string
	}{
		{name: "a role of its own", effective: "public", setting: `"$user", public`, role: "app"},
		{name: "quoted is a different schema", effective: "public", setting: `"GODWIT", public`, role: "app"},
		{name: "nothing set", role: "app"},
		{name: "the journal is on the effective path", effective: "godwit,public", setting: `"$user", public`, role: "godwit", want: "godwit"},
		{name: "the role resolves to it before the journal exists", effective: "public", setting: `"$user", public`, role: "godwit", want: "$user"},
		{name: "named outright before the schema exists", effective: "public", setting: "godwit, public", role: "app", want: "godwit"},
		{name: "unquoted folds like an identifier", effective: "public", setting: "GODWIT, public", role: "app", want: "godwit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := journalOnPath(tc.effective, tc.setting, tc.role)
			if ok != (tc.want != "") || got != tc.want {
				t.Fatalf("element = %q, ok = %t, want %q", got, ok, tc.want)
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
	if err := s.CreateRun(ctx, id, "app", RolloutDirect, searchPathFiles(), Timeouts{}, Provenance{}, "", nil); err != nil {
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

// The quickstart's scratch role is named godwit, so an unpinned "$user" resolves to the journal schema the
// replay's own bootstrap creates, and the history lands there instead of public.
func TestValidateReplaysUnqualifiedDDLIntoPublic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, pool := newStore(t)
	sched, _ := newScheduler(t, s, Config{Holder: "h"})
	id := "77777777-0000-0000-0000-000000000002"
	queueRun(t, s, id, map[string]string{
		"20260901120000_orders.up.sql":   "CREATE TABLE orders (id bigint PRIMARY KEY);",
		"20260901120000_orders.down.sql": "DROP TABLE orders;",
	})
	sched.Tick(ctx)
	waitState(t, s, id, StateSucceeded)

	v := NewValidator(NewScratch(pool, ""), s, func() string { return "unqualified" })
	plans, err := buildPlans([]engine.Migration{{
		Version: 20260901120001, Name: "status", Checksum: "c",
		UpSQL:   "ALTER TABLE public.orders ADD COLUMN status text;",
		DownSQL: "ALTER TABLE public.orders DROP COLUMN status;",
	}}, engine.DirectionUp)
	if err != nil {
		t.Fatal(err)
	}
	val, err := v.Validate(ctx, "app", plans, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(val.Base, "column public.orders.id bigint") {
		t.Fatalf("base = %q", val.Base)
	}
}

// Pinning must not win over the target's own path: a declared one is still mirrored, which is what keeps the
// replay's fingerprints comparable with the target's.
func TestValidateMirrorsTheTargetSearchPath(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, pool := newStore(t)
	registerSearchPathTarget(t, s, "app,public")
	sched := NewScheduler(s, map[string]creds.Provider{"plain": plainProvider{}}, PGEngine{}, Policies(), Config{Holder: "h"}, testLog)
	id := "77777777-0000-0000-0000-000000000003"
	if err := s.CreateRun(ctx, id, "app", RolloutDirect, searchPathFiles(), Timeouts{}, Provenance{}, "", nil); err != nil {
		t.Fatal(err)
	}
	sched.Tick(ctx)
	waitState(t, s, id, StateSucceeded)

	v := NewValidator(NewScratch(pool, ""), s, func() string { return "mirrored" })
	val, err := v.Validate(ctx, "app", nil, "app,public")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(val.Base, "column app.migrations.note text") {
		t.Fatalf("base = %q", val.Base)
	}
}

// A target whose own role is named godwit resolves "$user" to the journal schema, so its unqualified DDL
// lands beside the journal's own tables; the observation refuses before anything is planned against it.
func TestObserveRefusesAPathThatReachesTheJournal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newStore(t)
	sched, targetDSN := newScheduler(t, s, Config{Holder: "h"})
	cfg, err := pgx.ParseConfig(targetDSN)
	if err != nil {
		t.Fatal(err)
	}
	execDSN(t, targetDSN, "ALTER DATABASE "+pgx.Identifier{cfg.Database}.Sanitize()+" RESET search_path")
	insp := NewInspector(sched)

	_, err = insp.Observe(ctx, "app")
	if !errors.Is(err, ErrJournalOnSearchPath) || !strings.Contains(err.Error(), `element "$user"`) ||
		!strings.Contains(err.Error(), "--search-path public") {
		t.Fatalf("before the journal is bootstrapped: %v", err)
	}

	execDSN(t, targetDSN, "CREATE SCHEMA "+JournalSchema)
	if _, err := insp.Observe(ctx, "app"); !errors.Is(err, ErrJournalOnSearchPath) || !strings.Contains(err.Error(), `element "godwit"`) {
		t.Fatalf("with the journal bootstrapped: %v", err)
	}

	if err := s.RegisterTarget(ctx, "app", "plain", map[string]string{"dsn": targetDSN, ConfigSearchPath: "public"}); err != nil {
		t.Fatal(err)
	}
	obs, err := insp.Observe(ctx, "app")
	if err != nil || obs.SearchPath != "public" {
		t.Fatalf("with a declared path = %+v, err = %v", obs, err)
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
