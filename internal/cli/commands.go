package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/spf13/cobra"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/internal/config"
	"github.com/SamuelMolling/godwit/internal/engine"
	"github.com/SamuelMolling/godwit/internal/report"
)

type targetFlags struct {
	dsn              string
	dir              string
	lockTimeout      time.Duration
	statementTimeout time.Duration
}

func (f *targetFlags) register(cmd *cobra.Command, withDSN bool) {
	d := config.Defaults()
	cmd.Flags().StringVar(&f.dir, "dir", d.Dir, "migration directory")
	configKeys(cmd, "dir")
	if withDSN {
		configKeys(cmd, "lock-timeout", "statement-timeout")
		cmd.Flags().StringVar(&f.dsn, "dsn", os.Getenv("GODWIT_DSN"),
			"target database DSN (or GODWIT_DSN, which keeps the password out of the process arguments)")
		cmd.Flags().DurationVar(&f.lockTimeout, "lock-timeout", d.LockTimeout, "lock_timeout for each statement")
		cmd.Flags().DurationVar(&f.statementTimeout, "statement-timeout", d.StatementTimeout, "statement_timeout for each statement (0 disables)")
	}
}

func (f *targetFlags) executor(ctx context.Context) (*engine.Executor, func(), error) {
	if f.dsn == "" {
		return nil, nil, errors.New("--dsn (or GODWIT_DSN) is required")
	}
	conn, err := pgx.Connect(ctx, f.dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("connect: %w", err)
	}
	exec := engine.New(conn, engine.Options{
		LockTimeout:      f.lockTimeout,
		StatementTimeout: f.statementTimeout,
	})

	return exec, func() { _ = conn.Close(context.Background()) }, nil
}

type reportFlags struct {
	format     string
	planFormat string
}

func (f *reportFlags) register(cmd *cobra.Command, what string) {
	f.registerFormats(cmd, strings.TrimSpace(what+" output format: text, markdown or json"))
}

func (f *reportFlags) registerRun(cmd *cobra.Command) {
	f.registerFormats(cmd, "output format: text or markdown")
}

func (f *reportFlags) registerFormats(cmd *cobra.Command, formats string) {
	cmd.Flags().StringVar(&f.format, "format", "text", formats)
	cmd.Flags().StringVar(&f.planFormat, "plan-format", config.PlanFormatSchema,
		"what the report says: schema (what the migrations do to the database) or statements (the SQL the run would execute)")
	configKeys(cmd, "plan-format")
}

func (f *reportFlags) writer() (func(io.Writer, report.Plan), error) {
	return report.PlanWriter(f.format, f.planFormat)
}

func (f *reportFlags) runWriter() (func(io.Writer, report.Run), error) {
	return report.RunWriter(f.format, f.planFormat)
}

