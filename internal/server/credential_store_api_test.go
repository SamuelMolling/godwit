package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/internal/controlplane"
)

func otherVault(t *testing.T, role, mount, dsn string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/auth/"+mount+"/login", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["jwt"] != "sa-jwt" || body["role"] != role {
			http.Error(w, `{"errors":["permission denied"]}`, http.StatusForbidden)

			return
		}
		_, _ = w.Write([]byte(`{"auth":{"client_token":"store-token"}}`))
	})
	mux.HandleFunc("GET /v1/secret/data/app", func(w http.ResponseWriter, r *http.Request) {
		if tok := r.Header.Get("X-Vault-Token"); tok != "store-token" && tok != "root" {
			http.Error(w, `{"errors":["permission denied"]}`, http.StatusForbidden)

			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"data": map[string]string{"dsn": dsn}}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return srv
}

func TestCredentialStoreEndToEnd(t *testing.T) {
	targetDSN := newDatabase(t, "cs")
	vault := otherVault(t, "godwit", "kubernetes-prod", targetDSN)
	t.Setenv("VAULT_TOKEN_PRODUCTION", "root")
	ctx := context.Background()
	client := newClient(startService(t, newDatabase(t, "st"), "r1", nil), "")

	if _, err := client.RegisterTarget(ctx, connect.NewRequest(&godwitv1.RegisterTargetRequest{
		Name: "app", Provider: "vault", VaultPath: "secret/data/app", CredentialStore: "production",
	})); connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("an unregistered store must be refused, not stored: %v", err)
	}
	if _, err := client.RegisterCredentialStore(ctx, connect.NewRequest(&godwitv1.RegisterCredentialStoreRequest{
		Name: "production", VaultAddr: vault.URL, VaultTokenEnv: "VAULT_TOKEN_PRODUCTION",
	})); err != nil {
		t.Fatal(err)
	}
	if _, err := client.RegisterTarget(ctx, connect.NewRequest(&godwitv1.RegisterTargetRequest{
		Name: "app", Provider: "vault", VaultPath: "secret/data/app", CredentialStore: "production",
	})); err != nil {
		t.Fatal(err)
	}

	runToSuccess(t, client, migrationFiles(), nil)
	if !columnExists(t, targetDSN, "id") {
		t.Fatal("the run did not reach the database the store's Vault named")
	}

	stores, err := client.ListCredentialStores(ctx, connect.NewRequest(&godwitv1.ListCredentialStoresRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(stores.Msg.Stores) != 1 {
		t.Fatalf("stores = %+v", stores.Msg.Stores)
	}
	if s := stores.Msg.Stores[0]; s.Name != "production" || s.VaultAddr != vault.URL ||
		s.VaultTokenEnv != "VAULT_TOKEN_PRODUCTION" || s.Targets != 1 {
		t.Fatalf("store = %+v", s)
	}
	targets, err := client.ListTargets(ctx, connect.NewRequest(&godwitv1.ListTargetsRequest{}))
	if err != nil || targets.Msg.Targets[0].CredentialStore != "production" {
		t.Fatalf("targets = %+v, err = %v", targets.Msg.Targets, err)
	}
}

func TestVaultTargetWithoutAStoreRefuses(t *testing.T) {
	ctx := context.Background()
	storeDSN := newDatabase(t, "st")
	addr := startService(t, storeDSN, "r1", nil)
	client := newClient(addr, "")
	if _, err := client.RegisterCredentialStore(ctx, connect.NewRequest(&godwitv1.RegisterCredentialStoreRequest{
		Name: "elsewhere", VaultAddr: "https://vault.elsewhere.invalid", VaultK8SRole: "godwit",
	})); err != nil {
		t.Fatal(err)
	}
	if _, err := client.RegisterTarget(ctx, connect.NewRequest(&godwitv1.RegisterTargetRequest{
		Name: "app", Provider: "vault", CredentialStore: "elsewhere", VaultPath: "secret/data/app",
	})); err != nil {
		t.Fatal(err)
	}
	// A row that predates the column, which RegisterTarget can no longer write.
	conn, err := pgx.Connect(ctx, storeDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, `UPDATE cp_targets SET credential_store = NULL WHERE name = 'app'`); err != nil {
		t.Fatal(err)
	}

	_, err = client.CreateRun(ctx, connect.NewRequest(&godwitv1.CreateRunRequest{
		Target: "app", Files: migrationFiles(), SkipValidation: true,
	}))
	if err == nil {
		t.Fatal("a target with no store must not reach a database")
	}
	for _, want := range []string{"target app", "names no credential store", "godwit credential-store add"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal has to name the target and the command that fixes it: %v", err)
		}
	}
}

func TestProvidersThatReadNoVaultAreUntouched(t *testing.T) {
	targetDSN := newDatabase(t, "nv")
	secret := filepath.Join(t.TempDir(), "dsn")
	if err := os.WriteFile(secret, []byte(targetDSN), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	client := newClient(startService(t, newDatabase(t, "st"), "r1", nil), "")
	if _, err := client.RegisterTarget(ctx, connect.NewRequest(&godwitv1.RegisterTargetRequest{
		Name: "app", Provider: "kubernetes", SecretPath: secret,
	})); err != nil {
		t.Fatal(err)
	}
	if _, err := client.RegisterTarget(ctx, connect.NewRequest(&godwitv1.RegisterTargetRequest{
		Name: "sealed", Provider: "static", Dsn: targetDSN,
	})); err != nil {
		t.Fatal(err)
	}
	runToSuccess(t, client, migrationFiles(), nil)
	if !columnExists(t, targetDSN, "id") {
		t.Fatal("a kubernetes target must still resolve with no store in sight")
	}
}

func TestCredentialStoreRefusals(t *testing.T) {
	ctx := context.Background()
	client := newClient(startServiceCfg(t, Config{
		Listen: "127.0.0.1:0", StoreDSN: newDatabase(t, "st"), Keys: testKeys, Holder: "r1",
		Scheduler: controlplane.Config{Interval: 50 * time.Millisecond}, Log: testLog,
	}), "")

	cases := []struct {
		name string
		req  *godwitv1.RegisterCredentialStoreRequest
		want string
	}{
		{"no name", &godwitv1.RegisterCredentialStoreRequest{}, "name is required"},
		{"no address", &godwitv1.RegisterCredentialStoreRequest{Name: "s"}, "vault_addr is required"},
		{
			"not a url", &godwitv1.RegisterCredentialStoreRequest{Name: "s", VaultAddr: "://x"},
			"is not a URL",
		},
		{
			"no host", &godwitv1.RegisterCredentialStoreRequest{Name: "s", VaultAddr: "file:///etc/passwd"},
			"must be an http or https URL",
		},
		{
			"no auth",
			&godwitv1.RegisterCredentialStoreRequest{Name: "s", VaultAddr: "https://vault.production.example"},
			"needs vault_k8s_role (Kubernetes auth) or vault_token_env",
		},
		{
			"two ways to authenticate",
			&godwitv1.RegisterCredentialStoreRequest{
				Name: "s", VaultAddr: "https://vault.production.example",
				VaultK8SRole: "godwit", VaultTokenEnv: "VAULT_TOKEN",
			},
			"give one",
		},
		{
			"a variable that is not a vault token",
			&godwitv1.RegisterCredentialStoreRequest{
				Name: "s", VaultAddr: "https://vault.production.example", VaultTokenEnv: "GODWIT_STORE_DSN",
			},
			"must name a variable beginning with VAULT_TOKEN",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := client.RegisterCredentialStore(ctx, connect.NewRequest(tc.req))
			if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}

	if _, err := client.RegisterCredentialStore(ctx, connect.NewRequest(&godwitv1.RegisterCredentialStoreRequest{
		Name: "production", VaultAddr: "https://vault.production.example", VaultK8SRole: "godwit",
	})); err != nil {
		t.Fatalf("a store authenticating with Kubernetes auth: %v", err)
	}

	if _, err := client.RegisterTarget(ctx, connect.NewRequest(&godwitv1.RegisterTargetRequest{
		Name: "app", Provider: "kubernetes", SecretPath: "/secrets/app", CredentialStore: "production",
	})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("a kubernetes target reads no Vault: %v", err)
	}
}
