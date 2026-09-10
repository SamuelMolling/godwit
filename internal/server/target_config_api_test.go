package server

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
)

func TestGetTargetReadsBackTheRegistration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	baseURL := startService(t, newDatabase(t, "st"), "r1",
		[]string{"viewer:read:s-read", "ops:operator:s-ops", "root:admin:s-admin"})
	admin, ops, viewer := newClient(baseURL, "s-admin"), newClient(baseURL, "s-ops"), newClient(baseURL, "s-read")

	if _, err := admin.RegisterTarget(ctx, connect.NewRequest(&godwitv1.RegisterTargetRequest{
		Name: "app", Provider: "static", Dsn: newDatabase(t, "tg"),
		SearchPath: proto.String("app,public"), LockTimeout: proto.String("3s"),
		StatementTimeout: proto.String("1m"), KeepOld: proto.Bool(false),
		GithubRepositories: &godwitv1.TargetRepositories{Values: []string{"acme/orders"}},
	})); err != nil {
		t.Fatal(err)
	}

	if _, err := viewer.GetTarget(ctx, connect.NewRequest(&godwitv1.GetTargetRequest{Name: "app"})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("read must not reach a target's registration: %v", err)
	}
	got, err := ops.GetTarget(ctx, connect.NewRequest(&godwitv1.GetTargetRequest{Name: "app"}))
	if err != nil {
		t.Fatalf("operator must read a registration: %v", err)
	}
	want := &godwitv1.GetTargetResponse{
		Name: "app", Provider: "static", DsnRegistered: true, SearchPath: "app,public",
		LockTimeout: "3s", StatementTimeout: "1m", KeepOld: false, IgnoreAdoptedTables: true,
		GithubRepositories: []string{"acme/orders"},
	}
	if !proto.Equal(got.Msg, want) {
		t.Fatalf("registration = %+v, want %+v", got.Msg, want)
	}

	if _, err := admin.RegisterTarget(ctx, connect.NewRequest(&godwitv1.RegisterTargetRequest{
		Name: "app", RequirePlan: proto.Bool(true),
	})); err != nil {
		t.Fatal(err)
	}
	got, err = ops.GetTarget(ctx, connect.NewRequest(&godwitv1.GetTargetRequest{Name: "app"}))
	if err != nil {
		t.Fatal(err)
	}
	want.RequirePlan = true
	if !proto.Equal(got.Msg, want) {
		t.Fatalf("one setting changed must keep the rest: %+v, want %+v", got.Msg, want)
	}

	if _, err := admin.RegisterTarget(ctx, connect.NewRequest(&godwitv1.RegisterTargetRequest{
		Name: "app", SearchPath: proto.String(""), StatementTimeout: proto.String(""),
		GithubRepositories: &godwitv1.TargetRepositories{},
	})); err != nil {
		t.Fatal(err)
	}
	got, err = ops.GetTarget(ctx, connect.NewRequest(&godwitv1.GetTargetRequest{Name: "app"}))
	if err != nil {
		t.Fatal(err)
	}
	want.SearchPath, want.StatementTimeout, want.GithubRepositories = "", "", nil
	if !proto.Equal(got.Msg, want) {
		t.Fatalf("a setting sent empty must be cleared: %+v, want %+v", got.Msg, want)
	}

	if _, err := ops.GetTarget(ctx, connect.NewRequest(&godwitv1.GetTargetRequest{Name: "ghost"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("unregistered target = %v", err)
	}
	if _, err := ops.GetTarget(ctx, connect.NewRequest(&godwitv1.GetTargetRequest{})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("nameless target = %v", err)
	}
}

func TestRegisterTargetMergeRefusals(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client := newClient(startService(t, newDatabase(t, "st"), "r1", nil), "")
	if _, err := client.RegisterTarget(ctx, connect.NewRequest(&godwitv1.RegisterTargetRequest{
		Name: "app", Provider: "kubernetes", SecretPath: "/run/secrets/app",
	})); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		req  *godwitv1.RegisterTargetRequest
		want string
	}{
		{"no provider on a fresh target", &godwitv1.RegisterTargetRequest{Name: "fresh"}, "a new one needs a provider"},
		{"unknown provider", &godwitv1.RegisterTargetRequest{Name: "fresh", Provider: "hsm"}, "unknown provider hsm"},
		{"nameless", &godwitv1.RegisterTargetRequest{}, "name is required"},
		{
			"a field the provider does not read",
			&godwitv1.RegisterTargetRequest{Name: "app", VaultPath: "secret/data/app"},
			"vault_path is a setting of the vault provider, and this target's provider is kubernetes",
		},
		{
			"a provider switch that brings nothing",
			&godwitv1.RegisterTargetRequest{Name: "app", Provider: "vault"},
			"vault provider requires vault_path",
		},
		{
			"a template carrying its own password",
			&godwitv1.RegisterTargetRequest{
				Name: "app", Provider: "vault", VaultPath: "database/creds/app",
				VaultTemplate: proto.String("postgres://app:hunter2@db/app"),
			},
			"takes the password from the secret",
		},
		{
			"the same in keyword form",
			&godwitv1.RegisterTargetRequest{
				Name: "app", Provider: "vault", VaultPath: "database/creds/app",
				VaultTemplate: proto.String("host=db user={{username}} password=hunter2"),
			},
			"takes the password from the secret",
		},
		{
			"a timeout that does not parse",
			&godwitv1.RegisterTargetRequest{Name: "app", LockTimeout: proto.String("soon")},
			"lock_timeout",
		},
	}
	for _, tc := range cases {
		_, err := client.RegisterTarget(ctx, connect.NewRequest(tc.req))
		if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: err = %v", tc.name, err)
		}
	}

	kept, err := client.GetTarget(ctx, connect.NewRequest(&godwitv1.GetTargetRequest{Name: "app"}))
	if err != nil || kept.Msg.Provider != "kubernetes" || kept.Msg.SecretPath != "/run/secrets/app" {
		t.Fatalf("a refused registration must change nothing: %+v, err = %v", kept.Msg, err)
	}
}

