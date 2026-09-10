package cli

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/gen/godwit/v1/godwitv1connect"
	"github.com/SamuelMolling/godwit/internal/config"
	"github.com/SamuelMolling/godwit/internal/creds"
	"github.com/SamuelMolling/godwit/internal/engine"
	"github.com/SamuelMolling/godwit/internal/report"
)

func newTargetStatusCmd() *cobra.Command {
	flags := &clientFlags{}
	var dir string
	cmd := &cobra.Command{
		Use:   "status <name>",
		Short: "Show what a target has applied, what is pending, its last run and drift baseline",
		Args:  cobra.ExactArgs(1),
		RunE: flags.runE(func(cmd *cobra.Command, client godwitv1connect.GodwitServiceClient, args []string) error {
			files, err := optionalFiles(dir)
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
	cmd.Flags().StringVar(&dir, "dir", config.Defaults().Dir, "migration directory to compare against; an absent one compares against nothing")
	configKeys(cmd, "dir")

	return cmd
}

func newTargetShowCmd() *cobra.Command {
	flags := &clientFlags{}
	cmd := &cobra.Command{
		Use:   "show <name>",
		Short: "Show how a target is registered: its provider, where its credential is read from and the settings its runs inherit",
		Long: "The credential itself is not part of it: a static target's DSN is sealed in the registration and\n" +
			"never shown, and changing another setting with `godwit target add` keeps it.",
		Args: cobra.ExactArgs(1),
		RunE: flags.runE(func(cmd *cobra.Command, client godwitv1connect.GodwitServiceClient, args []string) error {
			resp, err := client.GetTarget(cmd.Context(), connect.NewRequest(&godwitv1.GetTargetRequest{Name: args[0]}))
			if err != nil {
				return err
			}
			flags.print(cmd, resp.Msg, targetText(resp.Msg))

			return nil
		}),
	}
	flags.register(cmd)

	return cmd
}

func targetText(t *godwitv1.GetTargetResponse) string {
	rows := [][2]string{{"provider", t.Provider}}
	switch t.Provider {
	case creds.ProviderStatic:
		rows = append(rows, [2]string{"dsn registered", fmt.Sprintf("%t", t.DsnRegistered)})
	case creds.ProviderKubernetes:
		rows = append(rows, [2]string{"secret path", t.SecretPath})
	case creds.ProviderVault:
		rows = append(rows, [2]string{"credential store", t.CredentialStore},
			[2]string{"vault path", t.VaultPath}, [2]string{"vault template", orDefaultTemplate(t.VaultTemplate)})
	}
	rows = append(rows,
		[2]string{"lock timeout", orNone(t.LockTimeout)}, [2]string{"statement timeout", orNone(t.StatementTimeout)},
		[2]string{"search path", orNone(t.SearchPath)}, [2]string{"require plan", fmt.Sprintf("%t", t.RequirePlan)},
		[2]string{"keep old", fmt.Sprintf("%t", t.KeepOld)},
		[2]string{"ignore adopted tables", fmt.Sprintf("%t", t.IgnoreAdoptedTables)},
		[2]string{"github repositories", orNone(strings.Join(t.GithubRepositories, " "))})
	var b strings.Builder
	fmt.Fprintf(&b, "target %s\n", t.Name)
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	for _, r := range rows {
		fmt.Fprintf(w, "  %s\t%s\n", r[0], r[1])
	}
	_ = w.Flush()

	return strings.TrimSuffix(b.String(), "\n")
}

func orDefaultTemplate(s string) string {
	if s == "" {
		return "{{dsn}} (default)"
	}

	return s
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
	fmt.Fprintln(w, "NAME\tPROVIDER\tSTORE\tAPPLIED\tREADY PLANS\tNEEDS YOU\tDRIFT\tSEARCH PATH\tLOCK\tSTATEMENT\tREQUIRE PLAN\tGITHUB\tLAST RUN")
	for _, t := range targets {
		last := "none"
		if t.LastRun != nil {
			last = t.LastRun.Id + " " + report.StateName(t.LastRun.State)
		}
		drift := "clean"
		if t.UnresolvedDrift {
			drift = "drifted"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%d\t%d\t%s\t%s\t%s\t%s\t%t\t%s\t%s\n", t.Name, t.Provider,
			orNone(t.CredentialStore), t.AppliedCount, t.ReadyPlans,
			t.AttentionRuns, drift, orNone(t.SearchPath), orNone(t.LockTimeout), orNone(t.StatementTimeout), t.RequirePlan,
			orNone(strings.Join(t.GithubRepositories, " ")), last)
	}
	_ = w.Flush()

	return strings.TrimSuffix(b.String(), "\n")
}

// optionalFiles is the committed set for a command that only reads or generates: nothing is applied over it, so an absent directory is nothing to compare against.
func optionalFiles(dir string) ([]*godwitv1.MigrationFile, error) {
	migs, _, err := emptySet(dir)
	if err != nil {
		return nil, err
	}

	return protoFiles(migs), nil
}

func statusText(st *godwitv1.GetTargetStatusResponse) string {
	var b strings.Builder
	fmt.Fprintf(&b, "target %s: provider %s, lock timeout %s, statement timeout %s, search path %s\n",
		st.Target, st.Provider, orNone(st.LockTimeout), orNone(st.StatementTimeout), orNone(st.SearchPath))
	if st.Unreachable != "" {
		fmt.Fprintf(&b, "its own journal was not read: %s\n", st.Unreachable)
	}
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
		fmt.Fprintf(w, "  %s\t%s\t%s\n", engine.MigrationID(a.Version, a.Name, a.Repeatable), report.Stamp(a.AppliedAt), note)
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
	line := fmt.Sprintf("last run: %s %s %s", r.Id, r.Kind, report.StateName(r.State))
	if r.FinishedAt != nil {
		line += " finished " + report.Stamp(r.FinishedAt)
	}
	fmt.Fprintln(w, line)
}

func writeBaseline(w io.Writer, d *godwitv1.DriftBaseline) {
	if d == nil {
		fmt.Fprintln(w, "drift baseline: none")

		return
	}
	line := "drift baseline: taken " + report.Stamp(d.TakenAt)
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
