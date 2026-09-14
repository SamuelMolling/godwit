package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/SamuelMolling/godwit/internal/creds"
)

// published is what the GitHub App fences into a pull request comment: the connect error's message, and nothing under it.
func published(t *testing.T, err error) string {
	t.Helper()
	var c *connect.Error
	if !errors.As(err, &c) {
		t.Fatalf("want a connect error, got %v", err)
	}

	return c.Message()
}

func refusing(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)

	return srv
}

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
		if connect.CodeOf(out) != connect.CodeInternal || published(t, out) != connectionFailed {
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
}

func TestSafeRedactsAnUnclassifiedError(t *testing.T) {
	t.Parallel()

	plain := errors.New("plain failure")
	out := rpcErr(plain)
	if connect.CodeOf(out) != connect.CodeInternal || published(t, out) != callFailed {
		t.Fatalf("rpcErr = %v", out)
	}
	if detail(out) != "plain failure" || !errors.Is(out, plain) {
		t.Fatalf("the server log lost the detail: %q / %v", detail(out), out)
	}
}

func TestSafePublishesTheTargetsOwnPostgresError(t *testing.T) {
	t.Parallel()

	pg := &pgconn.PgError{Severity: "ERROR", Code: "42501", Message: "permission denied for schema orders"}
	out := rpcErr(fmt.Errorf("read the journal of app: %w", pg))
	if got := published(t, out); !strings.Contains(got, "permission denied for schema orders") {
		t.Fatalf("the reader needs the target's own answer: %q", got)
	}
	if strings.Contains(published(t, out), "read the journal of app") {
		t.Fatal("godwit's own wrapping is context for the log, not for the caller")
	}
	if !strings.Contains(detail(out), "read the journal of app") {
		t.Fatalf("the server log lost the wrapping: %q", detail(out))
	}
}

func TestSafeHidesTheUserARefusedLoginNames(t *testing.T) {
	t.Parallel()

	addr := refusingPostgres(t, `password authentication failed for user "orders_migrator"`)
	_, err := pgx.Connect(context.Background(), "postgres://orders_migrator:hunter2@"+addr+"/orders?sslmode=disable")
	if err == nil {
		t.Fatal("want the login to be refused")
	}
	var pg *pgconn.PgError
	if !errors.As(err, &pg) {
		t.Fatalf("want a PgError inside the connect error: %v", err)
	}
	out := rpcErr(err)
	if published(t, out) != connectionFailed {
		t.Fatalf("a dial failure carrying a PgError has to be redacted as a dial failure: %q", published(t, out))
	}
	if strings.Contains(out.Error(), "orders_migrator") {
		t.Fatalf("the DSN user reached the caller: %v", out)
	}
}

// refusingPostgres speaks just enough of the protocol to refuse a login, so the PgError arrives inside the ConnectError pgx wraps it in.
func refusingPostgres(t *testing.T, message string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		be := pgproto3.NewBackend(conn, conn)
		if _, err := be.ReceiveStartupMessage(); err != nil {
			return
		}
		be.Send(&pgproto3.ErrorResponse{Severity: "FATAL", Code: "28P01", Message: message})
		_ = be.Flush()
	}()

	return ln.Addr().String()
}

func TestSafeHidesAVaultSecretPath(t *testing.T) {
	t.Parallel()

	srv := refusing(t, http.StatusForbidden, `{"errors":["1 error occurred:\n\t* permission denied\n\n"]}`)
	v := creds.Vault{Address: srv.URL, Token: "s.root", Client: srv.Client()}
	_, err := v.DSN(context.Background(), map[string]string{
		creds.PathKey: "secret/data/production/orders/migrator", creds.TemplateKey: "postgres://{{username}}@db/orders",
	})
	if err == nil {
		t.Fatal("want the vault read to fail")
	}
	out := rpcErr(err)
	if published(t, out) != callFailed {
		t.Fatalf("the vault path reached the comment: %q", published(t, out))
	}
	for _, leak := range []string{"secret/data/production/orders/migrator", "permission denied", srv.URL} {
		if strings.Contains(out.Error(), leak) {
			t.Fatalf("%q reached the caller: %v", leak, out)
		}
	}
	if !strings.Contains(detail(out), "secret/data/production/orders/migrator") {
		t.Fatalf("the server log lost the path the operator has to look at: %q", detail(out))
	}
}

func TestSafeHidesAKMSResourceName(t *testing.T) {
	t.Parallel()

	const key = "projects/fireflies-prod/locations/us-east1/keyRings/godwit/cryptoKeys/store"
	srv := refusing(t, http.StatusForbidden,
		`{"error":{"code":403,"message":"Permission 'cloudkms.cryptoKeyVersions.useToEncrypt' denied on resource '`+key+`'"}}`)
	p := creds.GCPKMS{
		KeyName: key, Endpoint: srv.URL, Client: srv.Client(),
		Token: func(context.Context) (string, error) { return "ya29.token", nil },
	}
	_, err := p.Seal(context.Background(), []byte("aad"), "postgres://app:hunter2@db.internal/orders")
	if err == nil {
		t.Fatal("want the kms call to fail")
	}
	out := rpcErr(err)
	if published(t, out) != callFailed {
		t.Fatalf("the kms resource name reached the comment: %q", published(t, out))
	}
	for _, leak := range []string{key, "fireflies-prod", "keyRings", srv.URL} {
		if strings.Contains(out.Error(), leak) {
			t.Fatalf("%q reached the caller: %v", leak, out)
		}
	}
	if !strings.Contains(detail(out), key) {
		t.Fatalf("the server log lost the key the operator has to look at: %q", detail(out))
	}
}

func TestCredentialConfigurationIsPublished(t *testing.T) {
	t.Parallel()

	_, err := creds.Vault{}.DSN(context.Background(), map[string]string{creds.PathKey: "secret/data/app"})
	if err == nil {
		t.Fatal("want a vault with no address to refuse")
	}
	out := rpcErr(fmt.Errorf("target app: %w", err))
	if connect.CodeOf(out) != connect.CodeFailedPrecondition ||
		!strings.Contains(published(t, out), "this vault has no address") {
		t.Fatalf("an operator has to be told what to fix: %v", out)
	}
	if detail(out) != "" {
		t.Fatalf("a published error needs no second copy in the log: %q", detail(out))
	}
}
