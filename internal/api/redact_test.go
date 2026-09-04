package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
)

// pgx redacts the password of a DSN it cannot parse or dial and nothing else, so the host, the user, the
// database name and — for a credential provider that hands back a file instead of a DSN — the file body
// would otherwise travel to a read-scope caller inside an internal error.
func TestSafeHidesConnectionDetail(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	_, parseErr := pgx.ParseConfig("postgres://app:hunter2@db.internal:5432/orders?sslmode=bogus")
	if parseErr == nil {
		t.Fatal("want a parse error")
	}
	_, connErr := pgx.Connect(ctx, "postgres://app:hunter2@127.0.0.1:1/orders")
	if connErr == nil {
		t.Fatal("want a connect error")
	}
	for _, err := range []error{parseErr, connErr} {
		wrapped := fmt.Errorf("connect target: %w", err)
		out := rpcErr(wrapped)
		if connect.CodeOf(out) != connect.CodeInternal || !strings.Contains(out.Error(), connectionFailed) {
			t.Fatalf("rpcErr = %v", out)
		}
		for _, leak := range []string{"app", "db.internal", "orders", "127.0.0.1"} {
			if strings.Contains(out.Error(), leak) {
				t.Fatalf("%q reached the caller: %v", leak, out)
			}
		}
		if !strings.Contains(detail(out), err.Error()) {
			t.Fatalf("the server log lost the detail: %q", detail(out))
		}
		if !errors.Is(out, err) {
			t.Fatalf("the cause must stay in the chain for the handlers that classify it: %v", out)
		}
	}

	plain := errors.New("plain failure")
	if out := rpcErr(plain); out.Error() != "internal: plain failure" || detail(out) != "" {
		t.Fatalf("an unrelated error must pass through: %v / %q", out, detail(out))
	}
}
