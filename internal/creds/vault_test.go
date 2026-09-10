package creds_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SamuelMolling/godwit/internal/creds"
	"github.com/SamuelMolling/godwit/internal/creds/credstest"
)

func fakeVault(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/auth/kubernetes/login", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["jwt"] != "sa-jwt" || body["role"] != "godwit" {
			http.Error(w, `{"errors":["permission denied"]}`, http.StatusForbidden)

			return
		}
		_, _ = w.Write([]byte(`{"auth":{"client_token":"k8s-token"}}`))
	})
	mux.HandleFunc("GET /v1/secret/data/app", func(w http.ResponseWriter, r *http.Request) {
		if tok := r.Header.Get("X-Vault-Token"); tok != "root" && tok != "k8s-token" {
			http.Error(w, `{"errors":["permission denied"]}`, http.StatusForbidden)

			return
		}
		_, _ = w.Write([]byte(`{"data":{"data":{"dsn":"postgres://vault"},"metadata":{"version":1}}}`))
	})
	mux.HandleFunc("GET /v1/database/creds/app", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"username":"v-user","password":"v-pass","ttl":3600}}`))
	})
	mux.HandleFunc("GET /v1/database/creds/marked", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"username":"{{password}}","password":"v-pass"}}`))
	})
	mux.HandleFunc("GET /v1/broken", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return srv
}

func TestVaultConformance(t *testing.T) {
	t.Parallel()

	srv := fakeVault(t)
	credstest.Conformance(t, creds.Vault{Address: srv.URL + "/", Token: "root"},
		map[string]string{"path": "secret/data/app"}, "postgres://vault")
}