func newPlanCmd() *cobra.Command {
	flags := &targetFlags{}
	remote := &clientFlags{}
	req := &godwitv1.PlanRunRequest{}
	rep := &reportFlags{}
	var save bool
	cmd := &cobra.Command{
		Use:   "plan",
		Short: "Show the statements a migration directory would run, offline or against a live target",
		Long: "Three forms, in order of what they touch:\n\n" +
			"  godwit plan --dir db/migrations         offline. Parses the files and prints both sides of every\n" +
			"                                          migration. No database, no service, nothing written.\n" +
			"  godwit plan --target app                live. The service works out what is pending on that target\n" +
			"                                          and replays it on a scratch database to prove it applies.\n" +
			"                                          Prints the result and stores nothing.\n" +
			"  godwit plan --target app --save         the same, and stores the plan on the service, so a later\n" +
			"                                          migrate binds to it and refuses if the target has moved.\n\n" +
			"--target is a flag here and only a flag: plan never reads it from godwit.yaml, so a bare `godwit plan`\n" +
			"is the offline form even in a repository whose config names a target.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			write, err := rep.writer()
			if err != nil {
				return err
			}
			if err := checkToVersion(cmd, req.ToVersion, "", req.Target); err != nil {
				return err
			}
			if save && req.Target == "" {
				return errors.New("--save needs --target: an offline plan is not made against a target, so there is nothing for a later migrate to bind to")
			}
			migs, why, err := emptySet(flags.dir)
			if err != nil {
				return err
			}
			if req.Target != "" {
				if why != "" {
					return remote.nothingYet(cmd, req.Target, why, write)
				}
				req.Files = protoFiles(migs)
				req.Persist = save

				return remote.planRun(cmd, req, write)
			}
			plans := make([]engine.Plan, 0, 2*len(migs))
			for _, m := range migs {
				for _, dir := range directionsOf(m) {
					p, err := engine.BuildPlan(m, dir)
					if err != nil {
						return err
					}
					plans = append(plans, p)
				}
			}
			write(cmd.OutOrStdout(), report.PlanOffline(why, plans))

			return nil
		},
	}
	cmd.AddCommand(newPlanShowCmd())
	flags.register(cmd, false)
	remote.register(cmd)
	rep.register(cmd, "")
	cmd.Flags().StringVar(&req.Target, "target", "", "target name; plans against the live database through the service instead of parsing the directory offline")
	cmd.Flags().BoolVar(&save, "save", false, "store the plan on the service so a later migrate can bind to it (needs --target)")
	cmd.Flags().StringVar(&req.Rollout, "rollout", "direct", "rollout policy: direct or expand-contract")
	cmd.Flags().StringSliceVar(&req.AcknowledgeHazards, "ack", nil, "hazard codes to acknowledge")
	cmd.Flags().BoolVar(&req.SkipValidation, "skip-validation", false, "skip the scratch-database validation")
	cmd.Flags().BoolVar(&req.AllowOutOfOrder, "allow-out-of-order", false, "plan pending versions older than the newest applied one instead of refusing them")
	cmd.Flags().StringVar(&req.Source, "source", "", "where the files come from, kept on the plan (e.g. github.com/org/repo@<sha>:db/migrations)")
	cmd.Flags().Int64Var(&req.ToVersion, "to", 0, "stop at this migration version: pending ones above it are reported as withheld and left for a later plan")
	configKeys(cmd, "rollout", "allow-out-of-order")

	return cmd
}

// checkToVersion refuses a version target godwit cannot resolve here; the ones that need the target's history are refused by the service.
func checkToVersion(cmd *cobra.Command, to int64, planID, target string) error {
	if !cmd.Flags().Changed("to") {
		return nil
	}
	switch {
	case to < 1:
		return errors.New("--to takes a migration version, the 14 digits its file name starts with")
	case planID != "":
		return errors.New("--to cannot be combined with --plan: the stored plan already fixes the set it covers")
	case target == "":
		return errors.New("--to needs --target: what it holds back is decided against the versions that target has applied")
	}

	return nil
}

// directionsOf is the sides a migration has: a checkpoint has no inverse, so it has only an up.
func directionsOf(m engine.Migration) []engine.Direction {
	if m.Checkpoint {
		return []engine.Direction{engine.DirectionUp}
	}

	return []engine.Direction{engine.DirectionUp, engine.DirectionDown}
}

func newUpCmd() *cobra.Command {
	flags := &targetFlags{}
	cmd := &cobra.Command{
		Use:   "up",
		Short: "Apply every pending migration to the database at --dsn, with no service involved",
		Long: "Same executor, same journal and same crash safety as a service run, without the service: no target to\n" +
			"register, no plan to bind, no ledger. What it applies is recorded in that database's own journal only.\n\n" +
			"`up` and `down` are the local pair, against --dsn. `migrate` and `revert` are the service pair, against --target.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			migs, why, err := emptySet(flags.dir)
			if err != nil {
				return err
			}
			exec, closeFn, err := flags.executor(cmd.Context())
			if err != nil {
				return err
			}
			defer closeFn()
			if why != "" {
				return nothingLocal(cmd, exec, why)
			}

			plans, err := upPlans(cmd.Context(), exec, migs)
			if err != nil {
				return err
			}
			for _, p := range plans {
				res, err := exec.Up(cmd.Context(), p)
				if err != nil {
					return err
				}
				printResult(cmd, p.Migration, p.Direction, res)
			}

			return nil
		},
	}
	flags.register(cmd, true)

	return cmd
}

