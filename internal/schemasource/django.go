package schemasource

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// DefaultPythonBin is the command line used to run manage.py when none is configured.
const DefaultPythonBin = "python"

// DefaultDjangoDatabase is the DATABASES alias read when none is configured.
const DefaultDjangoDatabase = "default"

// Django is a Source over a Django project: sqlmigrate's SQL for every migration in plan order; sqlmigrate introspects over the configured connection.
type Django struct {
	ManagePy string
	Bin      string
	Settings string
	Database string
	Run      Runner
}

var (
	pythonComment  = regexp.MustCompile(`#[^\n]*`)
	settingsModule = regexp.MustCompile(`DJANGO_SETTINGS_MODULE["'](?:\s*,|\s*\]\s*=)\s*["']([^"']+)["']`)
	databasesKey   = regexp.MustCompile(`\bDATABASES\s*=\s*\{`)
	engineKey      = regexp.MustCompile(`["']ENGINE["']\s*:\s*["']([^"']*)["']`)
	planLine       = regexp.MustCompile(`(?m)^\[[^\]\n]*\][ \t]+([^\s.]+)\.(\S+)[ \t]*$`)
)

// Load runs manage.py once for the migration plan and once per migration, and concatenates the SQL in plan order.
func (d Django) Load(ctx context.Context) (string, error) {
	if strings.TrimSpace(d.ManagePy) == "" {
		return "", errors.New("no Django project configured; pass --django manage.py or set schema_source.path")
	}
	if err := d.checkEngine(); err != nil {
		return "", err
	}
	bin := fields(d.Bin, DefaultPythonBin)
	plan, err := capture(ctx, d.Run, d.argv(bin, "showmigrations", "--plan"), d.wrap("manage.py showmigrations --plan"))
	if err != nil {
		return "", err
	}
	migrations := planLine.FindAllStringSubmatch(plan, -1)
	if len(migrations) == 0 {
		return "", fmt.Errorf("manage.py showmigrations --plan listed no migrations in %s", d.ManagePy)
	}
	var ddl strings.Builder
	for _, m := range migrations {
		app, name := m[1], m[2]
		sql, err := capture(ctx, d.Run, d.argv(bin, "sqlmigrate", app, name), d.wrap("manage.py sqlmigrate "+app+" "+name))
		if err != nil {
			return "", err
		}
		writeStatements(&ddl, sql)
	}
	if strings.TrimSpace(ddl.String()) == "" {
		return "", fmt.Errorf("manage.py sqlmigrate produced no DDL for the %d migration(s) of %s", len(migrations), d.ManagePy)
	}

	return ddl.String(), nil
}

func (d Django) argv(bin []string, sub ...string) []string {
	argv := append(append([]string{}, bin...), d.ManagePy)
	argv = append(argv, sub...)
	argv = append(argv, "--no-color")
	if d.Settings != "" {
		argv = append(argv, "--settings="+d.Settings)
	}
	if d.Database != "" {
		argv = append(argv, "--database="+d.Database)
	}

	return argv
}

func (d Django) wrap(label string) func(error, []byte) error {
	return func(err error, stderr []byte) error {
		if missingBinary(err) {
			return fmt.Errorf("the Python interpreter was not found (%w); install Python or point --python-bin / GODWIT_PYTHON_BIN at it", err)
		}

		return runFailed(label, err, stderr)
	}
}

func (d Django) checkEngine() error {
	module := d.Settings
	body, err := os.ReadFile(d.ManagePy)
	if err != nil {
		return err
	}
	if module == "" {
		m := settingsModule.FindSubmatch(pythonComment.ReplaceAll(body, nil))
		if m == nil {
			return fmt.Errorf("%s sets no DJANGO_SETTINGS_MODULE; set it there or in the environment", d.ManagePy)
		}
		module = string(m[1])
	}
	path := filepath.Join(append([]string{filepath.Dir(d.ManagePy)}, strings.Split(module, ".")...)...) + ".py"
	settings, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("cannot read the Django settings of %s: %w; use kind: command with your own dump instead", module, err)
	}
	alias := d.Database
	if alias == "" {
		alias = DefaultDjangoDatabase
	}
	engine, err := databaseEngine(string(pythonComment.ReplaceAll(settings, nil)), alias)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if !strings.Contains(strings.ToLower(engine), "postgres") {
		return fmt.Errorf("%s: DATABASES[%q][\"ENGINE\"] is %q; godwit targets PostgreSQL only (django.db.backends.postgresql)", path, alias, engine)
	}

	return nil
}

func databaseEngine(settings, alias string) (string, error) {
	block, ok := dictAt(settings, databasesKey)
	if !ok {
		return "", errors.New("no closed DATABASES dict")
	}
	entry, ok := dictAt(block, regexp.MustCompile(`["']`+regexp.QuoteMeta(alias)+`["']\s*:\s*\{`))
	if !ok {
		return "", fmt.Errorf("DATABASES has no %q alias", alias)
	}
	m := engineKey.FindStringSubmatch(entry)
	if m == nil {
		return "", fmt.Errorf("DATABASES[%q] has no ENGINE", alias)
	}

	return m[1], nil
}

func dictAt(s string, re *regexp.Regexp) (string, bool) {
	at := re.FindStringIndex(s)
	if at == nil {
		return "", false
	}
	depth := 0
	for i := at[1] - 1; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			if depth--; depth == 0 {
				return s[at[1]:i], true
			}
		}
	}

	return "", false
}

func writeStatements(out *strings.Builder, sql string) {
	for line := range strings.SplitSeq(sql, "\n") {
		switch strings.ToUpper(strings.TrimSpace(line)) {
		case "BEGIN;", "COMMIT;":
			continue
		}
		out.WriteString(line)
		out.WriteString("\n")
	}
}
