package api

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"
	"github.com/pashagolub/pgxmock/v4"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/internal/controlplane"
	"github.com/SamuelMolling/godwit/internal/creds"
)

func TestRegisterTargetStoreFailures(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mock.Close)
	s := NewServer(controlplane.NewStore(mock), nil, nil, creds.Keyring{})
	req := connect.NewRequest(&godwitv1.RegisterTargetRequest{
		Name: "app", Provider: "kubernetes", SecretPath: "/run/secrets/app",
	})

	mock.ExpectBegin()
	mock.ExpectQuery("FROM cp_targets").WithArgs("app").WillReturnError(errors.New("store down"))
	mock.ExpectRollback()
	if _, err := s.RegisterTarget(ctx, req); connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("unreadable target = %v", err)
	}

	mock.ExpectBegin()
	mock.ExpectQuery("FROM cp_targets").WithArgs("app").
		WillReturnRows(pgxmock.NewRows([]string{"provider", "credential_store", "config"}))
	mock.ExpectExec("INSERT INTO cp_targets").
		WithArgs("app", "kubernetes", pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(errors.New("store down"))
	mock.ExpectRollback()
	if _, err := s.RegisterTarget(ctx, req); connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("unwritable target = %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