func TestVaultKubernetesLoginAndTemplate(t *testing.T) {
	t.Parallel()

	srv := fakeVault(t)
	jwt := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(jwt, []byte("sa-jwt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := creds.Vault{Address: srv.URL, Role: "godwit", Mount: "kubernetes", JWTPath: jwt, Client: srv.Client()}
	got, err := p.DSN(context.Background(), map[string]string{
		"path":     "database/creds/app",
		"template": "postgres://{{username}}:{{password}}@db/app",
	})
	if err != nil || got != "postgres://v-user:v-pass@db/app" {
		t.Fatalf("got %q, err = %v", got, err)
	}
	got, err = p.DSN(context.Background(), map[string]string{"path": "secret/data/app"})
	if err != nil || got != "postgres://vault" {
		t.Fatalf("got %q, err = %v", got, err)
	}
}

// The error crosses the API, lands in cp_runs.error and is posted to Slack, so it must name only the
// template's own keys. It used to return the partially rendered template from the first unresolved
// marker to the end, which put every field substituted after it — the password included — in the message.
func TestVaultTemplateErrorCarriesNoSecret(t *testing.T) {
	t.Parallel()

	srv := fakeVault(t)
	p := creds.Vault{Address: srv.URL, Token: "root", Client: srv.Client()}
	_, err := p.DSN(context.Background(), map[string]string{
		"path":     "database/creds/app",
		"template": "postgres://{{missing}}:{{password}}@db/{{alsomissing}}",
	})
	if err == nil || !strings.Contains(err.Error(), "missing, alsomissing") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "v-pass") || strings.Contains(err.Error(), "v-user") {
		t.Fatalf("the error carried a substituted secret: %v", err)
	}

	_, err = p.DSN(context.Background(), map[string]string{"path": "database/creds/app", "template": "postgres://{{username"})
	if err == nil || !strings.Contains(err.Error(), "unclosed") {
		t.Fatalf("an unclosed marker must be refused, not silently kept: %v", err)
	}
}

// A field whose value contains a marker is substituted once and never rescanned.
func TestVaultRenderDoesNotRescan(t *testing.T) {
	t.Parallel()

	srv := fakeVault(t)
	p := creds.Vault{Address: srv.URL, Token: "root", Client: srv.Client()}
	got, err := p.DSN(context.Background(), map[string]string{
		"path": "database/creds/marked", "template": "postgres://{{username}}@db/app",
	})
	if err != nil || got != "postgres://{{password}}@db/app" {
		t.Fatalf("got %q, err = %v", got, err)
	}
}

func TestVaultErrors(t *testing.T) {
	t.Parallel()

	srv := fakeVault(t)
	jwt := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(jwt, []byte("wrong"), 0o600); err != nil {
		t.Fatal(err)
	}
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	cases := []struct {
		name   string
		p      creds.Vault
		config map[string]string
		want   string
	}{
		{"no address", creds.Vault{Token: "root"}, map[string]string{"path": "x"}, "no address"},
		{"jwt missing", creds.Vault{Address: srv.URL, JWTPath: "/nope"}, map[string]string{"path": "x"}, "service account token"},
		{"login denied", creds.Vault{Address: srv.URL, Mount: "kubernetes", JWTPath: jwt}, map[string]string{"path": "x"}, "status 403"},
		{"login unreachable", creds.Vault{Address: closed.URL, JWTPath: jwt}, map[string]string{"path": "x"}, "kubernetes login"},
		{"bad address", creds.Vault{Address: "::bad", Token: "root"}, map[string]string{"path": "x"}, "missing protocol"},
		{"read denied", creds.Vault{Address: srv.URL, Token: "bad"}, map[string]string{"path": "secret/data/app"}, "permission denied"},
		{"read not found", creds.Vault{Address: srv.URL, Token: "root"}, map[string]string{"path": "nope"}, "status 404"},
		{"bad json", creds.Vault{Address: srv.URL, Token: "root"}, map[string]string{"path": "broken"}, "read vault secret"},
		{"missing field", creds.Vault{Address: srv.URL, Token: "root"}, map[string]string{"path": "database/creds/app"}, "no field for dsn"},
		{
			"template leftover",
			creds.Vault{Address: srv.URL, Token: "root"},
			map[string]string{"path": "database/creds/app", "template": "{{username}}:{{ttl}}"},
			"no field for ttl",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.p.DSN(context.Background(), tc.config)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestNoEnvironmentReachesATargetsVault(t *testing.T) {
	t.Setenv("GODWIT_KMS_KEY", "godwit")
	t.Setenv("GODWIT_VAULT_TRANSIT_ADDR", "http://vault:8200")
	t.Setenv("GODWIT_VAULT_TRANSIT_TOKEN", "tok")
	t.Setenv("GODWIT_VAULT_TRANSIT_K8S_ROLE", "role")
	t.Setenv("GODWIT_VAULT_TRANSIT_K8S_MOUNT", "k8s")
	t.Setenv("GODWIT_VAULT_TRANSIT_K8S_JWT", "/jwt")
	t.Setenv("VAULT_ADDR", "http://target-vault:8200")
	t.Setenv("VAULT_TOKEN", "root")

	transit, err := creds.VaultTransitFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	p := transit.Vault
	if p.Address != "http://vault:8200" || p.Token != "tok" || p.Role != "role" || p.Mount != "k8s" || p.JWTPath != "/jwt" {
		t.Fatalf("vault = %+v", p)
	}

	_, err = creds.Registry(creds.Keyring{}, nil)["vault"].DSN(
		context.Background(), map[string]string{"path": "secret/data/app"})
	if err == nil || !strings.Contains(err.Error(), "names no credential store") {
		t.Fatalf("a target with no store must be refused whatever the environment holds: %v", err)
	}

	t.Setenv("GODWIT_VAULT_TRANSIT_K8S_MOUNT", "")
	t.Setenv("GODWIT_VAULT_TRANSIT_K8S_JWT", "")
	transit, err = creds.VaultTransitFromEnv()
	if err != nil || transit.Vault.Mount != "kubernetes" ||
		transit.Vault.JWTPath != "/var/run/secrets/kubernetes.io/serviceaccount/token" {
		t.Fatalf("defaults = %+v, err = %v", transit.Vault, err)
	}
	t.Setenv("GODWIT_VAULT_TRANSIT_ADDR", "")
	if _, err := creds.VaultTransitFromEnv(); err == nil ||
		!strings.Contains(err.Error(), "GODWIT_VAULT_TRANSIT_ADDR") {
		t.Fatalf("the key provider must name its own variable, not inherit one: %v", err)
	}
}

func TestVaultsReadFromTheStoreTheTargetNames(t *testing.T) {
	srv := fakeVault(t)
	t.Setenv("VAULT_TOKEN_DEMO", "root")
	lookup := func(_ context.Context, name string) (creds.VaultStore, error) {
		switch name {
		case "no-audience":
			return creds.VaultStore{Address: srv.URL, Role: "godwit"}, nil
		case "bad-audience":
			return creds.VaultStore{Address: srv.URL, Role: "godwit", Audience: "../../kubernetes.io/serviceaccount/token"}, nil
		case "token":
			return creds.VaultStore{Address: srv.URL, TokenEnv: "VAULT_TOKEN_DEMO"}, nil
		case "empty-token":
			return creds.VaultStore{Address: srv.URL, TokenEnv: "VAULT_TOKEN_ABSENT"}, nil
		}

		return creds.VaultStore{}, errors.New("not found")
	}
	p := creds.Registry(creds.Keyring{}, lookup)["vault"]
	credstest.Conformance(t, p,
		map[string]string{"path": "secret/data/app", creds.StoreConfigKey: "token"}, "postgres://vault")

	cases := []struct {
		name   string
		p      creds.Provider
		config map[string]string
		want   string
	}{
		{"no store", p, map[string]string{"path": "secret/data/app"}, "names no credential store"},
		{"kubernetes auth naming no audience", p, map[string]string{"path": "x", creds.StoreConfigKey: "no-audience"}, "names no audience"},
		{"an audience that escapes the token directory", p, map[string]string{"path": "x", creds.StoreConfigKey: "bad-audience"}, "must be a plain name"},
		{"no lookup", creds.Registry(creds.Keyring{}, nil)["vault"], map[string]string{"path": "x", creds.StoreConfigKey: "k8s"}, "resolves no stores"},
		{"unknown store", p, map[string]string{"path": "x", creds.StoreConfigKey: "ghost"}, `credential store "ghost": not found`},
		{"token variable unset", p, map[string]string{"path": "x", creds.StoreConfigKey: "empty-token"}, "VAULT_TOKEN_ABSENT"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.p.DSN(context.Background(), tc.config)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}
