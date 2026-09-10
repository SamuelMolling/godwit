package cli

import (
	"context"
	"net"
	"strings"
	"testing"

	"connectrpc.com/connect"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
)

func (s *stubService) RegisterCredentialStore(_ context.Context, req *connect.Request[godwitv1.RegisterCredentialStoreRequest]) (*connect.Response[godwitv1.RegisterCredentialStoreResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.store = req.Msg

	return connect.NewResponse(&godwitv1.RegisterCredentialStoreResponse{}), nil
}

func (s *stubService) ListCredentialStores(context.Context, *connect.Request[godwitv1.ListCredentialStoresRequest]) (*connect.Response[godwitv1.ListCredentialStoresResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return connect.NewResponse(&godwitv1.ListCredentialStoresResponse{Stores: s.stores}), nil
}

func TestCredentialStoreAdd(t *testing.T) {
	t.Parallel()
	stub := &stubService{}
	url := startStub(t, stub)

	code, out, errOut := runCLI("credential-store", "add", "production", "--server", url, "--token", "tok",
		"--vault-addr", "https://vault.production.example", "--vault-k8s-role", "godwit",
		"--vault-k8s-mount", "kubernetes-prod", "--vault-audience", "vault.production.example")
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
	if out != "credential store production: registered (https://vault.production.example)\n" {
		t.Fatalf("out = %q", out)
	}
	r := stub.store
	if r.Name != "production" || r.VaultAddr != "https://vault.production.example" ||
		r.VaultK8SRole != "godwit" || r.VaultK8SMount != "kubernetes-prod" ||
		r.VaultAudience != "vault.production.example" {
		t.Fatalf("request = %v", r)
	}

	if code, _, _ = runCLI("store", "add", "demo", "--server", url,
		"--vault-addr", "http://vault:8200", "--vault-token-env", "VAULT_TOKEN"); code != 0 {
		t.Fatalf("code = %d", code)
	}
	if stub.store.VaultTokenEnv != "VAULT_TOKEN" {
		t.Fatalf("request = %v", stub.store)
	}

	if code, _, errOut = runCLI("credential-store", "add", "x", "--server", url); code == 0 ||
		!strings.Contains(errOut, "vault-addr") {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
	if code, _, errOut = runCLI("credential-store", "add", "x", "--server", closedServer(t),
		"--vault-addr", "https://vault", "--vault-k8s-role", "godwit"); code != 1 ||
		!strings.Contains(errOut, "connection refused") {
		t.Fatalf("code = %d, stderr = %q", code, errOut)
	}
}

func closedServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	return "http://" + addr
}

func TestCredentialStores(t *testing.T) {
	t.Parallel()
	stub := &stubService{}
	url := startStub(t, stub)

	code, out, errOut := runCLI("credential-stores", "--server", url)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
	if !strings.Contains(out, "no credential store is registered") {
		t.Fatalf("out = %q", out)
	}

	stub.stores = []*godwitv1.CredentialStore{
		{
			Name: "production", VaultAddr: "https://vault.production.example", VaultK8SRole: "godwit",
			VaultK8SMount: "kubernetes", VaultAudience: "vault.production.example", Targets: 7,
		},
		{Name: "demo", VaultAddr: "http://vault:8200", VaultTokenEnv: "VAULT_TOKEN"},
	}
	code, out, errOut = runCLI("stores", "--server", url)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
	want := "NAME        VAULT                             AUTH                                                          TARGETS\n" +
		"production  https://vault.production.example  kubernetes kubernetes as godwit for vault.production.example  7\n" +
		"demo        http://vault:8200                 token from VAULT_TOKEN                                        0\n"
	if out != want {
		t.Fatalf("out = %q\nwant %q", out, want)
	}

	code, out, _ = runCLI("credential-stores", "--server", url, "--json")
	if code != 0 || len(decodeJSON(t, out)["stores"].([]any)) != 2 {
		t.Fatalf("code = %d, out = %q", code, out)
	}
	if code, _, errOut = runCLI("credential-stores", "--server", closedServer(t)); code != 1 ||
		!strings.Contains(errOut, "connection refused") {
		t.Fatalf("code = %d, stderr = %q", code, errOut)
	}
}
