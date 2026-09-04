package creds

import (
	"cmp"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
)

// ProviderVaultTransit names the Vault Transit key provider.
const ProviderVaultTransit = "vault-transit"

// VaultTransit seals a fresh data key per value with a Vault Transit key. It authenticates the same
// way the `vault` credential provider does, so a deployment already reading secrets from Vault gains
// a key provider without gaining a dependency.
type VaultTransit struct {
	Vault Vault
	Mount string
	Key   string
}

// VaultTransitFromEnv builds the provider from GODWIT_KMS_KEY, GODWIT_VAULT_TRANSIT_MOUNT and the
// VAULT_* variables the `vault` credential provider reads.
func VaultTransitFromEnv() (VaultTransit, error) {
	key := os.Getenv("GODWIT_KMS_KEY")
	if key == "" {
		return VaultTransit{}, errors.New("key provider vault-transit needs GODWIT_KMS_KEY, the transit key name")
	}
	v := VaultFromEnv()
	if v.Address == "" {
		return VaultTransit{}, errors.New("key provider vault-transit needs VAULT_ADDR")
	}

	return VaultTransit{Vault: v, Mount: cmp.Or(os.Getenv("GODWIT_VAULT_TRANSIT_MOUNT"), "transit"), Key: key}, nil
}

// Name implements KeyProvider.
func (VaultTransit) Name() string { return ProviderVaultTransit }

// KeyID implements KeyProvider.
func (p VaultTransit) KeyID() string { return p.Key }

// Seal implements KeyProvider.
func (p VaultTransit) Seal(ctx context.Context, aad []byte, plaintext string) ([]byte, error) {
	var out struct {
		Data struct {
			Plaintext  string `json:"plaintext"`
			Ciphertext string `json:"ciphertext"`
		} `json:"data"`
	}
	if err := p.post(ctx, "datakey/plaintext/"+p.Key, nil, &out); err != nil {
		return nil, fmt.Errorf("vault transit datakey: %w", err)
	}
	dataKey, err := base64.StdEncoding.DecodeString(out.Data.Plaintext)
	if err != nil {
		return nil, fmt.Errorf("vault transit datakey: decode plaintext: %w", err)
	}
	inner, err := sealGCM(dataKey, aad, plaintext)
	if err != nil {
		return nil, err
	}

	return joinBlob([]byte(out.Data.Ciphertext), inner)
}

// Open implements KeyProvider. Transit ciphertext carries its own key version, so a rotation inside
// Vault needs nothing here.
func (p VaultTransit) Open(ctx context.Context, aad []byte, keyID string, blob []byte) (string, error) {
	wrapped, inner, err := splitBlob(blob)
	if err != nil {
		return "", err
	}
	var out struct {
		Data struct {
			Plaintext string `json:"plaintext"`
		} `json:"data"`
	}
	body := map[string]string{"ciphertext": string(wrapped)}
	if err := p.post(ctx, "decrypt/"+cmp.Or(keyID, p.Key), body, &out); err != nil {
		return "", fmt.Errorf("vault transit decrypt: %w", err)
	}
	dataKey, err := base64.StdEncoding.DecodeString(out.Data.Plaintext)
	if err != nil {
		return "", fmt.Errorf("vault transit decrypt: decode plaintext: %w", err)
	}

	return openGCM(dataKey, aad, inner)
}

func (p VaultTransit) post(ctx context.Context, path string, body, out any) error {
	token, err := p.Vault.token(ctx)
	if err != nil {
		return err
	}
	if body == nil {
		body = map[string]string{}
	}

	return p.Vault.call(ctx, http.MethodPost, p.Mount+"/"+path, token, body, out)
}
