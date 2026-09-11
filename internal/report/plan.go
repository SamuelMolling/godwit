// Package report renders what a plan or a run does to a database, in the words the terminal and the pull request both read.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/SamuelMolling/godwit/internal/config"
	"github.com/SamuelMolling/godwit/internal/controlplane"
	"github.com/SamuelMolling/godwit/internal/engine"
)

type item struct {
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

// Hazards is how many hazards stand on what the run would execute, which is what gates it.
func (r Plan) Hazards() int {
	gated, _ := r.hazardGate()

	return gated
}

func (r Plan) hazardGate() (gated int, codes []string) {
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

func (r Plan) counts() (apply, revert int) {
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

func effectivePhase(p item, st engine.Statement) string {
	if st.Phase != "" {
		return st.Phase
	}

	return p.phase
}

func (r Plan) pauses() bool {
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

func (r Plan) verdict(m markup) string {
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
	line := m.glyph(mark) + m.bold(fmt.Sprintf("%s will be applied to %s.", Count(apply, "migration"), m.code(r.target)))
	if r.pauses() {
		line += " " + r.twoHalves(m)
	}

	return line
}

func (r Plan) twoHalves(m markup) string {
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

func (r Plan) expands() bool {
	for _, p := range r.items {
		if p.expanded && !p.skipped {
			return true
		}
	}

	return false
}

func (r Plan) state() (applied int, newest int64) {
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

func (r Plan) stateLine(m markup) string {
	applied, newest := r.state()
	if newest == 0 {
		return fmt.Sprintf("%s already has every migration this plan covers.", m.code(r.target))
	}

	return fmt.Sprintf("%s is at %d (%s).", m.code(r.target), newest, Count(applied, "migration"))
}

func (r Plan) notices(m markup) []string {
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

func (r Plan) footerLines(m markup) []string {
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
			m.glyph("⚠️"), Count(gated, "hazard"), m.code(ack), m.code("godwit apply "+ack)))
	}
	if r.describes() {
		return append(out, r.objectFooter())
	}

	return append(out, fmt.Sprintf("Plan: %d to apply, %d to revert, %d hazard(s) to acknowledge", apply, revert, gated))
}

func (r Plan) acts() bool {
	for _, p := range r.items {
		if !p.skipped && p.describable() {
			return true
		}
	}

	return false
}

// Verdict is the whole of a commit status description, which GitHub cuts at 140 characters.
func (r Plan) Verdict() string {
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
		parts = append(parts, Count(gated, "hazard")+" to acknowledge")
	}

	return strings.Join(parts, ", ")
}

func (r Plan) identityLines() []string {
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

func (r Plan) notRun() []item {
	out := make([]item, 0, len(r.items))
	for _, p := range r.items {
		if p.skipped && !p.inHistory() {
			out = append(out, p)
		}
	}

	return out
}

func (p item) inHistory() bool {
	return p.applied && !p.Migration.Repeatable && !p.withheld
}

func (p item) skipReason() string {
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

func (p item) phases(pauses bool) []string {
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

func (p item) summary(pauses bool) string {
	s := Count(len(p.Statements), "statement")
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

func statementFacts(i int, p item, st engine.Statement, m markup, pauses bool) string {
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

type markup struct {
	bold, code, glyph func(string) string
	href              func(text, url string) string
}

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
	terminal = markup{plain, plain, none, trailing}
	markdown = markup{func(s string) string { return "**" + s + "**" }, func(s string) string { return "`" + s + "`" }, lead, anchor}
)

func trailing(text, url string) string {
	if url == "" {
		return text
	}

	return text + " " + url
}

func anchor(text, url string) string {
	if url == "" {
		return text
	}

	return "[" + text + "](" + url + ")"
}

func were(n int) string {
	if n == 1 {
		return "was"
	}

	return "were"
}

// Count is a countable noun with its number, pluralised.
func Count(n int, noun string) string {
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

// Plan is what a run would do to a target, as the service answered it or as the files alone say.
type Plan struct {
	live      bool
	target    string
	rollout   string
	validated bool
	planID    string
	planKey   string
	observed  *planObservation
	drift     string
	stored    *storedPlan
	items     []item
	format    string
	nothing   string
}

func (r Plan) schema() bool {
	return r.format != config.PlanFormatStatements
}

func (r Plan) describes() bool {
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

func (r Plan) withheldLine() string {
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

func (r Plan) driftBlock(heading, indent, open, close string, pal Palette) string {
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

var planFormats = map[string]func(io.Writer, Plan){
	"text":     writePlanText,
	"markdown": writePlanMarkdown,
	"json":     writePlanJSON,
}

func writePlanText(w io.Writer, r Plan) {
	pal := Colors(w)
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

func writeMigrationText(w io.Writer, r Plan, p item, pal Palette) {
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

func writeStatementsText(w io.Writer, r Plan, p item) {
	for i, st := range p.Statements {
		fmt.Fprintln(w, "  "+statementFacts(i, p, st, terminal, r.pauses()))
		writeIndented(w, "      ", st.SQL+";", Mono)
		for _, h := range st.Hazards {
			fmt.Fprintf(w, "      hazard %s: %s\n", h.Code, h.Detail)
			WriteRecipeText(w, "        ", h.Recipe)
		}
	}
}

func writeIndented(w io.Writer, indent, body string, pal Palette) {
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

func writePlanMarkdown(w io.Writer, r Plan) {
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
	fmt.Fprintf(w, "<!-- godwit-plan-verdict: %s -->\n", r.Verdict())
	if r.live {
		gated, _ := r.hazardGate()
		fmt.Fprintf(w, "<!-- godwit-plan-hazards: %d -->\n", gated)
	}
}

// changeList holds one-line summaries only: a SQL comment opens with --, which a diff fence renders as a deletion.
func (r Plan) changeList() string {
	var b strings.Builder
	for _, p := range r.items {
		if p.skipped {
			continue
		}
		fmt.Fprintf(&b, "%s\n", Mono.change(p.Direction, p.Migration.ID()+downSuffix(p.Direction)+"  "+p.summary(r.pauses())))
	}
	if b.Len() == 0 {
		return ""
	}

	return "\n```diff\n" + b.String() + "```\n"
}

func writeMigrationMarkdown(w io.Writer, r Plan, p item) {
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

func writeStatementsMarkdown(w io.Writer, r Plan, p item) {
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

func (r Plan) driftDetails() string {
	if r.drift == "" {
		return ""
	}
	n := len(strings.Split(r.drift, "\n"))

	return fmt.Sprintf("\n<details><summary>%s on this database %s not made by a migration</summary>\n\n"+
		"```diff\n%s\n```\n\n</details>\n", Count(n, "change"), were(n), r.drift)
}

func (r Plan) notRunDetails() string {
	rows := r.notRun()
	if len(rows) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n<details><summary>%s this run will not execute</summary>\n\n", Count(len(rows), "migration"))
	b.WriteString("| Migration | Not executed because |\n|---|---|\n")
	for _, p := range rows {
		fmt.Fprintf(&b, "| `%s` | %s |\n", p.Migration.ID(), p.skipReason())
	}
	b.WriteString("\n</details>\n")

	return b.String()
}

func (r Plan) strategyMarkdown() string {
	lines := r.strategy(markdown)
	if len(lines) == 0 {
		return ""
	}

	return "\n<details><summary>" + strategyHeading + ": transactions, locks, and what a failure leaves behind" +
		"</summary>\n\n" + strings.Join(lines, "\n\n") + "\n\n</details>\n"
}

func (r Plan) detailsMarkdown() string {
	if !r.live || r.stored == nil {
		return ""
	}

	return "\n<details><summary>plan details</summary>\n\n```\n" + strings.Join(r.stored.lines(), "\n") +
		"\n```\n\n</details>\n"
}

func (r Plan) kind() string {
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

func writePlanJSON(w io.Writer, r Plan) {
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

// WriteRecipeText prints a hazard recipe under indent, and nothing at all for a hazard that carries none.
func WriteRecipeText(w io.Writer, indent, recipe string) {
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

func firstLine(sql string) string {
	_, body := engine.SplitExpanded(sql)
	line, _, _ := strings.Cut(body, "\n")

	return line
}
