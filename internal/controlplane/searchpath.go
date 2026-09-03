package controlplane

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// ConfigSearchPath is the target config key holding the per-target search_path.
const ConfigSearchPath = "search_path"

// JournalSchema is where godwit's journal lives on every target, whatever the search_path is.
const JournalSchema = "godwit"

var schemaName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_$]*$`)

// ParseSearchPath validates a comma-separated list of unquoted schema names and folds it the way PostgreSQL does;
// empty keeps the target role's own default.
func ParseSearchPath(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", nil
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		name := strings.TrimSpace(part)
		if !schemaName.MatchString(name) {
			return "", fmt.Errorf("%s: %q is not a schema name; give unquoted identifiers separated by commas", ConfigSearchPath, part)
		}
		name = strings.ToLower(name)
		if name == JournalSchema {
			return "", fmt.Errorf("%s: %q holds godwit's journal and must not be on a target's search path", ConfigSearchPath, name)
		}
		out = append(out, name)
	}

	return strings.Join(out, ","), nil
}

// dsnWithSearchPath pins the search path as a connection parameter, so every session godwit opens on the target carries it.
func dsnWithSearchPath(dsn, searchPath string) string {
	if searchPath == "" {
		return dsn
	}
	if !strings.HasPrefix(dsn, "postgres://") && !strings.HasPrefix(dsn, "postgresql://") {
		return dsn + " " + ConfigSearchPath + "=" + searchPath
	}
	base, query, _ := strings.Cut(dsn, "?")
	values, _ := url.ParseQuery(query)
	values.Set(ConfigSearchPath, searchPath)

	return base + "?" + values.Encode()
}
