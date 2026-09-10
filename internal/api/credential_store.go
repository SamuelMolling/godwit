package api

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"connectrpc.com/connect"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/internal/controlplane"
)

// RegisterCredentialStore stores a Vault targets may read their secrets from.
func (s *Server) RegisterCredentialStore(ctx context.Context, req *connect.Request[godwitv1.RegisterCredentialStoreRequest]) (*connect.Response[godwitv1.RegisterCredentialStoreResponse], error) {
	m := req.Msg
	if m.Name == "" {
		return nil, invalid("name is required")
	}
	if err := checkVaultAddr(m.VaultAddr, s.VaultHosts); err != nil {
		return nil, invalid(err.Error())
	}
	if err := checkVaultAuth(m.VaultK8SRole, m.VaultTokenEnv); err != nil {
		return nil, invalid(err.Error())
	}
	store := controlplane.CredentialStore{
		Name: m.Name, Address: m.VaultAddr, Role: m.VaultK8SRole, TokenEnv: m.VaultTokenEnv,
	}
	if m.VaultK8SRole != "" {
		store.Mount = cmp.Or(m.VaultK8SMount, "kubernetes")
		store.JWTPath = m.VaultK8SJwt
	}
	if err := s.store.RegisterCredentialStore(ctx, store); err != nil {
		return nil, rpcErr(err)
	}
	s.Log.Info("credential store registered", "store", store.Name, "vault_addr", store.Address,
		"vault_k8s_role", store.Role, "vault_k8s_mount", store.Mount, "vault_k8s_jwt", store.JWTPath,
		"vault_token_env", store.TokenEnv)
	s.audit(ctx, controlplane.AuditCredentialStore, "", "",
		fmt.Sprintf("store=%s vault_addr=%s vault_k8s_role=%s vault_k8s_mount=%s vault_token_env=%s",
			store.Name, store.Address, store.Role, store.Mount, store.TokenEnv))

	return connect.NewResponse(&godwitv1.RegisterCredentialStoreResponse{}), nil
}

// ListCredentialStores returns every registered store with the targets reading from it.
func (s *Server) ListCredentialStores(ctx context.Context, _ *connect.Request[godwitv1.ListCredentialStoresRequest]) (*connect.Response[godwitv1.ListCredentialStoresResponse], error) {
	stores, err := s.store.ListCredentialStores(ctx)
	if err != nil {
		return nil, rpcErr(err)
	}
	out := &godwitv1.ListCredentialStoresResponse{Stores: make([]*godwitv1.CredentialStore, 0, len(stores))}
	for _, st := range stores {
		out.Stores = append(out.Stores, &godwitv1.CredentialStore{
			Name: st.Name, VaultAddr: st.Address, VaultK8SRole: st.Role,
			VaultK8SMount: st.Mount, VaultK8SJwt: st.JWTPath, VaultTokenEnv: st.TokenEnv,
			Targets: int32(st.Targets),
		})
	}

	return connect.NewResponse(out), nil
}

// Without the prefix an admin could name GODWIT_STORE_DSN and have godwit post it to a host of theirs.
const tokenEnvPrefix = "VAULT_TOKEN"

func checkVaultAuth(role, tokenEnv string) error {
	switch {
	case role == "" && tokenEnv == "":
		return errors.New("a credential store needs vault_k8s_role (Kubernetes auth) or vault_token_env " +
			"(an environment variable of the service holding a token for this Vault)")
	case role != "" && tokenEnv != "":
		return errors.New("vault_k8s_role and vault_token_env are two ways to authenticate at one Vault; give one")
	case tokenEnv != "" && !strings.HasPrefix(tokenEnv, tokenEnvPrefix):
		return fmt.Errorf("vault_token_env %q must name a variable beginning with %s", tokenEnv, tokenEnvPrefix)
	}

	return nil
}

func checkVaultAddr(addr string, hosts []string) error {
	if addr == "" {
		return errors.New("vault_addr is required")
	}
	u, err := url.Parse(addr)
	if err != nil {
		return fmt.Errorf("vault_addr %q is not a URL: %w", addr, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("vault_addr %q must be an http or https URL with a host", addr)
	}
	if len(hosts) > 0 && !slices.ContainsFunc(hosts, func(h string) bool { return strings.EqualFold(h, u.Host) }) {
		return fmt.Errorf("vault_addr %q: this service accepts credential stores at %s only",
			addr, strings.Join(hosts, ", "))
	}

	return nil
}
