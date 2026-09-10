package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/spf13/cobra"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/internal/config"
	"github.com/SamuelMolling/godwit/internal/controlplane"
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

type planItem struct {
	engine.Plan
	applied        bool
	phase          string
	alreadyApplied bool
	effect         string
	note           string
	directives     []string
	expanded       bool
	notes          []string
	withheld       bool
	skipped        bool
	changes        []engine.ObjectChange
}

func (r planReport) hazardGate() (gated int, codes []string) {
	for _, p := range r.items {
		if p.skipped {
			continue
		}
		for _, st := range p.Statements {
			for _, h := range st.Hazards {
				gated++
				if !slices.Contains(codes, h.Code) {
					codes = append(codes, h.Code)
				}
			}
		}
	}

	return gated, codes
}

func (r planReport) counts() (apply, revert int) {
	for _, p := range r.items {
		switch {
		case p.skipped:
		case p.Direction == engine.DirectionDown:
			revert++
		default:
			apply++
		}
	}

	return apply, revert
}

func effectivePhase(p planItem, st engine.Statement) string {
	if st.Phase != "" {
		return st.Phase
	}

	return p.phase
}

func (r planReport) pauses() bool {
	if r.rollout != controlplane.RolloutExpandContract {
		return false
	}
	for _, p := range r.items {
		if p.skipped {
			continue
		}
		for _, st := range p.Statements {
			if effectivePhase(p, st) == engine.PhaseContract {
				return true
			}
		}
	}

	return false
}

func (r planReport) verdict(m markup) string {
	if r.nothing != "" {
		return m.glyph("ℹ️") + m.bold("No migration yet.") + " " + r.nothing + ", so there is nothing to plan."
	}
	if !r.live {
		return m.glyph("ℹ️") + m.bold("Offline plan.") + " Both sides of every migration in the directory, as written; no database was" +
			" consulted, so nothing here says what is pending."
	}
	apply, _ := r.counts()
	if apply == 0 {
		return m.glyph("✅") + m.bold("Nothing to apply.") + " " + r.stateLine(m)
	}
	mark := "🚀"
	if gated, _ := r.hazardGate(); gated > 0 {
		mark = "⚠️"
	}
	line := m.glyph(mark) + m.bold(fmt.Sprintf("%s will be applied to %s.", count(apply, "migration"), m.code(r.target)))
	if r.pauses() {
		line += " " + r.twoHalves(m)
	}

	return line
}

func (r planReport) twoHalves(m markup) string {
	out := "This runs in two halves. Now, godwit applies only what adds: new columns, indexes and constraints," +
		" which the application it is already running does not have to know about."
	if r.expands() {
		out += " A generated column change writes into a new column and holds the two in step with a trigger," +
			" so writes never stop while it fills."
	}

	return out + " Nothing is renamed and nothing is dropped yet. When you confirm, godwit runs the rest: the" +
		" renames, and the drops it held back. Until you confirm, the old columns are still there holding the data" +
		" you started with, and that is the way back. The run waits at " + m.code("awaiting_contract") +
		"; confirm it with " + m.code("godwit confirm") + " on the pull request."
}

func (r planReport) expands() bool {
	for _, p := range r.items {
		if p.expanded && !p.skipped {
			return true
		}
	}

	return false
}

func (r planReport) state() (applied int, newest int64) {
	if o := r.observed; o != nil {
		return int(o.AppliedCount), o.NewestApplied
	}
	for _, p := range r.items {
		if !p.applied || p.withheld {
			continue
		}
		applied++
		if !p.Migration.Repeatable {
			newest = max(newest, p.Migration.Version)
		}
	}

	return applied, newest
}

func (r planReport) stateLine(m markup) string {
	applied, newest := r.state()
	if newest == 0 {
		return fmt.Sprintf("%s already has every migration this plan covers.", m.code(r.target))
	}

	return fmt.Sprintf("%s is at %d (%s).", m.code(r.target), newest, count(applied, "migration"))
}

