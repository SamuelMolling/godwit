package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/gen/godwit/v1/godwitv1connect"
	"github.com/SamuelMolling/godwit/internal/engine"
	"github.com/SamuelMolling/godwit/internal/ui/link"
)

type runReport struct {
	command string
	run     *godwitv1.Run
	applied []*godwitv1.RunMigration
	plan    planReport
	public  string
}

func (r runReport) kind() string {
	if r.command != "" {
		return r.command
	}

	return r.run.GetKind()
}

func (r runReport) state() godwitv1.RunState {
	return r.run.GetState()
}

func (r runReport) holding() bool {
	return r.state() == godwitv1.RunState_RUN_STATE_AWAITING_CONTRACT
}

// took is silent on a run that came back for its contract phase: created to finished there is the wait for
// the confirm, not the time the statements spent on the database.
func (r runReport) took() string {
	created, finished := r.run.GetCreatedAt(), r.run.GetFinishedAt()
	if created == nil || finished == nil || r.run.GetPhase() == engine.PhaseContract {
		return ""
	}
	d := finished.AsTime().Sub(created.AsTime()).Round(time.Millisecond)
	if d < 0 {
		return ""
	}

	return d.String()
}

func (r runReport) statements() (ran, held int) {
	for _, p := range r.plan.items {
		if p.skipped || p.alreadyApplied {
			continue
		}
		for _, st := range p.Statements {
			if r.holding() && effectivePhase(p, st) == engine.PhaseContract {
				held++

				continue
			}
			ran++
		}
	}

	return ran, held
}

func (r runReport) ledger(id string) *godwitv1.RunMigration {
	for _, m := range r.applied {
		if m.GetMigration() == id {
			return m
		}
	}

	return nil
}

func (r runReport) landed(id string) bool {
	return r.ledger(id) != nil
}

func (r runReport) tail(what string) string {
	d := r.took()
	switch {
	case what != "" && d != "":
		return " " + what + ", in " + d + "."
	case what != "":
		return " " + what + "."
	case d != "":
		return " It took " + d + "."
	}

	return ""
}

func (r runReport) verdict(m markup) string {
	target := m.code(r.run.GetTarget())
	switch {
	case r.holding():
		ran, held := r.statements()

		return m.glyph("⏸️") + m.bold("Expand applied to "+target+"; the contract phase is held.") +
			r.tail(fmt.Sprintf("%d of %d statements ran", ran, ran+held))
	case r.state() == godwitv1.RunState_RUN_STATE_FAILED:
		return m.glyph("❌") + m.bold("The run stopped part way on "+target+".") + r.tail(r.progress())
	case r.state() == godwitv1.RunState_RUN_STATE_NEEDS_ATTENTION:
		return m.glyph("❌") + m.bold("The run stopped on "+target+" and needs attention.") + r.tail(r.progress())
	case r.state() == godwitv1.RunState_RUN_STATE_SUCCEEDED:
		return r.applause(m, target)
	default:
		return m.glyph("ℹ️") + m.bold("The run is "+stateName(r.state())+" on "+target+".")
	}
}

func (r runReport) applause(m markup, target string) string {
	if len(r.applied) == 0 {
		return m.glyph("✅") + m.bold("Nothing was applied to "+target+".") +
			" Every migration the run carried was already on the database."
	}
	ran, _ := r.statements()
	what := ""
	if ran > 0 {
		what = count(ran, "statement")
	}

	return m.glyph("✅") + m.bold(count(len(r.applied), "migration")+" applied to "+target+".") + r.tail(what)
}

// progress is what a stopped run got through, in migrations, which is what the ledger can answer.
func (r runReport) progress() string {
	planned, _ := r.plan.counts()
	if planned == 0 {
		return count(len(r.applied), "migration") + " applied"
	}

	return fmt.Sprintf("%d of %d migrations applied", len(r.applied), planned)
}

// stoppedRe reads the position out of the run's error, which the executor wraps around every statement failure.
var stoppedRe = regexp.MustCompile(`statement (\d+) of (\S+) \((?:up|down)\)`)

// stopPoint is where a run gave up: index -1 when only the migration is known, item unset when the plan does not carry it.
type stopPoint struct {
	index     int
	migration string
	item      planItem
	statement engine.Statement
	found     bool
}

func (r runReport) stop() stopPoint {
	s := stopPoint{index: -1}
	if match := stoppedRe.FindStringSubmatch(r.run.GetError()); match != nil {
		s.index, _ = strconv.Atoi(match[1])
		s.migration = match[2]
	}
	for _, p := range r.plan.items {
		if p.skipped {
			continue
		}
		if s.migration == "" && !r.landed(p.Migration.ID()) {
			s.migration = p.Migration.ID()
		}
		if p.Migration.ID() != s.migration || s.index < 0 || s.index >= len(p.Statements) {
			continue
		}
		s.item, s.statement, s.found = p, p.Statements[s.index], true
	}

	return s
}

