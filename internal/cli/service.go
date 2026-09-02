package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/gen/godwit/v1/godwitv1connect"
	"github.com/SamuelMolling/godwit/internal/config"
	"github.com/SamuelMolling/godwit/internal/engine"
)

func newTargetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "target",
		Short: "Manage targets on the service",
	}
	cmd.AddCommand(newTargetAddCmd(), newTargetBaselineCmd(), newTargetStatusCmd())

	return cmd
}

func newTargetBaselineCmd() *cobra.Command {
	flags := &clientFlags{}
	req := &godwitv1.BaselineTargetRequest{}
	var dir string
	cmd := &cobra.Command{
		Use:   "baseline <name>",
		Short: "Mark migrations up to a version as applied on an existing database without running them",
		Args:  cobra.ExactArgs(1),
		RunE: flags.runE(func(cmd *cobra.Command, client godwitv1connect.GodwitServiceClient, args []string) error {
			req.Target = args[0]
			files, err := migrationFiles(dir)
			if err != nil {
				return err
			}
			req.Files = files
			resp, err := client.BaselineTarget(cmd.Context(), connect.NewRequest(req))
			if err != nil {
				return err
			}
			flags.print(cmd, resp.Msg, fmt.Sprintf("target %s: baselined to version %d (run %s)", req.Target, req.Version, resp.Msg.RunId))

			return nil
		}),
	}
	flags.register(cmd)
	cmd.Flags().StringVar(&dir, "dir", config.Defaults().Dir, "migration directory")
	cmd.Flags().Int64Var(&req.Version, "version", 0, "highest migration version already present in the database")
	_ = cmd.MarkFlagRequired("version")
	configKeys(cmd, "dir")

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
	cmd.Flags().BoolVar(&req.RequirePlan, "require-plan", false, "refuse runs on this target without a stored plan")
	timeoutFlags(cmd, &req.LockTimeout, &req.StatementTimeout, "for runs on this target")
	_ = cmd.MarkFlagRequired("provider")

	return cmd
}

func newMigrateCmd() *cobra.Command {
	flags := &clientFlags{}
	req := &godwitv1.CreateRunRequest{}
	var dir, format string
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Send a migration directory to the service and watch the run",
		Args:  cobra.NoArgs,
		RunE: flags.runE(func(cmd *cobra.Command, client godwitv1connect.GodwitServiceClient, _ []string) error {
			write, ok := planFormats[format]
			if !ok {
				return fmt.Errorf("unknown format %q (want text, markdown or json)", format)
			}
			if req.Target == "" && req.PlanId == "" {
				return errors.New("--target (or target in godwit.yaml) is required")
			}
			if req.PlanId != "" && dryRun {
				return errors.New("--plan cannot be combined with --dry-run")
			}
			if req.PlanId != "" && !cmd.Flags().Changed("rollout") {
				req.Rollout = ""
			}
			if req.PlanId == "" || cmd.Flags().Changed("dir") {
				files, err := migrationFiles(dir)
				if err != nil {
					return err
				}
				req.Files = files
			}
			if dryRun {
				return flags.dryRun(cmd, client, req, write)
			}
			created, err := client.CreateRun(cmd.Context(), connect.NewRequest(req))
			if err != nil {
				return err
			}
			if !flags.json {
				fmt.Fprintln(cmd.OutOrStdout(), bindLine(created.Msg))
			}

			return flags.watch(cmd, client, created.Msg.RunId)
		}),
	}
	flags.register(cmd)
	cmd.Flags().StringVar(&req.Target, "target", "", "target name")
	cmd.Flags().StringVar(&dir, "dir", config.Defaults().Dir, "migration directory")
	cmd.Flags().StringVar(&req.Rollout, "rollout", "direct", "rollout policy: direct or expand-contract")
	cmd.Flags().StringSliceVar(&req.AcknowledgeHazards, "ack", nil, "hazard codes to acknowledge")
	cmd.Flags().BoolVar(&req.SkipValidation, "skip-validation", false, "skip the scratch-database validation")
	cmd.Flags().BoolVar(&req.AllowOutOfOrder, "allow-out-of-order", false, "apply pending versions older than the newest applied one instead of refusing them")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "run the admission checks on the service and print the plan without queueing a run")
	cmd.Flags().StringVar(&format, "format", "text", "dry-run output format: text, markdown or json")
	cmd.Flags().StringVar(&req.Source, "source", "", "where the files come from, kept on the run (e.g. github.com/org/repo@<sha>:db/migrations)")
	cmd.Flags().StringVar(&req.PlanId, "plan", "", "bind this stored plan by id; the plan supplies target, rollout and files unless given explicitly")
	timeoutFlags(cmd, &req.LockTimeout, &req.StatementTimeout, "for this run, overriding the target's")
	configKeys(cmd, "target", "dir", "rollout", "allow-out-of-order")

	return cmd
}

