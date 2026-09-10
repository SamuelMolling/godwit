package admission

import "errors"

var (
	// ErrInvalid marks a refusal the submitted set itself causes.
	ErrInvalid = errors.New("the submitted set cannot be run")
	// ErrRefused marks a refusal the target's own state causes.
	ErrRefused = errors.New("the target refuses this set")
)

type classified struct {
	class error
	err   error
}

func (c classified) Error() string { return c.err.Error() }

// Is matches the class, which carries no text of its own so the wrapped message reaches the caller whole.
func (c classified) Is(target error) bool { return target == c.class }

func (c classified) Unwrap() error { return c.err }

func invalid(err error) error { return classified{class: ErrInvalid, err: err} }

func precondition(err error) error { return classified{class: ErrRefused, err: err} }
