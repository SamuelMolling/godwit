// Package creds resolves target-database credentials through pluggable providers.
package creds

import (
	"context"
	"errors"
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

// Registry returns the built-in providers; only `static` needs a key, so an empty keyring leaves the others working.
func Registry(keys Keyring, stores func(ctx context.Context, name string) (VaultStore, error)) map[string]Provider {
	return map[string]Provider{
		ProviderStatic:     Static{Keys: keys},
		ProviderKubernetes: Kubernetes{},
		ProviderVault:      vaults{client: http.DefaultClient, lookup: stores},
	}
}

// ErrCredentialConfig marks a refusal an operator fixes in a target's or a store's registration; it names only registration values, so the API publishes its message where it redacts an unclassified failure.
var ErrCredentialConfig = errors.New("credential configuration")

type configErr struct{ err error }

func (c configErr) Error() string { return c.err.Error() }

// Is matches the sentinel, which carries no text of its own so the wrapped message reaches the caller whole.
func (c configErr) Is(target error) bool { return target == ErrCredentialConfig }

func (c configErr) Unwrap() error { return c.err }

// Misconfigured marks err as ErrCredentialConfig without adding to its message.
func Misconfigured(err error) error { return configErr{err} }
