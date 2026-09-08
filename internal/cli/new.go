package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/SamuelMolling/godwit/internal/config"
	"github.com/SamuelMolling/godwit/internal/engine"
)

var newNow = time.Now

// versionLayout is the 14-digit head of a migration file name, the only form the engine's loader accepts.
const versionLayout = "20060102150405"

// versionProbe bounds the walk to a free version: a whole minute of taken seconds is not a collision to step over.
const versionProbe = 60

const (
	upScaffold   = "-- write the forward migration here; godwit refuses an empty file\n"
	downScaffold = "-- write the inverse here; godwit refuses an empty file\n"
)

func newNewCmd() *cobra.Command {
	var dir string
	var repeatable bool
	cmd := &cobra.Command{
		Use:   "new <name>",
		Short: "Write the next migration pair, named the way the loader expects",
		Long: "Writes <timestamp>_<name>.up.sql and .down.sql in the migration directory, where the timestamp is " +
			"the current UTC second. Both files hold a placeholder line, because godwit refuses an empty side and " +
			"a migration with no statements: the pair fails plan and lint until the SQL is written.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if !nameRe.MatchString(name) {
				return fmt.Errorf("name %q must be snake_case ([a-z0-9_]+): the loader accepts no other file name", name)
			}
			id, err := nextID(dir, name, repeatable)
			if err != nil {
				return err
			}
			files, err := writeScaffold(dir, id)
			if err != nil {
				return err
			}
			for _, f := range files {
				fmt.Fprintln(cmd.OutOrStdout(), "wrote", f)
			}

			return nil
		},
	}
	cmd.Flags().StringVar(&dir, "dir", config.Defaults().Dir, "migration directory")
	cmd.Flags().BoolVar(&repeatable, "repeatable", false,
		"write R__<name>.{up,down}.sql instead: a migration with no version that re-runs whenever its body changes")
	configKeys(cmd, "dir")

	return cmd
}

// nextID steps over a taken version, which is only an ordering token, but refuses a taken repeatable name, which is the migration's identity.
func nextID(dir, name string, repeatable bool) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("read migration dir: %w", err)
	}
	if repeatable {
		id := engine.MigrationID(0, name, true)
		if taken(entries, id+".") {
			return "", fmt.Errorf("%s already exists in %s: a repeatable is keyed by its name, so edit it in place", id, dir)
		}

		return id, nil
	}
	start := newNow().UTC()
	for i := range versionProbe {
		version := start.Add(time.Duration(i) * time.Second).Format(versionLayout)
		if !taken(entries, version+"_") {
			return version + "_" + name, nil
		}
	}

	return "", fmt.Errorf("%s already holds a migration for every second from %s onwards", dir, start.Format(versionLayout))
}

func taken(entries []os.DirEntry, prefix string) bool {
	return slices.ContainsFunc(entries, func(e os.DirEntry) bool { return strings.HasPrefix(e.Name(), prefix) })
}

func writeScaffold(dir, id string) ([]string, error) {
	files := []string{filepath.Join(dir, id+".up.sql"), filepath.Join(dir, id+".down.sql")}
	for i, body := range []string{upScaffold, downScaffold} {
		if err := os.WriteFile(files[i], []byte(body), 0o644); err != nil {
			return nil, err
		}
	}

	return files, nil
}
