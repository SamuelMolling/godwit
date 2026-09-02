package server

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func probe(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	return resp.StatusCode, string(body)
}

func cutOffDatabase(t *testing.T, dsn string) {
	t.Helper()
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, testDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admin.Close(ctx) }()
	name := dsn[strings.LastIndex(dsn, "/")+1 : strings.Index(dsn, "?")]
	if _, err := admin.Exec(ctx, "ALTER DATABASE "+name+" ALLOW_CONNECTIONS false"); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1", name); err != nil {
		t.Fatal(err)
	}
}

func TestProbesEndToEnd(t *testing.T) {
	t.Parallel()
	storeDSN := newDatabase(t, "st")
	baseURL := startService(t, storeDSN, "r1", []string{"secret"})

	if code, body := probe(t, baseURL+"/healthz"); code != http.StatusOK || body != "ok\n" {
		t.Fatalf("healthz = %d %q", code, body)
	}
	if code, body := probe(t, baseURL+"/readyz"); code != http.StatusOK || body != "ok\n" {
		t.Fatalf("readyz = %d %q", code, body)
	}

	cutOffDatabase(t, storeDSN)

	if code, body := probe(t, baseURL+"/readyz"); code != http.StatusServiceUnavailable || !strings.HasPrefix(body, "store unavailable") {
		t.Fatalf("readyz with store down = %d %q", code, body)
	}
	if code, _ := probe(t, baseURL+"/healthz"); code != http.StatusOK {
		t.Fatalf("healthz with store down = %d", code)
	}
}
