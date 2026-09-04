package cli

import (
	"fmt"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/gen/godwit/v1/godwitv1connect"
)

func newMigrationsCmd() *cobra.Command {
	flags := &clientFlags{}
	req := &godwitv1.ListMigrationsRequest{}
	cmd := &cobra.Command{
		Use:   "migrations",
		Short: "Show which targets have which migration, from the control plane alone",
		Args:  cobra.NoArgs,
		RunE: flags.runE(func(cmd *cobra.Command, client godwitv1connect.GodwitServiceClient, _ []string) error {
			resp, err := client.ListMigrations(cmd.Context(), connect.NewRequest(req))
			if err != nil {
				return err
			}
			flags.print(cmd, resp.Msg, fleetTable(resp.Msg))

			return nil
		}),
	}
	flags.register(cmd)
	cmd.Flags().StringSliceVar(&req.Targets, "target", nil, "only these targets (repeatable); default every registered one")
	cmd.Flags().Int64Var(&req.FromVersion, "from", 0, "lowest version to report, inclusive; either bound leaves repeatables out")
	cmd.Flags().Int64Var(&req.ToVersion, "to", 0, "highest version to report, inclusive; either bound leaves repeatables out")
	cmd.Flags().BoolVar(&req.NotEverywhere, "not-everywhere", false, "only migrations at least one target does not have")
	cmd.Flags().StringVar(&req.InTarget, "in", "", "only migrations standing on this target")
	cmd.Flags().StringVar(&req.NotInTarget, "not-in", "", "only migrations not standing on this target")

	return cmd
}

func fleetTable(resp *godwitv1.ListMigrationsResponse) string {
	if len(resp.Targets) == 0 {
		return "no targets registered"
	}
	if len(resp.Migrations) == 0 {
		return "nothing stands on " + strings.Join(resp.Targets, ", ")
	}
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprint(w, "MIGRATION\tCHECKSUM")
	for _, t := range resp.Targets {
		fmt.Fprint(w, "\t"+strings.ToUpper(t))
	}
	fmt.Fprintln(w)
	for _, m := range resp.Migrations {
		cells := fleetCells(m)
		fmt.Fprintf(w, "%s\t%s", m.Migration, shortSum(m.Checksum))
		for _, t := range resp.Targets {
			fmt.Fprint(w, "\t"+cells[t])
		}
		fmt.Fprintln(w)
	}
	_ = w.Flush()
	fmt.Fprint(&b, "\n"+fleetSummary(resp))

	return strings.TrimSuffix(b.String(), "\n")
}

func fleetCells(m *godwitv1.FleetMigration) map[string]string {
	cells := make(map[string]string, len(m.AppliedOn)+len(m.MissingFrom))
	for _, o := range m.AppliedOn {
		cells[o.Target] = o.AppliedAt.AsTime().UTC().Format(time.DateOnly)
		if o.CollapsedBy != "" {
			cells[o.Target] += "*"
		}
	}
	for _, g := range m.MissingFrom {
		cells[g.Target] = "missing"
		if g.Holds {
			cells[g.Target] = "differs"
		}
		if g.Behind {
			cells[g.Target] = "-"
		}
	}

	return cells
}

func fleetSummary(resp *godwitv1.ListMigrationsResponse) string {
	gaps, diverged := 0, map[string]bool{}
	for _, m := range resp.Migrations {
		if len(m.MissingFrom) > 0 {
			gaps++
		}
		if m.Divergent {
			diverged[m.Migration] = true
		}
	}
	head := countOf(len(resp.Migrations), "migration") + ", " + countOf(len(resp.Targets), "target") + ": "
	if gaps == 0 {
		return head + "all on every target"
	}

	return fmt.Sprintf("%s%d not on every target, %d under more than one checksum\n%s",
		head, gaps, len(diverged),
		"key: - not there yet · missing the target is past it · differs another checksum · * recorded by a checkpoint")
}

func countOf(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}

	return strconv.Itoa(n) + " " + unit + "s"
}

func shortSum(sum string) string {
	if sum == "" {
		return "unknown"
	}

	return sum[:8]
}