func (r planReport) notices(m markup) []string {
	if !r.live || r.nothing != "" {
		return nil
	}
	var out []string
	if !r.validated {
		line := m.glyph("⚠️") + m.bold("Not validated.") + " These statements were never replayed on a scratch" +
			" database, so nothing has proved they apply."
		if r.schema() {
			line += " Nothing read the schema they leave behind either, so what follows is the SQL the run would" +
				" execute rather than what it does to the database."
		}
		out = append(out, line)
	}
	if o := r.observed; o != nil {
		if l := ignoredLine(o.IgnoredTables, m); l != "" {
			out = append(out, l)
		}
	}

	return out
}

func (r planReport) footerLines(m markup) []string {
	if r.nothing != "" {
		return nil
	}
	gated, codes := r.hazardGate()
	apply, revert := r.counts()
	var out []string
	if gated > 0 {
		ack := "--ack " + strings.Join(codes, ",")
		out = append(out, fmt.Sprintf("%s%s on what this run would execute: take the recipe printed with it,"+
			" or accept the risk with %s (%s on a pull request).",
			m.glyph("⚠️"), count(gated, "hazard"), m.code(ack), m.code("godwit apply "+ack)))
	}
	if r.describes() {
		return append(out, r.objectFooter())
	}

	return append(out, fmt.Sprintf("Plan: %d to apply, %d to revert, %d hazard(s) to acknowledge", apply, revert, gated))
}

func (r planReport) acts() bool {
	for _, p := range r.items {
		if !p.skipped && p.describable() {
			return true
		}
	}

	return false
}

// statusVerdict is the whole of a commit status description, which GitHub cuts at 140 characters.
func (r planReport) statusVerdict() string {
	if r.nothing != "" {
		return "no migration yet"
	}
	if !r.live {
		return "offline plan; no target was consulted"
	}
	apply, revert := r.counts()
	var parts []string
	if apply > 0 {
		parts = append(parts, fmt.Sprintf("%d to apply", apply))
	}
	if revert > 0 {
		parts = append(parts, fmt.Sprintf("%d to revert", revert))
	}
	if len(parts) == 0 {
		parts = append(parts, "nothing to apply")
	}
	if gated, _ := r.hazardGate(); gated > 0 {
		parts = append(parts, count(gated, "hazard")+" to acknowledge")
	}

	return strings.Join(parts, ", ")
}

// identityLines is what a terminal reader has nowhere else to read; markdown keeps the id in an HTML comment.
func (r planReport) identityLines() []string {
	if !r.live {
		return nil
	}
	var lines []string
	if r.planID != "" {
		lines = append(lines, "plan: "+r.planID)
	}
	if r.stored != nil {
		lines = append(lines, r.stored.lines()...)
	}

	return lines
}

// notRun leaves out what the target already has: there is nothing left to do about it.
func (r planReport) notRun() []planItem {
	out := make([]planItem, 0, len(r.items))
	for _, p := range r.items {
		if p.skipped && !p.inHistory() {
			out = append(out, p)
		}
	}

	return out
}

func (p planItem) inHistory() bool {
	return p.applied && !p.Migration.Repeatable && !p.withheld
}

func (p planItem) skipReason() string {
	switch {
	case p.withheld:
		return "held back by --to"
	case p.applied:
		return "unchanged since it was last applied"
	case p.note != "":
		return p.note
	}

	return "the run would not execute its body"
}

func (p planItem) phases(pauses bool) []string {
	var out []string
	if !pauses {
		return nil
	}
	for _, st := range p.Statements {
		if ph := effectivePhase(p, st); ph != "" && !slices.Contains(out, ph) {
			out = append(out, ph)
		}
	}
	return out
}

