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

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
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

type planItem struct {
	engine.Plan
	applied bool
	phase   string
}

type planObservation struct {
	HistoryHash       string `json:"history_hash"`
	SchemaFingerprint string `json:"schema_fingerprint"`
	AppliedCount      int32  `json:"applied_count"`
	NewestApplied     int64  `json:"newest_applied"`
	At                string `json:"at"`
}

type planReport struct {
	live      bool
	target    string
	rollout   string
	validated bool
	planID    string
	planKey   string
	observed  *planObservation
	drift     string
	items     []planItem
}

func (r planReport) headline() string {
	if r.planID != "" {
		return fmt.Sprintf("plan %s on %s (rollout %s, %s)", r.planID, r.target, r.rollout, validatedLabel(r.validated))
	}

	return fmt.Sprintf("dry run on %s (rollout %s, %s)", r.target, r.rollout, validatedLabel(r.validated))
}

func (r planReport) contract() []string {
	if r.planID == "" {
		return nil
	}
	lines := []string{"key: " + r.planKey}
	if o := r.observed; o != nil {
		lines = append(lines, fmt.Sprintf("observed: %d applied, newest %d, history %s, schema %s, at %s",
			o.AppliedCount, o.NewestApplied, o.HistoryHash, o.SchemaFingerprint, o.At))
	}

	return lines
}

func (r planReport) driftBlock(indent, open, close string) string {
	if r.drift == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("drift since baseline:\n" + open)
	for _, l := range strings.Split(r.drift, "\n") {
		b.WriteString(indent + l + "\n")
	}
	b.WriteString(close)

	return b.String()
}

var planFormats = map[string]func(io.Writer, planReport){
	"text":     writePlanText,
	"markdown": writePlanMarkdown,
	"json":     writePlanJSON,
}

func newPlanCmd() *cobra.Command {
	flags := &targetFlags{}
	remote := &clientFlags{}
	req := &godwitv1.PlanRunRequest{}
	var format string
	cmd := &cobra.Command{
		Use:   "plan",
		Short: "Parse migrations and print classified statements with hazards; with --target, store the plan on the service",
		RunE: func(cmd *cobra.Command, _ []string) error {
			write, ok := planFormats[format]
			if !ok {
				return fmt.Errorf("unknown format %q (want text, markdown or json)", format)
			}
			if req.Target != "" {
				files, err := migrationFiles(flags.dir)
				if err != nil {
					return err
				}
				req.Files = files

				return remote.persistPlan(cmd, req, write)
			}
			migs, err := engine.LoadDir(flags.dir)
			if err != nil {
				return err
			}
			report := planReport{items: make([]planItem, 0, 2*len(migs))}
			for _, m := range migs {
				for _, dir := range []engine.Direction{engine.DirectionUp, engine.DirectionDown} {
					p, err := engine.BuildPlan(m, dir)
					if err != nil {
						return err
					}
					report.items = append(report.items, planItem{Plan: p})
				}
			}
			write(cmd.OutOrStdout(), report)

			return nil
		},
	}
	flags.register(cmd, false)
	remote.register(cmd)
	cmd.Flags().StringVar(&format, "format", "text", "output format: text, markdown or json")
	cmd.Flags().StringVar(&req.Target, "target", "", "target name; plans against the live database and stores the plan on the service")
	cmd.Flags().StringVar(&req.Rollout, "rollout", "direct", "rollout policy: direct or expand-contract")
	cmd.Flags().StringSliceVar(&req.AcknowledgeHazards, "ack", nil, "hazard codes to acknowledge")
	cmd.Flags().BoolVar(&req.SkipValidation, "skip-validation", false, "skip the scratch-database validation")
	cmd.Flags().BoolVar(&req.AllowOutOfOrder, "allow-out-of-order", false, "plan pending versions older than the newest applied one instead of refusing them")
	cmd.Flags().StringVar(&req.Source, "source", "", "where the files come from, kept on the plan (e.g. github.com/org/repo@<sha>:db/migrations)")
	configKeys(cmd, "target", "rollout", "allow-out-of-order")

	return cmd
}

func writePlanText(w io.Writer, r planReport) {
	if r.live {
		fmt.Fprintln(w, r.headline())
		for _, l := range r.contract() {
			fmt.Fprintln(w, l)
		}
		fmt.Fprint(w, r.driftBlock("  ", "", ""))
	}
	for _, p := range r.items {
		fmt.Fprintf(w, "%d_%s (%s): %d statement(s)%s\n", p.Migration.Version, p.Migration.Name, p.Direction, len(p.Statements), p.liveSuffix())
		for i, st := range p.Statements {
			fmt.Fprintf(w, "  [%d] %-5s %s\n", i, statementMode(st), firstLine(st.SQL))
			for _, h := range st.Hazards {
				fmt.Fprintf(w, "        hazard %s: %s\n", h.Code, h.Detail)
				writeRecipeText(w, "          ", h.Recipe)
			}
		}
	}
}

