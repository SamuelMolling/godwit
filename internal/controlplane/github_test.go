package controlplane

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

func TestSetGitHubRepositories(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, want string
		in         []string
	}{
		{name: "none", in: nil, want: ""},
		{name: "one", in: []string{"acme/orders"}, want: "acme/orders"},
		{name: "a directory", in: []string{"acme/orders:db/migrations"}, want: "acme/orders:db/migrations"},
		{name: "blanks are dropped", in: []string{" acme/orders ", "", " "}, want: "acme/orders"},
		{name: "duplicates are dropped", in: []string{"acme/orders", "acme/orders"}, want: "acme/orders"},
		{name: "several", in: []string{"acme/orders", "acme/pay:db"}, want: "acme/orders,acme/pay:db"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			config := map[string]string{}
			err := SetGitHubRepositories(config, tc.in)
			if err != nil || config[configGitHubRepositories] != tc.want {
				t.Fatalf("SetGitHubRepositories(%v) = %q, %v; want %q", tc.in, config[configGitHubRepositories], err, tc.want)
			}
		})
	}
}

func TestSetGitHubRepositoriesRefusals(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ name, entry, want string }{
		{"no owner", "orders", "is not owner/repo"},
		{"an empty owner", "/orders", "is not owner/repo"},
		{"an empty name", "acme/", "is not owner/repo"},
		{"a third segment", "acme/orders/db", "is not owner/repo"},
		{"a space", "acme/or ders", "is not owner/repo"},
		{"a colon with nothing after it", "acme/orders:", "names no directory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := SetGitHubRepositories(map[string]string{}, []string{tc.entry})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("SetGitHubRepositories(%q) = %v, want %q", tc.entry, err, tc.want)
			}
		})
	}
}

func TestGitHubBindings(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newStore(t)

	if err := s.RegisterTarget(ctx, "unbound", "static", map[string]string{"dsn": "x"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterTarget(ctx, "orders", "static", map[string]string{
		"dsn": "x", configGitHubRepositories: "acme/orders,acme/pay:db",
	}); err != nil {
		t.Fatal(err)
	}
	bindings, err := s.GitHubBindings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 1 || bindings["orders"] != "acme/orders,acme/pay:db" {
		t.Fatalf("bindings = %v", bindings)
	}
	targets, err := s.ListTargets(ctx, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tg := range targets {
		want := 0
		if tg.Name == "orders" {
			want = 2
		}
		if len(tg.GitHubRepositories) != want {
			t.Fatalf("target %s carries %v", tg.Name, tg.GitHubRepositories)
		}
	}
}

func TestRecordDeliveryIsIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newStore(t)

	first, err := s.RecordDelivery(ctx, "d1", "issue_comment", "acme/orders")
	if err != nil || !first {
		t.Fatalf("RecordDelivery = %t, %v", first, err)
	}
	again, err := s.RecordDelivery(ctx, "d1", "issue_comment", "acme/orders")
	if err != nil || again {
		t.Fatalf("redelivery = %t, %v", again, err)
	}
	n, err := s.SweepDeliveries(ctx, time.Now().Add(-time.Hour))
	if err != nil || n != 0 {
		t.Fatalf("early sweep = %d, %v", n, err)
	}
	if n, err = s.SweepDeliveries(ctx, time.Now().Add(time.Hour)); err != nil || n != 1 {
		t.Fatalf("sweep = %d, %v", n, err)
	}
	if first, err = s.RecordDelivery(ctx, "d1", "issue_comment", "acme/orders"); err != nil || !first {
		t.Fatalf("after the sweep = %t, %v", first, err)
	}
}

func TestGitHubStoreFailures(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, pool := newStore(t)

	if _, err := pool.Exec(ctx, "DROP TABLE cp_webhook_deliveries"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "DROP TABLE cp_targets CASCADE"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordDelivery(ctx, "d1", "e", "r"); err == nil {
		t.Fatal("no error")
	}
	if _, err := s.SweepDeliveries(ctx, time.Now()); err == nil {
		t.Fatal("no error")
	}
	if _, err := s.GitHubBindings(ctx); err == nil {
		t.Fatal("no error")
	}
}

func TestGitHubBindingsRowFailure(t *testing.T) {
	t.Parallel()
	mock, s := newMockStore(t)

	mock.ExpectQuery("SELECT name, config").WithArgs(configGitHubRepositories).
		WillReturnRows(pgxmock.NewRows([]string{"name", "config"}).
			AddRow("orders", "acme/orders").RowError(0, errBoom))

	if _, err := s.GitHubBindings(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "read github bindings") {
		t.Fatalf("err = %v", err)
	}
}
