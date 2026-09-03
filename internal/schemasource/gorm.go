package schemasource

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// DefaultGoBin is the command line used to run the Go toolchain when none is configured.
const DefaultGoBin = "go"

const gormExample = "the package must print the collected DDL on stdout; examples/gorm/schema/main.go is a 20-line one"

// Gorm is a Source over a Go package that dry-runs GORM's migrator and prints the DDL; the package is the project's, not godwit's.
type Gorm struct {
	Pkg string
	Bin string
	Run Runner
}

// Load runs the Go package and returns its stdout untouched.
func (g Gorm) Load(ctx context.Context) (string, error) {
	if strings.TrimSpace(g.Pkg) == "" {
		return "", errors.New("no Go package configured; pass --gorm ./cmd/schema or set schema_source.path")
	}
	bin := fields(g.Bin, DefaultGoBin)
	argv := append(append([]string{}, bin...), "run", g.Pkg)
	label := "go run " + g.Pkg
	out, err := capture(ctx, g.Run, argv, func(err error, stderr []byte) error {
		if missingBinary(err) {
			return fmt.Errorf("the Go toolchain was not found (%w); install Go or point --go-bin / GODWIT_GO_BIN at it", err)
		}

		return fmt.Errorf("%w (%s)", runFailed(label, err, stderr), gormExample)
	})
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(out) == "" {
		return "", fmt.Errorf("%s produced no DDL: %s", label, gormExample)
	}

	return out, nil
}
