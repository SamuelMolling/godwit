package engine

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// RepeatablePrefix marks a migration file that has no version and re-applies whenever its content changes.
const RepeatablePrefix = "R__"

// Migration is one up/down pair loaded from disk: versioned, or repeatable and keyed by name.
type Migration struct {
	Version         int64
	Name            string
	Repeatable      bool
	UpSQL           string
	DownSQL         string
	Checksum        string
	Directives      []Directive
	RevertDirective bool
	// Checkpoint marks a migration whose body is the whole schema the versions up to Through produce.
	Checkpoint bool
	// Through is the newest version a checkpoint collapses; zero on every other migration.
	Through int64
}

// Collapses reports whether cp, a checkpoint, subsumes m.
func (m Migration) Collapses(cp Migration) bool {
	return !m.Repeatable && m.Version <= cp.Through
}

// NewestCheckpoint returns the newest checkpoint among migs, and whether there is one.
func NewestCheckpoint(migs []Migration) (Migration, bool) {
	var out Migration
	found := false
	for _, m := range migs {
		if m.Checkpoint && m.Version >= out.Version {
			out, found = m, true
		}
	}

	return out, found
}

// ID identifies a migration in plans, keys and reports.
func (m Migration) ID() string {
	return MigrationID(m.Version, m.Name, m.Repeatable)
}

// UpFile is the file name the up side is loaded from.
func (m Migration) UpFile() string { return m.ID() + ".up.sql" }

// DownFile is the file name the down side is loaded from.
func (m Migration) DownFile() string { return m.ID() + ".down.sql" }

// MigrationID renders a migration identity: the padded version and name, or the repeatable prefix and name.
func MigrationID(version int64, name string, repeatable bool) string {
	if repeatable {
		return RepeatablePrefix + name
	}

	return fmt.Sprintf("%014d_%s", version, name)
}

// CompareMigrations orders versioned migrations by version, then repeatables by name.
func CompareMigrations(a, b Migration) int {
	if a.Repeatable != b.Repeatable {
		if a.Repeatable {
			return 1
		}

		return -1
	}
	if a.Repeatable {
		return cmp.Compare(a.Name, b.Name)
	}

	return cmp.Compare(a.Version, b.Version)
}

var (
	fileRe   = regexp.MustCompile(`^(\d{14})_([a-z0-9_]+)\.(up|down)\.sql$`)
	repeatRe = regexp.MustCompile(`^R__([a-z0-9_]+)\.(up|down)\.sql$`)
)

// LoadDir reads a migration directory and returns migrations in apply order.
func LoadDir(dir string) ([]Migration, error) {
	return LoadFS(os.DirFS(dir))
}

// LoadFS is LoadDir over any filesystem root.
func LoadFS(fsys fs.FS) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("read migration dir: %w", err)
	}

	l := loader{byVersion: map[int64]*Migration{}, byName: map[string]*Migration{}}
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		body, err := l.read(fsys, e.Name())
		if err != nil {
			return nil, err
		}
		if err := l.add(e.Name(), body); err != nil {
			return nil, err
		}
	}

	return l.finish()
}

type loader struct {
	byVersion map[int64]*Migration
	byName    map[string]*Migration
}

func (l loader) read(fsys fs.FS, name string) (string, error) {
	if !fileRe.MatchString(name) && !repeatRe.MatchString(name) {
		return "", fmt.Errorf("unexpected file %q: want <yyyymmddhhmmss>_<snake_name>.{up,down}.sql or R__<snake_name>.{up,down}.sql", name)
	}
	body, err := fs.ReadFile(fsys, name)
	if err != nil {
		return "", fmt.Errorf("read %q: %w", name, err)
	}
	if strings.TrimSpace(string(body)) == "" {
		return "", fmt.Errorf("%q is empty", name)
	}

	return string(body), nil
}

