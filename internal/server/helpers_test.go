package server

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

// dropStoreTables breaks the control-plane schema to force internal errors.
func dropStoreTables(t *testing.T, storeDSN string) {
	t.Helper()
	execStore(t, storeDSN, `DROP TABLE cp_leases, cp_run_files, cp_runs, cp_drift_events, cp_snapshots, cp_targets, cp_notifications, cp_audit CASCADE`)
}

func execStore(t *testing.T, storeDSN, sql string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, storeDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, sql); err != nil {
		t.Fatal(err)
	}
}
