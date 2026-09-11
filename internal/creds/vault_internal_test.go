package creds

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func projectedTokenVault(t *testing.T, presented *[]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/auth/kubernetes/login", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		*presented = append(*presented, body["jwt"])
		_, _ = w.Write([]byte(`{"auth":{"client_token":"k8s-token"}}`))
	})
	mux.HandleFunc("GET /v1/secret/data/app", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Vault-Token") != "k8s-token" {
			http.Error(w, `{"errors":["permission denied"]}`, http.StatusForbidden)

			return
		}
		_, _ = w.Write([]byte(`{"data":{"data":{"dsn":"postgres://vault"}}}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return srv
}

func TestAStoreLogsInWithTheProjectedTokenAndARotatedOneIsPickedUp(t *testing.T) {
	t.Parallel()

	var presented []string
	srv := projectedTokenVault(t, &presented)
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := vaults{
		client: srv.Client(),
		path:   token,
		lookup: func(context.Context, string) (VaultStore, error) {
			return VaultStore{Address: srv.URL, Role: "godwit"}, nil
		},
	}
	config := map[string]string{"path": "secret/data/app", StoreConfigKey: "production"}
	if got, err := p.DSN(context.Background(), config); err != nil || got != "postgres://vault" {
		t.Fatalf("got %q, err = %v", got, err)
	}
	if err := os.WriteFile(token, []byte("second\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := p.DSN(context.Background(), config); err != nil || got != "postgres://vault" {
		t.Fatalf("got %q, err = %v", got, err)
	}
	if !slices.Equal(presented, []string{"first", "second"}) {
		t.Fatalf("presented = %v, want the file as it stood at each login", presented)
	}
}

func TestTheIdentityIsTheOneTheChartMints(t *testing.T) {
	t.Parallel()

	if VaultAudience != "godwit" {
		t.Fatalf("VaultAudience = %q; the chart's projected volume mints the other one", VaultAudience)
	}
	if VaultTokenPath != "/var/run/secrets/godwit/vault/token" {
		t.Fatalf("VaultTokenPath = %q; the chart mounts the other one", VaultTokenPath)
	}
	if VaultTokenPath == defaultJWTPath {
		t.Fatal("the projected token lands where the generic ServiceAccount token does")
	}
}
