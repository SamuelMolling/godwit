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

const connectionFailed = "cannot reach the database for this call; the detail is in the server log"

// safe hides connection failures: pgx redacts a DSN's password and nothing else, so the host, user, database name and a misconfigured provider's file body would otherwise reach a read token.
func safe(err error) error {
	var parse *pgconn.ParseConfigError
	var conn *pgconn.ConnectError
	if errors.As(err, &parse) || errors.As(err, &conn) {
		return &redacted{msg: connectionFailed, cause: err}
	}

	return err
}

func detail(err error) string {
	var r *redacted
	if errors.As(err, &r) {
		return r.cause.Error()
	}

	return ""
}
