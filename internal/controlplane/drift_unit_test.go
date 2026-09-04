package controlplane

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/SamuelMolling/godwit/internal/creds"
	"github.com/SamuelMolling/godwit/internal/engine"
	"github.com/SamuelMolling/godwit/internal/notify"
)

func TestDriftStoreClosedPool(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newStore(t)
	s.pool.(interface{ Close() }).Close()

	if err := s.SaveSnapshot(ctx, "a", "f", "d", ""); err == nil {
		t.Fatal("want error")
	}
	if _, err := s.SnapshotFor(ctx, "a"); err == nil {
		t.Fatal("want error")
	}
	if _, err := s.SnapshotTargets(ctx); err == nil {
		t.Fatal("want error")
	}
	if _, err := s.RecordDrift(ctx, "a", "f", "d"); err == nil {
		t.Fatal("want error")
	}
	if _, err := s.ResolveDrift(ctx, "a"); err == nil {
		t.Fatal("want error")
	}
	if _, _, err := s.GetTS(ctx, "run", "a"); err == nil {
		t.Fatal("want error")
	}
	if err := s.PutTS(ctx, "run", "a", "C", "1"); err == nil {
		t.Fatal("want error")
	}
	if _, err := s.ListDriftEvents(ctx, ""); err == nil {
		t.Fatal("want error")
	}
	if _, err := s.History(ctx, "a"); err == nil {
		t.Fatal("want error")
	}
}

func TestDriftStoreRowErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	mock, s := newMockStore(t)
	mock.ExpectQuery("SELECT target FROM cp_snapshots").
		WillReturnRows(pgxmock.NewRows([]string{"target"}).AddRow("a").RowError(0, errBoom))
	if _, err := s.SnapshotTargets(ctx); err == nil || !strings.Contains(err.Error(), "read snapshot targets") {
		t.Fatalf("err = %v", err)
	}

	mock2, s2 := newMockStore(t)
	mock2.ExpectQuery("FROM cp_drift_events").WithArgs("").
		WillReturnRows(pgxmock.NewRows([]string{"id", "target", "diff", "detected_at", "resolved_at"}).
			AddRow(int64(1), "a", "d", now(), nilTime()).RowError(0, errBoom))
	if _, err := s2.ListDriftEvents(ctx, ""); err == nil || !strings.Contains(err.Error(), "read drift events") {
		t.Fatalf("err = %v", err)
	}

	mock3, s3 := newMockStore(t)
	mock3.ExpectQuery("FROM cp_run_applied").WithArgs("a").
		WillReturnRows(pgxmock.NewRows([]string{"run_id", "migration", "up", "down", "expansion"}).
			AddRow("r", "m", "u", "d", []byte("null")).RowError(0, errBoom))
	if _, err := s3.History(ctx, "a"); err == nil || !strings.Contains(err.Error(), "read history") {
		t.Fatalf("err = %v", err)
	}
}

func TestBaselineErrorBranches(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("dsn resolution fails", func(t *testing.T) {
		t.Parallel()
		s, _ := newStore(t)
		sched := NewScheduler(s, map[string]creds.Provider{}, PGEngine{}, Policies(), Config{Holder: "h"}, testLog)
		if err := s.RegisterTarget(ctx, "app", "ghost", map[string]string{}); err != nil {
			t.Fatal(err)
		}
		sched.baseline(ctx, Run{ID: "id", Target: "app"}, testLog)
	})

	t.Run("target unreachable", func(t *testing.T) {
		t.Parallel()
		s, _ := newStore(t)
		sched := NewScheduler(s, map[string]creds.Provider{"plain": plainProvider{}},
			PGEngine{}, Policies(), Config{Holder: "h"}, testLog)
		if err := s.RegisterTarget(ctx, "app", "plain", map[string]string{"dsn": "postgres://bad:bad@127.0.0.1:1/x"}); err != nil {
			t.Fatal(err)
		}
		sched.baseline(ctx, Run{ID: "id", Target: "app"}, testLog)
	})

	t.Run("save fails", func(t *testing.T) {
		t.Parallel()
		s, _ := newStore(t)
		sched, _ := newScheduler(t, s, Config{Holder: "h"})
		if _, err := s.pool.Exec(ctx, "DROP TABLE cp_snapshots CASCADE"); err != nil {
			t.Fatal(err)
		}
		sched.baseline(ctx, Run{ID: "11111111-1111-1111-1111-111111111111", Target: "app"}, testLog)
	})
}

