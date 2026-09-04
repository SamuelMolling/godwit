package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/internal/config"
	"github.com/SamuelMolling/godwit/internal/lint"
	"github.com/SamuelMolling/godwit/internal/schemasource"
)

var lintFormats = map[string]func(io.Writer, lint.Report){
	"text":     writeLintText,
	"markdown": writeLintMarkdown,
	"json":     writeLintJSON,
}

func newLintCmd() *cobra.Command {
	flags := &clientFlags{}
	var dir, base, format, target string
	var ack []string
	var noSchemaCheck bool
	cmd := &cobra.Command{
		Use:   "lint",
		Short: "Check migrations for unacknowledged hazards and against the declared schema source",
		RunE: func(cmd *cobra.Command, _ []string) error {
			write, ok := lintFormats[format]
			if !ok {
				return fmt.Errorf("unknown format %q (want text, markdown or json)", format)
			}
			check, err := schemaCheck(cmd, flags, target, noSchemaCheck)
			if err != nil {
				return err
			}
			rep, err := lint.Check(dir, ack, lint.Options{Base: base, Schema: check})
			if err != nil {
				return err
			}
			write(cmd.OutOrStdout(), rep)
			if rep.Blocking > 0 {
				return fmt.Errorf("%d blocking finding(s)", rep.Blocking)
			}

			return nil
		},
	}
	flags.registerServer(cmd)
	cmd.Flags().StringVar(&dir, "dir", config.Defaults().Dir, "migration directory")
	cmd.Flags().StringVar(&target, "target", "", "target the committed migrations are replayed on for the schema check")
	configKeys(cmd, "dir", "target")
	cmd.Flags().StringSliceVar(&ack, "ack", nil, "hazard codes to acknowledge, e.g. H001,H003")
	cmd.Flags().StringVar(&format, "format", "text", "output format: text, markdown or json")
	cmd.Flags().StringVar(&base, "base", "", "git ref; only migrations added since it are checked")
	cmd.Flags().BoolVar(&noSchemaCheck, "no-schema-check", false, "skip the schema_source check")

	return cmd
}

// schemaCheck renders the declared schema source and hands lint a Diff that replays the committed files
// on a scratch database; without a server the check degrades to W002 rather than running the ORM at all.
func schemaCheck(cmd *cobra.Command, flags *clientFlags, target string, off bool) (*lint.SchemaCheck, error) {
	declared := configFrom(cmd.Context()).SchemaSource
	if off || declared == nil {
		return nil, nil
	}
	source, path, err := configSource(*declared, cmd)
	if err != nil {
		return nil, err
	}
	check := &lint.SchemaCheck{Path: path, Warn: !declared.LintEnabled()}
	if flags.server == "" {
		return check, nil
	}
	if target == "" {
		return nil, errors.New("--target (or target in godwit.yaml) is required by the schema check; pass --no-schema-check to skip it")
	}
	ddl, err := source.Load(cmd.Context())
	if err != nil {
		return nil, err
	}
	client := flags.dial()
	check.Diff = func(files map[string]string) (string, error) {
		res, err := client.Diff(cmd.Context(), connect.NewRequest(&godwitv1.DiffRequest{
			Target: target, Schema: ddl, Base: godwitv1.DiffBase_DIFF_BASE_FILES, Files: committedFiles(files),
		}))
		if err != nil {
			return "", apiError(err)
		}

		return res.Msg.UpSql, nil
	}

	return check, nil
}

// configSource is sourceFlags.fromConfig for a command that registers no source flag, so every binary
// falls back to the same environment variable godwit diff defaults it to.
func configSource(declared config.SchemaSource, cmd *cobra.Command) (schemasource.Source, string, error) {
	src := &sourceFlags{
		prismaBin:  envOr("GODWIT_PRISMA_BIN", schemasource.DefaultPrismaBin),
		goBin:      envOr("GODWIT_GO_BIN", schemasource.DefaultGoBin),
		pythonBin:  envOr("GODWIT_PYTHON_BIN", schemasource.DefaultPythonBin),
		alembicBin: envOr("GODWIT_ALEMBIC_BIN", schemasource.DefaultAlembicBin),
		drizzleBin: envOr("GODWIT_DRIZZLE_BIN", schemasource.DefaultDrizzleBin),
		settings:   os.Getenv("DJANGO_SETTINGS_MODULE"),
	}

	return src.fromConfig(declared, cmd)
}

func committedFiles(files map[string]string) []*godwitv1.MigrationFile {
	out := make([]*godwitv1.MigrationFile, 0, len(files))
	for _, name := range slices.Sorted(maps.Keys(files)) {
		out = append(out, &godwitv1.MigrationFile{Name: name, Body: files[name]})
	}

	return out
}

func writeLintText(w io.Writer, rep lint.Report) {
	for _, f := range rep.Findings {
		fmt.Fprintf(w, "%s: %s %s %s\n", f.File, f.Level, f.Code, f.Message)
		writeRecipeText(w, "    ", f.Recipe)
	}
	fmt.Fprintf(w, "%d finding(s), %d blocking\n", len(rep.Findings), rep.Blocking)
}

func writeLintMarkdown(w io.Writer, rep lint.Report) {
	fmt.Fprintln(w, "## godwit lint")
	fmt.Fprintln(w)
	if len(rep.Findings) > 0 {
		fmt.Fprintln(w, "| Migration | Level | Code | Message |")
		fmt.Fprintln(w, "|---|---|---|---|")
		for _, f := range rep.Findings {
			fmt.Fprintf(w, "| `%s` | %s | %s | %s |\n", f.File, f.Level, f.Code, f.Message)
		}
		fmt.Fprintln(w)
		for _, f := range rep.Findings {
			writeRecipeDetails(w, fmt.Sprintf("recipe for %s in `%s`", f.Code, f.File), f.Recipe)
		}
	}
	if rep.Blocking > 0 {
		fmt.Fprintf(w, "❌ %d blocking finding(s)\n", rep.Blocking)

		return
	}
	fmt.Fprintln(w, "✅ no unacknowledged hazards")
}

func writeLintJSON(w io.Writer, rep lint.Report) {
	data, _ := json.Marshal(rep)
	fmt.Fprintln(w, string(data))
}
