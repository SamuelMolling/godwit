package cli

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/SamuelMolling/godwit/internal/lint"
)

var lintFormats = map[string]func(io.Writer, lint.Report){
	"text":     writeLintText,
	"markdown": writeLintMarkdown,
	"json":     writeLintJSON,
}

func newLintCmd() *cobra.Command {
	var dir, base, format string
	var ack []string
	cmd := &cobra.Command{
		Use:   "lint",
		Short: "Check migrations for unacknowledged hazards without touching a database",
		RunE: func(cmd *cobra.Command, _ []string) error {
			write, ok := lintFormats[format]
			if !ok {
				return fmt.Errorf("unknown format %q (want text, markdown or json)", format)
			}
			rep, err := lint.Check(dir, ack, lint.Options{Base: base})
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
	cmd.Flags().StringVar(&dir, "dir", "migrations", "migration directory")
	cmd.Flags().StringSliceVar(&ack, "ack", nil, "hazard codes to acknowledge, e.g. H001,H003")
	cmd.Flags().StringVar(&format, "format", "text", "output format: text, markdown or json")
	cmd.Flags().StringVar(&base, "base", "", "git ref; only migrations added since it are checked")

	return cmd
}

func writeLintText(w io.Writer, rep lint.Report) {
	for _, f := range rep.Findings {
		fmt.Fprintf(w, "%s: %s %s %s\n", f.File, f.Level, f.Code, f.Message)
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
