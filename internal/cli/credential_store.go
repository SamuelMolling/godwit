package cli

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/gen/godwit/v1/godwitv1connect"
)

func newCredentialStoreCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "credential-store",
		Aliases: []string{"store"},
		Short:   "Manage the Vaults targets read their credentials from",
	}
	cmd.AddCommand(newCredentialStoreAddCmd())

	return cmd
}

func newCredentialStoreAddCmd() *cobra.Command {
	flags := &clientFlags{}
	req := &godwitv1.RegisterCredentialStoreRequest{}
	cmd := &cobra.Command{
		Use:   "add <name>",
		Short: "Register a Vault a target can name with `target add --credential-store`",
		Long: "Every vault target names a store, and the store is the only thing that says which Vault its\n" +
			"credentials are read from. With --vault-k8s-role the service logs in at that Vault with its own\n" +
			"ServiceAccount token, so that Vault needs a Kubernetes auth mount trusting this cluster and a role\n" +
			"bound to it; --vault-token-env names an environment variable of the service holding a token instead.",
		Args: cobra.ExactArgs(1),
		RunE: flags.runE(func(cmd *cobra.Command, client godwitv1connect.GodwitServiceClient, args []string) error {
			req.Name = args[0]
			resp, err := client.RegisterCredentialStore(cmd.Context(), connect.NewRequest(req))
			if err != nil {
				return err
			}
			flags.print(cmd, resp.Msg, fmt.Sprintf("credential store %s: registered (%s)", req.Name, req.VaultAddr))

			return nil
		}),
	}
	flags.register(cmd)
	cmd.Flags().StringVar(&req.VaultAddr, "vault-addr", "", "Vault base URL, e.g. https://vault.production.example")
	cmd.Flags().StringVar(&req.VaultK8SRole, "vault-k8s-role", "", "Kubernetes auth role godwit logs in as at that Vault")
	cmd.Flags().StringVar(&req.VaultK8SMount, "vault-k8s-mount", "", "Kubernetes auth mount at that Vault (default kubernetes)")
	cmd.Flags().StringVar(&req.VaultK8SJwt, "vault-k8s-jwt", "",
		"ServiceAccount token file this store's Kubernetes login presents (default the projected token)")
	cmd.Flags().StringVar(&req.VaultTokenEnv, "vault-token-env", "",
		"instead of Kubernetes auth: an environment variable of the service holding a token for this Vault, named VAULT_TOKEN*")
	_ = cmd.MarkFlagRequired("vault-addr")

	return cmd
}

func newCredentialStoresCmd() *cobra.Command {
	flags := &clientFlags{}
	cmd := &cobra.Command{
		Use:     "credential-stores",
		Aliases: []string{"stores"},
		Short:   "List the registered credential stores and how many targets read from each",
		Args:    cobra.NoArgs,
		RunE: flags.runE(func(cmd *cobra.Command, client godwitv1connect.GodwitServiceClient, _ []string) error {
			resp, err := client.ListCredentialStores(cmd.Context(), connect.NewRequest(&godwitv1.ListCredentialStoresRequest{}))
			if err != nil {
				return err
			}
			flags.print(cmd, resp.Msg, credentialStoresTable(resp.Msg.Stores))

			return nil
		}),
	}
	flags.register(cmd)

	return cmd
}

func credentialStoresTable(stores []*godwitv1.CredentialStore) string {
	if len(stores) == 0 {
		return "no credential store is registered, so no vault target can resolve; " +
			"`godwit credential-store add` registers one"
	}
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tVAULT\tAUTH\tTARGETS")
	for _, s := range stores {
		auth := "token from " + s.VaultTokenEnv
		if s.VaultK8SRole != "" {
			auth = "kubernetes " + s.VaultK8SMount + " as " + s.VaultK8SRole
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\n", s.Name, s.VaultAddr, auth, s.Targets)
	}
	_ = w.Flush()

	return strings.TrimSuffix(b.String(), "\n")
}