func bindLine(res *godwitv1.CreateRunResponse) string {
	switch {
	case res.Reattached:
		return "re-attached to run " + res.RunId
	case res.PlanId == "":
		return "no stored plan for this set: implicit plan"
	default:
		return "plan " + res.PlanId + ": bound"
	}
}

func (f *clientFlags) dryRun(cmd *cobra.Command, client godwitv1connect.GodwitServiceClient, req *godwitv1.CreateRunRequest, write func(io.Writer, planReport)) error {
	res, err := client.PlanRun(cmd.Context(), connect.NewRequest(&godwitv1.PlanRunRequest{
		Target: req.Target, Files: req.Files, AcknowledgeHazards: req.AcknowledgeHazards, SkipValidation: req.SkipValidation,
		Rollout: req.Rollout, AllowOutOfOrder: req.AllowOutOfOrder,
	}))
	if err != nil {
		return err
	}
	if f.json {
		f.print(cmd, res.Msg, "")

		return nil
	}
	write(cmd.OutOrStdout(), planReportFromProto(res.Msg))

	return nil
}

func (f *clientFlags) persistPlan(cmd *cobra.Command, req *godwitv1.PlanRunRequest, write func(io.Writer, planReport)) error {
	client, err := f.client()
	if err != nil {
		return err
	}
	req.Persist = true
	res, err := client.PlanRun(cmd.Context(), connect.NewRequest(req))
	if err != nil {
		return apiError(err)
	}
	if f.json {
		f.print(cmd, res.Msg, "")

		return nil
	}
	write(cmd.OutOrStdout(), planReportFromProto(res.Msg))

	return nil
}

func planReportFromProto(m *godwitv1.PlanRunResponse) planReport {
	r := planReport{
		live: true, target: m.Target, rollout: m.Rollout, validated: m.Validated, items: make([]planItem, 0, len(m.Migrations)),
		planID: m.PlanId, planKey: m.PlanKey, drift: m.Drift,
	}
	if m.Observed != nil {
		r.observed = &planObservation{
			HistoryHash: m.Observed.HistoryHash, SchemaFingerprint: m.Observed.SchemaFingerprint,
			AppliedCount: m.Observed.AppliedCount, NewestApplied: m.Observed.NewestApplied, At: stamp(m.Observed.At),
		}
	}
	for _, pm := range m.Migrations {
		p := engine.Plan{
			Migration: engine.Migration{Version: pm.Version, Name: pm.Name, Checksum: pm.Checksum},
			Direction: engine.DirectionUp,
		}
		for _, ps := range pm.Statements {
			st := engine.Statement{SQL: ps.Sql, NoTx: ps.NoTx}
			for _, h := range ps.Hazards {
				st.Hazards = append(st.Hazards, engine.Hazard{Code: h.Code, Detail: h.Detail, Recipe: h.Recipe})
			}
			p.Statements = append(p.Statements, st)
		}
		r.items = append(r.items, planItem{
			Plan: p, applied: pm.Applied, phase: pm.Phase, alreadyApplied: pm.AlreadyApplied, effect: pm.Effect, note: pm.Note,
		})
	}

	return r
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
	timeoutFlags(cmd, &req.LockTimeout, &req.StatementTimeout, "for this revert, overriding the target's")

	return cmd
}

