package controlplane

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
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
	if _, err := s.RecordDrift(ctx, "a", "d"); err == nil {
		t.Fatal("want error")
	}
	if err := s.ResolveDrift(ctx, "a"); err == nil {
		t.Fatal("want error")
	}
	if _, err := s.ListDriftEvents(ctx, ""); err == nil {
		t.Fatal("want error")
	}
	if _, err := s.HistoryFiles(ctx, "a"); err == nil {
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
	mock3.ExpectQuery("FROM cp_run_files").WithArgs("a").
		WillReturnRows(pgxmock.NewRows([]string{"run_id", "name", "body"}).
			AddRow("r", "n", "b").RowError(0, errBoom))
	if _, err := s3.HistoryFiles(ctx, "a"); err == nil || !strings.Contains(err.Error(), "read history files") {
		t.Fatalf("err = %v", err)
	}
}

func TestBaselineErrorBranches(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("dsn resolution fails", func(t *testing.T) {
		t.Parallel()
		s, _ := newStore(t)
		sched := NewScheduler(s, map[string]creds.Provider{}, PGEngine{}, Immediate{}, Config{Holder: "h"}, testLog)
		if err := s.RegisterTarget(ctx, "app", "ghost", map[string]string{}); err != nil {
			t.Fatal(err)
		}
		sched.baseline(ctx, Run{ID: "id", Target: "app"}, testLog)
	})

	t.Run("target unreachable", func(t *testing.T) {
		t.Parallel()
		s, _ := newStore(t)
		sched := NewScheduler(s, map[string]creds.Provider{"plain": plainProvider{}},
			PGEngine{}, Immediate{}, Config{Holder: "h"}, testLog)
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
	// Fingerprints match, but resolving drift fails: table dropped out from under it.
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

func TestValidatorCorruptHistory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, pool := newStore(t)
	sched, _ := newScheduler(t, s, Config{Holder: "h"})
	queueRun(t, s, "cccccccc-0000-0000-0000-000000000001", goodFiles())
	sched.Tick(ctx)
	waitState(t, s, "cccccccc-0000-0000-0000-000000000001", StateSucceeded)

	v := NewValidator(pool, s, func() string { return "hist" })
	good, err := buildPlans([]engine.Migration{{
		Version: 2, Name: "next",
		UpSQL: "CREATE TABLE n (id int);", DownSQL: "DROP TABLE n;",
	}})
	if err != nil {
		t.Fatal(err)
	}

	// Unloadable history file.
	if _, err := s.pool.Exec(ctx,
		`UPDATE cp_run_files SET name = 'garbage.txt' WHERE name = '20260901120000_t.up.sql'`); err != nil {
		t.Fatal(err)
	}
	if err := v.Validate(ctx, "app", good); err == nil || !strings.Contains(err.Error(), "history run 0") {
		t.Fatalf("err = %v", err)
	}

	// Replayable but failing history.
	if _, err := s.pool.Exec(ctx,
		`UPDATE cp_run_files SET name = '20260901120000_t.up.sql', body = 'SELECT 1/0;' WHERE name = 'garbage.txt'`); err != nil {
		t.Fatal(err)
	}
	if err := v.Validate(ctx, "app", good); err == nil || !strings.Contains(err.Error(), "replay history run 0") {
		t.Fatalf("err = %v", err)
	}
}

func TestValidatorScratchConnectFails(t *testing.T) {
	ctx := context.Background()
	s, pool := newStore(t)

	orig := connectScratch
	connectScratch = func(context.Context, *pgx.ConnConfig) (*pgx.Conn, error) { return nil, errBoom }
	defer func() { connectScratch = orig }()

	v := NewValidator(pool, s, func() string { return "connfail" })
	good, err := buildPlans([]engine.Migration{{
		Version: 1, Name: "ok",
		UpSQL: "SELECT 1;", DownSQL: "SELECT 1;",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Validate(ctx, "app", good); err == nil || !strings.Contains(err.Error(), "connect scratch database") {
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
	sched := NewScheduler(s, nil, PGEngine{}, Immediate{}, Config{Holder: "h"}, testLog)
	mon := NewDriftMonitor(s, sched, PGEngine{}, notify.None{}, 0, testLog)
	if mon.interval != 5*time.Minute {
		t.Fatalf("interval = %v", mon.interval)
	}
}
