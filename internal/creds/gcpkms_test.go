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

// fakeKMS answers the two Cloud KMS methods godwit calls, wrapping the data key as
// base64("<plaintext>|<aad>") so a decrypt under the wrong additional data is refused, as the real
// service refuses it.
func fakeKMS(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		switch {
		case strings.HasSuffix(r.URL.Path, ":encrypt"):
			writeJSON(w, map[string]string{
				"ciphertext": base64.StdEncoding.EncodeToString([]byte(in["plaintext"] + "|" + in["additionalAuthenticatedData"])),
			})
		default:
			raw, _ := base64.StdEncoding.DecodeString(in["ciphertext"])
			plain, aad, _ := strings.Cut(string(raw), "|")
			if aad != in["additionalAuthenticatedData"] {
				http.Error(w, "aad mismatch", http.StatusBadRequest)

				return
			}
			writeJSON(w, map[string]string{"plaintext": plain})
		}
	}))
	t.Cleanup(srv.Close)

	return srv
}

func writeJSON(w http.ResponseWriter, body any) {
	_ = json.NewEncoder(w).Encode(body)
}

func testKMS(t *testing.T) GCPKMS {
	t.Helper()
	srv := fakeKMS(t)

	return GCPKMS{
		KeyName:  "projects/p/locations/l/keyRings/r/cryptoKeys/k",
		Endpoint: srv.URL,
		Token:    func(context.Context) (string, error) { return "token", nil },
		Client:   srv.Client(),
	}
}

func TestGCPKMSRoundTrip(t *testing.T) {
	t.Parallel()

	ring := NewKeyring(testKMS(t))
	sealed, err := ring.Seal(context.Background(), "postgres://kms")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(sealed, "godwit1:gcpkms:") || strings.Contains(sealed, "postgres") {
		t.Fatalf("sealed = %q", sealed)
	}
	dsn, err := ring.Open(context.Background(), sealed)
	if err != nil || dsn != "postgres://kms" {
		t.Fatalf("dsn = %q, err = %v", dsn, err)
	}
	if ring.NeedsReseal(sealed) {
		t.Fatal("a value under the configured key must be left alone")
	}
}

func TestGCPKMSHeaderIsAuthenticated(t *testing.T) {
	t.Parallel()

	ring := NewKeyring(testKMS(t))
	sealed, err := ring.Seal(context.Background(), "postgres://kms")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.SplitN(sealed, ":", 4)
	other := base64.RawURLEncoding.EncodeToString([]byte("projects/p/locations/l/keyRings/r/cryptoKeys/other"))
	if _, err := ring.Open(context.Background(), parts[0]+":"+parts[1]+":"+other+":"+parts[3]); err == nil {
		t.Fatal("a rewritten key name must not open the value")
	}
}

func TestGCPKMSFromEnv(t *testing.T) {
	t.Setenv("GODWIT_KMS_KEY", "")
	if _, err := GCPKMSFromEnv(); err == nil {
		t.Fatal("want error")
	}
	t.Setenv("GODWIT_KMS_KEY", "projects/p/cryptoKeys/k")
	t.Setenv("GODWIT_KMS_ENDPOINT", "")
	p, err := GCPKMSFromEnv()
	if err != nil || p.Endpoint != DefaultGCPKMSEndpoint || p.KeyID() != "projects/p/cryptoKeys/k" {
		t.Fatalf("p = %+v, err = %v", p, err)
	}
}

func TestMetadataToken(t *testing.T) {
	t.Parallel()

	tok, err := MetadataToken("static", "unused", nil)(context.Background())
	if err != nil || tok != "static" {
		t.Fatalf("tok = %q, err = %v", tok, err)
	}

	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Metadata-Flavor") != "Google" {
			http.Error(w, "no flavor", http.StatusForbidden)

			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")

	body = `{"access_token":"abc"}`
	if tok, err := MetadataToken("", host, srv.Client())(context.Background()); err != nil || tok != "abc" {
		t.Fatalf("tok = %q, err = %v", tok, err)
	}
	body = `{}`
	if _, err := MetadataToken("", host, srv.Client())(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "no access_token") {
		t.Fatalf("err = %v", err)
	}
	if _, err := MetadataToken("", "127.0.0.1:1", srv.Client())(context.Background()); err == nil {
		t.Fatal("an unreachable metadata server must fail")
	}
	if _, err := MetadataToken("", "\x7f", srv.Client())(context.Background()); err == nil {
		t.Fatal("an unusable host must fail")
	}
}

