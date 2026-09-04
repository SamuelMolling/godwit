package creds

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
)

// ProviderEnv names the key provider that holds its keys in the process environment.
const ProviderEnv = "env"

const masterKeyBytes = 32

// KeyringFromEnv builds the keyring GODWIT_KEY_PROVIDER selects: `env` (the default), `gcpkms` or
// `vault-transit`. With no provider named and no GODWIT_MASTER_KEY it returns an empty keyring, and
// the service runs without a key.
func KeyringFromEnv() (Keyring, error) {
	name := os.Getenv("GODWIT_KEY_PROVIDER")
	switch name {
	case "", ProviderEnv:
		keys, err := envKeys()
		if err != nil {
			return Keyring{}, err
		}
		if len(keys) == 0 {
			if name == ProviderEnv {
				return Keyring{}, errors.New("key provider env needs GODWIT_MASTER_KEY (64 hex chars)")
			}

			return Keyring{}, nil
		}

		return NewKeyring(Env{keys: keys}), nil
	case ProviderGCPKMS:
		p, err := GCPKMSFromEnv()
		if err != nil {
			return Keyring{}, err
		}

		return NewKeyring(p), nil
	case ProviderVaultTransit:
		p, err := VaultTransitFromEnv()
		if err != nil {
			return Keyring{}, err
		}

		return NewKeyring(p), nil
	default:
		return Keyring{}, fmt.Errorf("unknown key provider %q: want %s, %s or %s",
			name, ProviderEnv, ProviderGCPKMS, ProviderVaultTransit)
	}
}

// Env seals with AES-256-GCM under a key read from the process environment. Further keys are accepted
// for opening only, so a new key can be in force before every value sealed under the old one is gone.
type Env struct {
	keys []envKey
}

type envKey struct {
	id  string
	key []byte
}

// NewEnv returns the provider sealing under primary and opening under primary or any of previous.
func NewEnv(primary []byte, previous ...[]byte) Env {
	keys := []envKey{newEnvKey(primary)}
	for _, k := range previous {
		keys = append(keys, newEnvKey(k))
	}

	return Env{keys: keys}
}

func newEnvKey(key []byte) envKey {
	sum := sha256.Sum256(key)

	return envKey{id: hex.EncodeToString(sum[:4]), key: key}
}

func envKeys() ([]envKey, error) {
	primary := os.Getenv("GODWIT_MASTER_KEY")
	if primary == "" {
		return nil, nil
	}
	key, err := hex.DecodeString(primary)
	if err != nil || len(key) != masterKeyBytes {
		return nil, errors.New("GODWIT_MASTER_KEY must be 64 hex chars (32 bytes)")
	}
	keys := []envKey{newEnvKey(key)}
	for _, spec := range strings.Split(os.Getenv("GODWIT_MASTER_KEY_PREVIOUS"), ",") {
		spec = strings.TrimSpace(spec)
		if spec == "" {
			continue
		}
		old, err := hex.DecodeString(spec)
		if err != nil || len(old) != masterKeyBytes {
			return nil, errors.New("every key in GODWIT_MASTER_KEY_PREVIOUS must be 64 hex chars (32 bytes)")
		}
		keys = append(keys, newEnvKey(old))
	}

	return keys, nil
}

// Name implements KeyProvider.
func (Env) Name() string { return ProviderEnv }

// KeyID implements KeyProvider: the first four bytes of the key's SHA-256, which names the key
// without revealing it.
func (p Env) KeyID() string { return p.keys[0].id }

// Seal implements KeyProvider.
func (p Env) Seal(_ context.Context, aad []byte, plaintext string) ([]byte, error) {
	return sealGCM(p.keys[0].key, aad, plaintext)
}

// Open implements KeyProvider. An empty keyID is a value sealed before the header existed: every key
// is tried, and GCM authentication decides.
func (p Env) Open(_ context.Context, aad []byte, keyID string, blob []byte) (string, error) {
	var last error
	for _, k := range p.keys {
		if keyID != "" && k.id != keyID {
			continue
		}
		plain, err := openGCM(k.key, aad, blob)
		if err == nil {
			return plain, nil
		}
		last = err
	}
	switch {
	case last != nil && keyID == "":
		return "", fmt.Errorf("no configured key opens this value, sealed before keys were named: %w", last)
	case last != nil:
		return "", fmt.Errorf("key %s does not open this value: %w", keyID, last)
	default:
		return "", fmt.Errorf("this value is sealed under key %s; put that key in GODWIT_MASTER_KEY or GODWIT_MASTER_KEY_PREVIOUS", keyID)
	}
}
