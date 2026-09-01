package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
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
		fmt.Fprintln(os.Stderr, "postgres container required for cli tests:", err)
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

func newTestDSN(t *testing.T) string {
	t.Helper()
	ctx := context.Background()

	admin, err := pgx.Connect(ctx, testDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admin.Close(ctx) }()

	name := fmt.Sprintf("cli%d", dbSeq.Add(1))
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}

	return strings.Replace(testDSN, "/godwit?", "/"+name+"?", 1)
}

func execSQL(t *testing.T, dsn, sql string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, sql); err != nil {
		t.Fatal(err)
	}
}

func runCLI(args ...string) (int, string, string) {
	var out, errOut strings.Builder
	code := Main(args, &out, &errOut)

	return code, out.String(), errOut.String()
}