// upPlans builds the up side of every migration and decides what a checkpoint among them does against
// the versions the database already holds.
func upPlans(ctx context.Context, exec *engine.Executor, migs []engine.Migration) ([]engine.Plan, error) {
	plans := make([]engine.Plan, 0, len(migs))
	for _, m := range migs {
		p, err := engine.BuildPlan(m, engine.DirectionUp)
		if err != nil {
			return nil, err
		}
		plans = append(plans, p)
	}
	rows, err := exec.Status(ctx, migs)
	if err != nil {
		return nil, err
	}
	var newest int64
	for _, r := range rows {
		if r.Applied && !r.Migration.Repeatable {
			newest = max(newest, r.Migration.Version)
		}
	}

	return engine.ShapeCheckpoint(plans, newest)
}

func printResult(cmd *cobra.Command, m engine.Migration, dir engine.Direction, res engine.Result) {
	verb := "applied"
	if dir == engine.DirectionDown {
		verb = "reverted"
	}
	state := fmt.Sprintf("%s (%d statement(s))", verb, res.Applied)
	if res.Skipped {
		state = "skipped"
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%s: %s\n", m.ID(), state)
}

func newStatusCmd() *cobra.Command {
	flags := &targetFlags{}
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show which migrations the database at --dsn has applied, asked of the database itself",
		RunE: func(cmd *cobra.Command, _ []string) error {
			migs, why, err := emptySet(flags.dir)
			if err != nil {
				return err
			}
			exec, closeFn, err := flags.executor(cmd.Context())
			if err != nil {
				return err
			}
			defer closeFn()
			if why != "" {
				return nothingLocal(cmd, exec, why)
			}

			rows, err := exec.Status(cmd.Context(), migs)
			if err != nil {
				return err
			}
			for _, r := range rows {
				state := "pending"
				switch {
				case r.Applied && r.Migration.Repeatable:
					state = "unchanged since " + r.AppliedAt.UTC().Format(time.RFC3339)
				case r.Applied:
					state = "applied " + r.AppliedAt.UTC().Format(time.RFC3339)
				}
				if r.Drifted {
					state += " (checksum drift!)"
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s: %s\n", r.Migration.ID(), state)
			}

			return nil
		},
	}
	flags.register(cmd, true)

	return cmd
}

func newDownCmd() *cobra.Command {
	flags := &targetFlags{}
	var version int64
	var yes bool
	cmd := &cobra.Command{
		Use:   "down",
		Short: "Undo one applied migration on the database at --dsn (dev only; production policy is roll-forward)",
		Long: "Runs one migration's down side against --dsn and removes it from that database's journal.\n\n" +
			"`up` and `down` are the local pair. The service pair is `migrate` and `revert` — and `revert` undoes\n" +
			"one whole run read from the ledger, not one version you name, which is why it is the production path.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !yes {
				return fmt.Errorf("down is destructive; re-run with --yes to confirm")
			}
			migs, err := engine.LoadDir(flags.dir)
			if err != nil {
				return err
			}
			for _, m := range migs {
				if m.Repeatable || m.Version != version {
					continue
				}
				if m.Checkpoint {
					return fmt.Errorf("%s is a checkpoint: it has no inverse, and the versions it collapses can no longer be reverted", m.ID())
				}
				p, err := engine.BuildPlan(m, engine.DirectionDown)
				if err != nil {
					return err
				}
				exec, closeFn, err := flags.executor(cmd.Context())
				if err != nil {
					return err
				}
				defer closeFn()
				res, err := exec.Down(cmd.Context(), p)
				if err != nil {
					return err
				}
				printResult(cmd, m, p.Direction, res)

				return nil
			}

			return fmt.Errorf("version %d not found in %s", version, flags.dir)
		},
	}
	flags.register(cmd, true)
	cmd.Flags().Int64Var(&version, "version", 0, "migration version to revert")
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm the revert")
	_ = cmd.MarkFlagRequired("version")

	return cmd
}
