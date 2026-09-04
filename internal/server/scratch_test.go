package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/SamuelMolling/godwit/internal/api"
	"github.com/SamuelMolling/godwit/internal/controlplane"
)

func newStorePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), newDatabase(t, "sc"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	return pool
}

func restrictedDSN(t *testing.T, storeDSN string) string {
	t.Helper()
	role := fmt.Sprintf("scratch%d", dbSeq.Add(1))
	execStore(t, storeDSN, "CREATE ROLE "+role+
		" LOGIN PASSWORD 'scratch' CREATEDB NOSUPERUSER NOCREATEROLE NOREPLICATION NOBYPASSRLS")

	return strings.Replace(testDSN, "godwit:godwit@", role+":scratch@", 1)
}

func captureLog() (*slog.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}

	return slog.New(slog.NewTextHandler(buf, nil)), buf
}

func TestScratchFallsBackToTheStoreLoudly(t *testing.T) {
	t.Parallel()
	pool := newStorePool(t)
	log, buf := captureLog()

	scratch, closeScratch, err := newScratch(context.Background(), Config{}, pool, log)
	if err != nil || scratch == nil {
		t.Fatalf("scratch = %v, err = %v", scratch, err)
	}
	closeScratch()
	for _, want := range []string{"is a superuser", "owns the store database", "set --scratch-dsn"} {
		if !strings.Contains(buf.String(), want) {
			t.Fatalf("missing %q in:\n%s", want, buf)
		}
	}
}

func TestScratchRefusesAPrivilegedDSN(t *testing.T) {
	t.Parallel()
	pool := newStorePool(t)
	log, _ := captureLog()

	_, _, err := newScratch(context.Background(), Config{ScratchDSN: testDSN}, pool, log)
	if !errors.Is(err, controlplane.ErrScratchPrivileged) || !strings.Contains(err.Error(), "is a superuser") {
		t.Fatalf("err = %v", err)
	}
}

func TestScratchAcceptsARestrictedDSN(t *testing.T) {
	t.Parallel()
	storeDSN := newDatabase(t, "sc")
	pool, err := pgxpool.New(context.Background(), storeDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	log, buf := captureLog()

	scratch, closeScratch, err := newScratch(context.Background(),
		Config{ScratchDSN: restrictedDSN(t, storeDSN)}, pool, log)
	if err != nil || scratch == nil {
		t.Fatalf("scratch = %v, err = %v", scratch, err)
	}
	closeScratch()
	if strings.Contains(buf.String(), "set --scratch-dsn") {
		t.Fatalf("isolated scratch must not warn about the fallback:\n%s", buf)
	}
}

func TestScratchDSNErrors(t *testing.T) {
	t.Parallel()
	pool := newStorePool(t)
	log, _ := captureLog()
	ctx := context.Background()

	if _, _, err := newScratch(ctx, Config{ScratchDSN: "://bad"}, pool, log); err == nil ||
		!strings.Contains(err.Error(), "scratch dsn") {
		t.Fatalf("bad dsn: %v", err)
	}
	if _, _, err := newScratch(ctx, Config{ScratchDSN: "postgres://x:x@127.0.0.1:1/x"}, pool, log); err == nil ||
		!strings.Contains(err.Error(), "inspect scratch role") {
		t.Fatalf("unreachable dsn: %v", err)
	}
}

// The scratch pool's only source of demand is the concurrency gate in front of the calls that use it.
func TestScratchConnsFollowsTheGate(t *testing.T) {
	t.Parallel()

	for heavy, want := range map[int]int{0: 2 * api.DefaultHeavyCalls, 1: 4, 4: 8, 16: 32} {
		if got := scratchConns(heavy); got != want {
			t.Fatalf("scratchConns(%d) = %d, want %d", heavy, got, want)
		}
	}
}
