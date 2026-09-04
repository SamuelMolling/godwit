package schemasource

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// DefaultDrizzleBin is the command line used to run Drizzle Kit when none is configured.
const DefaultDrizzleBin = "npx drizzle-kit"

// Drizzle is a Source over a Drizzle Kit config: the DDL of the whole schema from empty state,
// as `drizzle-kit export` prints it on stdout. No database is contacted.
type Drizzle struct {
	Config string
	Bin    string
	Run    Runner
}

var (
	blockComment   = regexp.MustCompile(`(?s)/\*.*?\*/`)
	drizzleDialect = regexp.MustCompile("\\bdialect\\s*:\\s*[\"'`]([^\"'`]*)")
)

// Load runs Drizzle Kit and returns its SQL script untouched.
func (d Drizzle) Load(ctx context.Context) (string, error) {
	if strings.TrimSpace(d.Config) == "" {
		return "", errors.New("no Drizzle project configured; pass --drizzle drizzle.config.ts or set schema_source.path")
	}
	if err := d.checkDialect(); err != nil {
		return "", err
	}
	bin := fields(d.Bin, DefaultDrizzleBin)
	argv := append(append([]string{}, bin...), "export", "--config="+d.Config)
	out, err := capture(ctx, d.Run, argv, func(err error, stderr []byte) error {
		if missingBinary(err) {
			return fmt.Errorf("drizzle-kit was not found (%w); install drizzle-kit in the project or point --drizzle-bin / GODWIT_DRIZZLE_BIN at it", err)
		}

		return runFailed("drizzle-kit export", err, stderr)
	})
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(out) == "" {
		return "", fmt.Errorf("drizzle-kit export produced no DDL for %s; it exits 0 with an empty script when the schema files declare no table or use a dialect other than the configured one", d.Config)
	}

	return out, nil
}

func (d Drizzle) checkDialect() error {
	body, err := os.ReadFile(d.Config)
	if err != nil {
		return err
	}
	clean := lineComment.ReplaceAll(blockComment.ReplaceAll(body, nil), nil)
	m := drizzleDialect.FindSubmatch(clean)
	if m == nil || string(m[1]) == "postgresql" {
		return nil
	}

	return fmt.Errorf("%s: dialect is %q; godwit targets PostgreSQL only (dialect: \"postgresql\")", d.Config, string(m[1]))
}
