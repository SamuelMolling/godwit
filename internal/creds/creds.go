// Package creds resolves target-database credentials through pluggable providers.
package creds

import "context"

// Provider resolves a target's connection string from its stored config.
type Provider interface {
	DSN(ctx context.Context, config map[string]string) (string, error)
}

// Registry returns the built-in providers.
func Registry(masterKey []byte) map[string]Provider {
	return map[string]Provider{
		"static":     Static{Key: masterKey},
		"kubernetes": Kubernetes{},
	}
}
