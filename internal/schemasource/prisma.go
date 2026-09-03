package schemasource

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// DefaultPrismaBin is the command line used to run the Prisma CLI when none is configured.
const DefaultPrismaBin = "npx prisma"

// Runner executes a command and returns its stdout and stderr.
type Runner func(ctx context.Context, name string, args ...string) (stdout, stderr []byte, err error)

// Exec is the Runner backed by os/exec.
func Exec(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()

	return out.Bytes(), errOut.Bytes(), err
}

// Prisma is a Source over a Prisma schema: the DDL of the whole datamodel from an empty database,
// as `prisma migrate diff --from-empty --script` renders it. No database is contacted.
type Prisma struct {
	Schema string
	Bin    string
	Run    Runner
}

var (
	lineComment = regexp.MustCompile(`//[^\n]*`)
	datasource  = regexp.MustCompile(`(?s)\bdatasource\s+\w+\s*\{(.*?)\}`)
	provider    = regexp.MustCompile(`\bprovider\s*=\s*("[^"]*"|[^\s]+)`)
	version     = regexp.MustCompile(`(?m)^prisma\s*:\s*v?(\d+)\.`)
)

// Load runs the Prisma CLI and returns its SQL script untouched.
func (p Prisma) Load(ctx context.Context) (string, error) {
	body, err := os.ReadFile(p.Schema)
	if err != nil {
		return "", err
	}
	if err := checkProvider(body); err != nil {
		return "", fmt.Errorf("%s: %w", p.Schema, err)
	}
	bin := strings.Fields(p.Bin)
	if len(bin) == 0 {
		bin = strings.Fields(DefaultPrismaBin)
	}
	run := p.Run
	if run == nil {
		run = Exec
	}
	major, err := prismaMajor(ctx, run, bin)
	if err != nil {
		return "", err
	}
	toSchema := "--to-schema-datamodel"
	if major >= 7 {
		toSchema = "--to-schema"
	}
	args := append(bin[1:], "migrate", "diff", "--from-empty", toSchema, p.Schema, "--script")
	out, errOut, err := run(ctx, bin[0], args...)
	if err != nil {
		return "", prismaError("migrate diff", err, errOut)
	}
	if strings.TrimSpace(string(out)) == "" {
		return "", errors.New("prisma migrate diff produced no DDL (on Prisma 7 the prisma.config.ts must set datasource.url; the value is never connected to)")
	}

	return string(out), nil
}

func checkProvider(body []byte) error {
	clean := lineComment.ReplaceAll(body, nil)
	block := datasource.FindSubmatch(clean)
	if block == nil {
		return errors.New("no datasource block in the Prisma schema")
	}
	m := provider.FindSubmatch(block[1])
	if m == nil {
		return errors.New("datasource has no provider")
	}
	got := strings.Trim(string(m[1]), `"`)
	switch got {
	case "postgresql", "postgres":
		return nil
	}

	return fmt.Errorf("datasource provider is %s; godwit targets PostgreSQL only (provider = \"postgresql\")", string(m[1]))
}

func prismaMajor(ctx context.Context, run Runner, bin []string) (int, error) {
	out, errOut, err := run(ctx, bin[0], append(bin[1:], "--version")...)
	if err != nil {
		return 0, prismaError("--version", err, errOut)
	}
	m := version.FindSubmatch(out)
	if m == nil {
		return 0, fmt.Errorf("cannot read the Prisma CLI major version from %q (supported: Prisma 5, 6 and 7)", strings.TrimSpace(string(out)))
	}
	major, _ := strconv.Atoi(string(m[1]))

	return major, nil
}

func prismaError(step string, err error, stderr []byte) error {
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("prisma CLI not found (%w); install prisma in the project or point --prisma-bin / GODWIT_PRISMA_BIN at it", err)
	}
	if detail := strings.TrimSpace(string(stderr)); detail != "" {
		return fmt.Errorf("prisma %s failed: %w: %s", step, err, detail)
	}

	return fmt.Errorf("prisma %s failed: %w", step, err)
}
