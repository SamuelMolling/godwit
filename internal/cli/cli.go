// Package cli wires the godwit command tree.
package cli

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

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
		var exit exitError
		if errors.As(err, &exit) {
			return exit.code
		}

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
	root.AddCommand(newVersionCmd(), newPlanCmd(), newApplyCmd(), newStatusCmd(), newDownCmd(), newServeCmd(),
		newTargetCmd(), newMigrateCmd(), newRevertCmd(), newRunCmd(), newRunsCmd(), newPlansCmd(), newAuditCmd(), newDriftCmd(), newLintCmd())

	return root
}

func configKeys(cmd *cobra.Command, keys ...string) {
	if cmd.Annotations == nil {
		cmd.Annotations = map[string]string{}
	}
	cmd.Annotations["config"] = strings.Join(append(strings.Split(cmd.Annotations["config"], ","), keys...), ",")
}

func applyConfig(cmd *cobra.Command, path string) error {
	keys, ok := cmd.Annotations["config"]
	if !ok {
		return nil
	}
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	values := map[string]string{
		"dir":                cfg.Dir,
		"target":             cfg.Target,
		"rollout":            cfg.Rollout,
		"server":             cfg.Server,
		"lock-timeout":       cfg.LockTimeout.String(),
		"statement-timeout":  cfg.StatementTimeout.String(),
		"allow-out-of-order": strconv.FormatBool(cfg.AllowOutOfOrder),
	}
	for name := range strings.SplitSeq(keys, ",") {
		if fl := cmd.Flags().Lookup(name); fl != nil && !fl.Changed && values[name] != "" {
			_ = fl.Value.Set(values[name])
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
