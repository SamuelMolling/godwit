package redact_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/SamuelMolling/godwit/internal/creds"
	"github.com/SamuelMolling/godwit/internal/redact"
)

func TestMessage(t *testing.T) {
	t.Parallel()

	pg := &pgconn.PgError{Severity: "ERROR", Code: "42501", Message: "permission denied for schema orders"}
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"unclassified", errors.New("read vault secret secret/data/prod: status 403"), redact.CallFailed},
		{"parse config", &pgconn.ParseConfigError{ConnString: "postgres://u:p@h/d"}, redact.ConnectionFailed},
		{"server answer", fmt.Errorf("read the journal of app: %w", pg), pg.Error()},
		{"deadline", fmt.Errorf("apply: %w", context.DeadlineExceeded), redact.Deadline},
		{
			"registration", creds.Misconfigured(errors.New(`vault target config missing "path"`)),
			`vault target config missing "path"`,
		},
		{
			"credential read", creds.Unreadable(fmt.Errorf("target orders: read vault secret %s: 403", "secret/data/prod")),
			redact.CredentialUnreadable,
		},
		{
			"registration inside a read", creds.Unreadable(creds.Misconfigured(errors.New("this vault has no address"))),
			"this vault has no address",
		},
		{
			"public", redact.Public(errors.New("20260901120000_x.up.sql: parse error")),
			"20260901120000_x.up.sql: parse error",
		},
		{
			"public under an unclassified wrapper",
			fmt.Errorf("target orders: %w", redact.Public(errors.New("unknown rollout policy"))), "unknown rollout policy",
		},
		{
			"wrapped prefix", redact.Wrap(pg, "statement %d of %s (up)", 2, "20260901120000_orders"),
			"statement 2 of 20260901120000_orders (up): " + pg.Error(),
		},
		{
			"wrapped over an unclassified cause", redact.Wrap(errors.New("boom"), "bound plan %s", "p1"),
			"bound plan p1: " + redact.CallFailed,
		},
		{
			"wrapped over a public cause", redact.Wrap(redact.Public(errors.New("assertion failed")), "statement %d", 1),
			"statement 1: assertion failed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := redact.Message(tc.err); got != tc.want {
				t.Fatalf("Message = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMarkersKeepTheCauseForTheLog(t *testing.T) {
	t.Parallel()

	cause := errors.New("read vault secret secret/data/prod/orders: status 403")
	for _, err := range []error{redact.Public(cause), redact.Wrap(cause, "statement %d", 1), creds.Unreadable(cause)} {
		if !errors.Is(err, cause) {
			t.Fatalf("%T dropped its cause", err)
		}
		if got := err.Error(); got == "" || !errors.Is(err, cause) {
			t.Fatalf("%T renders %q", err, got)
		}
	}
	if got := redact.Wrap(cause, "statement %d", 1).Error(); got != "statement 1: "+cause.Error() {
		t.Fatalf("Wrap renders %q", got)
	}
	if got := creds.Unreadable(cause).Error(); got != cause.Error() {
		t.Fatalf("Unreadable renders %q", got)
	}
}
