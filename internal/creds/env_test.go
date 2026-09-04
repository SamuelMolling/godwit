package creds_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/SamuelMolling/godwit/internal/creds"
)

func TestEnvKeyIDNamesTheKey(t *testing.T) {
	t.Parallel()

	p := creds.NewEnv(key)
	if p.Name() != "env" {
		t.Fatalf("name = %q", p.Name())
	}
	if len(p.KeyID()) != 8 || p.KeyID() == creds.NewEnv(bytes.Repeat([]byte("o"), 32)).KeyID() {
		t.Fatalf("key id = %q", p.KeyID())
	}
	if strings.Contains(p.KeyID(), hex.EncodeToString(key)) {
		t.Fatal("the key id must not carry the key")
	}
}

func TestEnvOpenUnknownKeyID(t *testing.T) {
	t.Parallel()

	sealed, err := envRing(bytes.Repeat([]byte("o"), 32)).Seal(context.Background(), "x")
	if err != nil {
		t.Fatal(err)
	}
	_, err = envRing(key).Open(context.Background(), sealed)
	if err == nil || !strings.Contains(err.Error(), "GODWIT_MASTER_KEY_PREVIOUS") {
		t.Fatalf("err = %v", err)
	}
}

func TestEnvOpenNamedKeyThatDoesNotOpen(t *testing.T) {
	t.Parallel()

	p := creds.NewEnv(key)
	_, err := p.Open(context.Background(), nil, p.KeyID(), []byte(strings.Repeat("x", 64)))
	if err == nil || !strings.Contains(err.Error(), "does not open this value") {
		t.Fatalf("err = %v", err)
	}
}

func TestKeyringFromEnvDefaultsToNoKey(t *testing.T) {
	t.Setenv("GODWIT_MASTER_KEY", "")
	t.Setenv("GODWIT_KEY_PROVIDER", "")

	ring, err := creds.KeyringFromEnv()
	if err != nil || ring.Configured() {
		t.Fatalf("ring = %v, err = %v", ring.Describe(), err)
	}
}

func TestKeyringFromEnvKeys(t *testing.T) {
	t.Setenv("GODWIT_KEY_PROVIDER", "env")
	t.Setenv("GODWIT_MASTER_KEY", strings.Repeat("ab", 32))
	t.Setenv("GODWIT_MASTER_KEY_PREVIOUS", " "+strings.Repeat("cd", 32)+" , ")

	ring, err := creds.KeyringFromEnv()
	if err != nil || !ring.Configured() {
		t.Fatalf("err = %v", err)
	}
	sealed, err := creds.NewKeyring(creds.NewEnv(mustHex(t, strings.Repeat("cd", 32)))).Seal(context.Background(), "postgres://old")
	if err != nil {
		t.Fatal(err)
	}
	dsn, err := ring.Open(context.Background(), sealed)
	if err != nil || dsn != "postgres://old" {
		t.Fatalf("dsn = %q, err = %v", dsn, err)
	}
}

func TestKeyringFromEnvErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"bad primary", map[string]string{"GODWIT_MASTER_KEY": "not-hex"}, "GODWIT_MASTER_KEY must be 64 hex chars"},
		{"short primary", map[string]string{"GODWIT_MASTER_KEY": "abcd"}, "GODWIT_MASTER_KEY must be 64 hex chars"},
		{
			"bad previous",
			map[string]string{"GODWIT_MASTER_KEY": strings.Repeat("ab", 32), "GODWIT_MASTER_KEY_PREVIOUS": "nope"},
			"GODWIT_MASTER_KEY_PREVIOUS",
		},
		{"env without a key", map[string]string{"GODWIT_KEY_PROVIDER": "env"}, "needs GODWIT_MASTER_KEY"},
		{"gcpkms without a key", map[string]string{"GODWIT_KEY_PROVIDER": "gcpkms"}, "needs GODWIT_KMS_KEY"},
		{"vault transit without a key", map[string]string{"GODWIT_KEY_PROVIDER": "vault-transit"}, "needs GODWIT_KMS_KEY"},
		{
			"vault transit without an address",
			map[string]string{"GODWIT_KEY_PROVIDER": "vault-transit", "GODWIT_KMS_KEY": "dsn"},
			"needs VAULT_ADDR",
		},
		{"unknown provider", map[string]string{"GODWIT_KEY_PROVIDER": "aws-kms"}, "unknown key provider"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, name := range []string{"GODWIT_KEY_PROVIDER", "GODWIT_MASTER_KEY", "GODWIT_MASTER_KEY_PREVIOUS", "GODWIT_KMS_KEY", "VAULT_ADDR"} {
				t.Setenv(name, "")
			}
			for name, value := range tc.env {
				t.Setenv(name, value)
			}
			_, err := creds.KeyringFromEnv()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestKeyringFromEnvKMSProviders(t *testing.T) {
	t.Setenv("GODWIT_KEY_PROVIDER", "gcpkms")
	t.Setenv("GODWIT_KMS_KEY", "projects/p/locations/l/keyRings/r/cryptoKeys/k")

	ring, err := creds.KeyringFromEnv()
	if err != nil || ring.Describe() != "gcpkms:projects/p/locations/l/keyRings/r/cryptoKeys/k" {
		t.Fatalf("ring = %q, err = %v", ring.Describe(), err)
	}

	t.Setenv("GODWIT_KEY_PROVIDER", "vault-transit")
	t.Setenv("GODWIT_KMS_KEY", "godwit")
	t.Setenv("VAULT_ADDR", "https://vault.example")
	ring, err = creds.KeyringFromEnv()
	if err != nil || ring.Describe() != "vault-transit:godwit" {
		t.Fatalf("ring = %q, err = %v", ring.Describe(), err)
	}
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	raw, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}

	return raw
}
