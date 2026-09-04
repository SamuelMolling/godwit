package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/gen/godwit/v1/godwitv1connect"
	"github.com/SamuelMolling/godwit/internal/config"
	"github.com/SamuelMolling/godwit/internal/engine"
)

type checkpointJSON struct {
	Version int64    `json:"version"`
	Name    string   `json:"name"`
	Through int64    `json:"through"`
	Covers  []string `json:"covers"`
	Body    string   `json:"body"`
	File    string   `json:"file,omitempty"`
}

func newCheckpointCmd() *cobra.Command {
	flags := &clientFlags{}
	var name, dir string
	var at int64
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "checkpoint",
		Short: "Collapse the migrations up to a version into one checkpoint file",
		Long: "Replays the migration directory on a scratch database and writes the schema it produces as one " +
			"checkpoint file. A database with no history runs the checkpoint instead of everything below it, and " +
			"every scratch replay starts from it. The migrations it collapses stay in the directory and can no " +
			"longer be reverted.",
		Args: cobra.NoArgs,
		RunE: flags.runE(func(cmd *cobra.Command, client godwitv1connect.GodwitServiceClient, _ []string) error {
			if !nameRe.MatchString(name) {
				return errNameRequired
			}
			files, err := migrationFiles(dir)
			if err != nil {
				return err
			}
			res, err := client.Checkpoint(cmd.Context(),
				connect.NewRequest(&godwitv1.CheckpointRequest{Files: files, At: at, Name: name}))
			if err != nil {
				return err
			}
			file := ""
			if !dryRun {
				if file, err = writeCheckpoint(dir, res.Msg); err != nil {
					return err
				}
			}
			writeCheckpointReport(cmd.OutOrStdout(), res.Msg, file, flags.json)

			return nil
		}),
	}
	flags.register(cmd)
	cmd.Flags().StringVar(&name, "name", "", "migration name, snake_case; becomes <timestamp>_<name>.up.sql")
	cmd.Flags().StringVar(&dir, "dir", config.Defaults().Dir, "migration directory")
	cmd.Flags().Int64Var(&at, "at", 0, "collapse every version at or below this one; default is the newest")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the checkpoint without writing the file")
	configKeys(cmd, "dir")

	return cmd
}

func writeCheckpoint(dir string, m *godwitv1.CheckpointResponse) (string, error) {
	file := filepath.Join(dir, engine.MigrationID(m.Version, m.Name, false)+".up.sql")
	if err := os.WriteFile(file, []byte(m.Body), 0o644); err != nil {
		return "", err
	}

	return file, nil
}

func writeCheckpointReport(w io.Writer, m *godwitv1.CheckpointResponse, file string, asJSON bool) {
	if asJSON {
		out := checkpointJSON{
			Version: m.Version, Name: m.Name, Through: m.Through,
			Covers: append([]string{}, m.Covers...), Body: m.Body, File: file,
		}
		body, _ := json.MarshalIndent(out, "", "  ")
		fmt.Fprintln(w, string(body))

		return
	}
	fmt.Fprintf(w, "checkpoint %s collapses %d migration(s), %s through %s\n",
		engine.MigrationID(m.Version, m.Name, false), len(m.Covers), m.Covers[0], m.Covers[len(m.Covers)-1])
	fmt.Fprintln(w, "they stay in the directory, they are never replayed again, and they can no longer be reverted")
	if file == "" {
		fmt.Fprintln(w)
		fmt.Fprint(w, m.Body)

		return
	}
	fmt.Fprintln(w, "wrote", file)
}
