package engine

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	pgquery "github.com/pganalyze/pg_query_go/v6"
)

// DirectiveMarker introduces a godwit directive inside a SQL line comment.
const DirectiveMarker = "godwit:"

// DirectiveRevert is the only directive a .down.sql may carry: it asks for the generated inverse.
const DirectiveRevert = "revert"

// DirectiveCheckpoint marks a migration whose body is the schema every version through= produces.
const DirectiveCheckpoint = "checkpoint"

// Directive is one `-- godwit:` line, parsed offline: an operation, its positional arguments and its options.
type Directive struct {
	Op   string
	Args []string
	Opts map[string]string
	Line int
	Text string
}

// DirectiveError reports a malformed or misplaced directive; lint turns it into E004.
type DirectiveError struct {
	File string
	Line int
	Msg  string
}

func (e *DirectiveError) Error() string {
	if e.File == "" {
		return fmt.Sprintf("line %d: godwit directive: %s", e.Line, e.Msg)
	}

	return fmt.Sprintf("%s:%d: godwit directive: %s", e.File, e.Line, e.Msg)
}

// ParseDirectives returns the directives of one SQL body, in file order.
func ParseDirectives(sql string) ([]Directive, error) {
	ds, _, derr := scanDirectives(sql)
	if derr != nil {
		return nil, derr
	}

	return ds, nil
}

type argKind int

const (
	argTable argKind = iota
	argColumn
	argType
	argCols
	argArrow
	argName
	argExpr
	argQuery
	argCmp
	argValue
)

type optKind int

const (
	optExpr optKind = iota
	optSet
	optWhere
	optInt
	optDuration
	optBool
	optIdent
	optVersion
	optAction
)

type opSpec struct {
	args     []argKind
	opts     map[string]optKind
	flags    []string
	required []string
}

var directiveOps = map[string]opSpec{
	"change-type": {
		args:  []argKind{argColumn, argType},
		opts:  map[string]optKind{"using": optExpr, "key": optIdent, "batch": optInt, "pause": optDuration, "keep-old": optBool},
		flags: []string{"not-null"},
	},
	"backfill": {
		args:     []argKind{argTable},
		opts:     map[string]optKind{"set": optSet, "where": optWhere, "key": optIdent, "batch": optInt, "pause": optDuration},
		required: []string{"set"},
	},
	"add-column": {
		args:  []argKind{argColumn, argType},
		opts:  map[string]optKind{"default": optExpr},
		flags: []string{"not-null"},
	},
	"add-not-null": {args: []argKind{argColumn}},
	"add-index": {
		args:  []argKind{argTable, argCols},
		opts:  map[string]optKind{"name": optIdent, "using": optIdent, "where": optWhere},
		flags: []string{"unique"},
	},
	"drop-index":    {args: []argKind{argTable}},
	"add-fk":        {args: []argKind{argColumn, argArrow, argColumn}, opts: map[string]optKind{"name": optIdent, "on-delete": optAction}},
	"add-check":     {args: []argKind{argTable, argName, argExpr}},
	"drop-column":   {args: []argKind{argColumn}},
	DirectiveAssert: {args: []argKind{argQuery, argCmp, argValue}},
	DirectiveRevert: {},
	DirectiveCheckpoint: {
		opts:     map[string]optKind{"through": optVersion},
		required: []string{"through"},
	},
}

var fkActions = []string{"cascade", "restrict", "set-null", "set-default", "no-action"}

