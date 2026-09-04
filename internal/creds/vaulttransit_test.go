package creds

import (
	"bytes"
	"context"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const transitPrefix = "vault:v1:"

func fakeTransit(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		if strings.Contains(r.URL.Path, "/datakey/plaintext/") {
			plain := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("d"), dataKeyBytes))
			writeJSON(w, map[string]any{"data": map[string]string{
				"plaintext": plain, "ciphertext": transitPrefix + plain,
			}})

			return
		}
		writeJSON(w, map[string]any{"data": map[string]string{
			"plaintext": strings.TrimPrefix(in["ciphertext"], transitPrefix),
		}})
	}))
	t.Cleanup(srv.Close)

	return srv
}

func testTransit(t *testing.T) VaultTransit {
	t.Helper()
	srv := fakeTransit(t)

	return VaultTransit{
		Vault: Vault{Address: srv.URL, Token: "root", Client: srv.Client()},
		Mount: "transit",
		Key:   "godwit",
	}
}

func TestVaultTransitRoundTrip(t *testing.T) {
	t.Parallel()

	ring := NewKeyring(testTransit(t))
	sealed, err := ring.Seal(context.Background(), "postgres://transit")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(sealed, "godwit1:vault-transit:") || strings.Contains(sealed, "postgres") {
		t.Fatalf("sealed = %q", sealed)
	}
	dsn, err := ring.Open(context.Background(), sealed)
	if err != nil || dsn != "postgres://transit" {
		t.Fatalf("dsn = %q, err = %v", dsn, err)
	}
	if ring.NeedsReseal(sealed) {
		t.Fatal("a value under the configured key must be left alone")
	}
}

func TestVaultTransitFromEnv(t *testing.T) {
	t.Setenv("GODWIT_KMS_KEY", "godwit")
	t.Setenv("VAULT_ADDR", "https://vault.example")
	t.Setenv("GODWIT_VAULT_TRANSIT_MOUNT", "kms")

	p, err := VaultTransitFromEnv()
	if err != nil || p.Mount != "kms" || p.Name() != ProviderVaultTransit {
		t.Fatalf("p = %+v, err = %v", p, err)
	}
}

func TestVaultTransitSealErrors(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	noAuth := testTransit(t)
	noAuth.Vault.Token, noAuth.Vault.JWTPath = "", "/nope"
	if _, err := noAuth.Seal(ctx, nil, "x"); err == nil {
		t.Fatal("a login failure must fail the seal")
	}

	bad := testTransit(t)
	bad.Vault.Address = badEndpoint(t, `{"data":{"plaintext":"!!!","ciphertext":"vault:v1:x"}}`)
	if _, err := bad.Seal(ctx, nil, "x"); err == nil || !strings.Contains(err.Error(), "decode plaintext") {
		t.Fatalf("err = %v", err)
	}

	down := testTransit(t)
	down.Vault.Address = "http://127.0.0.1:1"
	if _, err := down.Seal(ctx, nil, "x"); err == nil || !strings.Contains(err.Error(), "datakey") {
		t.Fatalf("err = %v", err)
	}

	huge := testTransit(t)
	huge.Vault.Address = badEndpoint(t, `{"data":{"plaintext":"`+
		base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("d"), dataKeyBytes))+`","ciphertext":"`+
		strings.Repeat("c", maxWrappedKey)+`"}}`)
	if _, err := huge.Seal(ctx, nil, "x"); err == nil || !strings.Contains(err.Error(), "wrapped data key") {
		t.Fatalf("err = %v", err)
	}
}

func TestVaultTransitSealAEADFailure(t *testing.T) {
	orig := newAEAD
	newAEAD = func(cipher.Block) (cipher.AEAD, error) { return nil, errors.New("boom") }
	defer func() { newAEAD = orig }()

	if _, err := testTransit(t).Seal(context.Background(), nil, "x"); err == nil {
		t.Fatal("want error")
	}
}

func TestVaultTransitOpenErrors(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	p := testTransit(t)
	if _, err := p.Open(ctx, nil, "", []byte{1}); err == nil {
		t.Fatal("a truncated blob must fail")
	}

	sealed, err := p.Seal(ctx, nil, "postgres://transit")
	if err != nil {
		t.Fatal(err)
	}
	down := p
	down.Vault.Address = "http://127.0.0.1:1"
	if _, err := down.Open(ctx, nil, "", sealed); err == nil || !strings.Contains(err.Error(), "decrypt") {
		t.Fatalf("err = %v", err)
	}

	bad := p
	bad.Vault.Address = badEndpoint(t, `{"data":{"plaintext":"!!!"}}`)
	if _, err := bad.Open(ctx, nil, "", sealed); err == nil || !strings.Contains(err.Error(), "decode plaintext") {
		t.Fatalf("err = %v", err)
	}

	wrong := p
	wrong.Vault.Address = badEndpoint(t, `{"data":{"plaintext":"`+
		base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("z"), dataKeyBytes))+`"}}`)
	if _, err := wrong.Open(ctx, nil, "godwit", sealed); err == nil {
		t.Fatal("the wrong data key must not open the value")
	}
}