func TestDriftCheckResolveError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newStore(t)
	mon, targetDSN := newMonitor(t, s, notify.None{})
	_ = targetDSN

	if err := mon.AcceptBaseline(ctx, "app"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, "DROP TABLE cp_drift_events CASCADE"); err != nil {
		t.Fatal(err)
	}
	if _, err := mon.Check(ctx, "app"); err == nil {
		t.Fatal("want error")
	}
}

func TestDriftCheckRecordError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newStore(t)
	mon, targetDSN := newMonitor(t, s, notify.None{})

	if err := mon.AcceptBaseline(ctx, "app"); err != nil {
		t.Fatal(err)
	}
	execTarget(t, targetDSN, "CREATE TABLE drifted (id int)")
	if _, err := s.pool.Exec(ctx, "DROP TABLE cp_drift_events CASCADE"); err != nil {
		t.Fatal(err)
	}
	if _, err := mon.Check(ctx, "app"); err == nil {
		t.Fatal("want error")
	}
}

func TestRecordDriftRequiresCurrentBaseline(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newStore(t)
	newScheduler(t, s, Config{Holder: "h"})
	if err := s.SaveSnapshot(ctx, "app", "fp2", "def", ""); err != nil {
		t.Fatal(err)
	}

	if created, err := s.RecordDrift(ctx, "app", "fp1", "diff"); err != nil || created {
		t.Fatalf("stale baseline: created = %v, err = %v", created, err)
	}
	if created, err := s.RecordDrift(ctx, "app", "fp2", "diff"); err != nil || !created {
		t.Fatalf("current baseline: created = %v, err = %v", created, err)
	}
	if created, err := s.RecordDrift(ctx, "app", "fp2", "diff"); err != nil || created {
		t.Fatalf("duplicate: created = %v, err = %v", created, err)
	}
	if events, err := s.ListDriftEvents(ctx, "app"); err != nil || len(events) != 1 {
		t.Fatalf("events = %+v, err = %v", events, err)
	}
}

func waitLocked(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var waiting bool
		if err := pool.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock')`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("second insert never blocked on the first")
}

func TestRecordDriftConcurrentInsertsOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, pool := newStore(t)
	newScheduler(t, s, Config{Holder: "h"})
	if err := s.SaveSnapshot(ctx, "app", "fp", "def", ""); err != nil {
		t.Fatal(err)
	}

	first, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Rollback(ctx) }()
	if created, err := NewStore(first).RecordDrift(ctx, "app", "fp", "diff"); err != nil || !created {
		t.Fatalf("first: created = %v, err = %v", created, err)
	}

	second, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Rollback(ctx) }()
	type outcome struct {
		created bool
		err     error
	}
	done := make(chan outcome, 1)
	go func() {
		created, err := NewStore(second).RecordDrift(ctx, "app", "fp", "diff")
		done <- outcome{created, err}
	}()
	waitLocked(t, pool)
	if err := first.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if got := <-done; got.err != nil || got.created {
		t.Fatalf("second: created = %v, err = %v", got.created, got.err)
	}
	if err := second.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if events, err := s.ListDriftEvents(ctx, "app"); err != nil || len(events) != 1 {
		t.Fatalf("events = %+v, err = %v", events, err)
	}
}