func timeoutFlags(cmd *cobra.Command, lock, statement *string, scope string) {
	cmd.Flags().StringVar(lock, "lock-timeout", "", "per-statement lock_timeout "+scope+" (e.g. 5s)")
	cmd.Flags().StringVar(statement, "statement-timeout", "", "per-statement statement_timeout "+scope+" (e.g. 2m, 0 disables)")
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
	if wait := time.Until(r.NotBefore.AsTime()).Round(time.Second); r.NotBefore != nil && wait > 0 {
		line += fmt.Sprintf(" (retry in %s)", wait)
	}

	return line
}

func stamp(ts *timestamppb.Timestamp) string {
	if ts == nil {
		return ""
	}

	return ts.AsTime().UTC().Format(time.RFC3339)
}

func newRunCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Inspect and steer runs on the service",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newRunGetCmd(), newRunWatchCmd(), newRunResumeCmd(), newRunConfirmCmd())

	return cmd
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
			flags.print(cmd, resp.Msg, fmt.Sprintf("%s\n  target: %s\n  kind: %s\n  rollout: %s\n  phase: %s\n  reverts: %s\n  lock_timeout: %s\n  statement_timeout: %s\n  created_by: %s\n  source: %s\n  plan: %s\n  created: %s\n  finished: %s",
				runLine(r), r.Target, r.Kind, r.Rollout, r.Phase, r.Reverts, r.LockTimeout, r.StatementTimeout, r.CreatedBy, r.Source, r.PlanId, stamp(r.CreatedAt), stamp(r.FinishedAt)))

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
	var latest, allowNone bool
	var target string
	cmd := &cobra.Command{
		Use:   "confirm [run-id]",
		Short: "Release the contract phase of an expand-contract run",
		Long: "Confirms the run given as argument, or with --latest the newest run on --target\n" +
			"that is awaiting its contract phase.",
		Args: cobra.MaximumNArgs(1),
		RunE: flags.runE(func(cmd *cobra.Command, client godwitv1connect.GodwitServiceClient, args []string) error {
			id := ""
			if len(args) == 1 {
				id = args[0]
			}
			switch {
			case latest && id != "":
				return errors.New("pass a run id or --latest, not both")
			case latest && target == "":
				return errors.New("--latest requires --target (or target in godwit.yaml)")
			case !latest && id == "":
				return errors.New("a run id or --latest is required")
			}
			if latest {
				listed, err := client.ListRuns(cmd.Context(), connect.NewRequest(&godwitv1.ListRunsRequest{Target: target}))
				if err != nil {
					return err
				}
				id = latestAwaiting(listed.Msg.Runs)
				if id == "" && allowNone {
					flags.print(cmd, listed.Msg, fmt.Sprintf("target %s: no run awaiting contract", target))

					return nil
				}
				if id == "" {
					return fmt.Errorf("target %s: no run awaiting contract", target)
				}
			}
			resp, err := client.ConfirmRollout(cmd.Context(), connect.NewRequest(&godwitv1.ConfirmRolloutRequest{RunId: id}))
			if err != nil {
				return err
			}
			flags.print(cmd, resp.Msg, fmt.Sprintf("run %s: contract confirmed", id))

			return nil
		}),
	}
	flags.register(cmd)
	cmd.Flags().BoolVar(&latest, "latest", false, "confirm the newest run awaiting contract on --target")
	cmd.Flags().StringVar(&target, "target", "", "target name (with --latest)")
	cmd.Flags().BoolVar(&allowNone, "allow-none", false, "with --latest, exit 0 when no run is awaiting contract")
	configKeys(cmd, "target")

	return cmd
}

func latestAwaiting(runs []*godwitv1.Run) string {
	for _, r := range runs {
		if r.State == godwitv1.RunState_RUN_STATE_AWAITING_CONTRACT {
			return r.Id
		}
	}

	return ""
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
	fmt.Fprintln(w, "ID\tTARGET\tKIND\tSTATE\tROLLOUT\tPHASE\tBY\tSOURCE\tCREATED")
	for _, r := range runs {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.Id, r.Target, r.Kind, stateName(r.State), r.Rollout, r.Phase,
			r.CreatedBy, r.Source, stamp(r.CreatedAt))
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
