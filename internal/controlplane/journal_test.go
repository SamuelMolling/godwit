package controlplane

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/SamuelMolling/godwit/internal/engine"
)

const journalIndexSQL = `CREATE INDEX CONCURRENTLY IF NOT EXISTS orders_customer_created_idx ON public.orders (customer_id, created_at DESC) WHERE NOT archived`

func journalFiles() map[string]string {
	return map[string]string{
		"20260901120000_orders.up.sql": "CREATE TABLE public.orders (id bigserial PRIMARY KEY, customer_id bigint NOT NULL, created_at timestamptz NOT NULL DEFAULT now(), archived boolean NOT NULL DEFAULT false);\n" +
			journalIndexSQL + ";\n",
		"20260901120000_orders.down.sql": "DROP TABLE public.orders;",
	}
}

func journalInspector(t *testing.T, files map[string]string, id, want string) (*Inspector, *Store) {
	t.Helper()
	ctx := context.Background()
	s, _ := newStore(t)
	sched, _ := newScheduler(t, s, Config{Holder: "h", MaxAttempts: 1})
	queueRun(t, s, id, files)
	sched.Tick(ctx)
	waitState(t, s, id, want)

	return NewInspector(sched), s
}

func clearJournal(t *testing.T, dsn string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	for _, table := range []string{"godwit.journal", "godwit.runs"} {
		if _, err := conn.Exec(ctx, "DELETE FROM "+table); err != nil {
			t.Fatal(err)
		}
	}
}

func TestInspectorJournalReadsEveryStatement(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const id = "d1d1d1d1-0000-0000-0000-000000000001"
	insp, _ := journalInspector(t, journalFiles(), id, StateSucceeded)

	j, err := insp.Journal(ctx, "app", "20260901120000_orders")
	if err != nil {
		t.Fatal(err)
	}
	if j.Target != "app" || j.RunID != id || j.AppliedAt.IsZero() || j.Adopted || j.RecordedOnly || j.BodiesSwept {
		t.Fatalf("journal = %+v", j)
	}
	if len(j.Attempts) != 1 {
		t.Fatalf("attempts = %d", len(j.Attempts))
	}
	a := j.Attempts[0]
	if a.Direction != "up" || a.State != "succeeded" || a.Error != "" || a.StmtCount != 2 || a.FinishedAt == nil {
		t.Fatalf("attempt = %+v", a.JournalRun)
	}
	if len(a.Statements) != 2 {
		t.Fatalf("statements = %+v", a.Statements)
	}
	if st := a.Statements[0]; st.Outcome != OutcomeDone || st.NoTx || st.DoneAt == nil || st.IntentAt != nil ||
		!strings.Contains(st.SQL, "CREATE TABLE public.orders") || st.Hash == "" {
		t.Fatalf("transactional statement = %+v", st)
	}
	if st := a.Statements[1]; st.Outcome != OutcomeDone || !st.NoTx || st.IntentAt == nil || st.DoneAt == nil ||
		st.Verifier != string(engine.VerifierCreateIndexConcurrently) || !strings.Contains(st.SQL, "orders_customer_created_idx") {
		t.Fatalf("non-transactional statement = %+v", st)
	}
}

