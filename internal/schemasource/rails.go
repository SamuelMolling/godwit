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

// Rails is a Source over a Rails application's committed db/structure.sql, stripped of the
// session settings, psql meta-commands and schema_migrations rows pg_dump leaves in it.
type Rails struct {
	Path string
}

const railsSchemaFormat = "set config.active_record.schema_format = :sql in config/application.rb and run bin/rails db:schema:dump"

var (
	pgDumpNoise    = regexp.MustCompile(`(?i)^(\\(un)?restrict\b|set\s+[a-z_.]+\s*(=|to)\s.*;$|select\s+pg_catalog\.set_config\(.*;$)`)
	versionInsert  = regexp.MustCompile(`(?i)^insert\s+into\s+("?[a-z_0-9]+"?\.)?"?schema_migrations"?\b`)
	dollarQuoteTag = regexp.MustCompile(`\$[A-Za-z_][A-Za-z_0-9]*\$|\$\$`)
)

// Load reads the dump the Rails application checked in and returns it as plain DDL.
func (r Rails) Load(context.Context) (string, error) {
	if strings.TrimSpace(r.Path) == "" {
		return "", errors.New("no Rails application configured; pass --rails . or set schema_source.path")
	}
	path, err := r.dump()
	if err != nil {
		return "", err
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	ddl := stripPgDump(string(body))
	if strings.TrimSpace(ddl) == "" {
		return "", fmt.Errorf("%s holds no DDL; %s to regenerate it", path, railsSchemaFormat)
	}

	return ddl, nil
}

func (r Rails) dump() (string, error) {
	info, err := os.Stat(r.Path)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		if filepath.Base(r.Path) == "schema.rb" {
			return "", rubySchema(r.Path)
		}

		return r.Path, nil
	}
	structure := filepath.Join(r.Path, "db", "structure.sql")
	if _, err := os.Stat(structure); err == nil {
		return structure, nil
	}
	ruby := filepath.Join(r.Path, "db", "schema.rb")
	if _, err := os.Stat(ruby); err == nil {
		return "", rubySchema(ruby)
	}

	return "", fmt.Errorf("%s has no db/structure.sql and no db/schema.rb; --rails takes the Rails application root or the dump itself", r.Path)
}

func rubySchema(path string) error {
	return fmt.Errorf("%s is a Ruby DSL, not SQL, and rendering it means booting ActiveRecord against a database; %s", path, railsSchemaFormat)
}

func stripPgDump(body string) string {
	var out strings.Builder
	var tag string
	skipping := false
	for line := range strings.SplitSeq(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if skipping {
			skipping = !strings.HasSuffix(trimmed, ";")

			continue
		}
		if tag == "" && pgDumpNoise.MatchString(trimmed) {
			continue
		}
		if tag == "" && versionInsert.MatchString(trimmed) {
			skipping = !strings.HasSuffix(trimmed, ";")

			continue
		}
		tag = dollarQuoteState(line, tag)
		out.WriteString(line)
		out.WriteString("\n")
	}

	return strings.TrimSuffix(out.String(), "\n")
}

func dollarQuoteState(line, tag string) string {
	for _, t := range dollarQuoteTag.FindAllString(line, -1) {
		switch {
		case tag == "":
			tag = t
		case t == tag:
			tag = ""
		}
	}

	return tag
}
