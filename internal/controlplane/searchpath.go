package controlplane

import (
	"errors"
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

// ParseSearchPath validates a comma-separated list of unquoted schema names and folds it the way PostgreSQL does.
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

// ErrJournalOnSearchPath marks a target whose sessions resolve unqualified names against the journal schema.
var ErrJournalOnSearchPath = errors.New("the target's search_path reaches godwit's journal schema")

func journalOnPath(effective, setting, role string) (string, bool) {
	for _, element := range append(strings.Split(effective, ","), strings.Split(setting, ",")...) {
		name := strings.TrimSpace(element)
		if strings.HasPrefix(name, `"`) {
			name = strings.Trim(name, `"`)
		} else {
			name = strings.ToLower(name)
		}
		resolved := name
		if name == "$user" {
			resolved = role
		}
		if resolved == JournalSchema {
			return name, true
		}
	}

	return "", false
}

func journalPathError(element, role string) error {
	return fmt.Errorf("%w: element %q resolves to schema %q under role %q, so a migration's unqualified CREATE TABLE would land "+
		"beside the journal's own tables and out of drift's sight; give the target a %s of its own (--search-path public), "+
		"or connect it with a role not named %s",
		ErrJournalOnSearchPath, element, JournalSchema, role, ConfigSearchPath, JournalSchema)
}

// Never PostgreSQL's default `"$user", public`: on a scratch role named godwit that resolves to the journal schema.
func scratchSearchPath(observed string) string {
	if observed == "" {
		return "public"
	}

	return observed
}

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