func (r runReport) broke() bool {
	return r.state() == godwitv1.RunState_RUN_STATE_FAILED || r.state() == godwitv1.RunState_RUN_STATE_NEEDS_ATTENTION
}

func (r runReport) links(m markup) string {
	parts := []string{"run " + linked(m, r.run.GetId(), link.Run(r.public, r.run.GetId()))}
	if id := r.run.GetPlanId(); id != "" {
		parts = append(parts, "plan "+linked(m, id, link.Plan(r.public, id)))
	}
	if short, href := commitOf(r.run.GetSource()); short != "" {
		parts = append(parts, "commit "+linked(m, short, href))
	}
	if at := stamp(r.run.GetFinishedAt()); at != "" {
		parts = append(parts, "finished "+m.code(at))
	}

	return strings.Join(parts, " · ")
}

func linked(m markup, text, href string) string {
	return m.href(m.code(text), href)
}

// commitOf reads the run's provenance, which the Action writes as <host>/<owner>/<repo>@<sha>[:<dir>].
var commitRe = regexp.MustCompile(`^([a-zA-Z0-9.-]+/[^/@\s]+/[^/@\s]+)@([0-9a-fA-F]{7,40})(?::|$)`)

func commitOf(source string) (short, href string) {
	match := commitRe.FindStringSubmatch(source)
	if match == nil {
		return "", ""
	}

	return match[2][:7], "https://" + match[1] + "/commit/" + match[2]
}

func (r runReport) outcomeList() string {
	stop := r.stop()
	var b strings.Builder
	for _, p := range r.plan.items {
		if p.skipped {
			continue
		}
		line := p.Migration.ID() + downSuffix(p.Direction) + "  " + p.summary(r.plan.pauses())
		switch entry := r.ledger(p.Migration.ID()); {
		case entry != nil && entry.GetHeld():
			b.WriteString(mono.change(p.Direction, line+", contract phase held") + "\n")
		case entry != nil:
			b.WriteString(mono.change(p.Direction, line) + "\n")
		case p.Migration.ID() == stop.migration && stop.index >= 0:
			fmt.Fprintf(&b, "  %s, stopped at statement %d\n", line, stop.index)
		default:
			b.WriteString("  " + line + "\n")
		}
	}
	if b.Len() == 0 {
		return ""
	}

	return "\n```diff\n" + b.String() + "```\n"
}

func (r runReport) heldStatements() []string {
	var out []string
	for _, p := range r.plan.items {
		if p.skipped {
			continue
		}
		for i, st := range p.Statements {
			if effectivePhase(p, st) == engine.PhaseContract {
				out = append(out, fmt.Sprintf("`%s` statement %d: %s", p.Migration.ID(), i, firstLine(st.SQL)))
			}
		}
	}

	return out
}

func (r runReport) changed() []planItem {
	var out []planItem
	for _, p := range r.plan.items {
		if !p.skipped && r.landed(p.Migration.ID()) {
			out = append(out, p)
		}
	}

	return out
}

func writeRunMarkdown(w io.Writer, r runReport) {
	fmt.Fprintf(w, "## godwit %s\n\n%s\n\n%s\n", r.kind(), r.verdict(markdown), r.links(markdown))
	fmt.Fprint(w, r.outcomeList())
	if r.broke() {
		writeStopMarkdown(w, r)
	}
	if r.holding() {
		writeHeldMarkdown(w, r)

		return
	}
	items := r.changed()
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(w, "\n<details><summary>what this changed on <code>%s</code></summary>\n", r.run.GetTarget())
	for _, p := range items {
		writeMigrationMarkdown(w, r.plan, p)
	}
	fmt.Fprint(w, "\n</details>\n")
}

func writeStopMarkdown(w io.Writer, r runReport) {
	stop := r.stop()
	if stop.migration != "" {
		fmt.Fprintf(w, "\n%s\n", stop.line(markdown))
	}
	if stop.found {
		fmt.Fprintf(w, "\n%s\n\n```sql\n%s;\n```\n",
			statementFacts(stop.index, stop.item, stop.statement, markdown, r.plan.pauses()), stop.statement.SQL)
	}
	if err := r.run.GetError(); err != "" {
		fmt.Fprintf(w, "\n```\n%s\n```\n", err)
	}
	fmt.Fprintf(w, "\n%s\n", r.plan.shape().onFailure())
}