func TestInspectorJournalNamesTheStatementThatFailed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const id = "d2d2d2d2-0000-0000-0000-000000000002"
	files := map[string]string{
		"20260901120000_orders.up.sql": "CREATE TABLE public.orders (id int);\n" +
			"ALTER TABLE public.orders ADD COLUMN total numeric NOT NULL DEFAULT (1 / 0);\n" +
			"CREATE INDEX CONCURRENTLY orders_total_idx ON public.orders (total);\n",
		"20260901120000_orders.down.sql": "DROP TABLE public.orders;",
	}
	insp, _ := journalInspector(t, files, id, StateFailed)

	j, err := insp.Journal(ctx, "app", "20260901120000_orders")
	if err != nil {
		t.Fatal(err)
	}
	if j.RunID != id || j.Adopted || j.RecordedOnly || j.BodiesSwept {
		t.Fatalf("journal = %+v", j)
	}
	a := j.Attempts[0]
	if a.State != "failed" || !strings.Contains(a.Error, "division by zero") {
		t.Fatalf("attempt = %+v", a.JournalRun)
	}
	want := []string{OutcomeDone, OutcomeFailed, OutcomeNotReached}
	if len(a.Statements) != len(want) {
		t.Fatalf("statements = %+v", a.Statements)
	}
	for i, outcome := range want {
		if a.Statements[i].Outcome != outcome || a.Statements[i].SQL == "" {
			t.Fatalf("statement %d = %+v, want %s", i, a.Statements[i], outcome)
		}
	}
	if a.Statements[1].DoneAt != nil || a.Statements[2].DoneAt != nil {
		t.Fatalf("nothing above the failure may be journalled done: %+v", a.Statements)
	}
}

func TestInspectorJournalWithoutTheStoredBodies(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const id = "d3d3d3d3-0000-0000-0000-000000000003"
	insp, s := journalInspector(t, journalFiles(), id, StateSucceeded)
	if _, err := s.pool.Exec(ctx, `DELETE FROM cp_run_files WHERE run_id = $1`, id); err != nil {
		t.Fatal(err)
	}

	j, err := insp.Journal(ctx, "app", "20260901120000_orders")
	if err != nil {
		t.Fatal(err)
	}
	if j.RunID != "" || !j.BodiesSwept || len(j.Attempts) != 1 {
		t.Fatalf("journal = %+v", j)
	}
	for _, st := range j.Attempts[0].Statements {
		if st.SQL != "" || st.Hash == "" || st.Outcome != OutcomeDone {
			t.Fatalf("statement = %+v", st)
		}
	}
}

func TestInspectorJournalOnAMigrationThatNeverRan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const id = "d4d4d4d4-0000-0000-0000-000000000004"
	insp, _ := journalInspector(t, journalFiles(), id, StateSucceeded)

	j, err := insp.Journal(ctx, "app", "20260101000000_ghost")
	if err != nil {
		t.Fatal(err)
	}
	if j.RunID != "" || len(j.Attempts) != 0 || j.RecordedOnly || j.Adopted || j.BodiesSwept {
		t.Fatalf("unknown migration = %+v", j)
	}
}

func TestInspectorJournalReportsRecordedOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const id = "d5d5d5d5-0000-0000-0000-000000000005"
	insp, s := journalInspector(t, journalFiles(), id, StateSucceeded)
	clearJournal(t, mustTargetDSN(t, s))

	j, err := insp.Journal(ctx, "app", "20260901120000_orders")
	if err != nil {
		t.Fatal(err)
	}
	if !j.RecordedOnly || len(j.Attempts) != 0 {
		t.Fatalf("journal = %+v", j)
	}

	if _, err := s.pool.Exec(ctx, `UPDATE cp_run_applied SET adopted = true WHERE run_id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if j, err = insp.Journal(ctx, "app", "20260901120000_orders"); err != nil || !j.Adopted || j.RecordedOnly {
		t.Fatalf("adopted = %+v, err = %v", j, err)
	}
}

func TestInspectorJournalTargetErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newStore(t)
	sched, _ := newScheduler(t, s, Config{Holder: "h"})
	insp := NewInspector(sched)

	if _, err := insp.Journal(ctx, "ghost", "20260901120000_orders"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown target err = %v", err)
	}
	if err := s.RegisterTarget(ctx, "nodsn", "plain", map[string]string{}); err != nil {
		t.Fatal(err)
	}
	j, err := insp.Journal(ctx, "nodsn", "20260901120000_orders")
	if err != nil || !strings.Contains(j.Unreachable, "missing dsn") || j.Target != "nodsn" {
		t.Fatalf("unresolved credential = %+v, err = %v", j, err)
	}
	if err := s.RegisterTarget(ctx, "broken", "plain", map[string]string{"dsn": "postgres://nobody@127.0.0.1:1/x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := insp.Journal(ctx, "broken", "20260901120000_orders"); err == nil || !strings.Contains(err.Error(), "connect target") {
		t.Fatalf("unreachable err = %v", err)
	}
	if _, err := s.pool.Exec(ctx, `DROP TABLE cp_run_applied CASCADE`); err != nil {
		t.Fatal(err)
	}
	if _, err := insp.Journal(ctx, "app", "20260901120000_orders"); err == nil ||
		!strings.Contains(err.Error(), "find the run that carried") {
		t.Fatalf("broken ledger err = %v", err)
	}
}

func TestMigrationKeySplitsRepeatablesFromVersions(t *testing.T) {
	t.Parallel()

	if v, r := migrationKey("R__views"); v != 0 || r != "views" {
		t.Fatalf("repeatable = %d, %q", v, r)
	}
	if v, r := migrationKey("20260901120000_orders"); v != 20260901120000 || r != "" {
		t.Fatalf("versioned = %d, %q", v, r)
	}
}

func TestExecutedStatementsRefuseWhatItCannotRebuild(t *testing.T) {
	t.Parallel()

	if up, down := executedStatements("20260901120000_orders", LedgerEntry{}); up != nil || down != nil {
		t.Fatalf("no body = %+v, %+v", up, down)
	}
	broken := LedgerEntry{UpSQL: "CREATE TABLE ((;", DownSQL: "DROP TABLE t;"}
	if up, _ := executedStatements("20260901120000_orders", broken); up != nil {
		t.Fatalf("unparseable body = %+v", up)
	}
	mismatched := LedgerEntry{
		UpSQL: "CREATE TABLE t (id int);", DownSQL: "DROP TABLE t;",
		Expansion: &Expansion{ID: "20260901120000_orders", UpSQL: "CREATE TABLE ((;", Phase: []string{"expand"}},
	}
	if up, _ := executedStatements("20260901120000_orders", mismatched); up != nil {
		t.Fatalf("unusable expansion = %+v", up)
	}
	if got := sideOf(map[string]string{}, engine.DirectionUp, nil); got != nil {
		t.Fatalf("no files = %+v", got)
	}
}

func TestAttemptsOfReadsTheDownSideOfARevert(t *testing.T) {
	t.Parallel()

	entry := LedgerEntry{UpSQL: "CREATE TABLE public.t (id int);", DownSQL: "DROP TABLE public.t;"}
	done := time.Now()
	runs := []engine.JournalRun{{
		ID: "r", Direction: "down", State: "succeeded", StmtCount: 1,
		Statements: []engine.JournalStatement{{Index: 0, DoneAt: &done}},
	}}
	got, swept := attemptsOf(runs, "20260901120000_t", entry)
	if swept || len(got) != 1 || got[0].Statements[0].Outcome != OutcomeDone ||
		!strings.Contains(got[0].Statements[0].SQL, "DROP TABLE public.t") {
		t.Fatalf("down attempt = %+v, swept = %v", got, swept)
	}
	entry.DownSQL = ""
	if _, swept := attemptsOf(runs, "20260901120000_t", entry); !swept {
		t.Fatal("a direction with no stored body is swept")
	}
}

func TestStatementsOfKeepsAJournalRowThePlanNoLongerMatches(t *testing.T) {
	t.Parallel()

	run := engine.JournalRun{State: "running", StmtCount: 2, Statements: []engine.JournalStatement{
		{Index: 0, Hash: "gone"},
	}}
	plan := []engine.Statement{{SQL: "SELECT 1", Hash: "here"}, {SQL: "SELECT 2", Hash: "other"}}
	got := statementsOf(run, plan)
	if len(got) != 2 || got[0].SQL != "" || got[0].Outcome != OutcomeInFlight {
		t.Fatalf("hash mismatch = %+v", got)
	}
	if got[1].SQL != "SELECT 2" || got[1].Outcome != OutcomeNotReached {
		t.Fatalf("unjournalled statement = %+v", got[1])
	}
}
