package api

import (
	"errors"

	"github.com/SamuelMolling/godwit/internal/redact"
)

type redacted struct {
	msg   string
	cause error
}

func (r *redacted) Error() string { return r.msg }

func (r *redacted) Unwrap() error { return r.cause }

func safe(err error) error {
	return &redacted{msg: redact.Message(err), cause: err}
}

func detail(err error) string {
	var r *redacted
	if errors.As(err, &r) {
		return r.cause.Error()
	}

	return ""
}