func (l loader) add(file, body string) error {
	if m := repeatRe.FindStringSubmatch(file); m != nil {
		mig := l.byName[m[1]]
		if mig == nil {
			mig = &Migration{Name: m[1], Repeatable: true}
			l.byName[m[1]] = mig
		}
		mig.set(m[2], body)

		return nil
	}
	m := fileRe.FindStringSubmatch(file)
	version, _ := strconv.ParseInt(m[1], 10, 64) // regex guarantees 14 digits
	mig := l.byVersion[version]
	if mig == nil {
		mig = &Migration{Version: version, Name: m[2]}
		l.byVersion[version] = mig
	}
	if mig.Name != m[2] {
		return fmt.Errorf("version %d has two names: %q and %q", version, mig.Name, m[2])
	}
	mig.set(m[3], body)

	return nil
}

func (m *Migration) set(side, body string) {
	if side == "up" {
		m.UpSQL = body

		return
	}
	m.DownSQL = body
}

func (m *Migration) loadDirectives() error {
	up, _, derr := scanDirectives(m.UpSQL)
	if derr != nil {
		derr.File = m.UpFile()

		return derr
	}
	down, sawSQL, derr := scanDirectives(m.DownSQL)
	if derr != nil {
		derr.File = m.DownFile()

		return derr
	}
	if i := slices.IndexFunc(up, func(d Directive) bool { return d.Op == DirectiveCheckpoint }); i >= 0 {
		return m.loadCheckpoint(up, i)
	}
	if m.Repeatable && len(up)+len(down) > 0 {
		return placement(m, up, down, "directives are not supported in a repeatable migration")
	}
	for _, d := range up {
		if d.Op == DirectiveRevert {
			return &DirectiveError{File: m.UpFile(), Line: d.Line, Msg: "the " + DirectiveRevert + " sentinel belongs in the .down.sql"}
		}
	}
	switch {
	case len(down) == 0:
	case len(down) == 1 && down[0].Op == DirectiveRevert && !sawSQL:
		m.RevertDirective = true
	default:
		return placement(m, nil, down, "a .down.sql carries hand-written SQL or the lone `-- "+DirectiveMarker+" "+DirectiveRevert+"` sentinel, not both")
	}
	m.Directives = up

	return nil
}

// loadCheckpoint validates the checkpoint directive at up[i]: it stands alone, on a versioned migration,
// over a version below its own, and the file has no inverse because the versions it collapses can never
// be reverted through it.
func (m *Migration) loadCheckpoint(up []Directive, i int) error {
	d := up[i]
	fail := func(format string, args ...any) error {
		return &DirectiveError{File: m.UpFile(), Line: d.Line, Msg: fmt.Sprintf(format, args...)}
	}
	if len(up) > 1 {
		return fail("a checkpoint carries no other directive")
	}
	if m.Repeatable {
		return fail("a checkpoint must be a versioned migration")
	}
	through, _ := strconv.ParseInt(d.Opts["through"], 10, 64) // the grammar guarantees 14 digits
	if through >= m.Version {
		return fail("through=%014d must be below the checkpoint's own version %014d", through, m.Version)
	}
	if strings.TrimSpace(m.DownSQL) != "" {
		return fail("a checkpoint has no inverse; delete %s", m.DownFile())
	}
	m.Checkpoint, m.Through = true, through

	return nil
}

func placement(m *Migration, up, down []Directive, msg string) error {
	file, first := m.UpFile(), up
	if len(first) == 0 {
		file, first = m.DownFile(), down
	}

	return &DirectiveError{File: file, Line: first[0].Line, Msg: msg}
}

func (l loader) finish() ([]Migration, error) {
	out := make([]Migration, 0, len(l.byVersion)+len(l.byName))
	for _, mig := range l.byVersion {
		out = append(out, *mig)
	}
	for _, mig := range l.byName {
		out = append(out, *mig)
	}
	for i := range out {
		if out[i].UpSQL == "" {
			return nil, fmt.Errorf("%s: missing up file", out[i].ID())
		}
		sum := sha256.Sum256([]byte(out[i].UpSQL))
		out[i].Checksum = hex.EncodeToString(sum[:])
		if err := out[i].loadDirectives(); err != nil {
			return nil, err
		}
		if out[i].DownSQL == "" && !out[i].Checkpoint {
			return nil, fmt.Errorf("%s: missing down file", out[i].ID())
		}
	}
	slices.SortFunc(out, CompareMigrations)

	return out, nil
}
