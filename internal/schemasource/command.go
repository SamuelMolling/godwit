package schemasource

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os/exec"
	"strings"
)

// Command is a Source over any command line that prints the whole desired database as DDL on stdout.
type Command struct {
	Argv []string
	Run  Runner
}

// Load runs the command and returns its stdout untouched.
func (c Command) Load(ctx context.Context) (string, error) {
	if len(c.Argv) == 0 {
		return "", errors.New("no command configured; pass --exec or set schema_source.command")
	}
	label := strings.Join(c.Argv, " ")
	out, err := capture(ctx, c.Run, c.Argv, func(err error, stderr []byte) error {
		if missingBinary(err) {
			return fmt.Errorf("%s not found (%w); check --exec / schema_source.command", c.Argv[0], err)
		}

		return runFailed(label, err, stderr)
	})
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(out) == "" {
		return "", fmt.Errorf("%s produced no DDL on stdout", label)
	}

	return out, nil
}

func capture(ctx context.Context, run Runner, argv []string, wrap func(error, []byte) error) (string, error) {
	if run == nil {
		run = Exec
	}
	out, errOut, err := run(ctx, argv[0], argv[1:]...)
	if err != nil {
		return "", wrap(err, errOut)
	}

	return string(out), nil
}

func missingBinary(err error) bool {
	return errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist)
}

func runFailed(label string, err error, stderr []byte) error {
	if detail := strings.TrimSpace(string(stderr)); detail != "" {
		return fmt.Errorf("%s failed: %w: %s", label, err, detail)
	}

	return fmt.Errorf("%s failed: %w", label, err)
}

func fields(value, fallback string) []string {
	if f := strings.Fields(value); len(f) > 0 {
		return f
	}

	return strings.Fields(fallback)
}