func (p planItem) summary(pauses bool) string {
	s := count(len(p.Statements), "statement")
	switch ph := p.phases(pauses); len(ph) {
	case 0:
	case 1:
		s += ", " + ph[0] + " phase"
	default:
		s += ", " + strings.Join(ph, " then ") + " phases"
	}
	if p.expanded {
		s += ", written by a directive"
	}
	switch {
	case p.alreadyApplied:
		s += "; its effect is already on the database, so the run records it without executing"
	case p.note != "":
		s += "; " + p.note
	}

	return s
}

func runsIn(st engine.Statement) string {
	switch {
	case st.Assert != nil:
		return "runs as a check"
	case st.Batch != nil:
		return "runs in batches"
	case st.NoTx:
		return "runs outside a transaction"
	default:
		return "runs inside a transaction"
	}
}

func statementFacts(i int, p planItem, st engine.Statement, m markup, pauses bool) string {
	s := m.code(fmt.Sprintf("statement %d", i)) + ", " + runsIn(st)
	if ph := effectivePhase(p, st); pauses && ph != "" && ph != p.phase {
		s += ", " + ph + " phase"
	}
	if b := st.Batch; b != nil {
		s += fmt.Sprintf(" over %s (%s), %d rows per transaction%s", b.Key, b.KeyKind, b.Size, pauseSuffix(b.Pause))
	}
	if a := st.Assert; a != nil {
		s += ", the result must be " + a.String()
	}

	return s
}

type markup struct{ bold, code, glyph func(string) string }

func plain(s string) string {
	return s
}

func none(string) string {
	return ""
}

func lead(s string) string {
	return s + " "
}

var (
	terminal = markup{plain, plain, none}
	markdown = markup{func(s string) string { return "**" + s + "**" }, func(s string) string { return "`" + s + "`" }, lead}
)

func were(n int) string {
	if n == 1 {
		return "was"
	}

	return "were"
}

func count(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}

	return fmt.Sprintf("%d %ss", n, noun)
}

type planObservation struct {
	HistoryHash       string   `json:"history_hash"`
	SchemaFingerprint string   `json:"schema_fingerprint"`
	AppliedCount      int32    `json:"applied_count"`
	NewestApplied     int64    `json:"newest_applied"`
	At                string   `json:"at"`
	IgnoredTables     []string `json:"ignored_tables,omitempty"`
}

type storedPlan struct {
	State           string   `json:"state"`
	RunID           string   `json:"run_id,omitempty"`
	SupersededBy    string   `json:"superseded_by,omitempty"`
	CreatedBy       string   `json:"created_by"`
	CreatedAt       string   `json:"created_at"`
	Source          string   `json:"source,omitempty"`
	Acked           []string `json:"acknowledged_hazards,omitempty"`
	AllowOutOfOrder bool     `json:"allow_out_of_order,omitempty"`
}

func (p storedPlan) lines() []string {
	state := "state: " + p.State
	switch {
	case p.RunID != "":
		state += " (run " + p.RunID + ")"
	case p.SupersededBy != "":
		state += " (by " + p.SupersededBy + ")"
	}
	by := "by: " + p.CreatedBy + " at " + p.CreatedAt
	if p.Source != "" {
		by += ", source " + p.Source
	}
	if len(p.Acked) > 0 {
		by += ", acked " + strings.Join(p.Acked, ",")
	}
	if p.AllowOutOfOrder {
		by += ", out-of-order allowed"
	}

	return []string{state, by}
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
	stored    *storedPlan
	items     []planItem
	format    string
	nothing   string
}

func (r planReport) schema() bool {
	return r.format != config.PlanFormatStatements
}

func (r planReport) describes() bool {
	return r.schema() && r.acts()
}

func ignoredLine(tables []string, m markup) string {
	if len(tables) == 0 {
		return ""
	}

	return fmt.Sprintf("%s%s %s. godwit keeps them out of the schema and its drift; drop what nothing reads any more,"+
		" or set %s on the target to count them.", m.glyph("⚠️"), m.bold("Left behind by another migration tool:"),
		strings.Join(tables, ", "), m.code(controlplane.ConfigIgnoreAdopted+"=false"))
}

