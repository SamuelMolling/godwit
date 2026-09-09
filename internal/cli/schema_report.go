package cli

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

func placeHazards(p planItem) placedHazards {
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

func schemaBlock(c engine.ObjectChange, ph placedHazards, indent string, pal palette) []string {
	var out []string
	for _, h := range ph.onObject[c.Ref()] {
		out = append(out, indent+hazardNote(h))
	}
	head := indent + opGlyph[c.Op] + " " + c.Kind + " " + quoteObject(c)
	attrs := shown(c)
	if len(attrs) == 0 && c.Unchanged == 0 {
		return append(out, pal.op(c.Op, head))
	}
	out = append(out, pal.op(c.Op, head+" {"))
	width := 0
	for _, a := range attrs {
		width = max(width, len(a.Name))
	}
	for _, a := range attrs {
		line := fmt.Sprintf("%s%s %-*s = %s", indent+"    ", opGlyph[a.Op], width, a.Name, attrValueText(a))
		for _, h := range ph.onAttr[c.Ref()+"\x00"+a.Name] {
			line += " " + hazardNote(h)
		}
		out = append(out, pal.op(a.Op, line))
	}
	if c.Unchanged > 0 {
		out = append(out, fmt.Sprintf("%s      # (%s hidden)", indent, count(c.Unchanged, "unchanged attribute")))
	}

	return append(out, indent+"  }")
}

func shown(c engine.ObjectChange) []engine.AttrChange {
	if c.Kind != engine.KindView && c.Kind != engine.KindMatView {
		return c.Attrs
	}
	var out []engine.AttrChange
	for _, a := range c.Attrs {
		if a.Op == engine.OpUpdate {
			out = append(out, engine.AttrChange{Op: a.Op, Name: a.Name, Old: []string{"(changed)"}, New: []string{"(changed)"}})
		}
	}

	return out
}

func quoteObject(c engine.ObjectChange) string {
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

func recipeBlocks(p planItem) []engine.Hazard {
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

func (p planItem) describable() bool {
	return len(p.changes) > 0
}

func (p planItem) undescribed() string {
	if p.effect != "" {
		return ""
	}
	if reason := p.Opaque(); reason != "" {
		return "  # a schema snapshot cannot see what this does (" + reason + "); the statements it runs are below"
	}

	return "  # no schema change was recorded for this one; the statements it runs are below"
}

func (p planItem) schemaHeading() string {
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

func (r planReport) objectCounts() (add, change, destroy int) {
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

func (r planReport) objectFooter() string {
	add, change, destroy := r.objectCounts()

	return fmt.Sprintf("Plan: %d to add, %d to change, %d to destroy.", add, change, destroy)
}

func writeSchemaText(w io.Writer, p planItem, pal palette) {
	ph := placeHazards(p)
	for _, h := range ph.loose {
		fmt.Fprintln(w, "  "+hazardNote(h))
	}
	for _, c := range p.changes {
		for _, l := range schemaBlock(c, ph, "  ", pal) {
			fmt.Fprintln(w, l)
		}
	}
	for _, h := range recipeBlocks(p) {
		fmt.Fprintf(w, "    recipe for %s:\n", h.Code)
		writeRecipeText(w, "      ", h.Recipe)
	}
}

func writeSchemaMarkdown(w io.Writer, p planItem) {
	ph := placeHazards(p)
	var lines []string
	for _, h := range ph.loose {
		lines = append(lines, hazardNote(h))
	}
	for _, c := range p.changes {
		lines = append(lines, schemaBlock(c, ph, "", mono)...)
	}
	fmt.Fprintf(w, "\n```diff\n%s\n```\n", strings.Join(lines, "\n"))
	for _, h := range recipeBlocks(p) {
		fmt.Fprintf(w, "\n<details><summary>%s recipe</summary>\n\n```sql\n%s\n```\n\n</details>\n", h.Code, h.Recipe)
	}
}
