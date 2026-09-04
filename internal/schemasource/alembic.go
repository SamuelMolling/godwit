package schemasource

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// DefaultAlembicBin is the command line used to run the Alembic CLI when none is configured.
const DefaultAlembicBin = "alembic"

// Alembic is a Source over an Alembic project: the SQL of every revision from base to head,
// as `alembic upgrade head --sql` renders it in offline mode. No database is contacted.
type Alembic struct {
	Config string
	Bin    string
	Run    Runner
}

var (
	iniComment    = regexp.MustCompile(`(?m)^[ \t]*[#;][^\n]*`)
	sqlalchemyURL = regexp.MustCompile(`(?m)^[ \t]*sqlalchemy\.url[ \t]*=[ \t]*([^\n]*)$`)
	urlScheme     = regexp.MustCompile(`^([A-Za-z0-9_.+-]+)://`)
)

// Load runs the Alembic CLI in offline mode and returns its SQL script without the transaction wrappers.
func (a Alembic) Load(ctx context.Context) (string, error) {
	if strings.TrimSpace(a.Config) == "" {
		return "", errors.New("no Alembic project configured; pass --alembic alembic.ini or set schema_source.path")
	}
	if err := a.checkDialect(); err != nil {
		return "", err
	}
	bin := fields(a.Bin, DefaultAlembicBin)
	argv := append(append([]string{}, bin...), "-c", a.Config, "upgrade", "head", "--sql")
	out, err := capture(ctx, a.Run, argv, func(err error, stderr []byte) error {
		if missingBinary(err) {
			return fmt.Errorf("the Alembic CLI was not found (%w); install alembic or point --alembic-bin / GODWIT_ALEMBIC_BIN at it", err)
		}

		return runFailed("alembic upgrade head --sql", err, stderr)
	})
	if err != nil {
		return "", err
	}
	var ddl strings.Builder
	writeStatements(&ddl, out)
	if strings.TrimSpace(ddl.String()) == "" {
		return "", fmt.Errorf("alembic upgrade head --sql produced no DDL for %s; offline mode renders every revision from base, so an empty history renders nothing", a.Config)
	}

	return ddl.String(), nil
}

func (a Alembic) checkDialect() error {
	body, err := os.ReadFile(a.Config)
	if err != nil {
		return err
	}
	var found []string
	for _, m := range sqlalchemyURL.FindAllStringSubmatch(string(iniComment.ReplaceAll(body, nil)), -1) {
		url := strings.TrimSpace(m[1])
		scheme := urlScheme.FindStringSubmatch(url)
		if scheme == nil {
			continue
		}
		dialect, _, _ := strings.Cut(scheme[1], "+")
		switch strings.ToLower(dialect) {
		case "postgresql", "postgres":
			return nil
		}
		found = append(found, scheme[1])
	}
	if len(found) == 0 {
		return nil
	}

	return fmt.Errorf("%s: sqlalchemy.url is %s; godwit targets PostgreSQL only (sqlalchemy.url = postgresql+psycopg://...)",
		a.Config, strings.Join(found, ", "))
}
