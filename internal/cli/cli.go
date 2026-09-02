// Package cli wires the godwit command tree.
package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/SamuelMolling/godwit/internal/config"
	"github.com/SamuelMolling/godwit/internal/version"
)

// Main runs the godwit CLI and returns the process exit code.
func Main(args []string, out, errOut io.Writer) int {
	root := newRootCmd()
	root.SetArgs(args)
	root.SetOut(out)
	root.SetErr(errOut)

	if err := root.Execute(); err != nil {
		fmt.Fprintf(errOut, "godwit: %v\n", err)
		return 1
	}

	return 0
}

func newRootCmd() *cobra.Command {
	var configPath string
	root := &cobra.Command{
		Use:           "godwit",
		Short:         "Crash-safe database migration service",
		Long:          "godwit is a pipeline-native database migration service with crash-safe execution.",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			return applyConfig(cmd, configPath)
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	root.PersistentFlags().StringVar(&configPath, "config", "", "path to godwit.yaml (default: nearest godwit.yaml up to the repo root)")
	run := newRunCmd()
	addRunSubcommands(run)
	root.AddCommand(newVersionCmd(), newPlanCmd(), run, newStatusCmd(), newDownCmd(), newServeCmd(),
		newTargetCmd(), newMigrateCmd(), newRevertCmd(), newRunsCmd(), newDriftCmd())

	return root
}

func applyConfig(cmd *cobra.Command, path string) error {
	flags := cmd.Flags()
	if flags.Lookup("dir") == nil {
		return nil
	}
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	for name, value := range map[string]string{
		"dir":               cfg.Dir,
		"lock-timeout":      cfg.LockTimeout.String(),
		"statement-timeout": cfg.StatementTimeout.String(),
	} {
		if fl := flags.Lookup(name); fl != nil && !fl.Changed {
			_ = fl.Value.Set(value)
		}
	}

	return nil
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the godwit version",
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Fprintln(cmd.OutOrStdout(), version.String())
		},
	}
}
