package controlplane

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/SamuelMolling/godwit/internal/creds"
	"github.com/SamuelMolling/godwit/internal/engine"
	"github.com/SamuelMolling/godwit/internal/metrics"
)

var testLog = slog.New(slog.NewTextHandler(io.Discard, nil))

// plainProvider returns the DSN stored unencrypted in config.
type plainProvider struct{}

func (plainProvider) DSN(_ context.Context, config map[string]string) (string, error) {
	dsn, ok := config["dsn"]
	if !ok {
		return "", errors.New("missing dsn")
	}

	return dsn, nil
}

func newScheduler(t *testing.T, s *Store, cfg Config) (*Scheduler, string) {
	t.Helper()
	targetDSN := newDatabase(t, "tg")
	if err := s.RegisterTarget(context.Background(), "app", "plain", map[string]string{"dsn": targetDSN}); err != nil {
		t.Fatal(err)
	}
	providers := map[string]creds.Provider{"plain": plainProvider{}}

	return NewScheduler(s, providers, PGEngine{}, Policies(), cfg, testLog), targetDSN
}

func queueRun(t *testing.T, s *Store, id string, files map[string]string) {
	t.Helper()
	if err := s.CreateRun(context.Background(), id, "app", RolloutDirect, files); err != nil {
		t.Fatal(err)
	}
}

func waitState(t *testing.T, s *Store, id, want string) Run {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		r, err := s.Run(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if r.State == want {
			return r
		}
		time.Sleep(50 * time.Millisecond)
	}
	r, _ := s.Run(context.Background(), id)
	t.Fatalf("run %s stuck in %q (want %q): %+v", id, r.State, want, r)

	return Run{}
}

func TestSchedulerAppliesRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newStore(t)
	sched, targetDSN := newScheduler(t, s, Config{Holder: "h1"})

	queueRun(t, s, "11111111-0000-0000-0000-000000000001", goodFiles())
	sched.Tick(ctx)

	r := waitState(t, s, "11111111-0000-0000-0000-000000000001", StateSucceeded)
	if r.Error != "" {
		t.Fatalf("run = %+v", r)
	}

	conn, err := pgx.Connect(ctx, targetDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	var n int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM t").Scan(&n); err != nil {
		t.Fatalf("table t missing on target: %v", err)
	}
}

func TestSchedulerRunLoop(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)
	sched, _ := newScheduler(t, s, Config{Holder: "h1", Interval: 50 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sched.Run(ctx)

	queueRun(t, s, "11111111-0000-0000-0000-000000000002", goodFiles())
	waitState(t, s, "11111111-0000-0000-0000-000000000002", StateSucceeded)
}

func TestSchedulerFailureAndPark(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newStore(t)
	sched, _ := newScheduler(t, s, Config{Holder: "h1", MaxAttempts: 2})

	bad := map[string]string{
		"20260901120000_bad.up.sql":   "SELECT 1/0;",
		"20260901120000_bad.down.sql": "SELECT 1;",
	}
	queueRun(t, s, "11111111-0000-0000-0000-000000000003", bad)

	for range 2 {
		sched.Tick(ctx)
		r := waitState(t, s, "11111111-0000-0000-0000-000000000003", StateFailed)
		if !strings.Contains(r.Error, "statement 0") {
			t.Fatalf("error = %q", r.Error)
		}
		if _, err := s.Resume(ctx, r.ID); err != nil {
			t.Fatal(err)
		}
	}
	// Third failure exhausts MaxAttempts=2... resume resets attempts, so re-fail once more, then park.
	sched.Tick(ctx)
	waitState(t, s, "11111111-0000-0000-0000-000000000003", StateFailed)
}

func TestSchedulerParksAfterBudget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newStore(t)
	sched, _ := newScheduler(t, s, Config{Holder: "h1", MaxAttempts: 1})

	bad := map[string]string{
		"20260901120000_bad.up.sql":   "SELECT 1/0;",
		"20260901120000_bad.down.sql": "SELECT 1;",
	}
	queueRun(t, s, "11111111-0000-0000-0000-000000000004", bad)

	// Attempt 1 fails; force requeue keeping attempts by expiring the lease path:
	sched.Tick(ctx)
	waitState(t, s, "11111111-0000-0000-0000-000000000004", StateFailed)
	// Simulate a crashed executor: put it back to running with an expired lease.
	if _, err := s.pool.Exec(ctx, `
		UPDATE cp_runs SET state = 'running', finished_at = NULL WHERE id = $1`,
		"11111111-0000-0000-0000-000000000004"); err != nil {
		t.Fatal(err)
	}
	// Claim (attempt 2) exceeds MaxAttempts=1 → parked.
	sched.Tick(ctx)
	r := waitState(t, s, "11111111-0000-0000-0000-000000000004", StateNeedsAttention)
	if !strings.Contains(r.Error, "gave up") {
		t.Fatalf("error = %q", r.Error)
	}
}

