// Package lint checks a migration directory the way a pull request gate does: no database involved.
package lint

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	pgquery "github.com/pganalyze/pg_query_go/v6"

	"github.com/SamuelMolling/godwit/internal/engine"
)

// Finding levels.
const (
	LevelError   = "error"
	LevelWarning = "warning"
)

// Finding codes that are not planner hazards.
const (
	CodeLoad        = "E001"
	CodeParse       = "E002"
	CodeModified    = "E003"
	CodeDirective   = "E004"
	CodeSchemaDrift = "E005"
	CodeNoOpDown    = "W001"
	CodeUnchecked   = "W002"
)

// Finding is one lint result attached to a migration file.
type Finding struct {
	File    string `json:"file"`
	Level   string `json:"level"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Recipe  string `json:"recipe,omitempty"`
}

// Report is the outcome of Check; Blocking counts the error-level findings.
type Report struct {
	Findings []Finding `json:"findings"`
	Blocking int       `json:"blocking"`
}

// GitFunc runs git with args inside dir and returns its stdout.
type GitFunc func(dir string, args ...string) ([]byte, error)

// DiffFunc returns the SQL the desired schema still needs on top of what files produce; empty means they match.
type DiffFunc func(files map[string]string) (string, error)

// SchemaCheck compares the committed migrations against the schema source godwit.yaml declares.
type SchemaCheck struct {
	// Path names the ORM schema in the finding.
	Path string
	// Diff is nil when no server was given, which downgrades the check to W002.
	Diff DiffFunc
	// Warn reports a stale migration as a warning, as schema_source.lint false asks.
	Warn bool
}

// Options tunes Check.
type Options struct {
	Base   string
	Git    GitFunc
	Schema *SchemaCheck
}

var (
	fileRe   = regexp.MustCompile(`^(\d{14})_[a-z0-9_]+\.(up|down)\.sql$`)
	repeatRe = regexp.MustCompile(`^R__[a-z0-9_]+\.(up|down)\.sql$`)
)

// Check lints dir; with opts.Base set, only migrations added since that git ref are reported.
func Check(dir string, acked []string, opts Options) (Report, error) {
	rep := Report{Findings: []Finding{}}
	sc, err := newScope(dir, opts)
	if err != nil {
		return Report{}, err
	}
	for _, f := range sc.modified {
		rep.add(Finding{File: f, Level: LevelError, Code: CodeModified, Message: "migration modified after merge"})
	}

	migs, err := engine.LoadDir(dir)
	if err != nil {
		rep.add(loadFinding(dir, err))

		return rep, nil
	}
	ackSet := map[string]bool{}
	for _, code := range acked {
		ackSet[strings.ToUpper(strings.TrimSpace(code))] = true
	}
	for _, m := range migs {
		if sc.includes(m) {
			rep.checkMigration(m, ackSet)
		}
	}
	if err := rep.checkSchema(opts.Schema, migs); err != nil {
		return Report{}, err
	}

	return rep, nil
}

// checkSchema asks the service what the desired schema still needs once the committed files have run.
func (r *Report) checkSchema(c *SchemaCheck, migs []engine.Migration) error {
	if c == nil {
		return nil
	}
	if c.Diff == nil {
		r.add(Finding{File: c.Path, Level: LevelWarning, Code: CodeUnchecked, Message: c.Path + " not checked: no server configured"})

		return nil
	}
	files := map[string]string{}
	for _, m := range migs {
		files[m.UpFile()], files[m.DownFile()] = m.UpSQL, m.DownSQL
	}
	residue, err := c.Diff(files)
	if err != nil {
		return err
	}
	if strings.TrimSpace(residue) == "" {
		return nil
	}
	level := LevelError
	if c.Warn {
		level = LevelWarning
	}
	r.add(Finding{
		File: c.Path, Level: level, Code: CodeSchemaDrift,
		Message: "the migration generated from " + c.Path + " is out of date", Recipe: residue,
	})

	return nil
}

func loadFinding(dir string, err error) Finding {
	var de *engine.DirectiveError
	if errors.As(err, &de) {
		return Finding{File: de.File, Level: LevelError, Code: CodeDirective, Message: err.Error()}
	}

	return Finding{File: dir, Level: LevelError, Code: CodeLoad, Message: err.Error()}
}

func (r *Report) add(f Finding) {
	r.Findings = append(r.Findings, f)
	if f.Level == LevelError {
		r.Blocking++
	}
}

func (r *Report) checkMigration(m engine.Migration, acked map[string]bool) {
	upFile, downFile := m.UpFile(), m.DownFile()

	up, err := engine.BuildPlan(m, engine.DirectionUp)
	if err != nil {
		r.add(Finding{File: upFile, Level: LevelError, Code: CodeParse, Message: err.Error()})
	}
	for _, st := range up.Statements {
		for _, h := range st.Hazards {
			if !acked[h.Code] {
				r.add(Finding{File: upFile, Level: LevelError, Code: h.Code, Message: h.Detail, Recipe: h.Recipe})
			}
		}
	}

	if m.RevertDirective {
		return
	}

	down, err := engine.BuildPlan(m, engine.DirectionDown)
	if err != nil {
		r.add(Finding{File: downFile, Level: LevelError, Code: CodeParse, Message: err.Error()})

		return
	}
	if noOp(down) {
		r.add(Finding{File: downFile, Level: LevelWarning, Code: CodeNoOpDown, Message: "down migration is a no-op"})
	}
}

func noOp(p engine.Plan) bool {
	for _, st := range p.Statements {
		res, _ := pgquery.Parse(st.SQL)
		if res.Stmts[0].Stmt.GetSelectStmt() == nil {
			return false
		}
	}

	return true
}

type scope struct {
	added    map[string]bool
	modified []string
}

func (s scope) includes(m engine.Migration) bool {
	return s.added == nil || s.added[m.ID()]
}

func newScope(dir string, opts Options) (scope, error) {
	if opts.Base == "" {
		return scope{}, nil
	}
	git := opts.Git
	if git == nil {
		git = runGit
	}
	added, err := changedFiles(git, dir, opts.Base, "A")
	if err != nil {
		return scope{}, err
	}
	modified, err := changedFiles(git, dir, opts.Base, "M")
	if err != nil {
		return scope{}, err
	}

	// A repeatable is meant to be edited in place; only versioned files are frozen once merged.
	sc := scope{added: map[string]bool{}}
	for _, f := range modified {
		if !repeatRe.MatchString(f) {
			sc.modified = append(sc.modified, f)
		}
	}
	for _, f := range added {
		sc.added[migrationID(f)] = true
	}

	return sc, nil
}

func migrationID(file string) string {
	id := strings.TrimSuffix(file, ".sql")

	return strings.TrimSuffix(strings.TrimSuffix(id, ".up"), ".down")
}

func changedFiles(git GitFunc, dir, base, filter string) ([]string, error) {
	out, err := git(dir, "diff", "--name-only", "--diff-filter="+filter, base, "--", ".")
	if err != nil {
		return nil, fmt.Errorf("git diff %s: %w", base, err)
	}
	var files []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if name := filepath.Base(line); fileRe.MatchString(name) || repeatRe.MatchString(name) {
			files = append(files, name)
		}
	}

	return files, nil
}

func runGit(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(exitErr.Stderr)))
	}

	return out, err
}
