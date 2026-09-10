// Package creds resolves target-database credentials through pluggable providers.
package creds

import (
	"context"
	"net/http"
)

// Provider resolves a target's connection string from its stored config.
type Provider interface {
	DSN(ctx context.Context, config map[string]string) (string, error)
}

// Provider names.
const (
	ProviderStatic     = "static"
	ProviderKubernetes = "kubernetes"
	ProviderVault      = "vault"
)

// Registry returns the built-in providers. Only `static` needs a key: `kubernetes` and `vault` read a
// secret godwit never held, so an empty keyring leaves them working.
func Registry(keys Keyring, stores func(ctx context.Context, name string) (VaultStore, error)) map[string]Provider {
	return map[string]Provider{
		ProviderStatic:     Static{Keys: keys},
		ProviderKubernetes: Kubernetes{},
		ProviderVault:      vaults{client: http.DefaultClient, lookup: stores},
	}
}
