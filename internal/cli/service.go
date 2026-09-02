package cli

import (
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/gen/godwit/v1/godwitv1connect"
	"github.com/SamuelMolling/godwit/internal/engine"
)

func newTargetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "target",
		Short: "Manage targets on the service",
	}
	cmd.AddCommand(newTargetAddCmd())

	return cmd
}

func newTargetAddCmd() *cobra.Command {
	flags := &clientFlags{}
	req := &godwitv1.RegisterTargetRequest{}
	cmd := &cobra.Command{
		Use:   "add <name>",
		Short: "Register a target with its credential provider",
		Args:  cobra.ExactArgs(1),
		RunE: flags.runE(func(cmd *cobra.Command, client godwitv1connect.GodwitServiceClient, args []string) error {
			req.Name = args[0]
			resp, err := client.RegisterTarget(cmd.Context(), connect.NewRequest(req))
			if err != nil {
				return err
			}
			flags.print(cmd, resp.Msg, fmt.Sprintf("target %s: registered (%s)", req.Name, req.Provider))

			return nil
		}),
	}
	flags.register(cmd)
	cmd.Flags().StringVar(&req.Provider, "provider", "", "credential provider: static, kubernetes or vault")
	cmd.Flags().StringVar(&req.Dsn, "dsn", "", "target DSN (static provider)")
	cmd.Flags().StringVar(&req.SecretPath, "secret-path", "", "mounted secret file (kubernetes provider)")
	cmd.Flags().StringVar(&req.VaultPath, "vault-path", "", "Vault secret path under /v1 (vault provider)")
	cmd.Flags().StringVar(&req.VaultTemplate, "vault-template", "", "DSN template over the Vault secret's fields")
	_ = cmd.MarkFlagRequired("provider")

	return cmd
}

func newMigrateCmd() *cobra.Command {
	flags := &clientFlags{}
	req := &godwitv1.CreateRunRequest{}
	var dir string
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Send a migration directory to the service and watch the run",
		Args:  cobra.NoArgs,
		RunE: flags.runE(func(cmd *cobra.Command, client godwitv1connect.GodwitServiceClient, _ []string) error {
			files, err := migrationFiles(dir)
			if err != nil {
				return err
			}
			req.Files = files
			created, err := client.CreateRun(cmd.Context(), connect.NewRequest(req))
			if err != nil {
				return err
			}

			return flags.watch(cmd, client, created.Msg.RunId)
		}),
	}
	flags.register(cmd)
	cmd.Flags().StringVar(&req.Target, "target", "", "target name")
	cmd.Flags().StringVar(&dir, "dir", "migrations", "migration directory")
	cmd.Flags().StringVar(&req.Rollout, "rollout", "direct", "rollout policy: direct or expand-contract")
	cmd.Flags().StringSliceVar(&req.AcknowledgeHazards, "ack", nil, "hazard codes to acknowledge")
	cmd.Flags().BoolVar(&req.SkipValidation, "skip-validation", false, "skip the scratch-database validation")
	_ = cmd.MarkFlagRequired("target")

	return cmd
}

func migrationFiles(dir string) ([]*godwitv1.MigrationFile, error) {
	migs, err := engine.LoadDir(dir)
	if err != nil {
		return nil, err
	}
	files := make([]*godwitv1.MigrationFile, 0, 2*len(migs))
	for _, m := range migs {
		files = append(files,
			&godwitv1.MigrationFile{Name: fmt.Sprintf("%014d_%s.up.sql", m.Version, m.Name), Body: m.UpSQL},
			&godwitv1.MigrationFile{Name: fmt.Sprintf("%014d_%s.down.sql", m.Version, m.Name), Body: m.DownSQL},
		)
	}

	return files, nil
}

