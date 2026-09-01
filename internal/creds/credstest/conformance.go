// Package credstest holds the conformance suite every credential provider must pass.
package credstest

import (
	"context"
	"fmt"
	"testing"

	"github.com/SamuelMolling/godwit/internal/creds"
)

// Check runs the provider contract and returns the violations found.
func Check(p creds.Provider, config map[string]string, want string) []string {
	var problems []string
	got, err := p.DSN(context.Background(), config)
	if err != nil {
		problems = append(problems, fmt.Sprintf("DSN() error: %v", err))
	} else if got != want {
		problems = append(problems, fmt.Sprintf("DSN() = %q, want %q", got, want))
	}
	if _, err := p.DSN(context.Background(), map[string]string{}); err == nil {
		problems = append(problems, "DSN() with empty config must fail")
	}

	return problems
}

// Conformance fails the test with every violation Check finds.
func Conformance(t testing.TB, p creds.Provider, config map[string]string, want string) {
	t.Helper()
	for _, problem := range Check(p, config, want) {
		t.Error(problem)
	}
}
