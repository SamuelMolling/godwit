package controlplane

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/SamuelMolling/godwit/internal/creds"
	"github.com/SamuelMolling/godwit/internal/engine"
)

func TestTimeoutsOptions(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		in      Timeouts
		want    engine.Options
		wantErr string
	}{
		{name: "empty inherits", in: Timeouts{}, want: engine.Options{}},
		{name: "both set", in: Timeouts{Lock: "2s", Statement: "1m"}, want: engine.Options{LockTimeout: 2 * time.Second, StatementTimeout: time.Minute}},
		{name: "statement zero disables", in: Timeouts{Statement: "0"}, want: engine.Options{}},
		{name: "lock zero rejected", in: Timeouts{Lock: "0"}, wantErr: "lock_timeout: 0 must be at least 1ms"},
		{name: "lock negative rejected", in: Timeouts{Lock: "-1s"}, wantErr: "lock_timeout: -1s must be at least 1ms"},
		{name: "statement sub-millisecond rejected", in: Timeouts{Statement: "500us"}, wantErr: "statement_timeout: 500us must be at least 1ms"},
		{name: "lock unparsable", in: Timeouts{Lock: "soon"}, wantErr: `lock_timeout: time: invalid duration "soon"`},
		{name: "statement unparsable", in: Timeouts{Lock: "1s", Statement: "5"}, wantErr: `statement_timeout: time: missing unit in duration "5"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := tc.in.Options()
			if tc.wantErr != "" {
				if err == nil || err.Error() != tc.wantErr {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}

				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("opts = %+v, err = %v", got, err)
			}
		})
	}
}

func TestTimeoutsOverAndTarget(t *testing.T) {
	t.Parallel()
	target := TargetTimeouts(map[string]string{"dsn": "x", ConfigLockTimeout: "5s", ConfigStatementTimeout: "1m"})
	if target != (Timeouts{Lock: "5s", Statement: "1m"}) {
		t.Fatalf("target = %+v", target)
	}
	if got := (Timeouts{Statement: "0"}).Over(target); got != (Timeouts{Lock: "5s", Statement: "0"}) {
		t.Fatalf("merged = %+v", got)
	}
	if got := (Timeouts{Lock: "1s"}).Over(Timeouts{}); got != (Timeouts{Lock: "1s"}) {
		t.Fatalf("merged = %+v", got)
	}
	if got := TargetTimeouts(nil); got != (Timeouts{}) {
		t.Fatalf("nil config = %+v", got)
	}
}

func TestStorePersistsTimeouts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newStore(t)
	if err := s.RegisterTarget(ctx, "app", "static", map[string]string{}); err != nil {
		t.Fatal(err)
	}
	id, revert := "88888888-0000-0000-0000-000000000001", "88888888-0000-0000-0000-000000000002"
	if err := s.CreateRun(ctx, id, "app", RolloutDirect, goodFiles(), Timeouts{Lock: "2s"}); err != nil {
		t.Fatal(err)
	}
	r, err := s.Run(ctx, id)
	if err != nil || r.Timeouts != (Timeouts{Lock: "2s"}) {
		t.Fatalf("run = %+v, err = %v", r, err)
	}
	if err := s.Finish(ctx, id, StateSucceeded, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRevert(ctx, revert, id, Timeouts{Statement: "0"}); err != nil {
		t.Fatal(err)
	}
	runs, err := s.ListRuns(ctx, "app")
	if err != nil || len(runs) != 2 || runs[0].Timeouts != (Timeouts{Statement: "0"}) || runs[1].Timeouts != (Timeouts{Lock: "2s"}) {
		t.Fatalf("runs = %+v, err = %v", runs, err)
	}
}

const settingsBody = `CREATE TABLE settings AS SELECT current_setting('lock_timeout') AS lock, current_setting('statement_timeout') AS statement;`

func settingsFiles() map[string]string {
	return map[string]string{
		"20260901120000_settings.up.sql":   settingsBody,
		"20260901120000_settings.down.sql": "DROP TABLE settings;",
	}
}

func readSettings(t *testing.T, dsn string) (string, string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	var lock, statement string
	if err := conn.QueryRow(ctx, "SELECT lock, statement FROM settings").Scan(&lock, &statement); err != nil {
		t.Fatal(err)
	}

	return lock, statement
}

func TestSchedulerAppliesEffectiveTimeouts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cases := []struct {
		name        string
		target, run Timeouts
		lock, stmt  string
	}{
		{name: "defaults", lock: "5s", stmt: "0"},
		{name: "target", target: Timeouts{Lock: "2s", Statement: "1m"}, lock: "2s", stmt: "1min"},
		{name: "run overrides target", target: Timeouts{Lock: "2s", Statement: "1m"}, run: Timeouts{Lock: "1500ms", Statement: "0"}, lock: "1500ms", stmt: "0"},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, _ := newStore(t)
			targetDSN := newDatabase(t, "tt")
			config := map[string]string{"dsn": targetDSN, ConfigLockTimeout: tc.target.Lock, ConfigStatementTimeout: tc.target.Statement}
			if err := s.RegisterTarget(ctx, "app", "plain", config); err != nil {
				t.Fatal(err)
			}
			sched := NewScheduler(s, map[string]creds.Provider{"plain": plainProvider{}}, PGEngine{}, Policies(), Config{Holder: "h"}, testLog)
			id := "99999999-0000-0000-0000-00000000000" + string(rune('1'+i))
			if err := s.CreateRun(ctx, id, "app", RolloutDirect, settingsFiles(), tc.run); err != nil {
				t.Fatal(err)
			}
			sched.Tick(ctx)
			waitState(t, s, id, StateSucceeded)
			if lock, stmt := readSettings(t, targetDSN); lock != tc.lock || stmt != tc.stmt {
				t.Fatalf("lock_timeout = %q, statement_timeout = %q", lock, stmt)
			}
		})
	}
}

func TestSchedulerRejectsBadStoredTimeout(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, pool := newStore(t)
	sched, _ := newScheduler(t, s, Config{Holder: "h"})
	id := "99999999-0000-0000-0000-000000000009"
	queueRun(t, s, id, goodFiles())
	if _, err := pool.Exec(ctx, `UPDATE cp_runs SET lock_timeout = 'soon' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	sched.Tick(ctx)
	if r := waitState(t, s, id, StateFailed); !strings.Contains(r.Error, `lock_timeout: time: invalid duration "soon"`) {
		t.Fatalf("error = %q", r.Error)
	}
}
