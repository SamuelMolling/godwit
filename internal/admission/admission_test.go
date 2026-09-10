package admission

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/pashagolub/pgxmock/v4"

	"github.com/SamuelMolling/godwit/internal/controlplane"
	"github.com/SamuelMolling/godwit/internal/engine"
	"github.com/SamuelMolling/godwit/internal/metrics"
)

type stubObserver struct {
	obs controlplane.Observation
	err error
}

func (o stubObserver) Observe(context.Context, string) (controlplane.Observation, error) {
	return o.obs, o.err
}

func newGate(t *testing.T) (Gate, pgxmock.PgxPoolIface) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mock.Close)

	return Gate{
		Store:   controlplane.NewStore(mock),
		Log:     slog.New(slog.DiscardHandler),
		Metrics: metrics.New(),
	}, mock
}

func toFiles() map[string]string {
	return map[string]string{
		"20260901120000_a.up.sql":   "CREATE TABLE a (id int);",
		"20260901120000_a.down.sql": "DROP TABLE a;",
		"20260901120100_b.up.sql":   "CREATE TABLE b (id int);",
		"20260901120100_b.down.sql": "DROP TABLE b;",
		"R__v.up.sql":               "CREATE OR REPLACE VIEW v AS SELECT 1;",
		"R__v.down.sql":             "DROP VIEW v;",
	}
}

func toSet(t *testing.T, files map[string]string) Set {
	t.Helper()
	set, err := NewSet("", files)
	if err != nil {
		t.Fatal(err)
	}

	return set
}

func expectTarget(mock pgxmock.PgxPoolIface) {
	mock.ExpectQuery("SELECT provider, coalesce\\(credential_store, ..\\), config FROM cp_targets").WithArgs("app").
		WillReturnRows(pgxmock.NewRows([]string{"provider", "credential_store", "config"}).
			AddRow("static", "", []byte(`{}`)))
}

func expectAppliedVersions(mock pgxmock.PgxPoolIface, versions ...int64) {
	rows := pgxmock.NewRows([]string{"version"})
	for _, v := range versions {
		rows.AddRow(v)
	}
	mock.ExpectQuery("SELECT DISTINCT left").WithArgs("app").WillReturnRows(rows)
	mock.ExpectQuery("SELECT DISTINCT ON \\(a.migration\\)").WithArgs("app").WillReturnRows(pgxmock.NewRows([]string{"migration", "body"}))
}

func ids(plans []engine.Plan) []string {
	out := make([]string, 0, len(plans))
	for _, p := range plans {
		out = append(out, p.Migration.ID())
	}

	return out
}

func TestNewSetRefusesWhatItCannotRead(t *testing.T) {
	t.Parallel()

	if _, err := NewSet("weird", nil); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "unknown rollout policy weird") {
		t.Fatalf("rollout = %v", err)
	}
	if _, err := NewSet("", map[string]string{"20260901120000_a.up.sql": "SELECT 1;"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("files = %v", err)
	}
}

func TestStopAtUnsetKeepsTheWholeSet(t *testing.T) {
	t.Parallel()
	g, _ := newGate(t)
	set := toSet(t, toFiles())

	out, err := g.StopAt(context.Background(), "app", set, 0)
	if err != nil || len(out.Plans) != 3 || len(out.Withheld) != 0 || len(out.Files) != 6 {
		t.Fatalf("set = %+v, err = %v", out, err)
	}
}

func TestStopAtRefusesAVersionTheSetDoesNotHold(t *testing.T) {
	t.Parallel()
	g, _ := newGate(t)

	_, err := g.StopAt(context.Background(), "app", toSet(t, toFiles()), 20260901119999)
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "20260901120000, 20260901120100") {
		t.Fatalf("err = %v", err)
	}

	only := map[string]string{"R__v.up.sql": "CREATE OR REPLACE VIEW v AS SELECT 1;", "R__v.down.sql": "DROP VIEW v;"}
	if _, err := g.StopAt(context.Background(), "app", toSet(t, only), 1); !strings.Contains(err.Error(), "repeatable migrations only") {
		t.Fatalf("err = %v", err)
	}
}

func TestStopAtAppliedError(t *testing.T) {
	t.Parallel()
	g, mock := newGate(t)
	mock.ExpectQuery("SELECT DISTINCT left").WithArgs("app").WillReturnError(errors.New("boom"))

	_, err := g.StopAt(context.Background(), "app", toSet(t, toFiles()), 20260901120000)
	if err == nil || errors.Is(err, ErrInvalid) || errors.Is(err, ErrRefused) {
		t.Fatalf("a store failure is not a refusal: %v", err)
	}
}

func TestStopAtRefusesATargetBehindHistory(t *testing.T) {
	t.Parallel()
	g, mock := newGate(t)
	expectAppliedVersions(mock, 20260901120000, 20260901120100)

	_, err := g.StopAt(context.Background(), "app", toSet(t, toFiles()), 20260901120000)
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "it never reverts") {
		t.Fatalf("err = %v", err)
	}
}

func TestStopAtRefusesATargetThatSelectsNothing(t *testing.T) {
	t.Parallel()
	files := toFiles()
	files["20260901120200_c.up.sql"], files["20260901120200_c.down.sql"] = "CREATE TABLE c (id int);", "DROP TABLE c;"
	g, mock := newGate(t)
	expectAppliedVersions(mock, 20260901120000, 20260901120100)

	_, err := g.StopAt(context.Background(), "app", toSet(t, files), 20260901120100)
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "the pending set starts at 20260901120200_c") {
		t.Fatalf("err = %v", err)
	}
}

func TestStopAtKeepsWhatIsAtOrBelowTheTarget(t *testing.T) {
	t.Parallel()
	g, mock := newGate(t)
	expectAppliedVersions(mock)

	set, err := g.StopAt(context.Background(), "app", toSet(t, toFiles()), 20260901120000)
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Plans) != 1 || set.Plans[0].Migration.Version != 20260901120000 {
		t.Fatalf("plans = %+v", set.Plans)
	}
	if got := ids(set.Withheld); len(got) != 2 || got[0] != "20260901120100_b" || got[1] != "R__v" {
		t.Fatalf("withheld = %v", got)
	}
	if len(set.Files) != 2 || set.Files["20260901120000_a.up.sql"] == "" || set.Files["20260901120000_a.down.sql"] == "" {
		t.Fatalf("files = %v; the run carries only what it applies", set.Files)
	}
}
