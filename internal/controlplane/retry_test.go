package controlplane

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v4"
)

func TestTransientAndFailureDetail(t *testing.T) {
	t.Parallel()
	pg := func(code string) error { return fmt.Errorf("exec: %w", &pgconn.PgError{Code: code, Message: "m"}) }
	cases := []struct {
		name      string
		err       error
		transient bool
		code      string
		prefix    string
	}{
		{"serialization", pg("40001"), true, "40001", "transient: "},
		{"deadlock", pg("40P01"), true, "40P01", "transient: "},
		{"lock timeout", pg("55P03"), true, "55P03", "transient: "},
		{"statement cancelled", pg("57014"), true, "57014", "transient: "},
		{"too many connections", pg("53300"), true, "53300", "transient: "},
		{"insufficient resources", pg("53000"), true, "53000", "transient: "},
		{"disk full", pg("53100"), true, "53100", "transient: "},
		{"out of memory", pg("53200"), true, "53200", "transient: "},
		{"configuration limit", pg("53400"), true, "53400", "transient: "},
		{"operator intervention", pg("57000"), true, "57000", "transient: "},
		{"admin shutdown", pg("57P01"), true, "57P01", "transient: "},
		{"crash shutdown", pg("57P02"), true, "57P02", "transient: "},
		{"cannot connect now", pg("57P03"), true, "57P03", "transient: "},
		{"idle session timeout", pg("57P05"), true, "57P05", "transient: "},
		{"database dropped", pg("57P04"), false, "57P04", "sql: "},
		{"system error", pg("58000"), true, "58000", "transient: "},
		{"io error", pg("58030"), true, "58030", "transient: "},
		{"undefined file", pg("58P01"), false, "58P01", "sql: "},
		{"duplicate file", pg("58P02"), false, "58P02", "sql: "},
		{"connection class", pg("08006"), true, "08006", "transient: "},
		{"division by zero", pg("22012"), false, "22012", "sql: "},
		{"undefined table", pg("42P01"), false, "42P01", "sql: "},
		{"net error", fmt.Errorf("dial: %w", &net.OpError{Op: "dial", Err: errors.New("refused")}), true, ReasonNetwork, "transient: "},
		{"eof", fmt.Errorf("read: %w", io.EOF), true, ReasonNetwork, "transient: "},
		{"unexpected eof", io.ErrUnexpectedEOF, true, ReasonNetwork, "transient: "},
		{"conn closed", fmt.Errorf("exec: %w", pgconn.ErrConnClosed), true, ReasonNetwork, "transient: "},
		{"deadline", fmt.Errorf("apply: %w", context.DeadlineExceeded), true, ReasonTimeout, "transient: "},
		{"control plane", errors.New("unknown rollout"), false, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			code, ok := classify(tc.err)
			if ok != tc.transient || code != tc.code || Transient(tc.err) != tc.transient {
				t.Fatalf("classify = %q, %v", code, ok)
			}
			if got := FailureDetail(tc.err); got != tc.prefix+tc.err.Error() {
				t.Fatalf("detail = %q", got)
			}
		})
	}
}

func TestBackoff(t *testing.T) {
	t.Parallel()
	half := func() float64 { return 0.5 }
	cases := []struct {
		attempts int
		jitter   func() float64
		want     time.Duration
	}{
		{1, half, 2 * time.Second},
		{2, half, 4 * time.Second},
		{4, half, 16 * time.Second},
		{60, half, MaxBackoff},
		{1, func() float64 { return 0 }, 1600 * time.Millisecond},
		{1, func() float64 { return 1 }, 2400 * time.Millisecond},
	}
	for _, tc := range cases {
		if got := Backoff(2*time.Second, tc.attempts, tc.jitter); got != tc.want {
			t.Fatalf("Backoff(attempts=%d) = %s, want %s", tc.attempts, got, tc.want)
		}
	}
	if d := Backoff(time.Second, 1, defaultJitter); d < 800*time.Millisecond || d > 1200*time.Millisecond {
		t.Fatalf("default jitter = %s", d)
	}
	if got := retryDetail(errors.New("x"), 1500*time.Millisecond); got != "x (retry in 1.5s)" {
		t.Fatalf("retryDetail = %q", got)
	}
}

func flakyFiles(code string) map[string]string {
	return map[string]string{
		"20260901120000_seq.up.sql":     "CREATE SEQUENCE flaky;",
		"20260901120000_seq.down.sql":   "DROP SEQUENCE flaky;",
		"20260901120001_flaky.up.sql":   "DO $$ BEGIN IF nextval('flaky') < 2 THEN RAISE EXCEPTION 'flaky' USING ERRCODE = '" + code + "'; END IF; END $$;",
		"20260901120001_flaky.down.sql": "SELECT 1;",
	}
}

