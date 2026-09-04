package engine

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v4"
)

type countingDB struct {
	DB
	schemas int
}

func (c *countingDB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if sql == bootstrapDDL[0] {
		c.schemas++
	}

	return c.DB.Exec(ctx, sql, args...)
}

func (c *countingDB) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := c.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}

	return &countingTx{Tx: tx, db: c}, nil
}

type countingTx struct {
	pgx.Tx
	db *countingDB
}

func (t *countingTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if sql == bootstrapDDL[0] {
		t.db.schemas++
	}

	return t.Tx.Exec(ctx, sql, args...)
}

func TestSessionBootstrapsOncePerConnection(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	counted := &countingDB{DB: newTestDB(t)()}
	session := NewSession(counted)
	if NewSession(session) != session {
		t.Fatal("wrapping a session must return it")
	}
	for i, sql := range []string{`CREATE TABLE s1 (id int)`, `CREATE TABLE s2 (id int)`} {
		m := Migration{Version: int64(i + 1), Name: "s", UpSQL: sql, Checksum: hashSQL(sql)}
		if _, err := New(session, Options{}).Up(ctx, buildPlanT(t, m, DirectionUp)); err != nil {
			t.Fatal(err)
		}
	}
	if counted.schemas != 1 {
		t.Fatalf("bootstrap ran %d times over one connection, want 1", counted.schemas)
	}
	var applied int
	if err := session.QueryRow(ctx, `SELECT count(*) FROM godwit.migrations`).Scan(&applied); err != nil || applied != 2 {
		t.Fatalf("applied = %d, err = %v", applied, err)
	}
}

func TestBootstrapSurvivesConcurrentSessions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	connect := newTestDB(t)
	conns := []DB{connect(), connect(), connect(), connect()}
	errs := make([]error, len(conns))
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i, conn := range conns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs[i] = bootstrap(ctx, conn)
		}()
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("session %d: %v", i, err)
		}
	}
}

func TestEnsureSchemaRemembersOnlySuccess(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	mock, err := pgxmock.NewConn()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mock.Close(ctx) }()

	mock.ExpectBegin()
	mock.ExpectExec("pg_advisory_xact_lock").WithArgs(bootstrapLock).WillReturnResult(pgxmock.NewResult("SELECT", 1))
	mock.ExpectExec(regexp.QuoteMeta(bootstrapDDL[0])).WillReturnError(errBoom)
	mock.ExpectRollback()
	session := NewSession(mock)
	if err := ensureSchema(ctx, session); err == nil || !strings.Contains(err.Error(), "bootstrap godwit schema") {
		t.Fatalf("err = %v", err)
	}
	expectBootstrap(mock)
	for range 2 {
		if err := ensureSchema(ctx, session); err != nil {
			t.Fatal(err)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestBootstrapWaitsForWhoeverHoldsTheLock(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	connect := newTestDB(t)
	holder, conn := connect(), connect()
	if _, err := holder.Exec(ctx, `SELECT pg_advisory_lock($1)`, bootstrapLock); err != nil {
		t.Fatal(err)
	}
	blocked, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	if err := bootstrap(blocked, conn); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("bootstrap must wait for the lock, got %v", err)
	}
	if _, err := holder.Exec(ctx, `SELECT pg_advisory_unlock($1)`, bootstrapLock); err != nil {
		t.Fatal(err)
	}
	if err := bootstrap(ctx, connect()); err != nil {
		t.Fatalf("bootstrap must proceed once the lock is free: %v", err)
	}
}

func TestBootstrapReportsANameHeldByAnotherObject(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	conn := newTestDB(t)()
	for _, ddl := range []string{`CREATE SCHEMA godwit`, `CREATE TYPE godwit.journal AS ENUM ('planted')`} {
		if _, err := conn.Exec(ctx, ddl); err != nil {
			t.Fatal(err)
		}
	}
	err := bootstrap(ctx, conn)
	var pge *pgconn.PgError
	if !errors.As(err, &pge) || pge.Code != "42710" || !strings.Contains(pge.Message, `type "journal" already exists`) {
		t.Fatalf("bootstrap must report the object in the way, got %v", err)
	}
	var tables int
	if err := conn.QueryRow(ctx,
		`SELECT count(*) FROM pg_tables WHERE schemaname = 'godwit'`).Scan(&tables); err != nil || tables != 0 {
		t.Fatalf("a failed bootstrap must leave no tables behind: %d, %v", tables, err)
	}
}

func TestBootstrapReportsEveryStepOfTheTransaction(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	for name, expect := range map[string]func(pgxmock.PgxConnIface){
		"begin": func(mock pgxmock.PgxConnIface) {
			mock.ExpectBegin().WillReturnError(errBoom)
		},
		"lock": func(mock pgxmock.PgxConnIface) {
			mock.ExpectBegin()
			mock.ExpectExec("pg_advisory_xact_lock").WithArgs(bootstrapLock).WillReturnError(errBoom)
			mock.ExpectRollback()
		},
		"commit": func(mock pgxmock.PgxConnIface) {
			mock.ExpectBegin()
			mock.ExpectExec("pg_advisory_xact_lock").WithArgs(bootstrapLock).WillReturnResult(pgxmock.NewResult("SELECT", 1))
			for _, ddl := range bootstrapDDL {
				mock.ExpectExec(regexp.QuoteMeta(ddl)).WillReturnResult(pgxmock.NewResult("DDL", 0))
			}
			mock.ExpectCommit().WillReturnError(errBoom)
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			mock, err := pgxmock.NewConn()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = mock.Close(ctx) }()

			expect(mock)
			if err := bootstrap(ctx, mock); err == nil || !strings.Contains(err.Error(), "bootstrap godwit schema") {
				t.Fatalf("err = %v", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
