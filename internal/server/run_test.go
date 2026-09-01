package server

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/SamuelMolling/godwit/internal/controlplane"
)

func mustPool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	return pool
}

func countRows(t *testing.T, dsn, table string) int {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	var n int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		return 0
	}

	return n
}

func TestServeClosedListener(t *testing.T) {
	t.Parallel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_ = ln.Close()
	if err := serve(&http.Server{ReadHeaderTimeout: time.Second}, ln); err == nil {
		t.Fatal("closed listener must surface an error")
	}
}

func TestRunWithoutOnReadyShutsDownCleanly(t *testing.T) {
	t.Parallel()

	// Without OnReady, a fully migrated store is the only readiness signal.
	ref := newDatabase(t, "ref")
	if err := controlplane.Migrate(context.Background(), mustPool(t, ref)); err != nil {
		t.Fatal(err)
	}
	want := countRows(t, ref, "godwit.migrations")

	storeDSN := newDatabase(t, "st")
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- Run(ctx, Config{
			Listen:         "127.0.0.1:0",
			StoreDSN:       storeDSN,
			WebhookURL:     "http://127.0.0.1:1/hook",
			SkipValidation: true,
			Log:            testLog,
		})
	}()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) && countRows(t, storeDSN, "godwit.migrations") < want {
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Run() = %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run() did not shut down")
	}
}
