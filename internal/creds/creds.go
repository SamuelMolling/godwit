// Package creds resolves target-database credentials through pluggable providers.
package creds

import "context"

// Provider resolves a target's connection string from its stored config.
type Provider interface {
	DSN(ctx context.Context, config map[string]string) (string, error)
}

// Registry returns the built-in providers. Only `static` needs a key: `kubernetes` and `vault` read a
// secret godwit never held, so an empty keyring leaves them working.
func Registry(keys Keyring) map[string]Provider {
	return map[string]Provider{
		"static":     Static{Keys: keys},
		"kubernetes": Kubernetes{},
		"vault":      VaultFromEnv(),
	}
}
