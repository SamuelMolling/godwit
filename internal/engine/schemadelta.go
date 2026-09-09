package engine

import (
	"slices"
	"sort"
	"strings"
)

// Operations a schema change carries.
const (
	OpCreate  = "create"
	OpUpdate  = "update"
	OpDestroy = "destroy"
)

// Object kinds a schema change describes.
const (
	KindTable     = "table"
	KindIndex     = "index"
	KindSequence  = "sequence"
	KindEnum      = "enum"
	KindView      = "view"
	KindMatView   = "materialized view"
	KindFunction  = "function"
	KindProcedure = "procedure"
)

// BodyAttr is the attribute a function or procedure keeps its body under, as a digest rather than the source.
const BodyAttr = "body"

// AttrChange is one column, constraint or property of an object the change touches.
type AttrChange struct {
	Op   string   `json:"op"`
	Name string   `json:"name"`
	Old  []string `json:"old,omitempty"`
	New  []string `json:"new,omitempty"`
}

// ObjectChange is one schema object a migration creates, changes or destroys.
type ObjectChange struct {
	Op        string       `json:"op"`
	Kind      string       `json:"kind"`
	Schema    string       `json:"schema"`
	Name      string       `json:"name"`
	Attrs     []AttrChange `json:"attrs,omitempty"`
	Unchanged int          `json:"unchanged,omitempty"`
}

// Ref is the qualified object, the way a hazard names it.
func (c ObjectChange) Ref() string {
	if c.Schema == "" {
		return c.Name
	}

	return c.Schema + "." + c.Name
}

type objectKey struct{ kind, schema, name string }

type attrValue struct {
	name  string
	order int
	parts []string
}

type object struct {
	present bool
	attrs   map[string]attrValue
}

func snapshotObjects(definition string) map[objectKey]*object {
	out := map[objectKey]*object{}
	for _, line := range strings.Split(definition, "\n") {
		kind, rest, ok := strings.Cut(line, " ")
		if ok {
			readSnapshotLine(out, kind, rest)
		}
	}

	return out
}

func readSnapshotLine(out map[objectKey]*object, kind, rest string) {
	ref, body, _ := strings.Cut(rest, " ")
	switch kind {
	case "table":
		at(out, tableKey(ref)).present = true
	case "column":
		owner, name := split(ref)
		at(out, tableKey(owner)).put("column", name, 0, columnParts(body))
	case "constraint":
		owner, name := split(ref)
		at(out, tableKey(owner)).put("constraint", name, 1, []string{body})
	case "trigger":
		owner, name := split(ref)
		at(out, tableKey(owner)).put("trigger", name, 2, []string{body})
	case "function":
		routine(out, KindFunction, rest)
	case "procedure":
		routine(out, KindProcedure, rest)
	case "index":
		single(out, KindIndex, ref, "definition", body)
	case "sequence":
		sequence(out, ref, body)
	case "type":
		single(out, KindEnum, ref, "values", strings.TrimPrefix(body, "enum "))
	case "view":
		single(out, KindView, ref, "definition", body)
	case "matview":
		single(out, KindMatView, ref, "definition", body)
	}
}

func tableKey(ref string) objectKey {
	schema, name := split(ref)

	return objectKey{KindTable, schema, name}
}

func single(out map[objectKey]*object, kind, ref, name, body string) {
	schema, obj := split(ref)
	o := at(out, objectKey{kind, schema, obj})
	o.present = true
	o.put("", name, 0, []string{body})
}

// routine keys on the argument list so overloads differ, and reads returns last: it is the value with spaces.
func routine(out map[objectKey]*object, kind, rest string) {
	ident, body, _ := strings.Cut(rest, ") ")
	head, args, _ := strings.Cut(ident, "(")
	schema, name := split(head)
	o := at(out, objectKey{kind, schema, name + "(" + args + ")"})
	o.present = true
	props, returns, ok := strings.Cut(body, " returns=")
	for i, part := range strings.Fields(props) {
		prop, value, _ := strings.Cut(part, "=")
		o.put("", prop, i, []string{value})
	}
	if ok {
		o.put("", "returns", len(strings.Fields(props)), []string{returns})
	}
}

func sequence(out map[objectKey]*object, ref, body string) {
	schema, name := split(ref)
	o := at(out, objectKey{KindSequence, schema, name})
	o.present = true
	for i, part := range strings.Fields(body) {
		prop, value, ok := strings.Cut(part, "=")
		if !ok {
			prop, value = "type", part
		}
		o.put("", prop, i, []string{value})
	}
}

