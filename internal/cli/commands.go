package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	configKeys(cmd, "dir")
	if withDSN {
		configKeys(cmd, "lock-timeout", "statement-timeout")
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

var planFormats = map[string]func(io.Writer, []engine.Plan){
	"text":     writePlanText,
	"markdown": writePlanMarkdown,
	"json":     writePlanJSON,
}

func newPlanCmd() *cobra.Command {
	flags := &targetFlags{}
	var format string
	cmd := &cobra.Command{
		Use:   "plan",
		Short: "Parse migrations and print classified statements with hazards",
		RunE: func(cmd *cobra.Command, _ []string) error {
			write, ok := planFormats[format]
			if !ok {
				return fmt.Errorf("unknown format %q (want text, markdown or json)", format)
			}
			migs, err := engine.LoadDir(flags.dir)
			if err != nil {
				return err
			}
			plans := make([]engine.Plan, 0, 2*len(migs))
			for _, m := range migs {
				for _, dir := range []engine.Direction{engine.DirectionUp, engine.DirectionDown} {
					p, err := engine.BuildPlan(m, dir)
					if err != nil {
						return err
					}
					plans = append(plans, p)
				}
			}
			write(cmd.OutOrStdout(), plans)

			return nil
		},
	}
	flags.register(cmd, false)
	cmd.Flags().StringVar(&format, "format", "text", "output format: text, markdown or json")

	return cmd
}

func writePlanText(w io.Writer, plans []engine.Plan) {
	for _, p := range plans {
		fmt.Fprintf(w, "%d_%s (%s): %d statement(s)\n", p.Migration.Version, p.Migration.Name, p.Direction, len(p.Statements))
		for i, st := range p.Statements {
			fmt.Fprintf(w, "  [%d] %-5s %s\n", i, statementMode(st), firstLine(st.SQL))
			for _, h := range st.Hazards {
				fmt.Fprintf(w, "        hazard %s: %s\n", h.Code, h.Detail)
			}
		}
	}
}

func writePlanMarkdown(w io.Writer, plans []engine.Plan) {
	fmt.Fprintln(w, "## godwit plan")
	fmt.Fprintln(w)
	hazards := 0
	if len(plans) > 0 {
		fmt.Fprintln(w, "| Migration | Direction | # | Mode | Statement | Hazards |")
		fmt.Fprintln(w, "|---|---|---|---|---|---|")
	}
	for _, p := range plans {
		for i, st := range p.Statements {
			codes := make([]string, 0, len(st.Hazards))
			for _, h := range st.Hazards {
				codes = append(codes, fmt.Sprintf("%s: %s", h.Code, h.Detail))
			}
			hazards += len(st.Hazards)
			fmt.Fprintf(w, "| `%d_%s` | %s | %d | %s | `%s` | %s |\n", p.Migration.Version, p.Migration.Name, p.Direction, i,
				statementMode(st), markdownCell(oneLine(st.SQL)), markdownCell(strings.Join(codes, "; ")))
		}
	}
	if len(plans) > 0 {
		fmt.Fprintln(w)
	}
	if hazards > 0 {
		fmt.Fprintf(w, "⚠️ %d hazard(s); acknowledge them with `--ack`\n", hazards)

		return
	}
	fmt.Fprintln(w, "✅ no hazards")
}

func markdownCell(s string) string {
	return strings.ReplaceAll(s, "|", "\\|")
}

func oneLine(sql string) string {
	line := strings.Join(strings.Fields(sql), " ")
	if len(line) > 120 {
		return line[:119] + "…"
	}

	return line
}

type planJSON struct {
	Version    int64           `json:"version"`
	Name       string          `json:"name"`
	Direction  string          `json:"direction"`
	Statements []statementJSON `json:"statements"`
}

type statementJSON struct {
	SQL     string       `json:"sql"`
	Mode    string       `json:"mode"`
	Hazards []hazardJSON `json:"hazards"`
}

type hazardJSON struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

func writePlanJSON(w io.Writer, plans []engine.Plan) {
	out := make([]planJSON, 0, len(plans))
	for _, p := range plans {
		pj := planJSON{Version: p.Migration.Version, Name: p.Migration.Name, Direction: string(p.Direction), Statements: []statementJSON{}}
		for _, st := range p.Statements {
			hazards := make([]hazardJSON, 0, len(st.Hazards))
			for _, h := range st.Hazards {
				hazards = append(hazards, hazardJSON{Code: h.Code, Detail: h.Detail})
			}
			pj.Statements = append(pj.Statements, statementJSON{SQL: st.SQL, Mode: statementMode(st), Hazards: hazards})
		}
		out = append(out, pj)
	}
	data, _ := json.Marshal(out)
	fmt.Fprintln(w, string(data))
}

func statementMode(st engine.Statement) string {
	if st.NoTx {
		return "no-tx"
	}

	return "tx"
}

func firstLine(sql string) string {
	line, _, _ := strings.Cut(sql, "\n")

	return line
}

func newApplyCmd() *cobra.Command {
	flags := &targetFlags{}
	cmd := &cobra.Command{
		Use:   "apply",
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
