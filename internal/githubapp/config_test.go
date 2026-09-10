package githubapp

import (
	"strings"
	"testing"
	"time"
)

func TestNewRefusesAConfigurationThatCouldNotBeSafe(t *testing.T) {
	t.Parallel()

	base := func() Config {
		return Config{
			Secret: testSecret, Store: newStore(nil), API: &fakeAPI{}, Runner: &fakeRunner{},
			Log: testLog, Associations: []string{"OWNER", "MEMBER", "COLLABORATOR"},
		}
	}
	for _, tc := range []struct {
		name string
		with func(cfg *Config)
		want string
	}{
		{"no webhook secret", func(cfg *Config) { cfg.Secret = "" }, "secret is required"},
		{"no store", func(cfg *Config) { cfg.Store = nil }, "needs a store"},
		{"no api client", func(cfg *Config) { cfg.API = nil }, "needs a store"},
		{"no logger", func(cfg *Config) { cfg.Log = nil }, "needs a store"},
		{"no runner", func(cfg *Config) { cfg.Runner = nil }, "needs a store"},
		{
			"an association that is not access", func(cfg *Config) { cfg.Associations = []string{"OWNER", "CONTRIBUTOR"} },
			"CONTRIBUTOR is not access",
		},
		{"an association that is not one", func(cfg *Config) { cfg.Associations = []string{"WRITER"} }, "unknown author association"},
		{"an empty association list", func(cfg *Config) { cfg.Associations = []string{" ", ""} }, "no comment could ever command"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := base()
			tc.with(&cfg)
			_, err := New(cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("New = %v, want it to carry %q", err, tc.want)
			}
		})
	}
}

func TestNewFillsInTheDefaults(t *testing.T) {
	t.Parallel()

	r, err := New(Config{Secret: testSecret, Store: newStore(nil), API: &fakeAPI{}, Runner: &fakeRunner{}, Log: testLog})
	if err != nil {
		t.Fatal(err)
	}
	if r.cfg.MaxBodyBytes != defaultMaxBodyBytes || r.cfg.MaxAge != defaultMaxAge {
		t.Fatalf("limits = %d, %s", r.cfg.MaxBodyBytes, r.cfg.MaxAge)
	}
	if r.cfg.Now().IsZero() {
		t.Fatal("Now returned the zero time")
	}
	r.cfg.Record("push", "ignored")
}

func TestShortAndOrNone(t *testing.T) {
	t.Parallel()

	if got := short("abc"); got != "abc" {
		t.Fatalf("short = %q", got)
	}
	if got := orNone("open"); got != "open" {
		t.Fatalf("orNone = %q", got)
	}
	if got := orNone(""); got != "none" {
		t.Fatalf("orNone = %q", got)
	}
}

func TestLoginAndNameShapes(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		ok   bool
		got  bool
	}{
		{"empty login", false, validLogin("")},
		{"empty name", false, validName("")},
		{"a dotted repository name", true, validName("orders.git")},
		{"a repository name with a slash", false, validName("orders/db")},
		{"a repository with no owner", false, validRepository("/orders")},
	} {
		if tc.got != tc.ok {
			t.Fatalf("%s = %t, want %t", tc.name, tc.got, tc.ok)
		}
	}
}

func TestFreshIgnoresAnEventWithNoTimestampOfItsOwn(t *testing.T) {
	t.Parallel()

	f := newFixture(t, bound, writer(t))
	if out := f.receiver.fresh(&request{name: "plan"}); out != nil {
		t.Fatalf("fresh = %+v, want nil for an event carrying no time", out)
	}
	if out := f.receiver.fresh(&request{name: "apply", at: now.Add(-time.Minute)}); out != nil {
		t.Fatalf("fresh = %+v, want nil inside the window", out)
	}
}
