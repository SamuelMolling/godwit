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
	Version    int64
	Name       string
	Repeatable bool
	UpSQL      string
	DownSQL    string
	Checksum   string
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
		if out[i].DownSQL == "" {
			return nil, fmt.Errorf("%s: missing down file", out[i].ID())
		}
		sum := sha256.Sum256([]byte(out[i].UpSQL))
		out[i].Checksum = hex.EncodeToString(sum[:])
	}
	slices.SortFunc(out, CompareMigrations)

	return out, nil
}
