package report

import (
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/SamuelMolling/godwit/internal/engine"
)

const actionsHeading = "godwit will perform the following actions:"

var opGlyph = map[string]string{engine.OpCreate: "+", engine.OpDestroy: "-", engine.OpUpdate: "~"}

func hazardNote(h engine.Hazard) string {
	return "# " + h.Short() + " (" + h.Code + ")"
}

type placedHazards struct {
	onAttr   map[string][]engine.Hazard
	onObject map[string][]engine.Hazard
	loose    []engine.Hazard
}

func placeHazards(p item) placedHazards {
	out := placedHazards{onAttr: map[string][]engine.Hazard{}, onObject: map[string][]engine.Hazard{}}
	for _, st := range p.Statements {
		for _, h := range st.Hazards {
			out.place(p.changes, h)
		}
	}

	return out
}

func (ph *placedHazards) place(changes []engine.ObjectChange, h engine.Hazard) {
	for _, c := range changes {
		if !namesObject(h.Object, c) {
			continue
		}
		if slices.ContainsFunc(c.Attrs, func(a engine.AttrChange) bool { return a.Name == h.Attribute }) {
			key := c.Ref() + "\x00" + h.Attribute
			ph.onAttr[key] = append(ph.onAttr[key], h)

			return
		}
		ph.onObject[c.Ref()] = append(ph.onObject[c.Ref()], h)

		return
	}
	ph.loose = append(ph.loose, h)
}

func namesObject(ref string, c engine.ObjectChange) bool {
	if ref == "" {
		return false
	}
	if strings.Contains(ref, ".") {
		return ref == c.Ref()
	}

	return ref == c.Name
}

type blockLine struct {
	op   string
	pad  int
	text string
}

func schemaBlock(c engine.ObjectChange, ph placedHazards) []blockLine {
	var out []blockLine
	for _, h := range ph.onObject[c.Ref()] {
		out = append(out, blockLine{pad: 2, text: hazardNote(h)})
	}
	head := c.Kind + " " + quoteObject(c)
	attrs := shown(c)
	if len(attrs) == 0 && c.Unchanged == 0 {
		return append(out, blockLine{op: c.Op, pad: 2, text: head})
	}
	out = append(out, blockLine{op: c.Op, pad: 2, text: head + " {"})
	width := 0
	for _, a := range attrs {
		width = max(width, len(a.Name))
	}
	for _, a := range attrs {
		text := fmt.Sprintf("%-*s = %s", width, a.Name, attrValueText(a))
		for _, h := range ph.onAttr[c.Ref()+"\x00"+a.Name] {
			text += " " + hazardNote(h)
		}
		out = append(out, blockLine{op: a.Op, pad: 6, text: text})
	}
	if c.Unchanged > 0 {
		out = append(out, blockLine{pad: 8, text: fmt.Sprintf("# (%s hidden)", Count(c.Unchanged, "unchanged attribute"))})
	}

	return append(out, blockLine{pad: 4, text: "}"})
}

func terminalLine(l blockLine, pal Palette) string {
	if l.op == "" {
		return strings.Repeat(" ", l.pad) + l.text
	}

	return pal.op(l.op, strings.Repeat(" ", l.pad)+opGlyph[l.op]+" "+l.text)
}

// diffLine leads with the marker: GitHub highlights a fenced line only when the marker is its first character.
func diffLine(l blockLine) string {
	if l.op == "" {
		return strings.Repeat(" ", l.pad) + l.text
	}

	return opGlyph[l.op] + strings.Repeat(" ", l.pad+1) + l.text
}

func shown(c engine.ObjectChange) []engine.AttrChange {
	switch c.Kind {
	case engine.KindView, engine.KindMatView:
		return changedOnly(c.Attrs)
	case engine.KindFunction, engine.KindProcedure:
		return maskBody(c.Attrs)
	default:
		return c.Attrs
	}
}

func changedOnly(attrs []engine.AttrChange) []engine.AttrChange {
	var out []engine.AttrChange
	for _, a := range attrs {
		if a.Op == engine.OpUpdate {
			out = append(out, engine.AttrChange{Op: a.Op, Name: a.Name, Old: []string{"(changed)"}, New: []string{"(changed)"}})
		}
	}

	return out
}