func writePlanMarkdown(w io.Writer, r planReport) {
	if r.live {
		fmt.Fprintln(w, "## godwit "+r.kind())
		fmt.Fprintln(w)
		fmt.Fprintf(w, "Target `%s`, rollout `%s`, %s.\n", r.target, r.rollout, validatedLabel(r.validated))
		for _, l := range r.contract() {
			fmt.Fprintln(w)
			fmt.Fprintln(w, l)
		}
		if block := r.driftBlock("", "\n```diff\n", "```\n"); block != "" {
			fmt.Fprintln(w)
			fmt.Fprint(w, block)
		}
	} else {
		fmt.Fprintln(w, "## godwit plan")
	}
	fmt.Fprintln(w)
	hazards := 0
	if len(r.items) > 0 {
		fmt.Fprintln(w, "| Migration | Direction | # | Mode | Statement | Hazards |"+r.liveHeader())
		fmt.Fprintln(w, "|---|---|---|---|---|---|"+r.liveRule())
	}
	for _, p := range r.items {
		for i, st := range p.Statements {
			codes := make([]string, 0, len(st.Hazards))
			for _, h := range st.Hazards {
				codes = append(codes, fmt.Sprintf("%s: %s", h.Code, h.Detail))
			}
			hazards += len(st.Hazards)
			fmt.Fprintf(w, "| `%d_%s` | %s | %d | %s | `%s` | %s |%s\n", p.Migration.Version, p.Migration.Name, p.Direction, i,
				statementMode(st), markdownCell(oneLine(st.SQL)), markdownCell(strings.Join(codes, "; ")), p.liveCells(r.live))
		}
	}
	if len(r.items) > 0 {
		fmt.Fprintln(w)
	}
	for _, p := range r.items {
		for i, st := range p.Statements {
			for _, h := range st.Hazards {
				writeRecipeDetails(w, fmt.Sprintf("recipe for %s in `%d_%s` (%s) #%d", h.Code, p.Migration.Version, p.Migration.Name, p.Direction, i), h.Recipe)
			}
		}
	}
	if hazards > 0 {
		fmt.Fprintf(w, "⚠️ %d hazard(s); acknowledge them with `--ack`\n", hazards)

		return
	}
	fmt.Fprintln(w, "✅ no hazards")
}

func (r planReport) kind() string {
	if r.planID != "" {
		return "plan " + r.planID
	}

	return "dry run"
}

func validatedLabel(validated bool) string {
	if validated {
		return "validated on a scratch database"
	}

	return "not validated"
}

func (r planReport) liveHeader() string {
	if r.live {
		return " Phase | Status |"
	}

	return ""
}

func (r planReport) liveRule() string {
	if r.live {
		return "---|---|"
	}

	return ""
}

func (p planItem) status() string {
	if p.applied {
		return "applied"
	}

	return "pending"
}

func (p planItem) liveSuffix() string {
	if p.phase == "" {
		return ""
	}

	return fmt.Sprintf(" [%s, %s]", p.phase, p.status())
}

func (p planItem) liveCells(live bool) string {
	if !live {
		return ""
	}

	return fmt.Sprintf(" %s | %s |", p.phase, p.status())
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
	Recipe string `json:"recipe,omitempty"`
}

type livePlanJSON struct {
	planJSON
	Applied bool   `json:"applied"`
	Phase   string `json:"phase"`
}

type dryRunJSON struct {
	Target     string           `json:"target"`
	Rollout    string           `json:"rollout"`
	Validated  bool             `json:"validated"`
	PlanID     string           `json:"plan_id,omitempty"`
	PlanKey    string           `json:"plan_key,omitempty"`
	Observed   *planObservation `json:"observed,omitempty"`
	Drift      string           `json:"drift,omitempty"`
	Migrations []livePlanJSON   `json:"migrations"`
}

func toPlanJSON(p engine.Plan) planJSON {
	pj := planJSON{Version: p.Migration.Version, Name: p.Migration.Name, Direction: string(p.Direction), Statements: []statementJSON{}}
	for _, st := range p.Statements {
		hazards := make([]hazardJSON, 0, len(st.Hazards))
		for _, h := range st.Hazards {
			hazards = append(hazards, hazardJSON{Code: h.Code, Detail: h.Detail, Recipe: h.Recipe})
		}
		pj.Statements = append(pj.Statements, statementJSON{SQL: st.SQL, Mode: statementMode(st), Hazards: hazards})
	}

	return pj
}

func writePlanJSON(w io.Writer, r planReport) {
	var out any
	if r.live {
		live := dryRunJSON{
			Target: r.target, Rollout: r.rollout, Validated: r.validated, Migrations: []livePlanJSON{},
			PlanID: r.planID, PlanKey: r.planKey, Observed: r.observed, Drift: r.drift,
		}
		for _, p := range r.items {
			live.Migrations = append(live.Migrations, livePlanJSON{planJSON: toPlanJSON(p.Plan), Applied: p.applied, Phase: p.phase})
		}
		out = live
	} else {
		plans := make([]planJSON, 0, len(r.items))
		for _, p := range r.items {
			plans = append(plans, toPlanJSON(p.Plan))
		}
		out = plans
	}
	data, _ := json.Marshal(out)
	fmt.Fprintln(w, string(data))
}

func writeRecipeText(w io.Writer, indent, recipe string) {
	if recipe == "" {
		return
	}
	for _, line := range strings.Split(recipe, "\n") {
		fmt.Fprintln(w, indent+line)
	}
}

func writeRecipeDetails(w io.Writer, summary, recipe string) {
	if recipe == "" {
		return
	}
	fmt.Fprintf(w, "<details><summary>%s</summary>\n\n```sql\n%s\n```\n\n</details>\n\n", summary, recipe)
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
