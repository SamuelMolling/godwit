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
			[]string{"id", "target", "state", "coalesce", "attempts", "rollout", "phase", "coalesce", "created_at", "finished_at"}).
			AddRow("r1", "app", StateQueued, "", 0, RolloutDirect, PhaseExpand, "", now(), nilTime()).RowError(0, errBoom))

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
