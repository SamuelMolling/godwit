package report

import (
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/internal/engine"
	"github.com/SamuelMolling/godwit/internal/ui/link"
)

// Run is what one run did to its target, read against the plan it was bound to.
type Run struct {
	command string
	run     *godwitv1.Run
	applied []*godwitv1.RunMigration
	plan    Plan
	public  string
}

func (r Run) kind() string {
	if r.command != "" {
		return r.command
	}

	return r.run.GetKind()
}

// State is what became of the run, which is what a check run's conclusion follows.
func (r Run) State() godwitv1.RunState {
	return r.run.GetState()
}

func (r Run) holding() bool {
	return r.State() == godwitv1.RunState_RUN_STATE_AWAITING_CONTRACT
}

// took is silent on a run that came back for its contract phase: created to finished there is the wait for
// the confirm, not the time the statements spent on the database.
func (r Run) took() string {
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

func (r Run) statements() (ran, held int) {
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

func (r Run) ledger(id string) *godwitv1.RunMigration {
	for _, m := range r.applied {
		if m.GetMigration() == id {
			return m
		}
	}

	return nil
}

func (r Run) landed(id string) bool {
	return r.ledger(id) != nil
}

func (r Run) tail(what string) string {
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

func (r Run) verdict(m markup) string {
	target := m.code(r.run.GetTarget())
	switch {
	case r.holding():
		ran, held := r.statements()

		return m.glyph("⏸️") + m.bold("Expand applied to "+target+"; the contract phase is held.") +
			r.tail(fmt.Sprintf("%d of %d statements ran", ran, ran+held))
	case r.State() == godwitv1.RunState_RUN_STATE_FAILED:
		return m.glyph("❌") + m.bold("The run stopped part way on "+target+".") + r.tail(r.progress())
	case r.State() == godwitv1.RunState_RUN_STATE_NEEDS_ATTENTION:
		return m.glyph("❌") + m.bold("The run stopped on "+target+" and needs attention.") + r.tail(r.progress())
	case r.State() == godwitv1.RunState_RUN_STATE_SUCCEEDED:
		return r.applause(m, target)
	default:
		return m.glyph("ℹ️") + m.bold("The run is "+StateName(r.State())+" on "+target+".")
	}
}

func (r Run) applause(m markup, target string) string {
	if len(r.applied) == 0 {
		return m.glyph("✅") + m.bold("Nothing was applied to "+target+".") +
			" Every migration the run carried was already on the database."
	}
	ran, _ := r.statements()
	what := ""
	if ran > 0 {
		what = Count(ran, "statement")
	}

	return m.glyph("✅") + m.bold(Count(len(r.applied), "migration")+" applied to "+target+".") + r.tail(what)
}

// progress is what a stopped run got through, in migrations, which is what the ledger can answer.
func (r Run) progress() string {
	planned, _ := r.plan.counts()
	if planned == 0 {
		return Count(len(r.applied), "migration") + " applied"
	}

	return fmt.Sprintf("%d of %d migrations applied", len(r.applied), planned)
}

// stoppedRe reads the position out of the run's error, which the executor wraps around every statement failure.
var stoppedRe = regexp.MustCompile(`statement (\d+) of (\S+) \((?:up|down)\)`)

// stopPoint is where a run gave up: index -1 when only the migration is known, item unset when the plan does not carry it.
type stopPoint struct {
	index     int
	migration string
	item      item
	statement engine.Statement
	found     bool
}

func (r Run) stop() stopPoint {
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

func (r Run) broke() bool {
	return r.State() == godwitv1.RunState_RUN_STATE_FAILED || r.State() == godwitv1.RunState_RUN_STATE_NEEDS_ATTENTION
}

func (r Run) links(m markup) string {
	parts := []string{"run " + linked(m, r.run.GetId(), link.Run(r.public, r.run.GetId()))}
	if id := r.run.GetPlanId(); id != "" {
		parts = append(parts, "plan "+linked(m, id, link.Plan(r.public, id)))
	}
	if short, href := commitOf(r.run.GetSource()); short != "" {
		parts = append(parts, "commit "+linked(m, short, href))
	}
	if at := Stamp(r.run.GetFinishedAt()); at != "" {
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

func (r Run) outcomeList() string {
	stop := r.stop()
	var b strings.Builder
	for _, p := range r.plan.items {
		if p.skipped {
			continue
		}
		line := p.Migration.ID() + downSuffix(p.Direction) + "  " + p.summary(r.plan.pauses())
		switch entry := r.ledger(p.Migration.ID()); {
		case entry != nil && entry.GetHeld():
			b.WriteString(Mono.change(p.Direction, line+", contract phase held") + "\n")
		case entry != nil:
			b.WriteString(Mono.change(p.Direction, line) + "\n")
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

func (r Run) heldStatements() []string {
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

func (r Run) changed() []item {
	var out []item
	for _, p := range r.plan.items {
		if !p.skipped && r.landed(p.Migration.ID()) {
			out = append(out, p)
		}
	}

	return out
}

func writeRunMarkdown(w io.Writer, r Run) {
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

func writeStopMarkdown(w io.Writer, r Run) {
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
			" the migration itself is not in the target's history.", Count(s.index, "statement"), agree(s.index, "is", "are"))
	}

	return out
}

func writeHeldMarkdown(w io.Writer, r Run) {
	held := r.heldStatements()
	if len(held) > 0 {
		fmt.Fprintf(w, "\n`godwit confirm` runs the %s godwit held back:\n\n- %s\n",
			Count(len(held), "statement"), strings.Join(held, "\n- "))
	}
	if r.plan.pauses() {
		fmt.Fprintf(w, "\n%s\n", r.plan.twoHalves(markdown))
	}
}

func writeRunText(w io.Writer, r Run) {
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

func writeStopText(w io.Writer, r Run) {
	stop := r.stop()
	if stop.migration != "" {
		fmt.Fprintln(w, stop.line(terminal))
	}
	if stop.found {
		fmt.Fprintln(w, statementFacts(stop.index, stop.item, stop.statement, terminal, r.plan.pauses()))
		writeIndented(w, "    ", stop.statement.SQL+";", Mono)
	}
	if err := r.run.GetError(); err != "" {
		fmt.Fprintln(w, err)
	}
	fmt.Fprintln(w, r.plan.shape().onFailure())
}

var runFormats = map[string]func(io.Writer, Run){
	"text":     writeRunText,
	"markdown": writeRunMarkdown,
}

// StateName is a run state in the words the reports and the tables use.
func StateName(s godwitv1.RunState) string {
	return strings.ToLower(strings.TrimPrefix(s.String(), "RUN_STATE_"))
}