func newRevertCmd() *cobra.Command {
	flags := &clientFlags{}
	req := &godwitv1.RevertRunRequest{}
	cmd := &cobra.Command{
		Use:   "revert <run-id>",
		Short: "Apply the down side of a run and watch the revert",
		Args:  cobra.ExactArgs(1),
		RunE: flags.runE(func(cmd *cobra.Command, client godwitv1connect.GodwitServiceClient, args []string) error {
			req.RunId = args[0]
			resp, err := client.RevertRun(cmd.Context(), connect.NewRequest(req))
			if err != nil {
				return err
			}

			return flags.watch(cmd, client, resp.Msg.RunId)
		}),
	}
	flags.register(cmd)
	cmd.Flags().StringSliceVar(&req.AcknowledgeHazards, "ack", nil, "hazard codes to acknowledge")
	cmd.Flags().BoolVar(&req.SkipValidation, "skip-validation", false, "skip the scratch-database validation")

	return cmd
}

func (f *clientFlags) watch(cmd *cobra.Command, client godwitv1connect.GodwitServiceClient, id string) error {
	stream, err := client.WatchRun(cmd.Context(), connect.NewRequest(&godwitv1.WatchRunRequest{RunId: id}))
	if err != nil {
		return err
	}
	var last *godwitv1.Run
	for stream.Receive() {
		r := stream.Msg().Run
		if last != nil && last.State == r.State && last.Attempts == r.Attempts {
			continue
		}
		last = r
		f.print(cmd, stream.Msg(), runLine(r))
	}
	if err := stream.Err(); err != nil {
		return err
	}
	switch last.GetState() {
	case godwitv1.RunState_RUN_STATE_FAILED, godwitv1.RunState_RUN_STATE_NEEDS_ATTENTION:
		return fmt.Errorf("run %s %s: %s", id, stateName(last.State), last.Error)
	default:
		return nil
	}
}

func stateName(s godwitv1.RunState) string {
	return strings.ToLower(strings.TrimPrefix(s.String(), "RUN_STATE_"))
}

func runLine(r *godwitv1.Run) string {
	line := fmt.Sprintf("run %s: %s", r.Id, stateName(r.State))
	if r.Attempts > 0 {
		line += fmt.Sprintf(" (attempt %d)", r.Attempts)
	}
	if r.Error != "" {
		line += ": " + r.Error
	}

	return line
}

func stamp(ts *timestamppb.Timestamp) string {
	if ts == nil {
		return ""
	}

	return ts.AsTime().UTC().Format(time.RFC3339)
}

func addRunSubcommands(run *cobra.Command) {
	run.AddCommand(newRunGetCmd(), newRunWatchCmd(), newRunResumeCmd(), newRunConfirmCmd())
}

func newRunGetCmd() *cobra.Command {
	flags := &clientFlags{}
	cmd := &cobra.Command{
		Use:   "get <run-id>",
		Short: "Show one run",
		Args:  cobra.ExactArgs(1),
		RunE: flags.runE(func(cmd *cobra.Command, client godwitv1connect.GodwitServiceClient, args []string) error {
			resp, err := client.GetRun(cmd.Context(), connect.NewRequest(&godwitv1.GetRunRequest{RunId: args[0]}))
			if err != nil {
				return err
			}
			r := resp.Msg.Run
			flags.print(cmd, resp.Msg, fmt.Sprintf("%s\n  target: %s\n  rollout: %s\n  phase: %s\n  reverts: %s\n  created: %s\n  finished: %s",
				runLine(r), r.Target, r.Rollout, r.Phase, r.Reverts, stamp(r.CreatedAt), stamp(r.FinishedAt)))

			return nil
		}),
	}
	flags.register(cmd)

	return cmd
}

func newRunWatchCmd() *cobra.Command {
	flags := &clientFlags{}
	cmd := &cobra.Command{
		Use:   "watch <run-id>",
		Short: "Stream a run's state changes until it settles",
		Args:  cobra.ExactArgs(1),
		RunE: flags.runE(func(cmd *cobra.Command, client godwitv1connect.GodwitServiceClient, args []string) error {
			return flags.watch(cmd, client, args[0])
		}),
	}
	flags.register(cmd)

	return cmd
}

func newRunResumeCmd() *cobra.Command {
	flags := &clientFlags{}
	cmd := &cobra.Command{
		Use:   "resume <run-id>",
		Short: "Requeue a failed or parked run",
		Args:  cobra.ExactArgs(1),
		RunE: flags.runE(func(cmd *cobra.Command, client godwitv1connect.GodwitServiceClient, args []string) error {
			resp, err := client.ResumeRun(cmd.Context(), connect.NewRequest(&godwitv1.ResumeRunRequest{RunId: args[0]}))
			if err != nil {
				return err
			}
			flags.print(cmd, resp.Msg, fmt.Sprintf("run %s: resumed", args[0]))

			return nil
		}),
	}
	flags.register(cmd)

	return cmd
}

