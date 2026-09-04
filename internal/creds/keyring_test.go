package creds_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/SamuelMolling/godwit/internal/creds"
)

var key = bytes.Repeat([]byte("k"), 32)

func envRing(primary []byte, previous ...[]byte) creds.Keyring {
	return creds.NewKeyring(creds.NewEnv(primary, previous...))
}

type failingProvider struct {
	seal error
	open error
}

func (failingProvider) Name() string  { return "env" }
func (failingProvider) KeyID() string { return "ff" }

func (p failingProvider) Seal(context.Context, []byte, string) ([]byte, error) {
	return nil, p.seal
}

func (p failingProvider) Open(context.Context, []byte, string, []byte) (string, error) {
	return "", p.open
}

func TestKeyringRoundTrip(t *testing.T) {
	t.Parallel()

	ring := envRing(key)
	sealed, err := ring.Seal(context.Background(), "postgres://u:p@h/db")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(sealed, "godwit1:env:") {
		t.Fatalf("sealed = %q", sealed)
	}
	if strings.Contains(sealed, "postgres") {
		t.Fatal("not sealed")
	}
	dsn, err := ring.Open(context.Background(), sealed)
	if err != nil || dsn != "postgres://u:p@h/db" {
		t.Fatalf("dsn = %q, err = %v", dsn, err)
	}
}

func TestKeyringUnconfigured(t *testing.T) {
	t.Parallel()

	var ring creds.Keyring
	if ring.Configured() {
		t.Fatal("zero keyring must not be configured")
	}
	if ring.Describe() != "none" {
		t.Fatalf("describe = %q", ring.Describe())
	}
	if _, err := ring.Seal(context.Background(), "x"); !errors.Is(err, creds.ErrNoKey) {
		t.Fatalf("err = %v", err)
	}
	sealed, err := envRing(key).Seal(context.Background(), "x")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ring.Open(context.Background(), sealed); !errors.Is(err, creds.ErrNoKey) {
		t.Fatalf("err = %v", err)
	}
	if ring.NeedsReseal(sealed) {
		t.Fatal("an unconfigured keyring reseals nothing")
	}
}

func TestKeyringDescribe(t *testing.T) {
	t.Parallel()

	if got := envRing(key).Describe(); !strings.HasPrefix(got, "env:") || len(got) != len("env:")+8 {
		t.Fatalf("describe = %q", got)
	}
}

func TestKeyringProviderMismatch(t *testing.T) {
	t.Parallel()

	sealed := "godwit1:gcpkms:" + base64.RawURLEncoding.EncodeToString([]byte("projects/p")) + ":AAAA"
	_, err := envRing(key).Open(context.Background(), sealed)
	if err == nil || !strings.Contains(err.Error(), `sealed by key provider "gcpkms"`) {
		t.Fatalf("err = %v", err)
	}
}

func TestKeyringMalformed(t *testing.T) {
	t.Parallel()

	ring := envRing(key)
	for _, bad := range []string{
		"!!!not-base64!!!",
		"godwit1:env:!!!:AAAA",
		"godwit1:env:ZW52:!!!",
	} {
		if _, err := ring.Open(context.Background(), bad); err == nil {
			t.Fatalf("%q must fail", bad)
		}
		if ring.NeedsReseal(bad) {
			t.Fatalf("%q is not resealable", bad)
		}
	}
}

func TestKeyringSealProviderError(t *testing.T) {
	t.Parallel()

	ring := creds.NewKeyring(failingProvider{seal: errors.New("kms down")})
	if _, err := ring.Seal(context.Background(), "x"); err == nil {
		t.Fatal("want error")
	}
}

func TestKeyringHeaderIsAuthenticated(t *testing.T) {
	t.Parallel()

	ring := envRing(key)
	sealed, err := ring.Seal(context.Background(), "postgres://x")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.SplitN(sealed, ":", 4)
	tampered := parts[0] + ":" + parts[1] + ":" + base64.RawURLEncoding.EncodeToString([]byte("deadbeef")) + ":" + parts[3]
	if _, err := ring.Open(context.Background(), tampered); err == nil {
		t.Fatal("a rewritten header must not open")
	}
}

func TestKeyringLegacyCiphertext(t *testing.T) {
	t.Parallel()

	legacy := legacySeal(t, key, "postgres://legacy")
	ring := envRing(key)
	dsn, err := ring.Open(context.Background(), legacy)
	if err != nil || dsn != "postgres://legacy" {
		t.Fatalf("dsn = %q, err = %v", dsn, err)
	}
	if !ring.NeedsReseal(legacy) {
		t.Fatal("a headerless value must be resealed")
	}
	other := bytes.Repeat([]byte("o"), 32)
	if _, err := envRing(other).Open(context.Background(), legacy); err == nil {
		t.Fatal("the wrong key must not open a headerless value")
	}
}

func TestKeyringNeedsReseal(t *testing.T) {
	t.Parallel()

	old := bytes.Repeat([]byte("o"), 32)
	sealed, err := envRing(old).Seal(context.Background(), "postgres://x")
	if err != nil {
		t.Fatal(err)
	}
	rotated := envRing(key, old)
	if !rotated.NeedsReseal(sealed) {
		t.Fatal("a value under the previous key must be resealed")
	}
	dsn, err := rotated.Open(context.Background(), sealed)
	if err != nil || dsn != "postgres://x" {
		t.Fatalf("dsn = %q, err = %v", dsn, err)
	}
	fresh, err := rotated.Seal(context.Background(), "postgres://x")
	if err != nil {
		t.Fatal(err)
	}
	if rotated.NeedsReseal(fresh) {
		t.Fatal("a value under the primary key must be left alone")
	}
}

// legacySeal writes the headerless base64(nonce ‖ AES-256-GCM) form static targets held before the
// ciphertext named its key.
func legacySeal(t *testing.T, k []byte, plaintext string) string {
	t.Helper()
	sealed, err := creds.NewKeyring(legacyProvider{key: k}).Seal(context.Background(), plaintext)
	if err != nil {
		t.Fatal(err)
	}

	return strings.SplitN(sealed, ":", 4)[3]
}

type legacyProvider struct{ key []byte }

func (legacyProvider) Name() string  { return "env" }
func (legacyProvider) KeyID() string { return "" }

func (p legacyProvider) Seal(ctx context.Context, _ []byte, plaintext string) ([]byte, error) {
	return creds.NewEnv(p.key).Seal(ctx, nil, plaintext)
}

func (p legacyProvider) Open(ctx context.Context, _ []byte, id string, blob []byte) (string, error) {
	return creds.NewEnv(p.key).Open(ctx, nil, id, blob)
}
