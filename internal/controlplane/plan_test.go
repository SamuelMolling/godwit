package controlplane

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"

	"github.com/SamuelMolling/godwit/internal/engine"
)

func mig(version int64, name, up string) engine.Migration {
	m := engine.Migration{Version: version, Name: name, UpSQL: up, DownSQL: "SELECT 1;"}
	m.Checksum = sum(up)

	return m
}

func applied(m engine.Migration, at time.Time) engine.Applied {
	return engine.Applied{Version: m.Version, Name: m.Name, Checksum: m.Checksum, AppliedAt: at}
}

func TestPendingAndPlanKey(t *testing.T) {
	t.Parallel()
	a, b, c := mig(1, "a", "SELECT 1;"), mig(2, "b", "SELECT 2;"), mig(3, "c", "SELECT 3;")
	live := []engine.Applied{applied(a, time.Now())}

	pending, err := Pending([]engine.Migration{c, b, a}, live)
	if err != nil || len(pending) != 2 || pending[0].Version != 2 || pending[1].Version != 3 {
		t.Fatalf("pending = %+v, err = %v", pending, err)
	}
	key := PlanKey("app", RolloutDirect, pending)
	if key != PlanKey("app", RolloutDirect, []engine.Migration{b, c}) || key == PlanKey("app", RolloutExpandContract, pending) ||
		key == PlanKey("other", RolloutDirect, pending) || key == PlanKey("app", RolloutDirect, pending[:1]) {
		t.Fatal("key must depend on target, rollout and the pending files only")
	}
	changed := b
	changed.DownSQL = "SELECT 22;"
	if key == PlanKey("app", RolloutDirect, []engine.Migration{changed, c}) {
		t.Fatal("key must cover the down file")
	}

	edited := mig(1, "a", "SELECT 11;")
	if _, err := Pending([]engine.Migration{edited}, live); !errors.Is(err, ErrAppliedContent) ||
		!strings.HasPrefix(err.Error(), "00000000000001_a ") {
		t.Fatalf("err = %v", err)
	}
}

func TestHistoryHash(t *testing.T) {
	t.Parallel()
	a, b := mig(1, "a", "SELECT 1;"), mig(2, "b", "SELECT 2;")
	now := time.Now()
	one := HistoryHash([]engine.Applied{applied(a, now), applied(b, now)})
	if one != (Observation{Applied: []engine.Applied{applied(b, now), applied(a, now)}}).HistoryHash() {
		t.Fatal("hash must not depend on order")
	}
	if one == HistoryHash([]engine.Applied{applied(a, now)}) || one == HistoryHash(nil) {
		t.Fatal("hash must depend on the versions")
	}
	changed := applied(b, now)
	changed.Checksum = "other"
	if one == HistoryHash([]engine.Applied{applied(a, now), changed}) {
		t.Fatal("hash must depend on the checksums")
	}
}

func planFor(t *testing.T, migs ...engine.Migration) []engine.Plan {
	t.Helper()
	plans := make([]engine.Plan, 0, len(migs))
	for _, m := range migs {
		p, err := engine.BuildPlan(m, engine.DirectionUp)
		if err != nil {
			t.Fatal(err)
		}
		plans = append(plans, p)
	}

	return plans
}

func TestBuildPlanMigrationsAndSameStatements(t *testing.T) {
	t.Parallel()
	a := mig(1, "a", "CREATE TABLE a (id int);")
	b := mig(2, "b", "ALTER TABLE a DROP COLUMN id;\nCREATE INDEX CONCURRENTLY i ON a (id);")
	ms := BuildPlanMigrations(RolloutExpandContract, planFor(t, a, b), []int64{1})
	if len(ms) != 2 || !ms[0].Applied || ms[0].Phase != PhaseExpand || ms[1].Applied || ms[1].Phase != PhaseContract ||
		ms[1].Checksum != b.Checksum || len(ms[1].Statements) != 2 || !ms[1].Statements[1].NoTx ||
		len(ms[1].Statements[0].Hazards) == 0 || ms[1].Statements[0].Hazards[0].Code == "" {
		t.Fatalf("migrations = %+v", ms)
	}
	p := Plan{Migrations: ms}
	if pending := p.Pending(); len(pending) != 1 || pending[0].Version != 2 {
		t.Fatalf("pending = %+v", pending)
	}

	again := BuildPlanMigrations(RolloutExpandContract, planFor(t, a, b), nil)
	if !SameStatements(p.Pending(), again[1:]) {
		t.Fatal("same files must have the same statements regardless of the applied flag")
	}
	if SameStatements(ms, again[1:]) {
		t.Fatal("different lengths must differ")
	}
	direct := BuildPlanMigrations(RolloutDirect, planFor(t, a, b), nil)
	if SameStatements(ms, direct) {
		t.Fatal("phase changes must differ")
	}
	edited := BuildPlanMigrations(RolloutExpandContract, planFor(t, a, mig(2, "b", "ALTER TABLE a DROP COLUMN id;")), nil)
	if SameStatements(ms, edited) {
		t.Fatal("statement changes must differ")
	}
}

