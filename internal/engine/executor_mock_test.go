package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"
)

var (
	errBoom = errors.New("boom")
	cicHash = hashSQL("CREATE INDEX CONCURRENTLY i ON tt (v)")
)

func newMockExec(t *testing.T, opts ...Option) (pgxmock.PgxConnIface, *Executor) {
	t.Helper()
	mock, err := pgxmock.NewConn()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mock.Close(context.Background()) })

	return mock, New(mock, Options{}, opts...)
}

func expectLock(mock pgxmock.PgxConnIface) {
	mock.ExpectQuery("SELECT current_database").
		WillReturnRows(pgxmock.NewRows([]string{"current_database"}).AddRow("db"))
	mock.ExpectExec("SELECT pg_advisory_lock").WithArgs(pgxmock.AnyArg()).WillReturnResult(pgxmock.NewResult("SELECT", 1))
}

func expectBootstrap(mock pgxmock.PgxConnIface) {
	for range bootstrapDDL {
		mock.ExpectExec("CREATE").WillReturnResult(pgxmock.NewResult("CREATE", 0))
	}
}

func expectNotApplied(mock pgxmock.PgxConnIface) {
	mock.ExpectQuery("SELECT checksum FROM godwit.migrations").WithArgs(pgxmock.AnyArg()).WillReturnError(pgx.ErrNoRows)
}

func expectNewRun(mock pgxmock.PgxConnIface) {
	mock.ExpectQuery("SELECT id FROM godwit.runs").WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).WillReturnError(pgx.ErrNoRows)
	mock.ExpectExec("INSERT INTO godwit.runs").WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).WillReturnResult(pgxmock.NewResult("INSERT", 1))
}

func expectMarkFailed(mock pgxmock.PgxConnIface) {
	mock.ExpectExec("UPDATE godwit.runs SET state = 'failed'").WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).WillReturnResult(pgxmock.NewResult("UPDATE", 1))
}

func txPlan(t *testing.T) Plan {
	t.Helper()

	return buildPlanT(t, Migration{
		Version: 1, Name: "m", Checksum: "c",
		UpSQL: "SELECT 1;", DownSQL: "SELECT 2;",
	}, DirectionUp)
}

func cicPlan(t *testing.T) Plan {
	t.Helper()

	return buildPlanT(t, Migration{
		Version: 1, Name: "m", Checksum: "c",
		UpSQL: "CREATE INDEX CONCURRENTLY i ON tt (v);", DownSQL: "SELECT 2;",
	}, DirectionUp)
}

func wantErr(t *testing.T, err error, substr string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), substr) {
		t.Fatalf("err = %v, want containing %q", err, substr)
	}
}