func TestRegisterTargetProviderSwitchDropsTheOldCredential(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client := newClient(startService(t, newDatabase(t, "st"), "r1", nil), "")
	if _, err := client.RegisterTarget(ctx, connect.NewRequest(&godwitv1.RegisterTargetRequest{
		Name: "app", Provider: "static", Dsn: newDatabase(t, "tg"), LockTimeout: proto.String("4s"),
	})); err != nil {
		t.Fatal(err)
	}
	if _, err := client.RegisterTarget(ctx, connect.NewRequest(&godwitv1.RegisterTargetRequest{
		Name: "app", Provider: "kubernetes", SecretPath: "/run/secrets/app",
	})); err != nil {
		t.Fatal(err)
	}
	got, err := client.GetTarget(ctx, connect.NewRequest(&godwitv1.GetTargetRequest{Name: "app"}))
	if err != nil {
		t.Fatal(err)
	}
	want := &godwitv1.GetTargetResponse{
		Name: "app", Provider: "kubernetes", SecretPath: "/run/secrets/app",
		LockTimeout: "4s", KeepOld: true, IgnoreAdoptedTables: true,
	}
	if !proto.Equal(got.Msg, want) {
		t.Fatalf("switched target = %+v, want %+v", got.Msg, want)
	}
}

func TestGetTargetStatusOfATargetThatCannotBeRead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client := newClient(startService(t, newDatabase(t, "st"), "r1", nil), "")
	if _, err := client.RegisterTarget(ctx, connect.NewRequest(&godwitv1.RegisterTargetRequest{
		Name: "app", Provider: "kubernetes", SecretPath: "/run/secrets/gone", LockTimeout: proto.String("6s"),
	})); err != nil {
		t.Fatal(err)
	}
	st, err := client.GetTargetStatus(ctx, connect.NewRequest(&godwitv1.GetTargetStatusRequest{Target: "app"}))
	if err != nil {
		t.Fatalf("a target whose credential does not resolve must still be described: %v", err)
	}
	if st.Msg.Provider != "kubernetes" || st.Msg.LockTimeout != "6s" || st.Msg.Applied != nil {
		t.Fatalf("status = %+v", st.Msg)
	}
	if !strings.Contains(st.Msg.Unreachable, "read secret") {
		t.Fatalf("unreachable = %q", st.Msg.Unreachable)
	}
}