func newRunConfirmCmd() *cobra.Command {
	flags := &clientFlags{}
	cmd := &cobra.Command{
		Use:   "confirm <run-id>",
		Short: "Release the contract phase of an expand-contract run",
		Args:  cobra.ExactArgs(1),
		RunE: flags.runE(func(cmd *cobra.Command, client godwitv1connect.GodwitServiceClient, args []string) error {
			resp, err := client.ConfirmRollout(cmd.Context(), connect.NewRequest(&godwitv1.ConfirmRolloutRequest{RunId: args[0]}))
			if err != nil {
				return err
			}
			flags.print(cmd, resp.Msg, fmt.Sprintf("run %s: contract confirmed", args[0]))

			return nil
		}),
	}
	flags.register(cmd)

	return cmd
}

func newRunsCmd() *cobra.Command {
	flags := &clientFlags{}
	req := &godwitv1.ListRunsRequest{}
	cmd := &cobra.Command{
		Use:   "runs",
		Short: "List runs",
		Args:  cobra.NoArgs,
		RunE: flags.runE(func(cmd *cobra.Command, client godwitv1connect.GodwitServiceClient, _ []string) error {
			resp, err := client.ListRuns(cmd.Context(), connect.NewRequest(req))
			if err != nil {
				return err
			}
			flags.print(cmd, resp.Msg, runsTable(resp.Msg.Runs))

			return nil
		}),
	}
	flags.register(cmd)
	cmd.Flags().StringVar(&req.Target, "target", "", "only runs on this target")

	return cmd
}

func runsTable(runs []*godwitv1.Run) string {
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tTARGET\tSTATE\tROLLOUT\tPHASE\tCREATED")
	for _, r := range runs {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", r.Id, r.Target, stateName(r.State), r.Rollout, r.Phase, stamp(r.CreatedAt))
	}
	_ = w.Flush()

	return strings.TrimSuffix(b.String(), "\n")
}

func newDriftCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "drift",
		Short: "Check or accept schema drift on a target",
	}
	cmd.AddCommand(newDriftCheckCmd(), newDriftAcceptCmd())

	return cmd
}

func newDriftCheckCmd() *cobra.Command {
	flags := &clientFlags{}
	cmd := &cobra.Command{
		Use:   "check <target>",
		Short: "Diff the live schema against the last known fingerprint",
		Args:  cobra.ExactArgs(1),
		RunE: flags.runE(func(cmd *cobra.Command, client godwitv1connect.GodwitServiceClient, args []string) error {
			resp, err := client.CheckDrift(cmd.Context(), connect.NewRequest(&godwitv1.CheckDriftRequest{Target: args[0]}))
			if err != nil {
				return err
			}
			human := fmt.Sprintf("target %s: no drift", args[0])
			if resp.Msg.Drifted {
				human = fmt.Sprintf("target %s: drifted\n%s", args[0], resp.Msg.Diff)
			}
			flags.print(cmd, resp.Msg, human)

			return nil
		}),
	}
	flags.register(cmd)

	return cmd
}

func newDriftAcceptCmd() *cobra.Command {
	flags := &clientFlags{}
	cmd := &cobra.Command{
		Use:   "accept <target>",
		Short: "Bless the live schema as the new baseline",
		Args:  cobra.ExactArgs(1),
		RunE: flags.runE(func(cmd *cobra.Command, client godwitv1connect.GodwitServiceClient, args []string) error {
			resp, err := client.AcceptBaseline(cmd.Context(), connect.NewRequest(&godwitv1.AcceptBaselineRequest{Target: args[0]}))
			if err != nil {
				return err
			}
			flags.print(cmd, resp.Msg, fmt.Sprintf("target %s: baseline accepted", args[0]))

			return nil
		}),
	}
	flags.register(cmd)

	return cmd
}
