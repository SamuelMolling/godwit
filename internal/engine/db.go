package engine

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// DB is the single-session connection surface the executor needs; *pgx.Conn satisfies it.
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Session is a DB that remembers the godwit schema has been bootstrapped on it, so a caller running many
// Executors over one connection pays for it once; it is no more concurrency-safe than that connection.
type Session struct {
	DB
	ready bool
}

// NewSession wraps db; wrapping a Session returns it, so the memory survives being passed on.
func NewSession(db DB) *Session {
	if s, ok := db.(*Session); ok {
		return s
	}

	return &Session{DB: db}
}

func (s *Session) ensure(ctx context.Context) error {
	if s.ready {
		return nil
	}
	if err := bootstrap(ctx, s.DB); err != nil {
		return err
	}
	s.ready = true

	return nil
}