func TestDriftEventDedupMigration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, newDatabase(t, "cp"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	before := slices.IndexFunc(storeMigrations, func(m engine.Migration) bool { return m.Name == "drift_event_dedup" })
	if n, err := applyMigrations(ctx, conn.Conn(), storeMigrations[:before]); err != nil || n != before {
		t.Fatalf("applied = %d, err = %v", n, err)
	}
	conn.Release()
	if _, err := pool.Exec(ctx, `
		INSERT INTO cp_targets (name, provider, config) VALUES ('app', 'plain', '{}'), ('other', 'plain', '{}');
		INSERT INTO cp_drift_events (target, diff, resolved_at) VALUES
			('app', 'same', NULL), ('app', 'same', NULL), ('app', 'same', NULL),
			('app', 'distinct', NULL), ('app', 'same', now()), ('other', 'same', NULL)`); err != nil {
		t.Fatal(err)
	}

	if n, err := Migrate(ctx, pool); err != nil || n != len(storeMigrations)-before {
		t.Fatalf("applied = %d, err = %v", n, err)
	}
	var open []int64
	rows, err := pool.Query(ctx, `SELECT id FROM cp_drift_events WHERE resolved_at IS NULL ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	var id int64
	if _, err := pgx.ForEachRow(rows, []any{&id}, func() error {
		open = append(open, id)

		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if want := []int64{1, 4, 6}; !slices.Equal(open, want) {
		t.Fatalf("open events = %v, want %v", open, want)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO cp_drift_events (target, diff) VALUES ('app', 'same')`); err == nil {
		t.Fatal("duplicate open event must be refused")
	}
}

type barrierEngine struct {
	Engine
	ready *sync.WaitGroup
}

func (e barrierEngine) Snapshot(ctx context.Context, dsn string) (string, string, error) {
	e.ready.Done()
	e.ready.Wait()

	return e.Engine.Snapshot(ctx, dsn)
}

func TestDriftMonitorsRaceRecordOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newStore(t)
	notifier := &recordingNotifier{}
	mon, targetDSN := newMonitor(t, s, notifier)
	if err := mon.AcceptBaseline(ctx, "app"); err != nil {
		t.Fatal(err)
	}
	execTarget(t, targetDSN, "CREATE TABLE rogue (id int)")

	const replicas = 3
	ready := &sync.WaitGroup{}
	ready.Add(replicas)
	mon.engine = barrierEngine{Engine: PGEngine{}, ready: ready}
	var checks sync.WaitGroup
	for range replicas {
		checks.Go(func() {
			if d, err := mon.Check(ctx, "app"); err != nil || !d.Drifted {
				t.Errorf("drift = %+v, err = %v", d, err)
			}
		})
	}
	checks.Wait()

	if events, err := s.ListDriftEvents(ctx, "app"); err != nil || len(events) != 1 {
		t.Fatalf("events = %+v, err = %v", events, err)
	}
	if got := notifier.types(); got != "drift:accepted drift:detected" {
		t.Fatalf("notifications = %q", got)
	}
}

type acceptBetween struct {
	Engine
	accept func()
	once   sync.Once
}

func (e *acceptBetween) Snapshot(ctx context.Context, dsn string) (string, string, error) {
	def, fp, err := e.Engine.Snapshot(ctx, dsn)
	e.once.Do(e.accept)

	return def, fp, err
}

func TestDriftCheckIgnoresBaselineAcceptedMidCheck(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newStore(t)
	notifier := &recordingNotifier{}
	accepting, targetDSN := newMonitor(t, s, notifier)
	if err := accepting.AcceptBaseline(ctx, "app"); err != nil {
		t.Fatal(err)
	}
	execTarget(t, targetDSN, "CREATE TABLE rogue (id int)")

	racing := *accepting
	racing.engine = &acceptBetween{Engine: PGEngine{}, accept: func() {
		if err := accepting.AcceptBaseline(ctx, "app"); err != nil {
			t.Fatal(err)
		}
	}}
	if _, err := racing.Check(ctx, "app"); err != nil {
		t.Fatal(err)
	}
	if events, err := s.ListDriftEvents(ctx, "app"); err != nil || len(events) != 0 {
		t.Fatalf("events = %+v, err = %v", events, err)
	}
	if d, err := racing.Check(ctx, "app"); err != nil || d.Drifted {
		t.Fatalf("drift = %+v, err = %v", d, err)
	}
	if got := notifier.types(); got != "drift:accepted drift:accepted" {
		t.Fatalf("notifications = %q", got)
	}
}

