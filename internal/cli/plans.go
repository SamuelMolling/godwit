package cli

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/gen/godwit/v1/godwitv1connect"
	"github.com/SamuelMolling/godwit/internal/report"
)

func newPlansCmd() *cobra.Command {
	flags := &clientFlags{}
	req := &godwitv1.ListPlansRequest{}
	cmd := &cobra.Command{
		Use:   "plans",
		Short: "List a target's stored plans, newest first",
		Args:  cobra.NoArgs,
		RunE: flags.runE(func(cmd *cobra.Command, client godwitv1connect.GodwitServiceClient, _ []string) error {
			resp, err := client.ListPlans(cmd.Context(), connect.NewRequest(req))
			if err != nil {
				return err
			}
			flags.print(cmd, resp.Msg, plansTable(resp.Msg.Plans))

			return nil
		}),
	}
	flags.register(cmd)
	cmd.Flags().StringVar(&req.Target, "target", "", "target name")
	cmd.Flags().Int32Var(&req.Limit, "limit", 0, "most recent plans to list (100 when zero)")
	configKeys(cmd, "target")

	return cmd
}

func plansTable(plans []*godwitv1.Plan) string {
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tSTATE\tROLLOUT\tPENDING\tVALIDATED\tBY\tSOURCE\tCREATED\tRUN")
	for _, p := range plans {
		pending := 0
		for _, m := range p.Migrations {
			if !m.Applied && !m.Withheld {
				pending++
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%t\t%s\t%s\t%s\t%s\n", p.Id, planStateLabel(p), p.Rollout, pending, p.Validated,
			p.CreatedBy, p.Source, report.Stamp(p.CreatedAt), p.RunId)
	}
	_ = w.Flush()

	return strings.TrimSuffix(b.String(), "\n")
}

func planStateLabel(p *godwitv1.Plan) string {
	if p.SupersededBy != "" {
		return p.State + " by " + p.SupersededBy
	}

	return p.State
}

func newPlanShowCmd() *cobra.Command {
	flags := &clientFlags{}
	rep := &reportFlags{}
	cmd := &cobra.Command{
		Use:   "show <plan-id>",
		Short: "Show a stored plan: statements, hazards, observation, drift, state and the run that applied it",
		Args:  cobra.ExactArgs(1),
		RunE: flags.runE(func(cmd *cobra.Command, client godwitv1connect.GodwitServiceClient, args []string) error {
			write, err := rep.writer()
			if err != nil {
				return err
			}
			resp, err := client.GetPlan(cmd.Context(), connect.NewRequest(&godwitv1.GetPlanRequest{PlanId: args[0]}))
			if err != nil {
				return err
			}
			if flags.json {
				flags.print(cmd, resp.Msg, "")

				return nil
			}
			write(cmd.OutOrStdout(), report.PlanFromStored(resp.Msg.Plan))

			return nil
		}),
	}
	flags.register(cmd)
	rep.register(cmd, "")

	return cmd
}