// ValidateDirective checks one directive against the grammar: known op, right arity, known options, parseable values.
func ValidateDirective(d Directive) error {
	spec, ok := directiveOps[d.Op]
	if !ok {
		return fmt.Errorf("unknown operation %q; known: %s", d.Op, strings.Join(sortedOps(), ", "))
	}
	if len(d.Args) != len(spec.args) {
		return fmt.Errorf("%s takes %d argument(s), got %d", d.Op, len(spec.args), len(d.Args))
	}
	for i, a := range d.Args {
		if err := checkArg(spec.args[i], a); err != nil {
			return fmt.Errorf("%s argument %d: %w", d.Op, i+1, err)
		}
	}
	for name, v := range d.Opts {
		kind, ok := spec.opts[name]
		if !ok {
			if slices.Contains(spec.flags, name) {
				continue
			}

			return fmt.Errorf("%s has no option %q; known: %s", d.Op, name, strings.Join(optionNames(spec), ", "))
		}
		if err := checkOpt(kind, v); err != nil {
			return fmt.Errorf("%s option %s: %w", d.Op, name, err)
		}
	}
	for _, name := range spec.required {
		if _, ok := d.Opts[name]; !ok {
			return fmt.Errorf("%s requires %s=", d.Op, name)
		}
	}
	if d.Op == DirectiveAssert {
		if _, _, err := ParseAssert(d); err != nil {
			return err
		}
	}

	return nil
}

func sortedOps() []string {
	out := make([]string, 0, len(directiveOps))
	for op := range directiveOps {
		out = append(out, op)
	}
	slices.Sort(out)

	return out
}

func optionNames(spec opSpec) []string {
	out := make([]string, 0, len(spec.opts)+len(spec.flags))
	for name := range spec.opts {
		out = append(out, name+"=")
	}
	out = append(out, spec.flags...)
	slices.Sort(out)

	return out
}

func checkArg(kind argKind, v string) error {
	switch kind {
	case argTable:
		return checkRef(v, 1, 2)
	case argColumn:
		return checkRef(v, 2, 3)
	case argName:
		return checkRef(v, 1, 1)
	case argType:
		return checkParses("SELECT NULL::"+v, "not a type")
	case argCols:
		if !strings.HasPrefix(v, "(") || !strings.HasSuffix(v, ")") {
			return fmt.Errorf("%q is not a parenthesised column list", v)
		}

		return checkParses("CREATE INDEX ON t "+v, "not a column list")
	case argArrow:
		if v != "->" {
			return fmt.Errorf("expected ->, got %q", v)
		}

		return nil
	case argQuery:
		return checkAssertQuery(v)
	case argCmp:
		return checkAssertOp(v)
	case argValue:
		return checkAssertValue(v)
	default:
		return checkParses("SELECT "+v, "not an expression")
	}
}

func checkOpt(kind optKind, v string) error {
	switch kind {
	case optExpr:
		return checkParses("SELECT "+v, "not an expression")
	case optSet:
		return checkParses("UPDATE t SET "+v, "not an assignment list")
	case optWhere:
		return checkParses("SELECT 1 WHERE "+v, "not a condition")
	case optIdent:
		return checkRef(v, 1, 1)
	case optInt:
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return fmt.Errorf("%q is not a positive integer", v)
		}

		return nil
	case optDuration:
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			return fmt.Errorf("%q is not a duration such as 100ms", v)
		}

		return nil
	case optBool:
		if v != "true" && v != "false" {
			return fmt.Errorf("%q is not true or false", v)
		}

		return nil
	case optVersion:
		if n, err := strconv.ParseInt(v, 10, 64); err != nil || len(v) != 14 || n <= 0 {
			return fmt.Errorf("%q is not a 14-digit migration version", v)
		}

		return nil
	default:
		if !slices.Contains(fkActions, v) {
			return fmt.Errorf("%q is not one of %s", v, strings.Join(fkActions, ", "))
		}

		return nil
	}
}

func checkParses(sql, what string) error {
	if _, err := pgquery.Parse(sql); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}

	return nil
}

func checkRef(v string, minParts, maxParts int) error {
	parts, err := refParts(v)
	if err != nil {
		return err
	}
	if len(parts) < minParts || len(parts) > maxParts {
		return fmt.Errorf("%q must have %s", v, refShape(minParts, maxParts))
	}

	return nil
}

