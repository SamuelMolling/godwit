package githubapp

import (
	"strings"
	"testing"
)

func TestBindKeepsOnlyThisRepository(t *testing.T) {
	t.Parallel()

	stored := map[string]string{
		"orders":   "acme/orders, acme/orders:db/migrations",
		"payments": "acme/payments",
		"shared":   "acme/orders:svc/billing",
	}
	got := bind(stored, testRepo)
	want := bindings{
		{target: "orders"},
		{target: "orders", dir: "db/migrations"},
		{target: "shared", dir: "svc/billing"},
	}
	if len(got) != len(want) {
		t.Fatalf("Bind = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Bind = %+v, want %+v", got, want)
		}
	}
	if targets := strings.Join(got.targets(), ","); targets != "orders,shared" {
		t.Fatalf("Targets = %s", targets)
	}
}

func TestGrant(t *testing.T) {
	t.Parallel()

	b := bind(map[string]string{
		"orders":  "acme/orders",
		"billing": "acme/orders:svc/billing",
	}, testRepo)
	for _, tc := range []struct {
		name, dir, target string
		want              bool
	}{
		{"a repository-wide binding reaches any directory", "db/migrations", "orders", true},
		{"a directory-scoped binding reaches its own directory", "svc/billing", "billing", true},
		{"and no other", "db/migrations", "billing", false},
		{"a target this repository never named", "db/migrations", "payments", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := b.grant(testRepo, tc.dir, tc.target)
			if (err == nil) != tc.want {
				t.Fatalf("Grant(%q, %q) = %v, want granted = %t", tc.dir, tc.target, err, tc.want)
			}
			if err != nil && !strings.Contains(err.Error(), "ask a godwit operator") {
				t.Fatalf("grant = %q", err)
			}
		})
	}
}

func TestGrantNamesTheDirectoryItWasAskedAbout(t *testing.T) {
	t.Parallel()

	err := bindings{}.grant(testRepo, "db/migrations", "orders")
	if !strings.Contains(err.Error(), "--github-repo acme/orders:db/migrations") {
		t.Fatalf("grant = %q", err)
	}
	root := bindings{}.grant("acme/payments", "", "orders")
	if !strings.Contains(root.Error(), "--github-repo acme/payments)") {
		t.Fatalf("grant = %q", root)
	}
}