func TestUpErrorPaths(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		setup   func(mock pgxmock.PgxConnIface)
		wantErr string
	}{
		{
			name: "database name scan fails",
			setup: func(mock pgxmock.PgxConnIface) {
				mock.ExpectQuery("SELECT current_database").WillReturnError(errBoom)
			},
			wantErr: "resolve database name",
		},
		{
			name: "advisory lock fails",
			setup: func(mock pgxmock.PgxConnIface) {
				mock.ExpectQuery("SELECT current_database").
					WillReturnRows(pgxmock.NewRows([]string{"current_database"}).AddRow("db"))
				mock.ExpectExec("SELECT pg_advisory_lock").WithArgs(pgxmock.AnyArg()).WillReturnError(errBoom)
			},
			wantErr: "acquire advisory lock",
		},
		{
			name: "bootstrap fails",
			setup: func(mock pgxmock.PgxConnIface) {
				expectLock(mock)
				mock.ExpectExec("CREATE SCHEMA").WillReturnError(errBoom)
			},
			wantErr: "bootstrap godwit schema",
		},
		{
			name: "applied check fails",
			setup: func(mock pgxmock.PgxConnIface) {
				expectLock(mock)
				expectBootstrap(mock)
				mock.ExpectQuery("SELECT checksum FROM godwit.migrations").WithArgs(pgxmock.AnyArg()).WillReturnError(errBoom)
			},
			wantErr: "check applied version",
		},
		{
			name: "find open run fails",
			setup: func(mock pgxmock.PgxConnIface) {
				expectLock(mock)
				expectBootstrap(mock)
				expectNotApplied(mock)
				mock.ExpectQuery("SELECT id FROM godwit.runs").WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).WillReturnError(errBoom)
			},
			wantErr: "find open run",
		},
		{
			name: "insert run fails",
			setup: func(mock pgxmock.PgxConnIface) {
				expectLock(mock)
				expectBootstrap(mock)
				expectNotApplied(mock)
				mock.ExpectQuery("SELECT id FROM godwit.runs").WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).WillReturnError(pgx.ErrNoRows)
				mock.ExpectExec("INSERT INTO godwit.runs").WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).WillReturnError(errBoom)
			},
			wantErr: "insert run",
		},
		{
			name: "journal query fails",
			setup: func(mock pgxmock.PgxConnIface) {
				expectLock(mock)
				expectBootstrap(mock)
				expectNotApplied(mock)
				mock.ExpectQuery("SELECT id FROM godwit.runs").WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
					WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("r1"))
				mock.ExpectQuery("SELECT stmt_idx, state, sql_hash").WithArgs(pgxmock.AnyArg()).WillReturnError(errBoom)
			},
			wantErr: "load journal",
		},
		{
			name: "journal scan fails",
			setup: func(mock pgxmock.PgxConnIface) {
				expectLock(mock)
				expectBootstrap(mock)
				expectNotApplied(mock)
				mock.ExpectQuery("SELECT id FROM godwit.runs").WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
					WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("r1"))
				mock.ExpectQuery("SELECT stmt_idx, state, sql_hash").WithArgs(pgxmock.AnyArg()).
					WillReturnRows(pgxmock.NewRows([]string{"stmt_idx", "state", "sql_hash"}).AddRow("no", "done", "h"))
			},
			wantErr: "read journal",
		},
		{
			name: "journal rows error",
			setup: func(mock pgxmock.PgxConnIface) {
				expectLock(mock)
				expectBootstrap(mock)
				expectNotApplied(mock)
				mock.ExpectQuery("SELECT id FROM godwit.runs").WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
					WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("r1"))
				mock.ExpectQuery("SELECT stmt_idx, state, sql_hash").WithArgs(pgxmock.AnyArg()).
					WillReturnRows(pgxmock.NewRows([]string{"stmt_idx", "state", "sql_hash"}).
						AddRow(0, "done", "h").RowError(0, errBoom))
			},
			wantErr: "read journal",
		},
		{
			name: "reopen run fails",
			setup: func(mock pgxmock.PgxConnIface) {
				expectLock(mock)
				expectBootstrap(mock)
				expectNotApplied(mock)
				mock.ExpectQuery("SELECT id FROM godwit.runs").WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
					WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("r1"))
				mock.ExpectQuery("SELECT stmt_idx, state, sql_hash").WithArgs(pgxmock.AnyArg()).
					WillReturnRows(pgxmock.NewRows([]string{"stmt_idx", "state", "sql_hash"}))
				mock.ExpectExec("UPDATE godwit.runs SET state = 'running'").WithArgs(pgxmock.AnyArg()).WillReturnError(errBoom)
			},
			wantErr: "reopen run",
		},
		{
			name: "begin fails",
			setup: func(mock pgxmock.PgxConnIface) {
				expectLock(mock)
				expectBootstrap(mock)
				expectNotApplied(mock)
				expectNewRun(mock)
				mock.ExpectBegin().WillReturnError(errBoom)
				expectMarkFailed(mock)
			},
			wantErr: "begin",
		},
		{
			name: "set timeout fails",
			setup: func(mock pgxmock.PgxConnIface) {
				expectLock(mock)
				expectBootstrap(mock)
				expectNotApplied(mock)
				expectNewRun(mock)
				mock.ExpectBegin()
				mock.ExpectExec("SET LOCAL lock_timeout").WillReturnError(errBoom)
				mock.ExpectRollback()
				expectMarkFailed(mock)
			},
			wantErr: "set timeouts",
		},
		{
			name: "journal insert fails",
			setup: func(mock pgxmock.PgxConnIface) {
				expectLock(mock)
				expectBootstrap(mock)
				expectNotApplied(mock)
				expectNewRun(mock)
				mock.ExpectBegin()
				mock.ExpectExec("SET LOCAL lock_timeout").WillReturnResult(pgxmock.NewResult("SET", 0))
				mock.ExpectExec("SET LOCAL statement_timeout").WillReturnResult(pgxmock.NewResult("SET", 0))
				mock.ExpectExec("SELECT 1").WillReturnResult(pgxmock.NewResult("SELECT", 1))
				mock.ExpectExec("INSERT INTO godwit.journal").WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).WillReturnError(errBoom)
				mock.ExpectRollback()
				expectMarkFailed(mock)
			},
			wantErr: "journal statement 0",
		},
		{
			name: "commit fails",
			setup: func(mock pgxmock.PgxConnIface) {
				expectLock(mock)
				expectBootstrap(mock)
				expectNotApplied(mock)
				expectNewRun(mock)
				mock.ExpectBegin()
				mock.ExpectExec("SET LOCAL lock_timeout").WillReturnResult(pgxmock.NewResult("SET", 0))
				mock.ExpectExec("SET LOCAL statement_timeout").WillReturnResult(pgxmock.NewResult("SET", 0))
				mock.ExpectExec("SELECT 1").WillReturnResult(pgxmock.NewResult("SELECT", 1))
				mock.ExpectExec("INSERT INTO godwit.journal").WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).WillReturnResult(pgxmock.NewResult("INSERT", 1))
				mock.ExpectCommit().WillReturnError(errBoom)
				mock.ExpectRollback()
				expectMarkFailed(mock)
			},
			wantErr: "commit",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mock, exec := newMockExec(t)
			tc.setup(mock)
			_, err := exec.Up(context.Background(), txPlan(t))
			wantErr(t, err, tc.wantErr)
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestFinalizeErrorPaths(t *testing.T) {
	t.Parallel()

	expectThroughStatements := func(mock pgxmock.PgxConnIface) {
		expectLock(mock)
		expectBootstrap(mock)
		expectNotApplied(mock)
		expectNewRun(mock)
		mock.ExpectBegin()
		mock.ExpectExec("SET LOCAL lock_timeout").WillReturnResult(pgxmock.NewResult("SET", 0))
		mock.ExpectExec("SET LOCAL statement_timeout").WillReturnResult(pgxmock.NewResult("SET", 0))
		mock.ExpectExec("SELECT 1").WillReturnResult(pgxmock.NewResult("SELECT", 1))
		mock.ExpectExec("INSERT INTO godwit.journal").WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).WillReturnResult(pgxmock.NewResult("INSERT", 1))
		mock.ExpectCommit()
	}

	cases := []struct {
		name    string
		setup   func(mock pgxmock.PgxConnIface)
		wantErr string
	}{
		{
			name: "begin fails",
			setup: func(mock pgxmock.PgxConnIface) {
				mock.ExpectBegin().WillReturnError(errBoom)
			},
			wantErr: "begin finalize",
		},
		{
			name: "record migration fails",
			setup: func(mock pgxmock.PgxConnIface) {
				mock.ExpectBegin()
				mock.ExpectExec("INSERT INTO godwit.migrations").WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).WillReturnError(errBoom)
				mock.ExpectRollback()
			},
			wantErr: "record migration",
		},
		{
			name: "close run fails",
			setup: func(mock pgxmock.PgxConnIface) {
				mock.ExpectBegin()
				mock.ExpectExec("INSERT INTO godwit.migrations").WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).WillReturnResult(pgxmock.NewResult("INSERT", 1))
				mock.ExpectExec("UPDATE godwit.runs SET state = 'succeeded'").WithArgs(pgxmock.AnyArg()).WillReturnError(errBoom)
				mock.ExpectRollback()
			},
			wantErr: "close run",
		},
		{
			name: "commit fails",
			setup: func(mock pgxmock.PgxConnIface) {
				mock.ExpectBegin()
				mock.ExpectExec("INSERT INTO godwit.migrations").WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).WillReturnResult(pgxmock.NewResult("INSERT", 1))
				mock.ExpectExec("UPDATE godwit.runs SET state = 'succeeded'").WithArgs(pgxmock.AnyArg()).WillReturnResult(pgxmock.NewResult("UPDATE", 1))
				mock.ExpectCommit().WillReturnError(errBoom)
				mock.ExpectRollback()
			},
			wantErr: "commit finalize",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mock, exec := newMockExec(t)
			expectThroughStatements(mock)
			tc.setup(mock)
			_, err := exec.Up(context.Background(), txPlan(t))
			wantErr(t, err, tc.wantErr)
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestNoTxErrorPaths(t *testing.T) {
	t.Parallel()

	preludeNewRun := func(mock pgxmock.PgxConnIface) {
		expectLock(mock)
		expectBootstrap(mock)
		expectNotApplied(mock)
		expectNewRun(mock)
	}
	preludeResumeIntent := func(mock pgxmock.PgxConnIface) {
		expectLock(mock)
		expectBootstrap(mock)
		expectNotApplied(mock)
		mock.ExpectQuery("SELECT id FROM godwit.runs").WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
			WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("r1"))
		mock.ExpectQuery("SELECT stmt_idx, state, sql_hash").WithArgs(pgxmock.AnyArg()).
			WillReturnRows(pgxmock.NewRows([]string{"stmt_idx", "state", "sql_hash"}).
				AddRow(0, "intent", cicHash))
		mock.ExpectExec("UPDATE godwit.runs SET state = 'running'").WithArgs(pgxmock.AnyArg()).WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	}

	cases := []struct {
		name    string
		setup   func(mock pgxmock.PgxConnIface)
		wantErr string
	}{
		{
			name: "intent insert fails",
			setup: func(mock pgxmock.PgxConnIface) {
				preludeNewRun(mock)
				mock.ExpectExec("INSERT INTO godwit.journal").WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).WillReturnError(errBoom)
				expectMarkFailed(mock)
			},
			wantErr: "journal intent for statement 0",
		},
		{
			name: "session set fails",
			setup: func(mock pgxmock.PgxConnIface) {
				preludeNewRun(mock)
				mock.ExpectExec("INSERT INTO godwit.journal").WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).WillReturnResult(pgxmock.NewResult("INSERT", 1))
				mock.ExpectExec("SET lock_timeout").WillReturnError(errBoom)
				expectMarkFailed(mock)
			},
			wantErr: "set timeouts",
		},
		{
			name: "done insert fails",
			setup: func(mock pgxmock.PgxConnIface) {
				preludeNewRun(mock)
				mock.ExpectExec("INSERT INTO godwit.journal").WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).WillReturnResult(pgxmock.NewResult("INSERT", 1))
				mock.ExpectExec("SET lock_timeout").WillReturnResult(pgxmock.NewResult("SET", 0))
				mock.ExpectExec("SET statement_timeout").WillReturnResult(pgxmock.NewResult("SET", 0))
				mock.ExpectExec("CREATE INDEX CONCURRENTLY").WillReturnResult(pgxmock.NewResult("CREATE", 0))
				mock.ExpectExec("RESET lock_timeout").WillReturnResult(pgxmock.NewResult("RESET", 0))
				mock.ExpectExec("RESET statement_timeout").WillReturnResult(pgxmock.NewResult("RESET", 0))
				mock.ExpectExec("INSERT INTO godwit.journal").WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).WillReturnError(errBoom)
				expectMarkFailed(mock)
			},
			wantErr: "journal done for statement 0",
		},
		{
			name: "reconcile inspect fails",
			setup: func(mock pgxmock.PgxConnIface) {
				preludeResumeIntent(mock)
				mock.ExpectQuery("SELECT i.indisvalid").WithArgs(pgxmock.AnyArg()).WillReturnError(errBoom)
				expectMarkFailed(mock)
			},
			wantErr: "inspect index",
		},
		{
			name: "reconcile drop invalid fails",
			setup: func(mock pgxmock.PgxConnIface) {
				preludeResumeIntent(mock)
				mock.ExpectQuery("SELECT i.indisvalid").WithArgs(pgxmock.AnyArg()).
					WillReturnRows(pgxmock.NewRows([]string{"indisvalid"}).AddRow(false))
				mock.ExpectExec("DROP INDEX").WillReturnError(errBoom)
				expectMarkFailed(mock)
			},
			wantErr: "drop invalid index",
		},
		{
			name: "reconcile done but record fails",
			setup: func(mock pgxmock.PgxConnIface) {
				preludeResumeIntent(mock)
				mock.ExpectQuery("SELECT i.indisvalid").WithArgs(pgxmock.AnyArg()).
					WillReturnRows(pgxmock.NewRows([]string{"indisvalid"}).AddRow(true))
				mock.ExpectExec("INSERT INTO godwit.journal").WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).WillReturnError(errBoom)
				expectMarkFailed(mock)
			},
			wantErr: "journal done for statement 0",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mock, exec := newMockExec(t)
			tc.setup(mock)
			_, err := exec.Up(context.Background(), cicPlan(t))
			wantErr(t, err, tc.wantErr)
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestStatusErrorPaths(t *testing.T) {
	t.Parallel()

	migs := []Migration{{Version: 1, Name: "m", Checksum: "c"}}
	cases := []struct {
		name    string
		setup   func(mock pgxmock.PgxConnIface)
		wantErr string
	}{
		{
			name: "bootstrap fails",
			setup: func(mock pgxmock.PgxConnIface) {
				mock.ExpectExec("CREATE SCHEMA").WillReturnError(errBoom)
			},
			wantErr: "bootstrap",
		},
		{
			name: "list fails",
			setup: func(mock pgxmock.PgxConnIface) {
				expectBootstrap(mock)
				mock.ExpectQuery("SELECT version, name, checksum, applied_at").WillReturnError(errBoom)
			},
			wantErr: "list applied",
		},
		{
			name: "scan fails",
			setup: func(mock pgxmock.PgxConnIface) {
				expectBootstrap(mock)
				mock.ExpectQuery("SELECT version, name, checksum, applied_at").
					WillReturnRows(pgxmock.NewRows([]string{"version", "name", "checksum", "applied_at"}).
						AddRow("no", "m", "c", "now"))
			},
			wantErr: "read applied",
		},
		{
			name: "rows error",
			setup: func(mock pgxmock.PgxConnIface) {
				expectBootstrap(mock)
				mock.ExpectQuery("SELECT version, name, checksum, applied_at").
					WillReturnRows(pgxmock.NewRows([]string{"version", "name", "checksum", "applied_at"}).
						AddRow(int64(1), "m", "c", time.Now()).RowError(0, errBoom))
			},
			wantErr: "read applied",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mock, exec := newMockExec(t)
			tc.setup(mock)
			_, err := exec.Status(context.Background(), migs)
			wantErr(t, err, tc.wantErr)
		})
	}
}

