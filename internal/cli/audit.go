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

func newAuditCmd() *cobra.Command {
	flags := &clientFlags{}
	req := &godwitv1.ListAuditRequest{}
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "List who did what on the service, newest first",
		Args:  cobra.NoArgs,
		RunE: flags.runE(func(cmd *cobra.Command, client godwitv1connect.GodwitServiceClient, _ []string) error {
			resp, err := client.ListAudit(cmd.Context(), connect.NewRequest(req))
			if err != nil {
				return err
			}
			flags.print(cmd, resp.Msg, auditTable(resp.Msg.Entries))

			return nil
		}),
	}
	flags.register(cmd)
	cmd.Flags().StringVar(&req.Target, "target", "", "only actions on this target")
	cmd.Flags().StringVar(&req.RunId, "run", "", "only actions on this run")
	cmd.Flags().Int32Var(&req.Limit, "limit", 0, "newest entries to show (default 100)")

	return cmd
}

func auditTable(entries []*godwitv1.AuditEntry) string {
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "AT\tACTOR\tACTION\tTARGET\tRUN\tDETAIL")
	for _, e := range entries {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", stamp(e.At), e.Actor, e.Action, e.Target, e.RunId, e.Detail)
	}
	_ = w.Flush()

	return strings.TrimSuffix(b.String(), "\n")
}