func TestSchedulerTransientRequeuesWithBackoff(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, pool := newStore(t)
	sched, targetDSN := newScheduler(t, s, Config{Holder: "h1", Interval: 10 * time.Second, Jitter: func() float64 { return 0.5 }})
	notifier := &recordingNotifier{}
	sched.Notifier = notifier
	const id = "44444444-0000-0000-0000-000000000001"
	queueRun(t, s, id, flakyFiles("40001"))

	sched.Tick(ctx)
	r := waitState(t, s, id, StateQueued)
	if r.Retries != 1 || r.Attempts != 1 || r.NotBefore == nil || !strings.HasPrefix(r.Error, "transient: statement 0 of 20260901120001_flaky") ||
		!strings.Contains(r.Error, "SQLSTATE 40001") || strings.Contains(r.Error, "retry in") {
		t.Fatalf("run = %+v", r)
	}
	sched.Tick(ctx)
	if again, _ := s.Run(ctx, id); again.Attempts != 1 || again.State != StateQueued {
		t.Fatalf("claimed before not_before: %+v", again)
	}
	if !tableExists(t, targetDSN, "flaky") {
		t.Fatal("first migration should stay applied across the retry")
	}
	// Expiring not_before in the store's own clock is what lets the next tick claim without racing the host's.
	if _, err := pool.Exec(ctx, "UPDATE cp_runs SET not_before = now() WHERE id = $1", id); err != nil {
		t.Fatal(err)
	}
	sched.Tick(ctx)
	r = waitState(t, s, id, StateSucceeded)
	if r.Attempts != 2 || r.Retries != 1 || r.Error != "" || r.NotBefore != nil {
		t.Fatalf("run = %+v", r)
	}
	if got := notifier.types(); got != "run:running run:retrying run:running run:succeeded" {
		t.Fatalf("notifications = %q", got)
	}
	if e := notifier.events[1]; e.State != StateQueued || !strings.HasPrefix(e.Detail, "transient: ") || !strings.Contains(e.Detail, "(retry in 10s)") {
		t.Fatalf("retrying event = %+v", e)
	}
	if out := scrape(t, sched.Metrics); !strings.Contains(out, `godwit_run_retries_total{code="40001",target="app"} 1`) {
		t.Fatalf("metrics:\n%s", out)
	}
}

// The store's clock sets not_before, so the test polls Tick instead of sleeping on the host's clock.
func tickUntil(t *testing.T, sched *Scheduler, s *Store, id, want string) Run {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		sched.Tick(context.Background())
		r, err := s.Run(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if r.State == want {
			return r
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("run %s never reached %q", id, want)

	return Run{}
}

func TestSchedulerGenuineFailsAtOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newStore(t)
	sched, _ := newScheduler(t, s, Config{Holder: "h1"})
	const id = "44444444-0000-0000-0000-000000000002"
	queueRun(t, s, id, flakyFiles("22012"))

	sched.Tick(ctx)
	r := waitState(t, s, id, StateFailed)
	if r.Retries != 0 || r.Attempts != 1 || r.NotBefore != nil || !strings.HasPrefix(r.Error, "sql: statement 0") {
		t.Fatalf("run = %+v", r)
	}
	if out := scrape(t, sched.Metrics); strings.Contains(out, "godwit_run_retries_total{") {
		t.Fatalf("metrics:\n%s", out)
	}
}

func TestSchedulerGivesUpAfterMaxAttempts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newStore(t)
	sched, _ := newScheduler(t, s, Config{Holder: "h1", Interval: 10 * time.Millisecond, MaxAttempts: 2, Jitter: func() float64 { return 0 }})
	notifier := &recordingNotifier{}
	sched.Notifier = notifier
	const id = "44444444-0000-0000-0000-000000000003"
	always := flakyFiles("55P03")
	always["20260901120001_flaky.up.sql"] = "DO $$ BEGIN RAISE EXCEPTION 'busy' USING ERRCODE = '55P03'; END $$;"
	queueRun(t, s, id, always)

	sched.Tick(ctx)
	if r := waitState(t, s, id, StateQueued); r.Retries != 1 {
		t.Fatalf("run = %+v", r)
	}
	r := tickUntil(t, sched, s, id, StateNeedsAttention)
	if r.Attempts != 2 || r.Retries != 1 || !strings.HasPrefix(r.Error, "transient: gave up after 2 attempts: statement 0") {
		t.Fatalf("run = %+v", r)
	}
	if got := notifier.types(); got != "run:running run:retrying run:running run:needs_attention" {
		t.Fatalf("notifications = %q", got)
	}
	if _, err := s.Resume(ctx, id); err != nil {
		t.Fatal(err)
	}
	if r, _ = s.Run(ctx, id); r.NotBefore != nil || r.Attempts != 0 || r.Retries != 1 {
		t.Fatalf("resumed = %+v", r)
	}
}

func TestStoreRetryErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newStore(t)
	if err := s.Retry(ctx, "44444444-0000-0000-0000-000000000009", "x", time.Second); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	mock, ms := newMockStore(t)
	mock.ExpectExec("WITH del AS").WithArgs("r1", "x", time.Second).WillReturnError(errBoom)
	if err := ms.Retry(ctx, "r1", "x", time.Second); err == nil || !strings.Contains(err.Error(), "retry run") {
		t.Fatalf("err = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSchedulerFailRetryStoreError(t *testing.T) {
	t.Parallel()
	mock, ms := newMockStore(t)
	mock.ExpectExec("WITH del AS").WithArgs("r1", pgxmock.AnyArg(), pgxmock.AnyArg()).WillReturnError(errBoom)
	sched := NewScheduler(ms, nil, PGEngine{}, Policies(), Config{Holder: "h"}, testLog)
	notifier := &recordingNotifier{}
	sched.Notifier = notifier
	finished := false
	sched.fail(context.Background(), Run{ID: "r1", Target: "app", Attempts: 1}, io.EOF, func(string, string) { finished = true })
	if finished || len(notifier.events) != 0 {
		t.Fatalf("finished = %v, events = %+v", finished, notifier.events)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestFilesHashMatchesMigrationBackfill(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, pool := newStore(t)
	files := map[string]string{"b.sql": "two\n", "a.sql": "one", "Z.sql": "zed"}
	if err := s.RegisterTarget(ctx, "app", "plain", map[string]string{"dsn": "x"}); err != nil {
		t.Fatal(err)
	}
	const id = "44444444-0000-0000-0000-000000000010"
	if err := s.SavePlan(ctx, Plan{ID: id, Target: "app", Key: "k", Rollout: RolloutDirect, CreatedBy: "ci"}, files); err != nil {
		t.Fatal(err)
	}
	var stored, sql string
	if err := pool.QueryRow(ctx, `
		SELECT p.files_hash, encode(sha256(convert_to(string_agg(f.name || E'\n' || f.body || E'\n', '' ORDER BY f.name COLLATE "C"), 'UTF8')), 'hex')
		FROM cp_plans p JOIN cp_plan_files f ON f.plan_id = p.id WHERE p.id = $1 GROUP BY p.files_hash`, id).Scan(&stored, &sql); err != nil {
		t.Fatal(err)
	}
	if want := FilesHash(files); stored != want || sql != want {
		t.Fatalf("stored = %s, sql = %s, want %s", stored, sql, want)
	}
	if FilesHash(map[string]string{"a.sql": "one\nb.sql\ntwo\n"}) == FilesHash(map[string]string{"a.sql": "one", "b.sql": "two"}) {
		t.Fatal("hash must not collide across file boundaries")
	}
}

func TestBoundAndRetirePlan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newStore(t)
	if err := s.RegisterTarget(ctx, "app", "plain", map[string]string{"dsn": "x"}); err != nil {
		t.Fatal(err)
	}
	files := goodFiles()
	hash := FilesHash(files)
	if _, err := s.BoundPlan(ctx, "app", RolloutDirect, hash); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	const planID, runID = "44444444-0000-0000-0000-000000000011", "44444444-0000-0000-0000-000000000012"
	if err := s.SavePlan(ctx, Plan{ID: planID, Target: "app", Key: "k", Rollout: RolloutDirect, CreatedBy: "ci"}, files); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BoundPlan(ctx, "app", RolloutDirect, hash); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ready plan must not re-attach: %v", err)
	}
	queueRun(t, s, runID, files)
	if err := s.BindPlan(ctx, planID, runID); err != nil {
		t.Fatal(err)
	}
	p, err := s.BoundPlan(ctx, "app", RolloutDirect, hash)
	if err != nil || p.ID != planID || p.RunID != runID {
		t.Fatalf("plan = %+v, err = %v", p, err)
	}
	if _, err := s.BoundPlan(ctx, "app", RolloutExpandContract, hash); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rollout must match: %v", err)
	}
	if err := s.RetirePlan(ctx, planID); err != nil {
		t.Fatal(err)
	}
	if err := s.RetirePlan(ctx, planID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second retire: %v", err)
	}
	if p, err := s.Plan(ctx, planID); err != nil || p.State != PlanSuperseded {
		t.Fatalf("plan = %+v, err = %v", p, err)
	}

	mock, ms := newMockStore(t)
	mock.ExpectQuery("AND files_hash = \\$3 AND state = 'bound'").WithArgs("app", RolloutDirect, hash).WillReturnError(errBoom)
	if _, err := ms.BoundPlan(ctx, "app", RolloutDirect, hash); err == nil || !strings.Contains(err.Error(), "load bound plan") {
		t.Fatalf("err = %v", err)
	}
	mock.ExpectExec("UPDATE cp_plans SET state = 'superseded' WHERE id = \\$1 AND state = 'bound'").WithArgs(planID).WillReturnError(errBoom)
	if err := ms.RetirePlan(ctx, planID); err == nil || !strings.Contains(err.Error(), "retire plan") {
		t.Fatalf("err = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
