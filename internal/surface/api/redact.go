package api

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

type redacted struct {
	msg   string
	cause error
}

func (r *redacted) Error() string { return r.msg }

func (r *redacted) Unwrap() error { return r.cause }

const (
	connectionFailed = "cannot reach the database for this call; the detail is in the server log"
	callFailed       = "the call failed; the detail is in the server log"
)

func safe(err error) error {
	var parse *pgconn.ParseConfigError
	var conn *pgconn.ConnectError
	if errors.As(err, &parse) || errors.As(err, &conn) {
		return &redacted{msg: connectionFailed, cause: err}
	}
	// A refused login arrives as a PgError naming the DSN's user, wrapped in the ConnectError decided above.
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return &redacted{msg: pg.Error(), cause: err}
	}

	return &redacted{msg: callFailed, cause: err}
}

func detail(err error) string {
	var r *redacted
	if errors.As(err, &r) {
		return r.cause.Error()
	}

	return ""
}