func maskBody(attrs []engine.AttrChange) []engine.AttrChange {
	out := make([]engine.AttrChange, 0, len(attrs))
	for _, a := range attrs {
		switch {
		case a.Name != engine.BodyAttr:
			out = append(out, a)
		case a.Op == engine.OpUpdate:
			out = append(out, engine.AttrChange{Op: a.Op, Name: a.Name, Old: []string{"(changed)"}, New: []string{"(changed)"}})
		}
	}

	return out
}

func quoteObject(c engine.ObjectChange) string {
	if c.Kind == engine.KindFunction || c.Kind == engine.KindProcedure {
		return c.Ref()
	}
	if c.Schema == "" {
		return `"` + c.Name + `"`
	}

	return `"` + c.Schema + `"."` + c.Name + `"`
}

func attrValueText(a engine.AttrChange) string {
	if a.Op == engine.OpUpdate {
		if slices.Equal(a.Old, a.New) {
			return strings.Join(a.New, " ")
		}

		return strings.Join(a.Old, " ") + " -> " + strings.Join(a.New, " ")
	}
	if a.Op == engine.OpDestroy {
		return strings.Join(a.Old, " ")
	}

	return strings.Join(a.New, " ")
}

func recipeBlocks(p item) []engine.Hazard {
	var out []engine.Hazard
	for _, st := range p.Statements {
		for _, h := range st.Hazards {
			if h.Recipe != "" && !slices.ContainsFunc(out, func(o engine.Hazard) bool { return o.Code == h.Code }) {
				out = append(out, h)
			}
		}
	}

	return out
}

func (p item) describable() bool {
	return len(p.changes) > 0
}

func (p item) undescribed() string {
	if p.effect != "" {
		return ""
	}
	if reason := p.Opaque(); reason != "" {
		return "godwit cannot describe what this one does to the database (" + reason + "), so the statements it runs" +
			" are below instead."
	}

	return "godwit cannot describe what this one does to the database: the schema before and after it is the same," +
		" so what it changes is something the snapshot does not cover — a grant, row-level security, a comment." +
		" The statements it runs are below instead."
}

func (p item) schemaHeading() string {
	out := "  # " + p.Migration.ID() + downSuffix(p.Direction)
	var about []string
	if p.expanded {
		about = append(about, "written by a directive")
	}
	switch {
	case p.alreadyApplied:
		about = append(about, "already on the database, so the run records it without executing")
	case p.note != "":
		about = append(about, p.note)
	}
	if len(about) > 0 {
		out += "  (" + strings.Join(about, "; ") + ")"
	}

	return out
}

func (r Plan) objectCounts() (add, change, destroy int) {
	for _, p := range r.items {
		if p.skipped {
			continue
		}
		for _, c := range p.changes {
			switch c.Op {
			case engine.OpCreate:
				add++
			case engine.OpDestroy:
				destroy++
			default:
				change++
			}
		}
	}

	return add, change, destroy
}

func (r Plan) objectFooter() string {
	add, change, destroy := r.objectCounts()

	return fmt.Sprintf("Plan: %d to add, %d to change, %d to destroy.", add, change, destroy)
}

func blockLines(p item) []blockLine {
	ph := placeHazards(p)
	var out []blockLine
	for _, h := range ph.loose {
		out = append(out, blockLine{pad: 2, text: hazardNote(h)})
	}
	for _, c := range p.changes {
		out = append(out, schemaBlock(c, ph)...)
	}

	return out
}

func writeSchemaText(w io.Writer, p item, pal Palette) {
	for _, l := range blockLines(p) {
		fmt.Fprintln(w, terminalLine(l, pal))
	}
	for _, h := range recipeBlocks(p) {
		fmt.Fprintf(w, "    recipe for %s:\n", h.Code)
		WriteRecipeText(w, "      ", h.Recipe)
	}
}

func writeSchemaMarkdown(w io.Writer, p item) {
	lines := make([]string, 0, len(p.changes))
	for _, l := range blockLines(p) {
		lines = append(lines, diffLine(l))
	}
	fmt.Fprintf(w, "\n```diff\n%s\n```\n", strings.Join(lines, "\n"))
	for _, h := range recipeBlocks(p) {
		fmt.Fprintf(w, "\n<details><summary>%s recipe</summary>\n\n```sql\n%s\n```\n\n</details>\n", h.Code, h.Recipe)
	}
}
