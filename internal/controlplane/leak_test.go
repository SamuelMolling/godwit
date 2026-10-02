package controlplane_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/internal/controlplane"
	"github.com/SamuelMolling/godwit/internal/creds"
	"github.com/SamuelMolling/godwit/internal/redact"
	"github.com/SamuelMolling/godwit/internal/report"
)

func refusing(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	return srv
}

func comment(t *testing.T, errText string) string {
	t.Helper()
	write, err := report.RunWriter("markdown", "statements")
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	write(&b, report.RunFrom(&godwitv1.Run{
		Id: "8f2b1c40-0000-0000-0000-000000000001", Target: "orders",
		State: godwitv1.RunState_RUN_STATE_FAILED, Error: errText,
	}, nil, nil, "migrate", "https://godwit.example"))

	return b.String()
}

func TestARunReportCarriesNoVaultSecretPath(t *testing.T) {
	t.Parallel()

	srv := refusing(t, http.StatusForbidden, `{"errors":["1 error occurred:\n\t* permission denied\n\n"]}`)
	const path = "secret/data/production/orders/migrator"
	column, err := controlplane.ColumnForTargetFailure(map[string]creds.Provider{
		creds.ProviderVault: creds.Vault{Address: srv.URL, Token: "s.root", Client: srv.Client()},
	}, "orders", creds.ProviderVault, map[string]string{
		creds.PathKey: path, creds.TemplateKey: "postgres://{{username}}@db/orders",
	})
	if err == nil {
		t.Fatal("want the vault read to fail")
	}
	if column != redact.CredentialUnreadable {
		t.Fatalf("cp_runs.error = %q", column)
	}
	body := comment(t, column)
	for _, leak := range []string{path, "permission denied", srv.URL} {
		if strings.Contains(body, leak) {
			t.Fatalf("%q reached the pull request comment:\n%s", leak, body)
		}
	}
	if !strings.Contains(body, "ask an operator to check its credential store") {
		t.Fatalf("the author is told nothing they can act on:\n%s", body)
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("the server log lost the path the operator has to look at: %q", err)
	}
}

func TestARunReportCarriesNoKMSResourceName(t *testing.T) {
	t.Parallel()

	const key = "projects/fireflies-prod/locations/us-east1/keyRings/godwit/cryptoKeys/store"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ":encrypt") {
			var in struct {
				Plaintext string `json:"plaintext"`
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			_ = json.NewEncoder(w).Encode(map[string]string{"ciphertext": in.Plaintext})

			return
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":403,"message":"Permission 'cloudkms.cryptoKeyVersions.useToDecrypt' denied on resource '` + key + `'"}}`))
	}))
	t.Cleanup(srv.Close)

	kms := creds.GCPKMS{
		KeyName: key, Endpoint: srv.URL, Client: srv.Client(),
		Token: func(context.Context) (string, error) { return "ya29.token", nil },
	}
	sealed, err := creds.NewKeyring(kms).Seal(context.Background(), "postgres://app:hunter2@db.internal/orders")
	if err != nil {
		t.Fatal(err)
	}
	column, err := controlplane.ColumnForTargetFailure(map[string]creds.Provider{
		creds.ProviderStatic: creds.Static{Keys: creds.NewKeyring(kms)},
	}, "orders", creds.ProviderStatic, map[string]string{creds.DSNKey: sealed})
	if err == nil {
		t.Fatal("want the kms open to fail")
	}
	if column != redact.CredentialUnreadable {
		t.Fatalf("cp_runs.error = %q", column)
	}
	body := comment(t, column)
	for _, leak := range []string{key, "fireflies-prod", "keyRings", srv.URL} {
		if strings.Contains(body, leak) {
			t.Fatalf("%q reached the pull request comment:\n%s", leak, body)
		}
	}
	if !strings.Contains(err.Error(), key) {
		t.Fatalf("the server log lost the resource name the operator has to look at: %q", err)
	}
}

func TestARunReportKeepsTheTargetsOwnAnswer(t *testing.T) {
	t.Parallel()

	err := redact.Wrap(&pgconn.PgError{
		Severity: "ERROR", Code: "42501", Message: "permission denied for schema orders",
	}, "statement 2 of %s (up)", "20260901120000_orders")
	column := controlplane.ColumnForRunFailure(err)
	if column != "sql: statement 2 of 20260901120000_orders (up): ERROR: permission denied for schema orders (SQLSTATE 42501)" {
		t.Fatalf("cp_runs.error = %q", column)
	}
	if body := comment(t, column); !strings.Contains(body, "permission denied for schema orders") ||
		!strings.Contains(body, "It stopped at statement 2 of") {
		t.Fatalf("the author lost the answer their own SQL got:\n%s", body)
	}
}