// withheldLine names what a version target left out, so the plan and the pull-request comment cannot be read as the whole set.
func (r planReport) withheldLine() string {
	ids := make([]string, 0, len(r.items))
	for _, p := range r.items {
		if p.withheld {
			ids = append(ids, p.Migration.ID())
		}
	}
	if len(ids) == 0 {
		return ""
	}

	return fmt.Sprintf("withheld: %d migration(s) in the directory this plan does not cover (%s)", len(ids), strings.Join(ids, ", "))
}

func (r planReport) driftBlock(heading, indent, open, close string, pal palette) string {
	if r.drift == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString(heading + "\n" + open)
	for _, l := range strings.Split(r.drift, "\n") {
		b.WriteString(indent + pal.diff(l) + "\n")
	}
	b.WriteString(close)

	return b.String()
}

var planFormats = map[string]func(io.Writer, planReport){
	"text":     writePlanText,
	"markdown": writePlanMarkdown,
	"json":     writePlanJSON,
}

type reportFlags struct {
	format     string
	planFormat string
}

func (f *reportFlags) register(cmd *cobra.Command, what string) {
	cmd.Flags().StringVar(&f.format, "format", "text", strings.TrimSpace(what+" output format: text, markdown or json"))
	cmd.Flags().StringVar(&f.planFormat, "plan-format", config.PlanFormatSchema,
		"what the report says: schema (what the migrations do to the database) or statements (the SQL the run would execute)")
	configKeys(cmd, "plan-format")
}

func (f *reportFlags) writer() (func(io.Writer, planReport), error) {
	write, ok := planFormats[f.format]
	if !ok {
		return nil, fmt.Errorf("unknown format %q (want text, markdown or json)", f.format)
	}
	if f.planFormat != config.PlanFormatSchema && f.planFormat != config.PlanFormatStatements {
		return nil, fmt.Errorf("unknown plan format %q (want schema or statements)", f.planFormat)
	}

	return func(w io.Writer, r planReport) {
		r.format = f.planFormat
		write(w, r)
	}, nil
}

