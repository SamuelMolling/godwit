package controlplane

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"

	"github.com/SamuelMolling/godwit/internal/creds"
)

func TestCredentialStoreRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, pool := newStore(t)

	if err := s.RegisterCredentialStore(ctx, CredentialStore{
		Name: "production", Address: "https://vault.production.example", Role: "godwit", Mount: "kubernetes",
		Audience: "vault.production.example",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterTarget(ctx, "app", "vault", map[string]string{
		"path": "secret/data/app", creds.StoreConfigKey: "production",
	}); err != nil {
		t.Fatal(err)
	}

	provider, config, err := s.Target(ctx, "app")
	if err != nil || provider != "vault" || config[creds.StoreConfigKey] != "production" || config["path"] != "secret/data/app" {
		t.Fatalf("provider = %q, config = %v, err = %v", provider, config, err)
	}
	want := creds.VaultStore{
		Address: "https://vault.production.example", Role: "godwit", Mount: "kubernetes",
		Audience: "vault.production.example",
	}
	if v, err := s.VaultStore(ctx, "production"); err != nil || v != want {
		t.Fatalf("store = %+v, err = %v", v, err)
	}
	if _, err := s.VaultStore(ctx, "ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}

	stores, err := s.ListCredentialStores(ctx)
	if err != nil || len(stores) != 1 || stores[0].Targets != 1 {
		t.Fatalf("stores = %+v, err = %v", stores, err)
	}

	if err := s.RegisterTarget(ctx, "app", "vault", map[string]string{
		"path": "secret/data/app", creds.StoreConfigKey: "ghost",
	}); err == nil || !strings.Contains(err.Error(), "register target") {
		t.Fatalf("err = %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM cp_credential_stores WHERE name = 'production'`); err == nil {
		t.Fatal("a store a target still reads from must not be removable")
	}

	if err := s.RegisterTarget(ctx, "plain", "kubernetes", map[string]string{"path": "/secrets/app"}); err != nil {
		t.Fatal(err)
	}
	if _, config, err = s.Target(ctx, "plain"); err != nil || len(config) != 1 {
		t.Fatalf("config = %v, err = %v", config, err)
	}
	summaries, err := s.ListTargets(ctx, time.Now())
	if err != nil || len(summaries) != 2 || summaries[0].CredentialStore != "production" || summaries[1].CredentialStore != "" {
		t.Fatalf("targets = %+v, err = %v", summaries, err)
	}
}

func TestCredentialStoreStoreErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mock, s := newMockStore(t)

	mock.ExpectExec("INSERT INTO cp_credential_stores").WithArgs("s", "", "", "", "", "").WillReturnError(errBoom)
	if err := s.RegisterCredentialStore(ctx, CredentialStore{Name: "s"}); err == nil ||
		!strings.Contains(err.Error(), "register credential store") {
		t.Fatalf("err = %v", err)
	}

	mock.ExpectQuery("FROM cp_credential_stores").WillReturnError(errBoom)
	if _, err := s.ListCredentialStores(ctx); err == nil || !strings.Contains(err.Error(), "list credential stores") {
		t.Fatalf("err = %v", err)
	}
	mock.ExpectQuery("FROM cp_credential_stores").WillReturnRows(
		pgxmock.NewRows([]string{"name", "address", "k8s_role", "k8s_mount", "k8s_audience", "token_env", "count"}).
			AddRow("s", "https://vault", "godwit", "kubernetes", "", "", 0).RowError(0, errBoom))
	if _, err := s.ListCredentialStores(ctx); err == nil || !strings.Contains(err.Error(), "list credential stores") {
		t.Fatalf("err = %v", err)
	}

	mock.ExpectQuery("SELECT address, k8s_role").WithArgs("s").WillReturnError(errBoom)
	if _, err := s.VaultStore(ctx, "s"); err == nil || !strings.Contains(err.Error(), "load credential store") {
		t.Fatalf("err = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
