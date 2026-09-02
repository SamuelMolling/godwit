package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/spf13/cobra"

	"github.com/SamuelMolling/godwit/internal/config"
	"github.com/SamuelMolling/godwit/internal/engine"
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
	if withDSN {
		cmd.Flags().StringVar(&f.dsn, "dsn", "", "target database DSN")
		cmd.Flags().DurationVar(&f.lockTimeout, "lock-timeout", d.LockTimeout, "lock_timeout for each statement")
		cmd.Flags().DurationVar(&f.statementTimeout, "statement-timeout", d.StatementTimeout, "statement_timeout for each statement (0 disables)")
		_ = cmd.MarkFlagRequired("dsn")
	}
}

func (f *targetFlags) executor(ctx context.Context) (*engine.Executor, func(), error) {
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

func newPlanCmd() *cobra.Command {
	flags := &targetFlags{}
	cmd := &cobra.Command{
		Use:   "plan",
		Short: "Parse migrations and print classified statements with hazards",
		RunE: func(cmd *cobra.Command, _ []string) error {
			migs, err := engine.LoadDir(flags.dir)
			if err != nil {
				return err
			}
			for _, m := range migs {
				for _, dir := range []engine.Direction{engine.DirectionUp, engine.DirectionDown} {
					p, err := engine.BuildPlan(m, dir)
					if err != nil {
						return err
					}
					printPlan(cmd, p)
				}
			}

			return nil
		},
	}
	flags.register(cmd, false)

	return cmd
}

func printPlan(cmd *cobra.Command, p engine.Plan) {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "%d_%s (%s): %d statement(s)\n", p.Migration.Version, p.Migration.Name, p.Direction, len(p.Statements))
	for i, st := range p.Statements {
		mode := "tx"
		if st.NoTx {
			mode = "no-tx"
		}
		fmt.Fprintf(out, "  [%d] %-5s %s\n", i, mode, firstLine(st.SQL))
		for _, h := range st.Hazards {
			fmt.Fprintf(out, "        hazard %s: %s\n", h.Code, h.Detail)
		}
	}
}

func firstLine(sql string) string {
	line, _, _ := strings.Cut(sql, "\n")

	return line
}

func newRunCmd() *cobra.Command {
	flags := &targetFlags{}
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Apply all pending migrations",
		RunE: func(cmd *cobra.Command, _ []string) error {
			migs, err := engine.LoadDir(flags.dir)
			if err != nil {
				return err
			}
			exec, closeFn, err := flags.executor(cmd.Context())
			if err != nil {
				return err
			}
			defer closeFn()

			for _, m := range migs {
				p, err := engine.BuildPlan(m, engine.DirectionUp)
				if err != nil {
					return err
				}
				res, err := exec.Up(cmd.Context(), p)
				if err != nil {
					return err
				}
				printResult(cmd, m, res)
			}

			return nil
		},
	}
	flags.register(cmd, true)

	return cmd
}

func printResult(cmd *cobra.Command, m engine.Migration, res engine.Result) {
	state := fmt.Sprintf("applied (%d statement(s))", res.Applied)
	if res.Skipped {
		state = "skipped"
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%d_%s: %s\n", m.Version, m.Name, state)
}

func newStatusCmd() *cobra.Command {
	flags := &targetFlags{}
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show applied state of every migration",
		RunE: func(cmd *cobra.Command, _ []string) error {
			migs, err := engine.LoadDir(flags.dir)
			if err != nil {
				return err
			}
			exec, closeFn, err := flags.executor(cmd.Context())
			if err != nil {
				return err
			}
			defer closeFn()

			rows, err := exec.Status(cmd.Context(), migs)
			if err != nil {
				return err
			}
			for _, r := range rows {
				state := "pending"
				if r.Applied {
					state = "applied " + r.AppliedAt.UTC().Format(time.RFC3339)
				}
				if r.Drifted {
					state += " (checksum drift!)"
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%d_%s: %s\n", r.Version, r.Name, state)
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
		Short: "Revert one applied migration (dev only; production policy is roll-forward)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !yes {
				return fmt.Errorf("down is destructive; re-run with --yes to confirm")
			}
			migs, err := engine.LoadDir(flags.dir)
			if err != nil {
				return err
			}
			for _, m := range migs {
				if m.Version != version {
					continue
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
				printResult(cmd, m, res)

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