func TestStaleDiff(t *testing.T) {
	t.Parallel()
	a, b, c := mig(1, "a", "SELECT 1;"), mig(2, "b", "SELECT 2;"), mig(3, "c", "SELECT 3;")
	at := time.Date(2026, 9, 1, 10, 30, 0, 0, time.UTC)
	p := Plan{Applied: []engine.Applied{applied(a, at), applied(b, at)}, SchemaFingerprint: "f1", SchemaDefinition: "table a\n"}

	same := StaleDiff(p, Observation{Applied: p.Applied, Fingerprint: "f1"})
	if len(same.Added)+len(same.Removed)+len(same.Schema) != 0 || !same.Explained("f1", "f1") || same.Reason() != StaleSchema {
		t.Fatalf("diff = %+v", same)
	}

	d := StaleDiff(p, Observation{Applied: []engine.Applied{applied(c, at), applied(a, at)}, Fingerprint: "f2", Definition: "table a\ntable c\n"})
	if len(d.Added) != 1 || d.Added[0].Version != 3 || d.Added[0].Name != "c" || !d.Added[0].At.Equal(at) ||
		len(d.Removed) != 1 || d.Removed[0].Version != 2 || len(d.Schema) != 1 || d.Schema[0] != "+ table c" {
		t.Fatalf("diff = %+v", d)
	}
	if d.Explained("f2", "f2") || d.Reason() != StaleHistory {
		t.Fatal("removals are never explained")
	}

	dd := mig(4, "d", "SELECT 4;")
	d = StaleDiff(Plan{SchemaFingerprint: "f1"}, Observation{Applied: []engine.Applied{applied(dd, at), applied(c, at)}, Fingerprint: "f1"})
	if len(d.Added) != 2 || d.Added[0].Version != 3 || d.Added[1].Version != 4 || len(d.Schema) != 0 {
		t.Fatalf("diff = %+v", d)
	}
	d = StaleDiff(Plan{Applied: []engine.Applied{applied(dd, at), applied(c, at)}, SchemaFingerprint: "f1"}, Observation{Fingerprint: "f1"})
	if len(d.Removed) != 2 || d.Removed[0].Version != 3 || d.Removed[1].Version != 4 {
		t.Fatalf("diff = %+v", d)
	}

	d = StaleDiff(p, Observation{Applied: []engine.Applied{applied(a, at), applied(b, at), applied(c, at)}, Fingerprint: "f2"})
	if d.Explained("f2", "f2") || d.Reason() != StaleHistory {
		t.Fatal("an addition without a run is unexplained")
	}
	d.Added[0].RunID = "r1"
	if !d.Explained("f2", "f2") || d.Explained("f1", "f2") || d.Reason() != StaleSchema {
		t.Fatalf("diff = %+v", d)
	}
	if got := d.Added[0].String(); got != "00000000000003_c" {
		t.Fatalf("change = %q", got)
	}
}

func TestPlanStaleError(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 1, 10, 30, 0, 0, time.UTC)
	p := Plan{
		ID: "aaaaaaaa-0000-0000-0000-000000000001", Key: "0123456789abcdef", Target: "app",
		CreatedAt: at, CreatedBy: "ci", Source: "repo@sha",
	}
	e := &PlanStale{Plan: p, Reason: StaleHistory, Hint: "re-plan", Diff: PlanDiff{
		Added: []HistoryChange{
			{Version: 3, Name: "c", At: at.Add(time.Hour), RunID: "bbbbbbbb-0000-0000-0000-000000000002"},
			{Version: 4, Name: "d", At: at.Add(2 * time.Hour)},
		},
		Removed: []HistoryChange{{Version: 2, Name: "b"}},
		Schema:  []string{"+ table c", "+ table d"},
	}}
	want := "plan aaaaaaaa on app is stale (planned 2026-09-01T10:30:00Z by ci, repo@sha)\n" +
		"  reason : history\n" +
		"  history: + 00000000000003_c   applied 11:30Z by run bbbbbbbb (explained)\n" +
		"           + 00000000000004_d   applied 12:30Z by no run (unexplained)\n" +
		"           - 00000000000002_b   removed from history\n" +
		"  schema : + table c\n" +
		"           + table d\n" +
		"           (2 changes not made by any run since the plan)\n" +
		"  files  : unchanged (key 01234567…)\n" +
		"fix: re-plan"
	if got := e.Error(); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}

	p.Source = ""
	e = &PlanStale{Plan: p, Reason: StaleSchema, Hint: "accept"}
	want = "plan aaaaaaaa on app is stale (planned 2026-09-01T10:30:00Z by ci)\n  reason : schema\n  files  : unchanged (key 01234567…)\nfix: accept"
	if got := e.Error(); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}

	e = &PlanStale{Plan: Plan{Target: "app"}, Reason: StaleContent, Hint: "restore the file"}
	if got := e.Error(); got != "migration set on app cannot bind\n  reason : content\nfix: restore the file" {
		t.Fatalf("got:\n%s", got)
	}
}

