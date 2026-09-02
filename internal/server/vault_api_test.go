package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
)

func TestVaultTargetEndToEnd(t *testing.T) {
	targetDSN := newDatabase(t, "tg")
	vault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/secret/data/app" || r.Header.Get("X-Vault-Token") != "root" {
			http.Error(w, `{"errors":[]}`, http.StatusNotFound)

			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"data": map[string]string{"dsn": targetDSN}}})
	}))
	t.Cleanup(vault.Close)
	t.Setenv("VAULT_ADDR", vault.URL)
	t.Setenv("VAULT_TOKEN", "root")

	ctx := context.Background()
	client := newClient(startService(t, newDatabase(t, "st"), "r1", nil), "")
	for _, req := range []*godwitv1.RegisterTargetRequest{
		{Name: "app", Provider: "vault", VaultPath: "secret/data/app"},
		{Name: "templated", Provider: "vault", VaultPath: "database/creds/app", VaultTemplate: "postgres://{{username}}:{{password}}@db/app"},
	} {
		if _, err := client.RegisterTarget(ctx, connect.NewRequest(req)); err != nil {
			t.Fatal(err)
		}
	}
	runToSuccess(t, client, migrationFiles(), nil)
	if !columnExists(t, targetDSN, "id") {
		t.Fatal("migration did not reach the vault-resolved target")
	}
}
