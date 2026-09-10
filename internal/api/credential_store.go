package api

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"connectrpc.com/connect"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/internal/controlplane"
	"github.com/SamuelMolling/godwit/internal/creds"
)

// RegisterCredentialStore stores a Vault targets may read their secrets from.
func (s *Server) RegisterCredentialStore(ctx context.Context, req *connect.Request[godwitv1.RegisterCredentialStoreRequest]) (*connect.Response[godwitv1.RegisterCredentialStoreResponse], error) {
	m := req.Msg
	if m.Name == "" {
		return nil, invalid("name is required")
	}
	if err := checkVaultAddr(m.VaultAddr); err != nil {
		return nil, invalid(err.Error())
	}
	if err := checkVaultAuth(m.VaultK8SRole, m.VaultTokenEnv, m.VaultAudience); err != nil {
		return nil, invalid(err.Error())
	}
	store := controlplane.CredentialStore{
		Name: m.Name, Address: m.VaultAddr, Role: m.VaultK8SRole, TokenEnv: m.VaultTokenEnv,
	}
	if m.VaultK8SRole != "" {
		store.Mount = cmp.Or(m.VaultK8SMount, "kubernetes")
		store.Audience = m.VaultAudience
	}
	if err := s.store.RegisterCredentialStore(ctx, store); err != nil {
		return nil, rpcErr(err)
	}
	s.Log.Info("credential store registered", "store", store.Name, "vault_addr", store.Address,
		"vault_k8s_role", store.Role, "vault_k8s_mount", store.Mount, "vault_audience", store.Audience,
		"vault_token_env", store.TokenEnv)
	s.audit(ctx, controlplane.AuditCredentialStore, "", "",
		fmt.Sprintf("store=%s vault_addr=%s vault_k8s_role=%s vault_k8s_mount=%s vault_audience=%s vault_token_env=%s",
			store.Name, store.Address, store.Role, store.Mount, store.Audience, store.TokenEnv))

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
			VaultK8SMount: st.Mount, VaultAudience: st.Audience, VaultTokenEnv: st.TokenEnv,
			Targets: int32(st.Targets),
		})
	}

	return connect.NewResponse(out), nil
}

// Without the prefix an admin could name GODWIT_STORE_DSN and have godwit post it to a host of theirs.
const tokenEnvPrefix = "VAULT_TOKEN"

func checkVaultAuth(role, tokenEnv, audience string) error {
	switch {
	case role == "" && tokenEnv == "":
		return errors.New("a credential store needs vault_k8s_role (Kubernetes auth) or vault_token_env " +
			"(an environment variable of the service holding a token for this Vault)")
	case role != "" && tokenEnv != "":
		return errors.New("vault_k8s_role and vault_token_env are two ways to authenticate at one Vault; give one")
	case tokenEnv != "" && !strings.HasPrefix(tokenEnv, tokenEnvPrefix):
		return fmt.Errorf("vault_token_env %q must name a variable beginning with %s", tokenEnv, tokenEnvPrefix)
	case tokenEnv != "" && audience != "":
		return errors.New("vault_audience belongs to Kubernetes auth; a store reading vault_token_env presents no token of its own")
	case role != "" && audience == "":
		return errors.New("vault_audience is required with vault_k8s_role: it names what this Vault checks the " +
			"token was minted for, and a token minted for nothing in particular is one any other Vault also accepts")
	}
	if role == "" {
		return nil
	}

	return creds.CheckAudience(audience)
}

func checkVaultAddr(addr string) error {
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

	return nil
}
