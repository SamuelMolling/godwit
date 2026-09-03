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
	"github.com/SamuelMolling/godwit/internal/engine"
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

func newTargetsCmd() *cobra.Command {
	flags := &clientFlags{}
	cmd := &cobra.Command{
		Use:   "targets",
		Short: "List the targets registered on the service with their settings, applied count, drift and ready plans",
		Args:  cobra.NoArgs,
		RunE: flags.runE(func(cmd *cobra.Command, client godwitv1connect.GodwitServiceClient, _ []string) error {
			resp, err := client.ListTargets(cmd.Context(), connect.NewRequest(&godwitv1.ListTargetsRequest{}))
			if err != nil {
				return err
			}
			flags.print(cmd, resp.Msg, targetsTable(resp.Msg.Targets))

			return nil
		}),
	}
	flags.register(cmd)

	return cmd
}

func targetsTable(targets []*godwitv1.TargetSummary) string {
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tPROVIDER\tAPPLIED\tREADY PLANS\tNEEDS YOU\tDRIFT\tSEARCH PATH\tLOCK\tSTATEMENT\tREQUIRE PLAN\tLAST RUN")
	for _, t := range targets {
		last := "none"
		if t.LastRun != nil {
			last = t.LastRun.Id + " " + stateName(t.LastRun.State)
		}
		drift := "clean"
		if t.UnresolvedDrift {
			drift = "drifted"
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%d\t%s\t%s\t%s\t%s\t%t\t%s\n", t.Name, t.Provider, t.AppliedCount, t.ReadyPlans,
			t.AttentionRuns, drift, orNone(t.SearchPath), orNone(t.LockTimeout), orNone(t.StatementTimeout), t.RequirePlan, last)
	}
	_ = w.Flush()

	return strings.TrimSuffix(b.String(), "\n")
}

func optionalFiles(cmd *cobra.Command, dir string) ([]*godwitv1.MigrationFile, error) {
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) && !cmd.Flags().Changed("dir") {
		return nil, nil
	}

	return migrationFiles(dir)
}

func statusText(st *godwitv1.GetTargetStatusResponse) string {
	var b strings.Builder
	fmt.Fprintf(&b, "target %s: provider %s, lock timeout %s, statement timeout %s, search path %s\n",
		st.Target, st.Provider, orNone(st.LockTimeout), orNone(st.StatementTimeout), orNone(st.SearchPath))
	fmt.Fprintf(&b, "applied (%d):\n", len(st.Applied))
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	for _, a := range st.Applied {
		note := ""
		switch {
		case a.ChecksumMismatch:
			note = "checksum mismatch"
		case a.Repeatable:
			note = "unchanged"
		}
		fmt.Fprintf(w, "  %s\t%s\t%s\n", engine.MigrationID(a.Version, a.Name, a.Repeatable), stamp(a.AppliedAt), note)
	}
	_ = w.Flush()
	if len(st.Pending) > 0 {
		fmt.Fprintf(&b, "pending (%d):\n", len(st.Pending))
		for _, p := range st.Pending {
			fmt.Fprintf(&b, "  %s\n", engine.MigrationID(p.Version, p.Name, p.Repeatable))
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
