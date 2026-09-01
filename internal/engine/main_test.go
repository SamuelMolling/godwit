package engine

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
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
		fmt.Fprintln(os.Stderr, "postgres container required for engine tests:", err)
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

// newTestDB creates an isolated database and returns a connector to it.
func newTestDB(t *testing.T) func() *pgx.Conn {
	t.Helper()
	ctx := context.Background()

	admin, err := pgx.Connect(ctx, testDSN)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("t%d", dbSeq.Add(1))
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	if err := admin.Close(ctx); err != nil {
		t.Fatal(err)
	}

	cfg, err := pgx.ParseConfig(testDSN)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Database = name

	return func() *pgx.Conn {
		conn, err := pgx.ConnectConfig(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close(context.Background()) })

		return conn
	}
}
