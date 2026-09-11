package creds

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

// KeyProvider seals and opens the secrets godwit keeps in a target's config.
type KeyProvider interface {
	Name() string
	KeyID() string
	Seal(ctx context.Context, aad []byte, plaintext string) ([]byte, error)
	Open(ctx context.Context, aad []byte, keyID string, blob []byte) (string, error)
}

// ErrNoKey reports that a value needs a key and the service was started without one.
var ErrNoKey = errors.New("no key is configured: set GODWIT_MASTER_KEY, or GODWIT_KEY_PROVIDER with GODWIT_KMS_KEY")

// Keyring seals and opens target secrets under one key provider; its zero value holds none, which is what an all-`vault` or all-`kubernetes` deployment runs with.
type Keyring struct {
	provider KeyProvider
}

// NewKeyring returns the keyring sealing under p.
func NewKeyring(p KeyProvider) Keyring { return Keyring{provider: p} }

// Configured reports whether a key provider is set.
func (k Keyring) Configured() bool { return k.provider != nil }

// Describe names the provider and key in force, or "none".
func (k Keyring) Describe() string {
	if k.provider == nil {
		return "none"
	}

	return k.provider.Name() + ":" + k.provider.KeyID()
}

// Base64 has no colon, so a value without this header is one of the headerless ciphertexts the `env` provider wrote before.
const headerPrefix = "godwit1"

func header(name, keyID string) string {
	return headerPrefix + ":" + name + ":" + base64.RawURLEncoding.EncodeToString([]byte(keyID))
}

// Seal prefixes a header naming the provider and key that opens the value, bound into the payload as additional authenticated data.
func (k Keyring) Seal(ctx context.Context, plaintext string) (string, error) {
	if k.provider == nil {
		return "", ErrNoKey
	}
	head := header(k.provider.Name(), k.provider.KeyID())
	blob, err := k.provider.Seal(ctx, []byte(head), plaintext)
	if err != nil {
		return "", err
	}

	return head + ":" + base64.StdEncoding.EncodeToString(blob), nil
}

// Open reads a value produced by Seal, or a headerless one written before the header existed.
func (k Keyring) Open(ctx context.Context, encoded string) (string, error) {
	s, err := parseSealed(encoded)
	if err != nil {
		return "", err
	}
	if k.provider == nil {
		return "", fmt.Errorf("%w (this value is sealed by key provider %q)", ErrNoKey, s.provider)
	}
	if s.provider != k.provider.Name() {
		return "", fmt.Errorf("value is sealed by key provider %q, but %q is configured", s.provider, k.provider.Name())
	}

	return k.provider.Open(ctx, s.aad, s.keyID, s.blob)
}

// NeedsReseal reports whether encoded is readable but sealed under something other than the key in force.
func (k Keyring) NeedsReseal(encoded string) bool {
	if k.provider == nil {
		return false
	}
	s, err := parseSealed(encoded)
	if err != nil {
		return false
	}

	return s.provider != k.provider.Name() || s.keyID != k.provider.KeyID()
}

type sealed struct {
	provider string
	keyID    string
	aad      []byte
	blob     []byte
}

func parseSealed(encoded string) (sealed, error) {
	parts := strings.SplitN(encoded, ":", 4)
	if len(parts) != 4 || parts[0] != headerPrefix {
		raw, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return sealed{}, fmt.Errorf("decode: %w", err)
		}

		return sealed{provider: ProviderEnv, blob: raw}, nil
	}
	id, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return sealed{}, fmt.Errorf("decode key id: %w", err)
	}
	raw, err := base64.StdEncoding.DecodeString(parts[3])
	if err != nil {
		return sealed{}, fmt.Errorf("decode: %w", err)
	}

	return sealed{
		provider: parts[1], keyID: string(id),
		aad: []byte(strings.Join(parts[:3], ":")), blob: raw,
	}, nil
}

const maxWrappedKey = 1 << 16

func joinBlob(wrapped, inner []byte) ([]byte, error) {
	if len(wrapped) >= maxWrappedKey {
		return nil, fmt.Errorf("wrapped data key is %d bytes, over the %d the format carries", len(wrapped), maxWrappedKey-1)
	}
	out := make([]byte, 2, 2+len(wrapped)+len(inner))
	binary.BigEndian.PutUint16(out, uint16(len(wrapped)))

	return append(append(out, wrapped...), inner...), nil
}

func splitBlob(blob []byte) (wrapped, inner []byte, err error) {
	if len(blob) < 2 {
		return nil, nil, errors.New("sealed value is truncated")
	}
	n := int(binary.BigEndian.Uint16(blob))
	if len(blob) < 2+n {
		return nil, nil, errors.New("sealed value is truncated")
	}

	return blob[2 : 2+n], blob[2+n:], nil
}