func TestSchedulerFailoverBetweenReplicas(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newStore(t)

	// Replica 1 claims with an immediately-expiring lease and dies.
	newScheduler(t, s, Config{Holder: "replica-1"})
	queueRun(t, s, "11111111-0000-0000-0000-000000000005", goodFiles())
	if _, ok, err := s.Claim(ctx, "replica-1", -time.Second); err != nil || !ok {
		t.Fatalf("ok = %v, err = %v", ok, err)
	}

	// Replica 2 recovers the abandoned run and finishes it.
	providers := map[string]creds.Provider{"plain": plainProvider{}}
	m := metrics.New()
	sched2 := NewScheduler(s, providers, PGEngine{Metrics: m}, Policies(), Config{Holder: "replica-2"}, testLog)
	sched2.Metrics = m
	sched2.Tick(ctx)

	r := waitState(t, s, "11111111-0000-0000-0000-000000000005", StateSucceeded)
	if r.Attempts != 2 {
		t.Fatalf("attempts = %d, want 2 (claim + recovery)", r.Attempts)
	}
	for _, want := range []string{
		`godwit_run_resumes_total{source="reconciler",target="app"} 1`,
		`godwit_run_duration_seconds_count{result="succeeded",target="app"} 1`,
		`godwit_statement_duration_seconds_count{kind="tx",target="app"}`,
	} {
		if body := scrape(t, m); !strings.Contains(body, want) {
			t.Fatalf("metrics missing %q:\n%s", want, body)
		}
	}
}

func scrape(t *testing.T, m *metrics.Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	return rec.Body.String()
}

func TestPGEngineObserver(t *testing.T) {
	t.Parallel()

	PGEngine{}.observer("app")(engine.StatementEvent{})

	m := metrics.New()
	PGEngine{Metrics: m}.observer("app")(engine.StatementEvent{Statement: engine.Statement{NoTx: true}})
	if body := scrape(t, m); !strings.Contains(body, `godwit_statement_duration_seconds_count{kind="no_tx",target="app"} 1`) {
		t.Fatalf("metrics:\n%s", body)
	}
}

func TestSchedulerErrorBranches(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("claim error", func(t *testing.T) {
		t.Parallel()
		s, _ := newStore(t)
		sched := NewScheduler(s, nil, PGEngine{}, Policies(), Config{Holder: "h"}, testLog)
		s.pool.(interface{ Close() }).Close()
		sched.Tick(ctx) // must not panic
	})

	cases := []struct {
		name    string
		files   map[string]string
		target  map[string]string
		provs   map[string]creds.Provider
		wantErr string
	}{
		{
			name:    "bad migration files",
			files:   map[string]string{"garbage.txt": "x"},
			target:  map[string]string{"dsn": "postgres://x"},
			provs:   map[string]creds.Provider{"plain": plainProvider{}},
			wantErr: "unexpected file",
		},
		{
			name:    "unparsable sql",
			files:   map[string]string{"20260901120000_x.up.sql": "NOT SQL", "20260901120000_x.down.sql": "SELECT 1;"},
			target:  map[string]string{"dsn": "postgres://x"},
			provs:   map[string]creds.Provider{"plain": plainProvider{}},
			wantErr: "parse",
		},
		{
			name:    "unknown provider",
			files:   goodFiles(),
			target:  map[string]string{"dsn": "postgres://x"},
			provs:   map[string]creds.Provider{},
			wantErr: "unknown credential provider",
		},
		{
			name:    "provider fails",
			files:   goodFiles(),
			target:  map[string]string{},
			provs:   map[string]creds.Provider{"plain": plainProvider{}},
			wantErr: "missing dsn",
		},
		{
			name:    "target unreachable",
			files:   goodFiles(),
			target:  map[string]string{"dsn": "postgres://bad:bad@127.0.0.1:1/x"},
			provs:   map[string]creds.Provider{"plain": plainProvider{}},
			wantErr: "connect target",
		},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, _ := newStore(t)
			if err := s.RegisterTarget(ctx, "app", "plain", tc.target); err != nil {
				t.Fatal(err)
			}
			id := "22222222-0000-0000-0000-00000000000" + string(rune('1'+i))
			queueRun(t, s, id, tc.files)
			sched := NewScheduler(s, tc.provs, PGEngine{}, Policies(), Config{Holder: "h"}, testLog)
			sched.Tick(ctx)
			r := waitState(t, s, id, StateFailed)
			if !strings.Contains(r.Error, tc.wantErr) {
				t.Fatalf("error = %q, want containing %q", r.Error, tc.wantErr)
			}
		})
	}
}