func refShape(minParts, maxParts int) string {
	switch {
	case maxParts == 1:
		return "a single name"
	case minParts == 1:
		return "the form <table> or <schema>.<table>"
	default:
		return "the form <table>.<column> or <schema>.<table>.<column>"
	}
}

func refParts(v string) ([]string, error) {
	res, err := pgquery.Parse("SELECT " + v)
	if err != nil {
		return nil, fmt.Errorf("not a name: %w", err)
	}
	list := res.Stmts[0].Stmt.GetSelectStmt().GetTargetList()
	if len(list) != 1 {
		return nil, fmt.Errorf("%q is not a name", v)
	}
	ref := list[0].GetResTarget().GetVal().GetColumnRef()
	if ref == nil {
		return nil, fmt.Errorf("%q is not a name", v)
	}
	out := make([]string, 0, len(ref.Fields))
	for _, f := range ref.Fields {
		name := f.GetString_().GetSval()
		if name == "" {
			return nil, fmt.Errorf("%q is not a name", v)
		}
		out = append(out, name)
	}

	return out, nil
}

type scanner struct {
	sql     string
	i       int
	line    int
	bol     bool
	sawCode bool
}

func scanDirectives(sql string) ([]Directive, bool, *DirectiveError) {
	s := &scanner{sql: sql, line: 1, bol: true}
	var out []Directive
	for s.i < len(s.sql) {
		switch c := s.sql[s.i]; {
		case c == '\n':
			s.advance()
			s.bol = true
		case c == ' ' || c == '\t' || c == '\r':
			s.advance()
		case c == '-' && s.peek() == '-':
			d, ok, derr := s.lineComment()
			if derr != nil {
				return nil, s.sawCode, derr
			}
			if ok {
				out = append(out, d)
			}
		case c == '/' && s.peek() == '*':
			s.blockComment()
		case c == '\'' || c == '"':
			s.quoted(c)
			s.mark()
		case c == '$' && s.dollarQuoted():
			s.mark()
		default:
			s.advance()
			s.mark()
		}
	}

	return out, s.sawCode, nil
}

func (s *scanner) mark() {
	s.bol, s.sawCode = false, true
}

func (s *scanner) advance() {
	if s.sql[s.i] == '\n' {
		s.line++
	}
	s.i++
}

func (s *scanner) peek() byte {
	if s.i+1 >= len(s.sql) {
		return 0
	}

	return s.sql[s.i+1]
}

func (s *scanner) lineComment() (Directive, bool, *DirectiveError) {
	atBOL, at, start := s.bol, s.line, s.i
	for s.i < len(s.sql) && s.sql[s.i] != '\n' {
		s.advance()
	}
	raw := strings.TrimSpace(s.sql[start:s.i])
	body, ok := directiveBody(raw)
	if !ok {
		return Directive{}, false, nil
	}
	if !atBOL {
		return Directive{}, false, &DirectiveError{Line: at, Msg: "a directive must start its own line"}
	}
	d, err := parseDirective(body, at, raw)
	if err != nil {
		return Directive{}, false, &DirectiveError{Line: at, Msg: err.Error()}
	}

	return d, true, nil
}

func directiveBody(raw string) (string, bool) {
	rest := strings.TrimLeft(strings.TrimPrefix(raw, "--"), " \t")
	if !strings.HasPrefix(rest, DirectiveMarker) {
		return "", false
	}

	return strings.TrimSpace(strings.TrimPrefix(rest, DirectiveMarker)), true
}

func (s *scanner) quoted(q byte) {
	s.advance()
	for s.i < len(s.sql) {
		if s.sql[s.i] != q {
			s.advance()

			continue
		}
		s.advance()
		if s.i < len(s.sql) && s.sql[s.i] == q {
			s.advance()

			continue
		}

		return
	}
}