func (s stopPoint) line(m markup) string {
	if s.index < 0 {
		return m.bold("It stopped in "+m.code(s.migration)) + ", which is not in the target's history."
	}
	out := m.bold(fmt.Sprintf("It stopped at statement %d of %s.", s.index, m.code(s.migration)))
	if s.index > 0 {
		out += fmt.Sprintf(" The %s before it in that migration committed and %s on the database;"+
			" the migration itself is not in the target's history.", count(s.index, "statement"), agree(s.index, "is", "are"))
	}

	return out
}

func writeHeldMarkdown(w io.Writer, r runReport) {
	held := r.heldStatements()
	if len(held) > 0 {
		fmt.Fprintf(w, "\n`godwit confirm` runs the %s godwit held back:\n\n- %s\n",
			count(len(held), "statement"), strings.Join(held, "\n- "))
	}
	if r.plan.pauses() {
		fmt.Fprintf(w, "\n%s\n", r.plan.twoHalves(markdown))
	}
}

func writeRunText(w io.Writer, r runReport) {
	fmt.Fprintln(w, r.verdict(terminal))
	fmt.Fprintln(w, r.links(terminal))
	for _, p := range r.plan.items {
		if p.skipped {
			continue
		}
		mark := "  "
		if r.landed(p.Migration.ID()) {
			mark = "+ "
		}
		fmt.Fprintln(w, mark+p.Migration.ID()+downSuffix(p.Direction)+"  "+p.summary(r.plan.pauses()))
	}
	if r.broke() {
		writeStopText(w, r)
	}
	if r.holding() {
		for _, l := range r.heldStatements() {
			fmt.Fprintln(w, "  held: "+strings.ReplaceAll(l, "`", ""))
		}
	}
}

func writeStopText(w io.Writer, r runReport) {
	stop := r.stop()
	if stop.migration != "" {
		fmt.Fprintln(w, stop.line(terminal))
	}
	if stop.found {
		fmt.Fprintln(w, statementFacts(stop.index, stop.item, stop.statement, terminal, r.plan.pauses()))
		writeIndented(w, "    ", stop.statement.SQL+";", mono)
	}
	if err := r.run.GetError(); err != "" {
		fmt.Fprintln(w, err)
	}
	fmt.Fprintln(w, r.plan.shape().onFailure())
}

var runFormats = map[string]func(io.Writer, runReport){
	"text":     writeRunText,
	"markdown": writeRunMarkdown,
}

func (f *reportFlags) runWriter() (func(io.Writer, runReport), error) {
	write, ok := runFormats[f.format]
	if !ok {
		return nil, fmt.Errorf("unknown format %q (want text or markdown)", f.format)
	}
	if err := f.checkPlanFormat(); err != nil {
		return nil, err
	}

	return func(w io.Writer, r runReport) {
		r.plan.format = f.planFormat
		write(w, r)
	}, nil
}

func newRunReportCmd() *cobra.Command {
	flags := &clientFlags{}
	report := &reportFlags{}
	var command string
	cmd := &cobra.Command{
		Use:   "report <run-id>",
		Short: "Report what one run did to its target: what it applied, what that changed, and where it stopped",
		Long: "Reads the run and the plan it was bound to, and renders the outcome the pull-request comment carries.\n" +
			"With GODWIT_PUBLIC_URL set, the run and its plan link to their pages in the UI; without it the report\n" +
			"names them and links nothing.",
		Args: cobra.ExactArgs(1),
		RunE: flags.runE(func(cmd *cobra.Command, client godwitv1connect.GodwitServiceClient, args []string) error {
			write, err := report.runWriter()
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
	report.registerRun(cmd)
	cmd.Flags().StringVar(&command, "command", "", "name the report heading carries (default: the run's kind)")

	return cmd
}

func loadRunReport(ctx context.Context, client godwitv1connect.GodwitServiceClient, id, command string) (runReport, *godwitv1.GetRunResponse, error) {
	got, err := client.GetRun(ctx, connect.NewRequest(&godwitv1.GetRunRequest{RunId: id}))
	if err != nil {
		return runReport{}, nil, err
	}
	r := runReport{
		command: command, run: got.Msg.Run, applied: got.Msg.Applied,
		plan:   planReport{live: true, target: got.Msg.Run.GetTarget(), rollout: got.Msg.Run.GetRollout()},
		public: os.Getenv("GODWIT_PUBLIC_URL"),
	}
	if planID := got.Msg.Run.GetPlanId(); planID != "" {
		plan, err := client.GetPlan(ctx, connect.NewRequest(&godwitv1.GetPlanRequest{PlanId: planID}))
		if err != nil {
			return runReport{}, nil, err
		}
		r.plan = planReportFromPlan(plan.Msg.Plan)
	}

	return r, got.Msg, nil
}
