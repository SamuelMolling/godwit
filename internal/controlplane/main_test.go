package controlplane

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

var testDSN string

func TestMain(m *testing.M) {
	ctx := context.Background()
	ctr, err := tcpostgres.Run(ctx, "postgres:17-alpine",
		tcpostgres.WithDatabase("godwit"),
		tcpostgres.WithUsername("godwit"),
		tcpostgres.WithPassword("godwit"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, "postgres container required for controlplane tests:", err)
		os.Exit(1)
	}
	testDSN, err = ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fmt.Fprintln(os.Stderr, "connection string:", err)
		os.Exit(1)
	}
	code := m.Run()
	_ = ctr.Terminate(ctx)
	os.Exit(code)
}

var dbSeq atomic.Int64

func newDatabase(t *testing.T, prefix string) string {
	t.Helper()
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, testDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admin.Close(ctx) }()
	name := fmt.Sprintf("%s%d", prefix, dbSeq.Add(1))
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}

	return strings.Replace(testDSN, "/godwit?", "/"+name+"?", 1)
}

// newStore returns a migrated store on a fresh database.
func newStore(t *testing.T) (*Store, *pgxpool.Pool) {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), newDatabase(t, "cp"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := Migrate(context.Background(), pool); err != nil {
		t.Fatal(err)
	}

	return NewStore(pool), pool
}