func (s *scanner) blockComment() {
	depth := 0
	for s.i < len(s.sql) {
		switch {
		case s.sql[s.i] == '/' && s.peek() == '*':
			depth++
			s.advance()
			s.advance()
		case s.sql[s.i] == '*' && s.peek() == '/':
			depth--
			s.advance()
			s.advance()
			if depth == 0 {
				return
			}
		default:
			s.advance()
		}
	}
}

var dollarTagRe = regexp.MustCompile(`^\$([A-Za-z_][A-Za-z0-9_]*)?\$`)

func (s *scanner) dollarQuoted() bool {
	loc := dollarTagRe.FindStringIndex(s.sql[s.i:])
	if loc == nil {
		return false
	}
	tag := s.sql[s.i : s.i+loc[1]]
	for range loc[1] {
		s.advance()
	}
	n := strings.Index(s.sql[s.i:], tag)
	if n < 0 {
		n = len(s.sql) - s.i
	} else {
		n += len(tag)
	}
	for range n {
		s.advance()
	}

	return true
}

func parseDirective(body string, line int, raw string) (Directive, error) {
	tokens, err := tokenize(body)
	if err != nil {
		return Directive{}, err
	}
	if len(tokens) == 0 {
		return Directive{}, errors.New("missing operation")
	}
	d := Directive{Op: tokens[0], Opts: map[string]string{}, Line: line, Text: raw}
	spec, ok := directiveOps[d.Op]
	if !ok {
		return Directive{}, fmt.Errorf("unknown operation %q; known: %s", d.Op, strings.Join(sortedOps(), ", "))
	}
	rest := tokens[1:]
	if len(rest) < len(spec.args) {
		return Directive{}, fmt.Errorf("%s takes %d argument(s), got %d", d.Op, len(spec.args), len(rest))
	}
	d.Args = rest[:len(spec.args)]
	for _, tok := range rest[len(spec.args):] {
		name, value, isPair := cutOption(tok)
		switch {
		case isPair:
			d.Opts[name] = value
		case slices.Contains(spec.flags, tok):
			d.Opts[tok] = "true"
		default:
			return Directive{}, fmt.Errorf("%s: unexpected argument %q", d.Op, tok)
		}
	}

	return d, ValidateDirective(d)
}

func cutOption(tok string) (name, value string, ok bool) {
	n := strings.Index(tok, "=")
	if n <= 0 {
		return "", "", false
	}
	name = tok[:n]
	if strings.ContainsAny(name, " \t") {
		return "", "", false
	}

	return name, tok[n+1:], true
}

func tokenize(body string) ([]string, error) {
	var out []string
	for i := 0; i < len(body); {
		if body[i] == ' ' || body[i] == '\t' {
			i++

			continue
		}
		var b strings.Builder
		for i < len(body) && body[i] != ' ' && body[i] != '\t' {
			var (
				text string
				next int
				err  error
			)
			switch body[i] {
			case '\'':
				text, next, err = readQuoted(body, i)
			case '(':
				text, next, err = readParens(body, i)
			default:
				text, next = body[i:i+1], i+1
			}
			if err != nil {
				return nil, err
			}
			b.WriteString(text)
			i = next
		}
		out = append(out, b.String())
	}

	return out, nil
}

func readQuoted(body string, i int) (string, int, error) {
	var b strings.Builder
	for j := i + 1; j < len(body); j++ {
		if body[j] != '\'' {
			b.WriteByte(body[j])

			continue
		}
		if j+1 < len(body) && body[j+1] == '\'' {
			b.WriteByte('\'')
			j++

			continue
		}

		return b.String(), j + 1, nil
	}

	return "", 0, errors.New("unterminated quoted value")
}

func readParens(body string, i int) (string, int, error) {
	depth := 0
	for j := i; j < len(body); j++ {
		switch body[j] {
		case '\'':
			_, next, err := readQuoted(body, j)
			if err != nil {
				return "", 0, err
			}
			j = next - 1
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return body[i : j+1], j + 1, nil
			}
		}
	}

	return "", 0, errors.New("unterminated (")
}
