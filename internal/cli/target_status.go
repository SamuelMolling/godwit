package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"text/tabwriter"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/gen/godwit/v1/godwitv1connect"
	"github.com/SamuelMolling/godwit/internal/config"
)

func newTargetStatusCmd() *cobra.Command {
	flags := &clientFlags{}
	var dir string
	cmd := &cobra.Command{
		Use:   "status <name>",
		Short: "Show what a target has applied, what is pending, its last run and drift baseline",
		Args:  cobra.ExactArgs(1),
		RunE: flags.runE(func(cmd *cobra.Command, client godwitv1connect.GodwitServiceClient, args []string) error {
			files, err := optionalFiles(cmd, dir)
			if err != nil {
				return err
			}
			resp, err := client.GetTargetStatus(cmd.Context(), connect.NewRequest(&godwitv1.GetTargetStatusRequest{
				Target: args[0], Files: files,
			}))
			if err != nil {
				return err
			}
			flags.print(cmd, resp.Msg, statusText(resp.Msg))

			return nil
		}),
	}
	flags.register(cmd)
	cmd.Flags().StringVar(&dir, "dir", config.Defaults().Dir, "migration directory to compare against (skipped when absent unless set explicitly)")
	configKeys(cmd, "dir")

	return cmd
}

func optionalFiles(cmd *cobra.Command, dir string) ([]*godwitv1.MigrationFile, error) {
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) && !cmd.Flags().Changed("dir") {
		return nil, nil
	}

	return migrationFiles(dir)
}

func statusText(st *godwitv1.GetTargetStatusResponse) string {
	var b strings.Builder
	fmt.Fprintf(&b, "target %s: provider %s, lock timeout %s, statement timeout %s\n",
		st.Target, st.Provider, orNone(st.LockTimeout), orNone(st.StatementTimeout))
	fmt.Fprintf(&b, "applied (%d):\n", len(st.Applied))
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	for _, a := range st.Applied {
		note := ""
		if a.ChecksumMismatch {
			note = "checksum mismatch"
		}
		fmt.Fprintf(w, "  %d\t%s\t%s\t%s\n", a.Version, a.Name, stamp(a.AppliedAt), note)
	}
	_ = w.Flush()
	if len(st.Pending) > 0 {
		fmt.Fprintf(&b, "pending (%d):\n", len(st.Pending))
		for _, p := range st.Pending {
			fmt.Fprintf(&b, "  %d  %s\n", p.Version, p.Name)
		}
	}
	writeLastRun(&b, st.LastRun)
	fmt.Fprintf(&b, "ready plans: %d\n", st.ReadyPlans)
	writeBaseline(&b, st.DriftBaseline)

	return strings.TrimSuffix(b.String(), "\n")
}

func writeLastRun(w io.Writer, r *godwitv1.Run) {
	if r == nil {
		fmt.Fprintln(w, "last run: none")

		return
	}
	line := fmt.Sprintf("last run: %s %s %s", r.Id, r.Kind, stateName(r.State))
	if r.FinishedAt != nil {
		line += " finished " + stamp(r.FinishedAt)
	}
	fmt.Fprintln(w, line)
}

func writeBaseline(w io.Writer, d *godwitv1.DriftBaseline) {
	if d == nil {
		fmt.Fprintln(w, "drift baseline: none")

		return
	}
	line := "drift baseline: taken " + stamp(d.TakenAt)
	if d.RunId != "" {
		line += " by run " + d.RunId
	}
	if d.UnresolvedDrift {
		line += ", unresolved drift"
	}
	fmt.Fprintln(w, line)
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}

	return s
}
