package api

import (
	"context"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/pashagolub/pgxmock/v4"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/internal/controlplane"
	"github.com/SamuelMolling/godwit/internal/creds"
)

func TestCredentialStoreStoreFailures(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mock.Close)
	s := NewServer(controlplane.NewStore(mock), nil, nil, creds.Keyring{})

	mock.ExpectExec("INSERT INTO cp_credential_stores").
		WithArgs("production", "https://vault.example", "godwit", "kubernetes", "").
		WillReturnError(errors.New("stores down"))
	if _, err := s.RegisterCredentialStore(ctx, connect.NewRequest(&godwitv1.RegisterCredentialStoreRequest{
		Name: "production", VaultAddr: "https://vault.example", VaultK8SRole: "godwit",
	})); connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("err = %v", err)
	}

	mock.ExpectQuery("FROM cp_credential_stores").WillReturnError(errors.New("stores down"))
	if _, err := s.ListCredentialStores(ctx, connect.NewRequest(&godwitv1.ListCredentialStoresRequest{})); connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("err = %v", err)
	}

	if _, err := s.RegisterTarget(ctx, connect.NewRequest(&godwitv1.RegisterTargetRequest{
		Name: "app", Provider: "vault", VaultPath: "secret/data/app",
	})); connect.CodeOf(err) != connect.CodeInvalidArgument ||
		!strings.Contains(err.Error(), "vault provider requires credential_store") {
		t.Fatalf("err = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
