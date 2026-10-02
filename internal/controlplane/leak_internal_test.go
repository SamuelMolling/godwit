package controlplane

import (
	"context"

	"github.com/SamuelMolling/godwit/internal/creds"
)

// ColumnForTargetFailure resolves a target's credentials the way a run does and returns what a failure writes to cp_runs.error.
func ColumnForTargetFailure(providers map[string]creds.Provider, name, provider string, config map[string]string) (string, error) {
	s := NewScheduler(nil, providers, PGEngine{}, Policies(), Config{Holder: "t"}, testLog)
	_, err := s.resolve(context.Background(), name, provider, config)

	return failureDetail(err), err
}

// ColumnForRunFailure returns what a run failing with err writes to cp_runs.error.
func ColumnForRunFailure(err error) string { return failureDetail(err) }