func TestPlanRequiredError(t *testing.T) {
	t.Parallel()
	a, b, c := mig(1, "a", "SELECT 1;"), mig(2, "b", "SELECT 2;"), mig(3, "c", "SELECT 3;")
	at := time.Date(2026, 9, 1, 10, 30, 0, 0, time.UTC)
	e := &PlanRequired{Target: "app", Key: "0123456789abcdef", Pending: []engine.Migration{b, c}}
	want := "target app requires a stored plan and none matches this migration set (key 01234567…)\n" +
		"  nearest: none\n" +
		"fix: run `godwit plan --target app` on the pull request; the Action does it with command: plan"
	if got := e.Error(); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	if e.FilesDiff() != "" {
		t.Fatalf("files diff = %q", e.FilesDiff())
	}

	e.Nearest = []Plan{
		{ID: "aaaaaaaa-1", CreatedAt: at, Migrations: []PlanMigration{
			{Version: 1, Name: "a", Checksum: a.Checksum, Applied: true},
			{Version: 2, Name: "b", Checksum: "other"},
		}},
		{ID: "bbbbbbbb-2", CreatedAt: at.Add(time.Hour), Migrations: []PlanMigration{
			{Version: 2, Name: "b", Checksum: b.Checksum},
			{Version: 3, Name: "c", Checksum: c.Checksum},
		}},
	}
	want = "target app requires a stored plan and none matches this migration set (key 01234567…)\n" +
		"  nearest: plan aaaaaaaa (2026-09-01T10:30Z) covers 00000000000002_b\n" +
		"           this set : 00000000000002_b (up checksum differs), 00000000000003_c (not in plan)\n" +
		"           plan bbbbbbbb (2026-09-01T11:30Z) covers 00000000000002_b, 00000000000003_c\n" +
		"           this set : 00000000000002_b, 00000000000003_c\n" +
		"fix: run `godwit plan --target app` on the pull request; the Action does it with command: plan"
	if got := e.Error(); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	if got := e.FilesDiff(); got != "aaaaaaaa: 00000000000002_b (up checksum differs), 00000000000003_c (not in plan)\n"+
		"bbbbbbbb: 00000000000002_b, 00000000000003_c" {
		t.Fatalf("files diff = %q", got)
	}
}

func TestPGEngineObserve(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newStore(t)
	sched, dsn := newScheduler(t, s, Config{Holder: "h"})
	insp := NewInspector(sched)

	before := time.Now().UTC()
	obs, err := insp.Observe(ctx, "app")
	if err != nil || len(obs.Applied) != 0 || obs.Fingerprint == "" || obs.At.Before(before) {
		t.Fatalf("empty target = %+v, err = %v", obs, err)
	}

	const id = "cccccccc-0000-0000-0000-000000000011"
	queueRun(t, s, id, goodFiles())
	sched.Tick(ctx)
	waitState(t, s, id, StateSucceeded)
	after, err := PGEngine{}.Observe(ctx, dsn)
	if err != nil || len(after.Applied) != 1 || after.Applied[0].Version != 20260901120000 || after.Fingerprint == obs.Fingerprint ||
		!strings.Contains(after.Definition, "godwit.t.id") || after.HistoryHash() == obs.HistoryHash() {
		t.Fatalf("after run = %+v, err = %v", after, err)
	}

	if _, err := (PGEngine{}).Observe(ctx, "postgres://nobody@127.0.0.1:1/x"); err == nil || !strings.Contains(err.Error(), "connect target") {
		t.Fatalf("unreachable err = %v", err)
	}
	if _, err := insp.Observe(ctx, "ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown target err = %v", err)
	}
}

func TestObserveQueryErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	mock, err := pgxmock.NewConn()
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery("SELECT to_regclass").WillReturnError(errBoom)
	if _, err := observe(ctx, mock); err == nil || !strings.Contains(err.Error(), "probe godwit schema") {
		t.Fatalf("err = %v", err)
	}

	mock.ExpectQuery("SELECT to_regclass").WillReturnRows(pgxmock.NewRows([]string{"present"}).AddRow(false))
	mock.ExpectQuery("SELECT c.table_schema").WillReturnError(errBoom)
	if _, err := observe(ctx, mock); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
