package controlplane

import (
	"fmt"
	"time"

	"github.com/SamuelMolling/godwit/internal/engine"
)

// Target config keys holding the per-target timeouts.
const (
	ConfigLockTimeout      = "lock_timeout"
	ConfigStatementTimeout = "statement_timeout"
)

// Timeouts are per-statement timeouts as Go duration strings; empty means inherit.
type Timeouts struct {
	Lock      string
	Statement string
}

// TargetTimeouts extracts the timeouts stored in a target's config.
func TargetTimeouts(config map[string]string) Timeouts {
	return Timeouts{Lock: config[ConfigLockTimeout], Statement: config[ConfigStatementTimeout]}
}

// Over returns t with every empty field taken from base.
func (t Timeouts) Over(base Timeouts) Timeouts {
	if t.Lock == "" {
		t.Lock = base.Lock
	}
	if t.Statement == "" {
		t.Statement = base.Statement
	}

	return t
}

// Options parses and validates the timeouts; the lock timeout must be positive, the statement timeout may be 0.
func (t Timeouts) Options() (engine.Options, error) {
	var opts engine.Options
	var err error
	if opts.LockTimeout, err = parseTimeout(ConfigLockTimeout, t.Lock, false); err != nil {
		return engine.Options{}, err
	}
	if opts.StatementTimeout, err = parseTimeout(ConfigStatementTimeout, t.Statement, true); err != nil {
		return engine.Options{}, err
	}

	return opts, nil
}

func parseTimeout(name, value string, allowZero bool) (time.Duration, error) {
	if value == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	if d == 0 && allowZero {
		return 0, nil
	}
	if d < time.Millisecond {
		return 0, fmt.Errorf("%s: %s must be at least 1ms", name, value)
	}

	return d, nil
}