func TestSchedulerUnknownRollout(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newStore(t)
	if err := s.RegisterTarget(ctx, "app", "plain", map[string]string{"dsn": "postgres://x"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRun(ctx, "33333333-0000-0000-0000-000000000001", "app", "canary", goodFiles()); err != nil {
		t.Fatal(err)
	}

	sched := NewScheduler(s, map[string]creds.Provider{"plain": plainProvider{}},
		PGEngine{}, Policies(), Config{Holder: "h"}, testLog)
	sched.Tick(ctx)

	r := waitState(t, s, "33333333-0000-0000-0000-000000000001", StateFailed)
	if !strings.Contains(r.Error, `unknown rollout policy "canary"`) {
		t.Fatalf("error = %q", r.Error)
	}
}

func TestSchedulerExpandContract(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newStore(t)
	sched, targetDSN := newScheduler(t, s, Config{Holder: "h1"})

	files := map[string]string{
		"20260901120000_t.up.sql":      upBody,
		"20260901120000_t.down.sql":    "DROP TABLE t;",
		"20260901120001_drop.up.sql":   "DROP TABLE t;",
		"20260901120001_drop.down.sql": upBody,
	}
	id := "55555555-0000-0000-0000-000000000001"
	if err := s.CreateRun(ctx, id, "app", RolloutExpandContract, files); err != nil {
		t.Fatal(err)
	}

	sched.Tick(ctx)
	r := waitState(t, s, id, StateAwaitingContract)
	if r.Phase != PhaseExpand || r.Error != "" {
		t.Fatalf("run = %+v", r)
	}
	if !tableExists(t, targetDSN, "t") {
		t.Fatal("expand phase must have created t")
	}
	if _, ok, err := s.Claim(ctx, "h1", time.Minute); err != nil || ok {
		t.Fatalf("awaiting run must not be claimable: ok = %v, err = %v", ok, err)
	}

	if err := s.Confirm(ctx, id); err != nil {
		t.Fatal(err)
	}
	sched.Tick(ctx)
	r = waitState(t, s, id, StateSucceeded)
	if r.Phase != PhaseContract {
		t.Fatalf("run = %+v", r)
	}
	if tableExists(t, targetDSN, "t") {
		t.Fatal("contract phase must have dropped t")
	}
}

func TestSchedulerRevertsRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newStore(t)
	sched, targetDSN := newScheduler(t, s, Config{Holder: "h1"})

	id := "77777777-0000-0000-0000-000000000001"
	queueRun(t, s, id, map[string]string{
		"20260901120000_t.up.sql":   upBody,
		"20260901120000_t.down.sql": "DROP TABLE t;",
		"20260901120001_u.up.sql":   "CREATE VIEW u AS SELECT id FROM t;",
		"20260901120001_u.down.sql": "DROP VIEW u;",
	})
	sched.Tick(ctx)
	waitState(t, s, id, StateSucceeded)

	revert := "77777777-0000-0000-0000-000000000002"
	if err := s.CreateRevert(ctx, revert, id); err != nil {
		t.Fatal(err)
	}
	sched.Tick(ctx)
	r := waitState(t, s, revert, StateSucceeded)
	if r.Reverts != id || r.Error != "" {
		t.Fatalf("revert run = %+v", r)
	}
	if tableExists(t, targetDSN, "t") || tableExists(t, targetDSN, "u") {
		t.Fatal("revert must drop u then t")
	}
	if r, _ = s.Run(ctx, id); r.State != StateReverted {
		t.Fatalf("original = %+v", r)
	}
	if snap, err := s.SnapshotFor(ctx, "app"); err != nil || strings.Contains(snap.Definition, "public.t.id") {
		t.Fatalf("baseline after revert = %+v, err = %v", snap, err)
	}
}

func tableExists(t *testing.T, dsn, name string) bool {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	var exists bool
	if err := conn.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", name).Scan(&exists); err != nil {
		t.Fatal(err)
	}

	return exists
}

func TestSchedulerHeartbeatKeepsLease(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newStore(t)

	sched, _ := newScheduler(t, s, Config{Holder: "h1", TTL: 300 * time.Millisecond})

	// A migration slow enough to need at least one heartbeat cycle.
	files := map[string]string{
		"20260901120000_slow.up.sql":   "SELECT pg_sleep(1);",
		"20260901120000_slow.down.sql": "SELECT 1;",
	}
	queueRun(t, s, "44444444-0000-0000-0000-000000000001", files)
	sched.Tick(ctx)

	r := waitState(t, s, "44444444-0000-0000-0000-000000000001", StateSucceeded)
	if r.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1 (lease must have been heartbeated, never stolen)", r.Attempts)
	}
}