func split(ref string) (string, string) {
	i := strings.LastIndex(ref, ".")
	if i < 0 {
		return "", ref
	}

	return ref[:i], ref[i+1:]
}

func at(out map[objectKey]*object, k objectKey) *object {
	o, ok := out[k]
	if !ok {
		o = &object{attrs: map[string]attrValue{}}
		out[k] = o
	}

	return o
}

func (o *object) put(kind, name string, order int, parts []string) {
	o.attrs[kind+"\x00"+name] = attrValue{name: name, order: order, parts: parts}
}

func columnParts(body string) []string {
	typ, rest, _ := strings.Cut(body, " null=")
	null, def, _ := strings.Cut(rest, " default=")
	parts := []string{typ, "NOT NULL"}
	if null == "YES" {
		parts[1] = "NULL"
	}
	if def != "<none>" && def != "" {
		parts = append(parts, "DEFAULT "+def)
	}

	return parts
}

var kindRank = []string{
	KindTable, KindIndex, KindSequence, KindEnum, KindView, KindMatView, KindFunction, KindProcedure,
}

// SchemaChanges is what a migration did to the schema, read from the snapshots taken around it.
func SchemaChanges(before, after string) []ObjectChange {
	from, to := snapshotObjects(before), snapshotObjects(after)
	keys := make([]objectKey, 0, len(to))
	for k := range to {
		keys = append(keys, k)
	}
	for k := range from {
		if _, ok := to[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return less(keys[i], keys[j]) })

	out := make([]ObjectChange, 0, len(keys))
	for _, k := range keys {
		if c, ok := objectChange(k, from[k], to[k]); ok {
			out = append(out, c)
		}
	}

	return out
}

func less(a, b objectKey) bool {
	switch {
	case a.kind != b.kind:
		return slices.Index(kindRank, a.kind) < slices.Index(kindRank, b.kind)
	case a.schema != b.schema:
		return a.schema < b.schema
	default:
		return a.name < b.name
	}
}

func objectChange(k objectKey, from, to *object) (ObjectChange, bool) {
	c := ObjectChange{Op: OpUpdate, Kind: k.kind, Schema: k.schema, Name: k.name}
	switch {
	case from == nil || !from.present:
		c.Op, c.Attrs = OpCreate, oneSided(to, OpCreate)
	case to == nil || !to.present:
		c.Op, c.Attrs = OpDestroy, oneSided(from, OpDestroy)
	default:
		c.Attrs, c.Unchanged = twoSided(from, to)
		if len(c.Attrs) == 0 {
			return ObjectChange{}, false
		}
	}

	return c, true
}

func oneSided(o *object, op string) []AttrChange {
	out := make([]sortable, 0, len(o.attrs))
	for key, v := range o.attrs {
		a := AttrChange{Op: op, Name: v.name, New: v.parts}
		if op == OpDestroy {
			a.Old, a.New = v.parts, nil
		}
		out = append(out, sortable{key: key, order: v.order, change: a})
	}

	return sorted(out)
}

func twoSided(from, to *object) ([]AttrChange, int) {
	var out []sortable
	unchanged := 0
	for key, now := range to.attrs {
		was, ok := from.attrs[key]
		switch {
		case !ok:
			out = append(out, sortable{key, now.order, AttrChange{Op: OpCreate, Name: now.name, New: now.parts}})
		case slices.Equal(was.parts, now.parts):
			unchanged++
		default:
			old, next := differing(was.parts, now.parts)
			out = append(out, sortable{key, now.order, AttrChange{Op: OpUpdate, Name: now.name, Old: old, New: next}})
		}
	}
	for key, was := range from.attrs {
		if _, ok := to.attrs[key]; !ok {
			out = append(out, sortable{key, was.order, AttrChange{Op: OpDestroy, Name: was.name, Old: was.parts}})
		}
	}

	return sorted(out), unchanged
}

func differing(was, now []string) ([]string, []string) {
	if len(was) != len(now) {
		return was, now
	}
	var old, next []string
	for i := range was {
		if was[i] != now[i] {
			old, next = append(old, was[i]), append(next, now[i])
		}
	}

	return old, next
}

type sortable struct {
	key    string
	order  int
	change AttrChange
}

func sorted(in []sortable) []AttrChange {
	sort.Slice(in, func(i, j int) bool {
		if in[i].order != in[j].order {
			return in[i].order < in[j].order
		}

		return in[i].key < in[j].key
	})
	out := make([]AttrChange, 0, len(in))
	for _, s := range in {
		out = append(out, s.change)
	}

	return out
}
