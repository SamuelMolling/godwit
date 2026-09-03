package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/gen/godwit/v1/godwitv1connect"
	"github.com/SamuelMolling/godwit/internal/config"
	"github.com/SamuelMolling/godwit/internal/engine"
)

var (
	diffNow    = time.Now
	nameRe     = regexp.MustCompile(`^[a-z0-9_]+$`)
	readSchema = os.ReadFile
)

type diffJSON struct {
	Target     string           `json:"target"`
	Changed    bool             `json:"changed"`
	UpSQL      string           `json:"up_sql"`
	DownSQL    string           `json:"down_sql"`
	Statements []statementJSON  `json:"statements"`
	Drift      string           `json:"drift,omitempty"`
	Observed   *planObservation `json:"observed,omitempty"`
	Files      []string         `json:"files"`
}

func newDiffCmd() *cobra.Command {
	flags := &clientFlags{}
	var target, schema, name, dir string
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "diff",
		Short: "Generate the migration from a target's live schema to a desired schema",
		Args:  cobra.NoArgs,
		RunE: flags.runE(func(cmd *cobra.Command, client godwitv1connect.GodwitServiceClient, _ []string) error {
			if target == "" {
				return errors.New("--target (or target in godwit.yaml) is required")
			}
			if schema == "" {
				return errors.New("--schema is required")
			}
			if !dryRun && !nameRe.MatchString(name) {
				return errors.New("--name is required and must be snake_case ([a-z0-9_]+)")
			}
			ddl, err := readSchema(schema)
			if err != nil {
				return err
			}
			res, err := client.Diff(cmd.Context(), connect.NewRequest(&godwitv1.DiffRequest{Target: target, Schema: string(ddl)}))
			if err != nil {
				return err
			}
			var files []string
			if res.Msg.UpSql != "" && !dryRun {
				if files, err = writeDiff(dir, name, res.Msg); err != nil {
					return err
				}
			}

			writeDiffReport(cmd.OutOrStdout(), res.Msg, files, schema, flags.json)

			return nil
		}),
	}
	flags.register(cmd)
	cmd.Flags().StringVar(&target, "target", "", "target name")
	cmd.Flags().StringVar(&schema, "schema", "", "file holding the whole desired database as DDL")
	cmd.Flags().StringVar(&name, "name", "", "migration name, snake_case; becomes <timestamp>_<name>.{up,down}.sql")
	cmd.Flags().StringVar(&dir, "dir", config.Defaults().Dir, "migration directory")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the migration without writing files")
	configKeys(cmd, "target", "dir")

	return cmd
}

func writeDiff(dir, name string, m *godwitv1.DiffResponse) ([]string, error) {
	prefix := filepath.Join(dir, diffNow().UTC().Format("20060102150405")+"_"+name)
	files := []string{prefix + ".up.sql", prefix + ".down.sql"}
	for i, body := range []string{m.UpSql, m.DownSql} {
		if err := os.WriteFile(files[i], []byte(body+"\n"), 0o644); err != nil {
			return nil, err
		}
	}

	return files, nil
}

func writeDiffReport(w io.Writer, m *godwitv1.DiffResponse, files []string, schema string, asJSON bool) {
	if asJSON {
		writeDiffJSON(w, m, files)

		return
	}
	if m.UpSql == "" {
		fmt.Fprintf(w, "no changes: %s already matches %s\n", m.Target, schema)

		return
	}
	fmt.Fprint(w, planReport{drift: m.Drift}.driftBlock("drift (the target's live schema, not its history, is the starting point):", "  ", "", ""))
	fmt.Fprintf(w, "%s -> %s: %d statement(s)\n", m.Target, schema, len(m.Statements))
	for i, st := range m.Statements {
		fmt.Fprintf(w, "  [%d] %-5s %s\n", i, statementMode(engine.Statement{NoTx: st.NoTx}), firstLine(st.Sql))
		for _, h := range st.Hazards {
			fmt.Fprintf(w, "        hazard %s: %s\n", h.Code, h.Detail)
			writeRecipeText(w, "          ", h.Recipe)
		}
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "-- up")
	fmt.Fprintln(w, m.UpSql)
	fmt.Fprintln(w, "-- down")
	fmt.Fprintln(w, m.DownSql)
	for _, f := range files {
		fmt.Fprintln(w, "wrote", f)
	}
}

func writeDiffJSON(w io.Writer, m *godwitv1.DiffResponse, files []string) {
	out := diffJSON{
		Target: m.Target, Changed: m.UpSql != "", UpSQL: m.UpSql, DownSQL: m.DownSql,
		Statements: []statementJSON{}, Drift: m.Drift, Files: append([]string{}, files...),
	}
	for _, st := range m.Statements {
		hazards := make([]hazardJSON, 0, len(st.Hazards))
		for _, h := range st.Hazards {
			hazards = append(hazards, hazardJSON{Code: h.Code, Detail: h.Detail, Recipe: h.Recipe})
		}
		out.Statements = append(out.Statements, statementJSON{SQL: st.Sql, Mode: statementMode(engine.Statement{NoTx: st.NoTx}), Hazards: hazards})
	}
	out.Observed = observationFromProto(m.Observed)
	body, _ := json.MarshalIndent(out, "", "  ")
	fmt.Fprintln(w, string(body))
}
