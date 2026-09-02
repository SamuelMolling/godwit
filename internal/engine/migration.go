package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Migration is one versioned up/down pair loaded from disk.
type Migration struct {
	Version  int64
	Name     string
	UpSQL    string
	DownSQL  string
	Checksum string
}

var fileRe = regexp.MustCompile(`^(\d{14})_([a-z0-9_]+)\.(up|down)\.sql$`)

// LoadDir reads a migration directory and returns migrations sorted by version.
func LoadDir(dir string) ([]Migration, error) {
	return LoadFS(os.DirFS(dir))
}

// LoadFS is LoadDir over any filesystem root.
func LoadFS(fsys fs.FS) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("read migration dir: %w", err)
	}

	byVersion := map[int64]*Migration{}
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		m := fileRe.FindStringSubmatch(e.Name())
		if m == nil {
			return nil, fmt.Errorf("unexpected file %q: want <yyyymmddhhmmss>_<snake_name>.{up,down}.sql", e.Name())
		}
		version, _ := strconv.ParseInt(m[1], 10, 64) // regex guarantees 14 digits
		body, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			return nil, fmt.Errorf("read %q: %w", e.Name(), err)
		}
		if strings.TrimSpace(string(body)) == "" {
			return nil, fmt.Errorf("%q is empty", e.Name())
		}

		mig := byVersion[version]
		if mig == nil {
			mig = &Migration{Version: version, Name: m[2]}
			byVersion[version] = mig
		}
		if mig.Name != m[2] {
			return nil, fmt.Errorf("version %d has two names: %q and %q", version, mig.Name, m[2])
		}
		if m[3] == "up" {
			mig.UpSQL = string(body)
		} else {
			mig.DownSQL = string(body)
		}
	}

	out := make([]Migration, 0, len(byVersion))
	for _, mig := range byVersion {
		if mig.UpSQL == "" {
			return nil, fmt.Errorf("%d_%s: missing up file", mig.Version, mig.Name)
		}
		if mig.DownSQL == "" {
			return nil, fmt.Errorf("%d_%s: missing down file", mig.Version, mig.Name)
		}
		sum := sha256.Sum256([]byte(mig.UpSQL))
		mig.Checksum = hex.EncodeToString(sum[:])
		out = append(out, *mig)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })

	return out, nil
}
