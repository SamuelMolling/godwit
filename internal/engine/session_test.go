package engine

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"

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

	mock.ExpectExec(regexp.QuoteMeta(bootstrapDDL[0])).WillReturnError(errBoom)
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

func TestBootstrapTreatsALostCreateRaceAsDone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	for code := range raceOnCreate {
		t.Run(code, func(t *testing.T) {
			t.Parallel()
			mock, err := pgxmock.NewConn()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = mock.Close(ctx) }()

			for i, ddl := range bootstrapDDL {
				exp := mock.ExpectExec(regexp.QuoteMeta(ddl))
				if i == 0 {
					exp.WillReturnError(&pgconn.PgError{Code: code, Message: "already exists"})

					continue
				}
				exp.WillReturnResult(pgxmock.NewResult("DDL", 0))
			}
			if err := ensureSchema(ctx, mock); err != nil {
				t.Fatalf("a lost create race must not fail bootstrap: %v", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestBootstrapSurvivesTheDuplicateTypeARaceLeaves(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	conn := newTestDB(t)()
	for _, ddl := range []string{`CREATE SCHEMA godwit`, `CREATE TYPE godwit.migrations AS ENUM ('planted')`} {
		if _, err := conn.Exec(ctx, ddl); err != nil {
			t.Fatal(err)
		}
	}
	_, err := conn.Exec(ctx, bootstrapDDL[1])
	var pge *pgconn.PgError
	if !errors.As(err, &pge) || pge.Code != "42710" {
		t.Fatalf("a taken composite type must raise 42710, got %v", err)
	}
	if err := bootstrap(ctx, conn); err != nil {
		t.Fatalf("bootstrap must survive %s: %v", pge.Code, err)
	}
}
