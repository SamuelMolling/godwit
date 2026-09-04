package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/pashagolub/pgxmock/v4"
)

func TestMarkAppliedIntegration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn := newTestDB(t)()

	migs := []Migration{
		{Version: 1, Name: "baseline", Checksum: "c1", UpSQL: "CREATE TABLE users (id int);", DownSQL: "DROP TABLE users;"},
		{Version: 2, Name: "index", Checksum: "c2", UpSQL: "CREATE INDEX i ON users (id);", DownSQL: "DROP INDEX i;"},
		{Repeatable: true, Name: "view", Checksum: "c3", UpSQL: "CREATE OR REPLACE VIEW v AS SELECT 1;"},
	}
	exec := New(conn, Options{})
	marked, err := exec.MarkApplied(ctx, migs)
	if err != nil || len(marked) != 3 {
		t.Fatalf("marked = %+v, err = %v", marked, err)
	}
	if n := countRows(t, conn, "godwit.migrations"); n != 2 {
		t.Fatalf("migrations = %d", n)
	}
	if n := countRows(t, conn, "godwit.repeatables"); n != 1 {
		t.Fatalf("repeatables = %d", n)
	}
	var exists bool
	if err := conn.QueryRow(ctx, "SELECT to_regclass('users') IS NOT NULL").Scan(&exists); err != nil || exists {
		t.Fatalf("users table exists = %v, err = %v", exists, err)
	}

	res, err := exec.Up(ctx, buildPlanT(t, migs[0], DirectionUp))
	if err != nil || !res.Skipped || !res.Recorded {
		t.Fatalf("up after baseline = %+v, %v", res, err)
	}
	status, err := exec.Status(ctx, migs)
	if err != nil || !status[0].Applied || !status[1].Applied || status[1].Drifted || !status[2].Applied {
		t.Fatalf("status = %+v, %v", status, err)
	}
}

// A second call over the same content adds nothing; one over different content refuses, because a
// checksum the target does not agree with is drift between the repository and the database.
func TestMarkAppliedAdoptsAJournalledTarget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn := newTestDB(t)()

	migs := []Migration{
		{Version: 1, Name: "a", Checksum: "c1", UpSQL: "SELECT 1;"},
		{Version: 2, Name: "b", Checksum: "c2", UpSQL: "SELECT 1;"},
		{Repeatable: true, Name: "v", Checksum: "c3", UpSQL: "SELECT 1;"},
	}
	exec := New(conn, Options{})
	if _, err := exec.MarkApplied(ctx, migs[:1]); err != nil {
		t.Fatal(err)
	}
	marked, err := exec.MarkApplied(ctx, migs)
	if err != nil || len(marked) != 2 {
		t.Fatalf("marked = %+v, err = %v", marked, err)
	}
	if marked, err := exec.MarkApplied(ctx, migs); err != nil || marked != nil {
		t.Fatalf("marked = %+v, err = %v", marked, err)
	}

	edited := []Migration{{Version: 1, Name: "a", Checksum: "other", UpSQL: "SELECT 2;"}}
	if _, err := exec.MarkApplied(ctx, edited); !errors.Is(err, ErrHistoryConflict) {
		t.Fatalf("conflicting checksum err = %v", err)
	}
}

func TestMarkAppliedErrorPaths(t *testing.T) {
	t.Parallel()

	migs := []Migration{{Version: 1, Name: "m", Checksum: "c"}}
	expectJournal := func(mock pgxmock.PgxConnIface) {
		mock.ExpectQuery("SELECT version, name, checksum").
			WillReturnRows(pgxmock.NewRows([]string{"version", "name", "checksum", "applied_at"}))
		mock.ExpectQuery("SELECT name, checksum").
			WillReturnRows(pgxmock.NewRows([]string{"name", "checksum", "applied_at"}))
	}
	cases := []struct {
		name    string
		setup   func(mock pgxmock.PgxConnIface)
		wantErr string
	}{
		{
			name: "lock fails",
			setup: func(mock pgxmock.PgxConnIface) {
				mock.ExpectQuery("SELECT current_database").WillReturnError(errBoom)
			},
			wantErr: "boom",
		},
		{
			name: "bootstrap fails",
			setup: func(mock pgxmock.PgxConnIface) {
				expectLock(mock)
				mock.ExpectExec("CREATE SCHEMA").WillReturnError(errBoom)
			},
			wantErr: "bootstrap",
		},
		{
			name: "journal read fails",
			setup: func(mock pgxmock.PgxConnIface) {
				expectLock(mock)
				expectBootstrap(mock)
				mock.ExpectQuery("SELECT version, name, checksum").WillReturnError(errBoom)
			},
			wantErr: "list applied",
		},
		{
			name: "repeatables read fails",
			setup: func(mock pgxmock.PgxConnIface) {
				expectLock(mock)
				expectBootstrap(mock)
				mock.ExpectQuery("SELECT version, name, checksum").
					WillReturnRows(pgxmock.NewRows([]string{"version", "name", "checksum", "applied_at"}))
				mock.ExpectQuery("SELECT name, checksum").WillReturnError(errBoom)
			},
			wantErr: "list repeatables",
		},
		{
			name: "begin fails",
			setup: func(mock pgxmock.PgxConnIface) {
				expectLock(mock)
				expectBootstrap(mock)
				expectJournal(mock)
				mock.ExpectBegin().WillReturnError(errBoom)
			},
			wantErr: "begin baseline",
		},
		{
			name: "insert fails",
			setup: func(mock pgxmock.PgxConnIface) {
				expectLock(mock)
				expectBootstrap(mock)
				expectJournal(mock)
				mock.ExpectBegin()
				mock.ExpectExec("INSERT INTO godwit.migrations").WithArgs(int64(1), "m", "c").WillReturnError(errBoom)
				mock.ExpectRollback()
			},
			wantErr: "mark 00000000000001_m applied",
		},
		{
			name: "commit fails",
			setup: func(mock pgxmock.PgxConnIface) {
				expectLock(mock)
				expectBootstrap(mock)
				expectJournal(mock)
				mock.ExpectBegin()
				mock.ExpectExec("INSERT INTO godwit.migrations").WithArgs(int64(1), "m", "c").WillReturnResult(pgxmock.NewResult("INSERT", 1))
				mock.ExpectCommit().WillReturnError(errBoom)
				mock.ExpectRollback()
			},
			wantErr: "commit baseline",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mock, exec := newMockExec(t)
			tc.setup(mock)
			_, err := exec.MarkApplied(context.Background(), migs)
			wantErr(t, err, tc.wantErr)
		})
	}
}
