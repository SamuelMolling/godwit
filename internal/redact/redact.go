// Package redact decides which part of an error godwit publishes and which part stays in the server log.
package redact

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/SamuelMolling/godwit/internal/creds"
)

// Messages that stand in for an error godwit will not publish.
const (
	ConnectionFailed     = "cannot reach the database for this call; the detail is in the server log"
	CallFailed           = "the call failed; the detail is in the server log"
	CredentialUnreadable = "the credentials for this target could not be read; ask an operator to check its credential store; the detail is in the server log"
	Deadline             = "the call ran past its deadline"
)

type publicErr struct{ err error }

func (p *publicErr) Error() string { return p.err.Error() }

func (p *publicErr) Unwrap() error { return p.err }

// Public marks err as publishable whole: the site that calls it states the error carries nothing but what the pull request already holds.
func Public(err error) error { return &publicErr{err: err} }

type wrapped struct {
	prefix string
	err    error
}

func (w *wrapped) Error() string { return w.prefix + ": " + w.err.Error() }

func (w *wrapped) Unwrap() error { return w.err }

// Wrap prefixes err with text Message keeps, so the site that knows the prefix names nothing private says so.
func Wrap(err error, format string, args ...any) error {
	return &wrapped{prefix: fmt.Sprintf(format, args...), err: err}
}

// Message is what godwit publishes for err: the parts it classified, and a stand-in for everything else.
func Message(err error) string {
	if errors.Is(err, creds.ErrCredentialConfig) {
		return err.Error()
	}
	if errors.Is(err, creds.ErrCredentialUnreadable) {
		return CredentialUnreadable
	}
	// A publishable prefix is decided before the error it marks as publishable whole, so an outer prefix is never dropped.
	var safe *wrapped
	if errors.As(err, &safe) {
		return safe.prefix + ": " + Message(safe.err)
	}
	var pub *publicErr
	if errors.As(err, &pub) {
		return pub.err.Error()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return Deadline
	}
	var parse *pgconn.ParseConfigError
	var conn *pgconn.ConnectError
	if errors.As(err, &parse) || errors.As(err, &conn) {
		return ConnectionFailed
	}
	// A refused login arrives as a PgError naming the DSN's user, wrapped in the ConnectError decided above.
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.Error()
	}

	return CallFailed
}