func TestValidatorCorruptHistory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, pool := newStore(t)
	sched, _ := newScheduler(t, s, Config{Holder: "h"})
	queueRun(t, s, "cccccccc-0000-0000-0000-000000000001", goodFiles())
	sched.Tick(ctx)
	waitState(t, s, "cccccccc-0000-0000-0000-000000000001", StateSucceeded)

	v := NewValidator(NewScratch(pool, ""), s, func() string { return "hist" })
	good, err := buildPlans([]engine.Migration{{
		Version: 2, Name: "next",
		UpSQL: "CREATE TABLE n (id int);", DownSQL: "DROP TABLE n;",
	}}, engine.DirectionUp)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.pool.Exec(ctx,
		`UPDATE cp_run_files SET body = '-- godwit: revert' WHERE name = '20260901120000_t.up.sql'`); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Validate(ctx, "app", good, ""); err == nil || !strings.Contains(err.Error(), "history run 0") {
		t.Fatalf("err = %v", err)
	}

	if _, err := s.pool.Exec(ctx,
		`UPDATE cp_run_files SET body = 'SELECT 1/0;' WHERE name = '20260901120000_t.up.sql'`); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Validate(ctx, "app", good, ""); err == nil || !strings.Contains(err.Error(), "replay history run 0") {
		t.Fatalf("err = %v", err)
	}
}

func TestValidatorScratchConnectFails(t *testing.T) {
	ctx := context.Background()
	s, pool := newStore(t)

	orig := connectScratch
	connectScratch = func(context.Context, *pgx.ConnConfig) (*pgx.Conn, error) { return nil, errBoom }
	defer func() { connectScratch = orig }()

	if err := s.RegisterTarget(ctx, "app", "plain", map[string]string{"dsn": "x"}); err != nil {
		t.Fatal(err)
	}
	v := NewValidator(NewScratch(pool, ""), s, func() string { return "connfail" })
	good, err := buildPlans([]engine.Migration{{
		Version: 1, Name: "ok",
		UpSQL: "SELECT 1;", DownSQL: "SELECT 1;",
	}}, engine.DirectionUp)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Validate(ctx, "app", good, ""); err == nil || !strings.Contains(err.Error(), "connect scratch database") {
		t.Fatalf("err = %v", err)
	}
}

func TestSchedulerBaselineAfterRunHasSnapshot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newStore(t)
	sched, _ := newScheduler(t, s, Config{Holder: "h"})
	queueRun(t, s, "bbbbbbbb-0000-0000-0000-000000000001", goodFiles())
	sched.Tick(ctx)
	waitState(t, s, "bbbbbbbb-0000-0000-0000-000000000001", StateSucceeded)

	if _, err := s.SnapshotFor(ctx, "app"); err != nil {
		t.Fatalf("baseline missing after success: %v", err)
	}
}

func TestNewDriftMonitorDefaultInterval(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)
	sched := NewScheduler(s, nil, PGEngine{}, Policies(), Config{Holder: "h"}, testLog)
	mon := NewDriftMonitor(s, sched, PGEngine{}, notify.None{}, 0, testLog)
	if mon.interval != 5*time.Minute {
		t.Fatalf("interval = %v", mon.interval)
	}
}

func TestDriftAcceptBaselineSaveError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newStore(t)
	mon, _ := newMonitor(t, s, notify.None{})
	if _, err := s.pool.Exec(ctx, "DROP TABLE cp_snapshots CASCADE"); err != nil {
		t.Fatal(err)
	}
	if err := mon.AcceptBaseline(ctx, "app"); err == nil {
		t.Fatal("want error")
	}
}
