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
	}
	exec := New(conn, Options{})
	if err := exec.MarkApplied(ctx, migs); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, conn, "godwit.migrations"); n != 2 {
		t.Fatalf("migrations = %d", n)
	}
	var exists bool
	if err := conn.QueryRow(ctx, "SELECT to_regclass('users') IS NOT NULL").Scan(&exists); err != nil || exists {
		t.Fatalf("users table exists = %v, err = %v", exists, err)
	}

	res, err := exec.Up(ctx, buildPlanT(t, migs[0], DirectionUp))
	if err != nil || !res.Skipped {
		t.Fatalf("up after baseline = %+v, %v", res, err)
	}
	status, err := exec.Status(ctx, migs)
	if err != nil || !status[0].Applied || !status[1].Applied || status[1].Drifted {
		t.Fatalf("status = %+v, %v", status, err)
	}

	err = exec.MarkApplied(ctx, migs[:1])
	if !errors.Is(err, ErrAlreadyMigrated) {
		t.Fatalf("second baseline err = %v", err)
	}
}

func TestMarkAppliedErrorPaths(t *testing.T) {
	t.Parallel()

	migs := []Migration{{Version: 1, Name: "m", Checksum: "c"}}
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
			name: "count fails",
			setup: func(mock pgxmock.PgxConnIface) {
				expectLock(mock)
				expectBootstrap(mock)
				mock.ExpectQuery("SELECT count").WillReturnError(errBoom)
			},
			wantErr: "count applied",
		},
		{
			name: "begin fails",
			setup: func(mock pgxmock.PgxConnIface) {
				expectLock(mock)
				expectBootstrap(mock)
				mock.ExpectQuery("SELECT count").WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(0))
				mock.ExpectBegin().WillReturnError(errBoom)
			},
			wantErr: "begin baseline",
		},
		{
			name: "insert fails",
			setup: func(mock pgxmock.PgxConnIface) {
				expectLock(mock)
				expectBootstrap(mock)
				mock.ExpectQuery("SELECT count").WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(0))
				mock.ExpectBegin()
				mock.ExpectExec("INSERT INTO godwit.migrations").WithArgs(int64(1), "m", "c").WillReturnError(errBoom)
				mock.ExpectRollback()
			},
			wantErr: "mark 1_m applied",
		},
		{
			name: "commit fails",
			setup: func(mock pgxmock.PgxConnIface) {
				expectLock(mock)
				expectBootstrap(mock)
				mock.ExpectQuery("SELECT count").WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(0))
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
			wantErr(t, exec.MarkApplied(context.Background(), migs), tc.wantErr)
		})
	}
}
