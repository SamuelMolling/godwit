package cli

import (
	"context"
	"os"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/gen/godwit/v1/godwitv1connect"
	"github.com/SamuelMolling/godwit/internal/report"
)

func newRunReportCmd() *cobra.Command {
	flags := &clientFlags{}
	rep := &reportFlags{}
	var command string
	cmd := &cobra.Command{
		Use:   "report <run-id>",
		Short: "Report what one run did to its target: what it applied, what that changed, and where it stopped",
		Long: "Reads the run and the plan it was bound to, and renders the outcome the pull-request comment carries.\n" +
			"With GODWIT_PUBLIC_URL set, the run and its plan link to their pages in the UI; without it the report\n" +
			"names them and links nothing.",
		Args: cobra.ExactArgs(1),
		RunE: flags.runE(func(cmd *cobra.Command, client godwitv1connect.GodwitServiceClient, args []string) error {
			write, err := rep.runWriter()
			if err != nil {
				return err
			}
			r, got, err := loadRunReport(cmd.Context(), client, args[0], command)
			if err != nil {
				return err
			}
			if flags.json {
				flags.print(cmd, got, "")

				return nil
			}
			write(cmd.OutOrStdout(), r)

			return nil
		}),
	}
	flags.register(cmd)
	rep.registerRun(cmd)
	cmd.Flags().StringVar(&command, "command", "", "name the report heading carries (default: the run's kind)")

	return cmd
}

func loadRunReport(ctx context.Context, client godwitv1connect.GodwitServiceClient, id, command string) (report.Run, *godwitv1.GetRunResponse, error) {
	got, err := client.GetRun(ctx, connect.NewRequest(&godwitv1.GetRunRequest{RunId: id}))
	if err != nil {
		return report.Run{}, nil, err
	}
	var plan *godwitv1.Plan
	if planID := got.Msg.Run.GetPlanId(); planID != "" {
		stored, err := client.GetPlan(ctx, connect.NewRequest(&godwitv1.GetPlanRequest{PlanId: planID}))
		if err != nil {
			return report.Run{}, nil, err
		}
		plan = stored.Msg.Plan
	}

	return report.RunFrom(got.Msg.Run, got.Msg.Applied, plan, command, os.Getenv("GODWIT_PUBLIC_URL")), got.Msg, nil
}
