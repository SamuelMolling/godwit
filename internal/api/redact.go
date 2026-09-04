package api

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// redacted carries a message the caller is allowed to see over the wire and the real cause for the log.
type redacted struct {
	msg   string
	cause error
}

func (r *redacted) Error() string { return r.msg }

func (r *redacted) Unwrap() error { return r.cause }

const connectionFailed = "cannot reach the database for this call; the detail is in the server log"

// safe hides connection failures from the caller. pgx redacts the password of a DSN it could not parse
// or dial and nothing else, so the host, user and database name of a target — or, for a credential
// provider that hands back the wrong file, its whole contents — would otherwise reach a read token.
func safe(err error) error {
	var parse *pgconn.ParseConfigError
	var conn *pgconn.ConnectError
	if errors.As(err, &parse) || errors.As(err, &conn) {
		return &redacted{msg: connectionFailed, cause: err}
	}

	return err
}

// detail is what safe kept off the wire, for the access log; it is empty for an error that was not redacted.
func detail(err error) string {
	var r *redacted
	if errors.As(err, &r) {
		return r.cause.Error()
	}

	return ""
}
