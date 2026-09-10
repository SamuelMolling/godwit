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

func audienceVault(t *testing.T, presented *[]string) *httptest.Server {
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

// The kubelet rewrites a projected token long before it expires and the one it replaced stops working,
// so the file is read at every login rather than once at start-up.
func TestTheStoresAudienceNamesTheTokenAndARotatedOneIsPickedUp(t *testing.T) {
	t.Parallel()

	var presented []string
	srv := audienceVault(t, &presented)
	dir := t.TempDir()
	token := filepath.Join(dir, "vault.production.example")
	if err := os.WriteFile(token, []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := vaults{
		client: srv.Client(),
		dir:    dir,
		lookup: func(context.Context, string) (VaultStore, error) {
			return VaultStore{Address: srv.URL, Role: "godwit", Audience: "vault.production.example"}, nil
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

func TestTheDefaultTokenDirectoryIsTheOneTheChartMounts(t *testing.T) {
	t.Parallel()

	if audienceTokenDir != "/var/run/secrets/godwit/vault" {
		t.Fatalf("audienceTokenDir = %q; godwit.vaultTokenDir in the chart mounts the other one", audienceTokenDir)
	}
	if audienceTokenDir == filepath.Dir(defaultJWTPath) {
		t.Fatal("the audience tokens land where the generic ServiceAccount token does")
	}
}