func TestGCPKMSSealErrors(t *testing.T) {
	p := testKMS(t)
	ctx := context.Background()

	orig := randReader
	randReader = bytes.NewReader(nil)
	_, err := p.Seal(ctx, nil, "x")
	randReader = orig
	if err == nil {
		t.Fatal("a dry random source must fail")
	}

	origAEAD := newAEAD
	newAEAD = func(cipher.Block) (cipher.AEAD, error) { return nil, errors.New("boom") }
	_, err = p.Seal(ctx, nil, "x")
	newAEAD = origAEAD
	if err == nil {
		t.Fatal("a broken AEAD must fail")
	}

	down := p
	down.Endpoint = "http://127.0.0.1:1"
	if _, err := down.Seal(ctx, nil, "x"); err == nil || !strings.Contains(err.Error(), "cloud kms encrypt") {
		t.Fatalf("err = %v", err)
	}

	bad := p
	bad.Endpoint = badEndpoint(t, `{"ciphertext":"!!!"}`)
	if _, err := bad.Seal(ctx, nil, "x"); err == nil || !strings.Contains(err.Error(), "decode ciphertext") {
		t.Fatalf("err = %v", err)
	}

	huge := p
	huge.Endpoint = badEndpoint(t, `{"ciphertext":"`+base64.StdEncoding.EncodeToString(make([]byte, maxWrappedKey))+`"}`)
	if _, err := huge.Seal(ctx, nil, "x"); err == nil || !strings.Contains(err.Error(), "wrapped data key") {
		t.Fatalf("err = %v", err)
	}
}

func TestGCPKMSOpenErrors(t *testing.T) {
	t.Parallel()

	p := testKMS(t)
	ctx := context.Background()
	if _, err := p.Open(ctx, nil, "", []byte{1}); err == nil {
		t.Fatal("a truncated blob must fail")
	}

	sealed, err := p.Seal(ctx, nil, "postgres://kms")
	if err != nil {
		t.Fatal(err)
	}
	down := p
	down.Endpoint = "http://127.0.0.1:1"
	if _, err := down.Open(ctx, nil, "", sealed); err == nil || !strings.Contains(err.Error(), "cloud kms decrypt") {
		t.Fatalf("err = %v", err)
	}

	bad := p
	bad.Endpoint = badEndpoint(t, `{"plaintext":"!!!"}`)
	if _, err := bad.Open(ctx, nil, "", sealed); err == nil || !strings.Contains(err.Error(), "decode plaintext") {
		t.Fatalf("err = %v", err)
	}

	wrong := p
	wrong.Endpoint = badEndpoint(t, `{"plaintext":"`+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("z"), 32))+`"}`)
	if _, err := wrong.Open(ctx, nil, "", sealed); err == nil || !strings.Contains(err.Error(), "decrypt") {
		t.Fatalf("err = %v", err)
	}
}

func TestGCPKMSCallErrors(t *testing.T) {
	t.Parallel()

	p := testKMS(t)
	p.Token = func(context.Context) (string, error) { return "", errors.New("no metadata") }
	if err := p.call(context.Background(), "k:encrypt", nil, nil); err == nil {
		t.Fatal("a token failure must fail the call")
	}

	p = testKMS(t)
	p.Endpoint = "://"
	if err := p.call(context.Background(), "k:encrypt", nil, nil); err == nil {
		t.Fatal("an unusable endpoint must fail")
	}
}

func TestJSONRequestErrors(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "denied", http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := jsonRequest(nil, req, nil); err == nil || !strings.Contains(err.Error(), "status 403") {
		t.Fatalf("err = %v", err)
	}

	garbage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	t.Cleanup(garbage.Close)
	req, err = http.NewRequestWithContext(context.Background(), http.MethodGet, garbage.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	var out struct{}
	if err := jsonRequest(garbage.Client(), req, &out); err == nil {
		t.Fatal("want a decode error")
	}
}

func badEndpoint(t *testing.T, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	return srv.URL
}
