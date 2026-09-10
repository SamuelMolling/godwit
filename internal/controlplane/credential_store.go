package controlplane

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/SamuelMolling/godwit/internal/creds"
)

// CredentialStore is a registered Vault a target's secret can be read from.
type CredentialStore struct {
	Name     string
	Address  string
	Role     string
	Mount    string
	TokenEnv string
	Targets  int
}

// RegisterCredentialStore upserts a credential store; a target already pointed at it moves with it.
func (s *Store) RegisterCredentialStore(ctx context.Context, cs CredentialStore) error {
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO cp_credential_stores (name, address, k8s_role, k8s_mount, token_env)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (name) DO UPDATE SET address = EXCLUDED.address, k8s_role = EXCLUDED.k8s_role,
		   k8s_mount = EXCLUDED.k8s_mount, token_env = EXCLUDED.token_env`,
		cs.Name, cs.Address, cs.Role, cs.Mount, cs.TokenEnv); err != nil {
		return fmt.Errorf("register credential store: %w", err)
	}

	return nil
}

// ListCredentialStores returns every registered store by name.
func (s *Store) ListCredentialStores(ctx context.Context) ([]CredentialStore, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT s.name, s.address, s.k8s_role, s.k8s_mount, s.token_env,
			(SELECT count(*) FROM cp_targets t WHERE t.credential_store = s.name)
		FROM cp_credential_stores s ORDER BY s.name`)
	if err != nil {
		return nil, fmt.Errorf("list credential stores: %w", err)
	}
	var out []CredentialStore
	var cs CredentialStore
	if _, err := pgx.ForEachRow(rows, []any{&cs.Name, &cs.Address, &cs.Role, &cs.Mount, &cs.TokenEnv, &cs.Targets}, func() error {
		out = append(out, cs)

		return nil
	}); err != nil {
		return nil, fmt.Errorf("list credential stores: %w", err)
	}

	return out, nil
}

// VaultStore answers where a named credential store points; it is the vault provider's lookup.
func (s *Store) VaultStore(ctx context.Context, name string) (creds.VaultStore, error) {
	var v creds.VaultStore
	err := s.pool.QueryRow(ctx,
		`SELECT address, k8s_role, k8s_mount, token_env FROM cp_credential_stores WHERE name = $1`,
		name).Scan(&v.Address, &v.Role, &v.Mount, &v.TokenEnv)
	if errors.Is(err, pgx.ErrNoRows) {
		return creds.VaultStore{}, ErrNotFound
	}
	if err != nil {
		return creds.VaultStore{}, fmt.Errorf("load credential store: %w", err)
	}

	return v, nil
}
