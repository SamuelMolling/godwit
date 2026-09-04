package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/gen/godwit/v1/godwitv1connect"
	"github.com/SamuelMolling/godwit/internal/config"
	"github.com/SamuelMolling/godwit/internal/engine"
	"github.com/SamuelMolling/godwit/internal/schemasource"
)

var (
	diffNow = time.Now
	nameRe  = regexp.MustCompile(`^[a-z0-9_]+$`)
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

	RepeatableObjects []string `json:"repeatable_objects"`
}

func newDiffCmd() *cobra.Command {
	flags := &clientFlags{}
	src := &sourceFlags{}
	var target, name, dir string
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "diff",
		Short: "Generate the migration from a target's live schema to a desired schema",
		Args:  cobra.NoArgs,
		RunE: flags.runE(func(cmd *cobra.Command, client godwitv1connect.GodwitServiceClient, _ []string) error {
			if target == "" {
				return errors.New("--target (or target in godwit.yaml) is required")
			}
			source, label, err := src.pick(cmd)
			if err != nil {
				return err
			}
			if !dryRun && !nameRe.MatchString(name) {
				return errors.New("--name is required and must be snake_case ([a-z0-9_]+)")
			}
			ddl, err := source.Load(cmd.Context())
			if err != nil {
				return err
			}
			committed, err := optionalFiles(cmd, dir)
			if err != nil {
				return err
			}
			res, err := client.Diff(cmd.Context(),
				connect.NewRequest(&godwitv1.DiffRequest{Target: target, Schema: ddl, Files: committed}))
			if err != nil {
				return err
			}
			var files []string
			if res.Msg.UpSql != "" && !dryRun {
				if files, err = writeDiff(dir, name, res.Msg); err != nil {
					return err
				}
			}

			writeDiffReport(cmd.OutOrStdout(), res.Msg, files, label, flags.json)

			return nil
		}),
	}
	flags.register(cmd)
	cmd.Flags().StringVar(&target, "target", "", "target name")
	src.register(cmd)
	cmd.Flags().StringVar(&name, "name", "", "migration name, snake_case; becomes <timestamp>_<name>.{up,down}.sql")
	cmd.Flags().StringVar(&dir, "dir", config.Defaults().Dir, "migration directory")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the migration without writing files")
	configKeys(cmd, "target", "dir")

	return cmd
}

type sourceFlags struct {
	schema     string
	prisma     string
	exec       string
	gorm       string
	django     string
	alembic    string
	rails      string
	drizzle    string
	prismaBin  string
	goBin      string
	pythonBin  string
	alembicBin string
	drizzleBin string
	database   string
	settings   string
}

func (s *sourceFlags) register(cmd *cobra.Command) {
	cmd.Flags().StringVar(&s.schema, "schema", "", "file holding the whole desired database as DDL")
	cmd.Flags().StringVar(&s.prisma, "prisma", "", "Prisma schema (schema.prisma) rendered to DDL by the Prisma CLI, no database needed")
	cmd.Flags().StringVar(&s.exec, "exec", "", "command line that prints the whole desired database as DDL on stdout")
	cmd.Flags().StringVar(&s.gorm, "gorm", "", "Go package, run with go run, that prints GORM's dry-run DDL on stdout")
	cmd.Flags().StringVar(&s.django, "django", "", "Django manage.py whose migrations are rendered with showmigrations and sqlmigrate")
	cmd.Flags().StringVar(&s.alembic, "alembic", "", "Alembic config (alembic.ini) whose revisions are rendered with upgrade head --sql")
	cmd.Flags().StringVar(&s.rails, "rails", "", "Rails application root (or the dump itself) whose committed db/structure.sql is the DDL")
	cmd.Flags().StringVar(&s.drizzle, "drizzle", "", "Drizzle Kit config (drizzle.config.ts) rendered to DDL by drizzle-kit export")
	cmd.Flags().StringVar(&s.prismaBin, "prisma-bin", envOr("GODWIT_PRISMA_BIN", schemasource.DefaultPrismaBin), "command line that runs the Prisma CLI ($GODWIT_PRISMA_BIN)")
	cmd.Flags().StringVar(&s.goBin, "go-bin", envOr("GODWIT_GO_BIN", schemasource.DefaultGoBin), "command line that runs the Go toolchain ($GODWIT_GO_BIN)")
	cmd.Flags().StringVar(&s.pythonBin, "python-bin", envOr("GODWIT_PYTHON_BIN", schemasource.DefaultPythonBin), "command line that runs manage.py ($GODWIT_PYTHON_BIN)")
	cmd.Flags().StringVar(&s.alembicBin, "alembic-bin", envOr("GODWIT_ALEMBIC_BIN", schemasource.DefaultAlembicBin), "command line that runs the Alembic CLI ($GODWIT_ALEMBIC_BIN)")
	cmd.Flags().StringVar(&s.drizzleBin, "drizzle-bin", envOr("GODWIT_DRIZZLE_BIN", schemasource.DefaultDrizzleBin), "command line that runs Drizzle Kit ($GODWIT_DRIZZLE_BIN)")
	cmd.Flags().StringVar(&s.database, "django-database", "", "DATABASES alias sqlmigrate introspects; empty leaves Django on its own default")
	s.settings = os.Getenv("DJANGO_SETTINGS_MODULE")
}

func (s *sourceFlags) named() []struct{ name, value string } {
	return []struct{ name, value string }{
		{"--schema", s.schema},
		{"--prisma", s.prisma},
		{"--exec", s.exec},
		{"--gorm", s.gorm},
		{"--django", s.django},
		{"--alembic", s.alembic},
		{"--rails", s.rails},
		{"--drizzle", s.drizzle},
	}
}