func TestReconcileDropIndexInspectError(t *testing.T) {
	t.Parallel()

	mock, _ := newMockExec(t)
	mock.ExpectQuery("SELECT to_regclass").WithArgs(pgxmock.AnyArg()).WillReturnError(errBoom)
	_, err := reconcile(context.Background(), mock, Statement{Verifier: VerifierDropIndexConcurrently, IndexName: "i"})
	wantErr(t, err, "inspect index")
}

func TestReconcileCreateIndexAbsent(t *testing.T) {
	t.Parallel()

	mock, _ := newMockExec(t)
	mock.ExpectQuery("SELECT i.indisvalid").WithArgs(pgxmock.AnyArg()).WillReturnError(pgx.ErrNoRows)
	done, err := reconcile(context.Background(), mock, Statement{Verifier: VerifierCreateIndexConcurrently, IndexName: "i"})
	if err != nil || done {
		t.Fatalf("done = %v, err = %v", done, err)
	}
}

func TestQuoteIndexQualified(t *testing.T) {
	t.Parallel()

	if got := quoteIndex(Statement{IndexSchema: "app", IndexName: "i"}); got != `"app"."i"` {
		t.Fatalf("quoteIndex = %q", got)
	}
}

func TestWithIDGenerator(t *testing.T) {
	t.Parallel()

	mock, exec := newMockExec(t, WithIDGenerator(func() string { return "fixed-id" }))
	expectLock(mock)
	expectBootstrap(mock)
	expectNotApplied(mock)
	mock.ExpectQuery("SELECT id FROM godwit.runs").WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).WillReturnError(pgx.ErrNoRows)
	mock.ExpectExec("INSERT INTO godwit.runs").
		WithArgs("fixed-id", int64(1), "up", 1).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectBegin().WillReturnError(errBoom)
	expectMarkFailed(mock)

	_, err := exec.Up(context.Background(), txPlan(t))
	wantErr(t, err, "begin")
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
