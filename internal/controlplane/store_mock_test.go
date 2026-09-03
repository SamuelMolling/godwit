package controlplane

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pashagolub/pgxmock/v4"
)

var errBoom = errors.New("boom")

func newMockStore(t *testing.T) (pgxmock.PgxPoolIface, *Store) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mock.Close)

	return mock, NewStore(mock)
}

func TestListRunsRowError(t *testing.T) {
	t.Parallel()
	mock, s := newMockStore(t)

	mock.ExpectQuery("SELECT id, target, state").WithArgs("").
		WillReturnRows(pgxmock.NewRows(
			[]string{"id", "target", "state", "coalesce", "attempts", "rollout", "phase", "coalesce", "kind", "coalesce", "coalesce", "created_at", "finished_at", "created_by", "source", "coalesce", "retries", "not_before"}).
			AddRow("r1", "app", StateQueued, "", 0, RolloutDirect, PhaseExpand, "", KindMigrate, "", "", now(), nilTime(), "anonymous", "", "", 0, nilTime()).RowError(0, errBoom))

	if _, err := s.ListRuns(context.Background(), ""); err == nil ||
		!strings.Contains(err.Error(), "read runs") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunFilesRowError(t *testing.T) {
	t.Parallel()
	mock, s := newMockStore(t)

	mock.ExpectQuery("SELECT name, body FROM cp_run_files").WithArgs("r1").
		WillReturnRows(pgxmock.NewRows([]string{"name", "body"}).
			AddRow("f", "b").RowError(0, errBoom))

	if _, err := s.RunFiles(context.Background(), "r1"); err == nil ||
		!strings.Contains(err.Error(), "read run files") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunStatsRowError(t *testing.T) {
	t.Parallel()
	mock, s := newMockStore(t)

	mock.ExpectQuery("SELECT target, state, count").
		WillReturnRows(pgxmock.NewRows([]string{"target", "state", "count", "extract"}).
			AddRow("app", StateQueued, 1, 0.5).RowError(0, errBoom))

	if _, err := s.RunStats(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "read run stats") {
		t.Fatalf("err = %v", err)
	}
}

func TestAppliedVersionsRowError(t *testing.T) {
	t.Parallel()
	mock, s := newMockStore(t)

	mock.ExpectQuery("SELECT DISTINCT left").WithArgs("app").
		WillReturnRows(pgxmock.NewRows([]string{"version"}).AddRow(int64(1)).RowError(0, errBoom))

	if _, err := s.Applied(context.Background(), "app"); err == nil ||
		!strings.Contains(err.Error(), "read applied versions") {
		t.Fatalf("err = %v", err)
	}
}

func TestAppliedRepeatablesRowError(t *testing.T) {
	t.Parallel()
	mock, s := newMockStore(t)

	mock.ExpectQuery("SELECT DISTINCT left").WithArgs("app").
		WillReturnRows(pgxmock.NewRows([]string{"version"}))
	mock.ExpectQuery("SELECT DISTINCT ON").WithArgs("app").
		WillReturnRows(pgxmock.NewRows([]string{"name", "body"}).AddRow("R__v.up.sql", "SELECT 1;").RowError(0, errBoom))

	if _, err := s.Applied(context.Background(), "app"); err == nil ||
		!strings.Contains(err.Error(), "read applied repeatables") {
		t.Fatalf("err = %v", err)
	}
}

func TestTransactCommitError(t *testing.T) {
	t.Parallel()
	mock, s := newMockStore(t)

	mock.ExpectBegin()
	mock.ExpectCommit().WillReturnError(errBoom)

	if err := s.Transact(context.Background(), func(*Store) error { return nil }); err == nil ||
		!strings.Contains(err.Error(), "commit transaction") {
		t.Fatalf("err = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