func newPlanCmd() *cobra.Command {
	flags := &targetFlags{}
	remote := &clientFlags{}
	req := &godwitv1.PlanRunRequest{}
	report := &reportFlags{}
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
			write, err := report.writer()
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
			report := planReport{nothing: why, items: make([]planItem, 0, 2*len(migs))}
			for _, m := range migs {
				for _, dir := range directionsOf(m) {
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
	cmd.AddCommand(newPlanShowCmd())
	flags.register(cmd, false)
	remote.register(cmd)
	report.register(cmd, "")
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

func writePlanText(w io.Writer, r planReport) {
	pal := colors(w)
	fmt.Fprintln(w, r.verdict(terminal))
	if l := r.withheldLine(); l != "" {
		fmt.Fprintln(w, l)
	}
	for _, l := range r.notices(terminal) {
		fmt.Fprintln(w, l)
	}
	if r.describes() {
		fmt.Fprintf(w, "\n%s\n", actionsHeading)
	}
	for _, p := range r.items {
		if p.skipped {
			continue
		}
		writeMigrationText(w, r, p, pal)
	}
	if rows := r.notRun(); len(rows) > 0 {
		fmt.Fprintf(w, "\nnot executed by this run (%d):\n", len(rows))
		for _, p := range rows {
			fmt.Fprintf(w, "  %s  %s\n", p.Migration.ID(), p.skipReason())
		}
	}
	if b := r.driftBlock("\nchanges outside migrations:", "  ", "", "", pal); b != "" {
		fmt.Fprint(w, b)
	}
	if lines := r.strategy(terminal); len(lines) > 0 {
		fmt.Fprintf(w, "\n%s:\n", strategyHeading)
		for _, l := range lines {
			fmt.Fprintln(w, "  "+l)
		}
	}
	if lines := r.identityLines(); len(lines) > 0 {
		fmt.Fprintln(w, "\nplan details:")
		for _, l := range lines {
			fmt.Fprintln(w, "  "+l)
		}
	}
	footer := r.footerLines(terminal)
	if len(footer) == 0 {
		return
	}
	fmt.Fprintln(w)
	for _, l := range footer {
		fmt.Fprintln(w, l)
	}
}

func downSuffix(d engine.Direction) string {
	if d == engine.DirectionDown {
		return " (down)"
	}

	return ""
}

func writeMigrationText(w io.Writer, r planReport, p planItem, pal palette) {
	if r.describes() {
		fmt.Fprintf(w, "\n%s\n", p.schemaHeading())
	} else {
		fmt.Fprintf(w, "\n%s\n", pal.change(p.Direction, p.Migration.ID()+downSuffix(p.Direction)+"  "+p.summary(r.pauses())))
	}
	for _, d := range p.directives {
		fmt.Fprintln(w, "  "+d)
	}
	if r.describes() && p.describable() {
		writeSchemaText(w, p, pal)
	} else {
		writeIndented(w, "  ", p.effect, pal)
		if l := p.undescribed(); r.describes() && l != "" {
			fmt.Fprintln(w, "  "+l)
		}
		writeStatementsText(w, r, p)
	}
	for _, n := range p.notes {
		fmt.Fprintln(w, "  note: "+n)
	}
}

func writeStatementsText(w io.Writer, r planReport, p planItem) {
	for i, st := range p.Statements {
		fmt.Fprintln(w, "  "+statementFacts(i, p, st, terminal, r.pauses()))
		writeIndented(w, "      ", st.SQL+";", mono)
		for _, h := range st.Hazards {
			fmt.Fprintf(w, "      hazard %s: %s\n", h.Code, h.Detail)
			writeRecipeText(w, "        ", h.Recipe)
		}
	}
}

func writeIndented(w io.Writer, indent, body string, pal palette) {
	if body == "" {
		return
	}
	for _, l := range strings.Split(body, "\n") {
		fmt.Fprintln(w, indent+pal.diff(l))
	}
}

func pauseSuffix(d time.Duration) string {
	if d <= 0 {
		return ""
	}

	return ", pausing " + d.String()
}

func durationText(d time.Duration) string {
	if d <= 0 {
		return ""
	}

	return d.String()
}

func writePlanMarkdown(w io.Writer, r planReport) {
	fmt.Fprintf(w, "## godwit %s\n\n%s\n", r.kind(), r.verdict(markdown))
	if l := r.withheldLine(); l != "" {
		fmt.Fprintf(w, "\n**%s**\n", l)
	}
	for _, l := range r.notices(markdown) {
		fmt.Fprintf(w, "\n%s\n", l)
	}
	fmt.Fprint(w, r.changeList())
	fmt.Fprint(w, r.driftDetails())
	if r.describes() {
		fmt.Fprintf(w, "\n%s\n", actionsHeading)
	}
	for _, p := range r.items {
		if p.skipped {
			continue
		}
		writeMigrationMarkdown(w, r, p)
	}
	fmt.Fprint(w, r.notRunDetails())
	fmt.Fprint(w, r.strategyMarkdown())
	fmt.Fprint(w, r.detailsMarkdown())
	for _, l := range r.footerLines(markdown) {
		fmt.Fprintf(w, "\n%s\n", l)
	}
	fmt.Fprintln(w)
	if r.planID != "" {
		fmt.Fprintf(w, "<!-- godwit-plan-id: %s -->\n", r.planID)
	}
	if r.planKey != "" {
		fmt.Fprintf(w, "<!-- godwit-plan-key: %s -->\n", r.planKey)
	}
	fmt.Fprintf(w, "<!-- godwit-plan-verdict: %s -->\n", r.statusVerdict())
	if r.live {
		gated, _ := r.hazardGate()
		fmt.Fprintf(w, "<!-- godwit-plan-hazards: %d -->\n", gated)
	}
}

// changeList holds one-line summaries only: a SQL comment opens with --, which a diff fence renders as a deletion.
func (r planReport) changeList() string {
	var b strings.Builder
	for _, p := range r.items {
		if p.skipped {
			continue
		}
		fmt.Fprintf(&b, "%s\n", mono.change(p.Direction, p.Migration.ID()+downSuffix(p.Direction)+"  "+p.summary(r.pauses())))
	}
	if b.Len() == 0 {
		return ""
	}

	return "\n```diff\n" + b.String() + "```\n"
}

func writeMigrationMarkdown(w io.Writer, r planReport, p planItem) {
	fmt.Fprintf(w, "\n### `%s`%s\n", p.Migration.ID(), downSuffix(p.Direction))
	for _, d := range p.directives {
		fmt.Fprintf(w, "\n```sql\n%s\n```\n", d)
	}
	if r.describes() && p.describable() {
		writeSchemaMarkdown(w, p)
	} else {
		if p.effect != "" {
			fmt.Fprintf(w, "\n```diff\n%s\n```\n", p.effect)
		}
		if l := p.undescribed(); r.describes() && l != "" {
			fmt.Fprintf(w, "\n%s\n", l)
		}
		writeStatementsMarkdown(w, r, p)
	}
	for _, n := range p.notes {
		fmt.Fprintf(w, "\nnote: %s\n", n)
	}
}

func writeStatementsMarkdown(w io.Writer, r planReport, p planItem) {
	for i, st := range p.Statements {
		fmt.Fprintf(w, "\n%s\n\n```sql\n%s;\n```\n", statementFacts(i, p, st, markdown, r.pauses()), st.SQL)
		for _, h := range st.Hazards {
			fmt.Fprintf(w, "\n**%s** %s\n", h.Code, h.Detail)
			if h.Recipe != "" {
				fmt.Fprintf(w, "\n```sql\n%s\n```\n", h.Recipe)
			}
		}
	}
}

func (r planReport) driftDetails() string {
	if r.drift == "" {
		return ""
	}
	n := len(strings.Split(r.drift, "\n"))

	return fmt.Sprintf("\n<details><summary>%s on this database %s not made by a migration</summary>\n\n"+
		"```diff\n%s\n```\n\n</details>\n", count(n, "change"), were(n), r.drift)
}

func (r planReport) notRunDetails() string {
	rows := r.notRun()
	if len(rows) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n<details><summary>%s this run will not execute</summary>\n\n", count(len(rows), "migration"))
	b.WriteString("| Migration | Not executed because |\n|---|---|\n")
	for _, p := range rows {
		fmt.Fprintf(&b, "| `%s` | %s |\n", p.Migration.ID(), p.skipReason())
	}
	b.WriteString("\n</details>\n")

	return b.String()
}

func (r planReport) strategyMarkdown() string {
	lines := r.strategy(markdown)
	if len(lines) == 0 {
		return ""
	}

	return "\n<details><summary>" + strategyHeading + ": transactions, locks, and what a failure leaves behind" +
		"</summary>\n\n" + strings.Join(lines, "\n\n") + "\n\n</details>\n"
}

func (r planReport) detailsMarkdown() string {
	if !r.live || r.stored == nil {
		return ""
	}

	return "\n<details><summary>plan details</summary>\n\n```\n" + strings.Join(r.stored.lines(), "\n") +
		"\n```\n\n</details>\n"
}

func (r planReport) kind() string {
	if r.live && r.planID == "" {
		return "dry run"
	}

	return "plan"
}

type planJSON struct {
	Version    int64           `json:"version"`
	Name       string          `json:"name"`
	Repeatable bool            `json:"repeatable,omitempty"`
	Direction  string          `json:"direction"`
	Statements []statementJSON `json:"statements"`
}

type statementJSON struct {
	SQL     string       `json:"sql"`
	Mode    string       `json:"mode"`
	Phase   string       `json:"phase,omitempty"`
	Batch   *batchJSON   `json:"batch,omitempty"`
	Assert  *assertJSON  `json:"assert,omitempty"`
	Hazards []hazardJSON `json:"hazards"`
}

type assertJSON struct {
	Op    string `json:"op"`
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

type batchJSON struct {
	Key   string `json:"key"`
	Kind  string `json:"kind"`
	Size  int    `json:"size"`
	Pause string `json:"pause,omitempty"`
}

type hazardJSON struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
	Recipe string `json:"recipe,omitempty"`
}

type livePlanJSON struct {
	planJSON
	Applied        bool     `json:"applied"`
	Phase          string   `json:"phase"`
	AlreadyApplied bool     `json:"already_applied,omitempty"`
	Effect         string   `json:"effect,omitempty"`
	Note           string   `json:"note,omitempty"`
	Directives     []string `json:"directives,omitempty"`
	Expanded       bool     `json:"expanded,omitempty"`
	Notes          []string `json:"notes,omitempty"`
	Withheld       bool     `json:"withheld,omitempty"`
	Skipped        bool     `json:"skipped,omitempty"`
}

type dryRunJSON struct {
	Target     string           `json:"target"`
	Rollout    string           `json:"rollout"`
	Validated  bool             `json:"validated"`
	PlanID     string           `json:"plan_id,omitempty"`
	PlanKey    string           `json:"plan_key,omitempty"`
	Observed   *planObservation `json:"observed,omitempty"`
	Drift      string           `json:"drift,omitempty"`
	Stored     *storedPlan      `json:"stored,omitempty"`
	Migrations []livePlanJSON   `json:"migrations"`
}

func toPlanJSON(p engine.Plan) planJSON {
	pj := planJSON{
		Version: p.Migration.Version, Name: p.Migration.Name, Repeatable: p.Migration.Repeatable,
		Direction: string(p.Direction), Statements: []statementJSON{},
	}
	for _, st := range p.Statements {
		hazards := make([]hazardJSON, 0, len(st.Hazards))
		for _, h := range st.Hazards {
			hazards = append(hazards, hazardJSON{Code: h.Code, Detail: h.Detail, Recipe: h.Recipe})
		}
		sj := statementJSON{SQL: st.SQL, Mode: statementMode(st), Phase: st.Phase, Hazards: hazards}
		if b := st.Batch; b != nil {
			sj.Batch = &batchJSON{Key: b.Key, Kind: b.KeyKind, Size: b.Size, Pause: durationText(b.Pause)}
		}
		if a := st.Assert; a != nil {
			sj.Assert = &assertJSON{Op: a.Op, Kind: a.Kind, Value: a.Value}
		}
		pj.Statements = append(pj.Statements, sj)
	}

	return pj
}

func writePlanJSON(w io.Writer, r planReport) {
	var out any
	if r.live {
		live := dryRunJSON{
			Target: r.target, Rollout: r.rollout, Validated: r.validated, Migrations: []livePlanJSON{},
			PlanID: r.planID, PlanKey: r.planKey, Observed: r.observed, Drift: r.drift, Stored: r.stored,
		}
		for _, p := range r.items {
			live.Migrations = append(live.Migrations, livePlanJSON{
				planJSON: toPlanJSON(p.Plan), Applied: p.applied, Phase: p.phase,
				AlreadyApplied: p.alreadyApplied, Effect: p.effect, Note: p.note,
				Directives: p.directives, Expanded: p.expanded, Notes: p.notes, Withheld: p.withheld,
				Skipped: p.skipped,
			})
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

func statementMode(st engine.Statement) string {
	switch {
	case st.Assert != nil:
		return "assert"
	case st.Batch != nil:
		return "batch"
	case st.NoTx:
		return "no-tx"
	default:
		return "tx"
	}
}

// firstLine skips the expander's marker: a reader shown only that never sees the statement.
func firstLine(sql string) string {
	_, body := engine.SplitExpanded(sql)
	line, _, _ := strings.Cut(body, "\n")

	return line
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