func (s *sourceFlags) pick(cmd *cobra.Command) (schemasource.Source, string, error) {
	var given, all []string
	for _, f := range s.named() {
		all = append(all, f.name)
		if f.value != "" {
			given = append(given, f.name)
		}
	}
	if len(given) > 1 {
		return nil, "", fmt.Errorf("%s are exclusive: one desired schema per diff", joinAnd(given))
	}
	if len(given) == 1 {
		return s.fromFlag(given[0])
	}
	configured := configFrom(cmd.Context()).SchemaSource
	if configured == nil {
		return nil, "", fmt.Errorf("one of %s, or schema_source in godwit.yaml is required", strings.Join(all, ", "))
	}

	return s.fromConfig(*configured, cmd)
}

func (s *sourceFlags) fromFlag(name string) (schemasource.Source, string, error) {
	switch name {
	case "--schema":
		return schemasource.File{Path: s.schema}, s.schema, nil
	case "--prisma":
		return prismaSource(s.prisma, s.prismaBin)
	case "--exec":
		return commandSource(strings.Fields(s.exec))
	case "--gorm":
		return gormSource(s.gorm, s.goBin)
	case "--django":
		return s.djangoSource(s.django, s.pythonBin)
	case "--alembic":
		return alembicSource(s.alembic, s.alembicBin)
	case "--rails":
		return railsSource(s.rails)
	}

	return drizzleSource(s.drizzle, s.drizzleBin)
}

func (s *sourceFlags) fromConfig(source config.SchemaSource, cmd *cobra.Command) (schemasource.Source, string, error) {
	flags := cmd.Flags()
	if source.Kind == config.SourceCommand {
		return commandSource(source.Command)
	}
	if source.Path == "" {
		return nil, "", fmt.Errorf("schema_source.path is required for kind %s", source.Kind)
	}
	switch source.Kind {
	case config.SourceFile:
		return schemasource.File{Path: source.Path}, source.Path, nil
	case config.SourcePrisma:
		return prismaSource(source.Path, pick(s.prismaBin, source.Bin, flags.Changed("prisma-bin")))
	case config.SourceGorm:
		return gormSource(source.Path, pick(s.goBin, source.Bin, flags.Changed("go-bin")))
	case config.SourceDjango:
		return s.djangoSource(source.Path, pick(s.pythonBin, source.Bin, flags.Changed("python-bin")))
	case config.SourceAlembic:
		return alembicSource(source.Path, pick(s.alembicBin, source.Bin, flags.Changed("alembic-bin")))
	case config.SourceRails:
		return railsSource(source.Path)
	}

	return drizzleSource(source.Path, pick(s.drizzleBin, source.Bin, flags.Changed("drizzle-bin")))
}

func pick(fromFlag, fromConfig string, flagGiven bool) string {
	if fromConfig != "" && !flagGiven {
		return fromConfig
	}

	return fromFlag
}

func joinAnd(names []string) string {
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

func prismaSource(schema, bin string) (schemasource.Source, string, error) {
	if strings.TrimSpace(bin) == "" {
		return nil, "", errors.New("--prisma-bin (or GODWIT_PRISMA_BIN) must name the Prisma CLI")
	}

	return schemasource.Prisma{Schema: schema, Bin: bin}, schema, nil
}

func commandSource(argv []string) (schemasource.Source, string, error) {
	if len(argv) == 0 {
		return nil, "", errors.New("--exec (or schema_source.command) must name a command that prints the desired schema as DDL")
	}

	return schemasource.Command{Argv: argv}, strings.Join(argv, " "), nil
}

func gormSource(pkg, bin string) (schemasource.Source, string, error) {
	if strings.TrimSpace(bin) == "" {
		return nil, "", errors.New("--go-bin (or GODWIT_GO_BIN) must name the Go toolchain")
	}

	return schemasource.Gorm{Pkg: pkg, Bin: bin}, "go run " + pkg, nil
}

func alembicSource(cfg, bin string) (schemasource.Source, string, error) {
	if strings.TrimSpace(bin) == "" {
		return nil, "", errors.New("--alembic-bin (or GODWIT_ALEMBIC_BIN) must name the Alembic CLI")
	}

	return schemasource.Alembic{Config: cfg, Bin: bin}, cfg, nil
}

func drizzleSource(cfg, bin string) (schemasource.Source, string, error) {
	if strings.TrimSpace(bin) == "" {
		return nil, "", errors.New("--drizzle-bin (or GODWIT_DRIZZLE_BIN) must name Drizzle Kit")
	}

	return schemasource.Drizzle{Config: cfg, Bin: bin}, cfg, nil
}

func railsSource(path string) (schemasource.Source, string, error) {
	return schemasource.Rails{Path: path}, path, nil
}

func (s *sourceFlags) djangoSource(managePy, bin string) (schemasource.Source, string, error) {
	if strings.TrimSpace(bin) == "" {
		return nil, "", errors.New("--python-bin (or GODWIT_PYTHON_BIN) must name the Python interpreter")
	}

	return schemasource.Django{ManagePy: managePy, Bin: bin, Settings: s.settings, Database: s.database}, managePy, nil
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
	if len(m.RepeatableObjects) > 0 {
		fmt.Fprintf(w, "declared by repeatable migrations, so the desired schema keeps them: %s\n",
			strings.Join(m.RepeatableObjects, ", "))
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
		RepeatableObjects: append([]string{}, m.RepeatableObjects...),
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
