package controlplane

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/SamuelMolling/godwit/internal/creds"
)

func TestNewHolderNames(t *testing.T) {
	t.Parallel()
	boom := func() (string, error) { return "", errors.New("no hostname") }
	host := func() (string, error) { return " box-1 ", nil }
	for _, tc := range []struct {
		name, given, want string
		hostname          func() (string, error)
	}{
		{name: "given", given: "worker-3", want: "worker-3", hostname: boom},
		{name: "hostname", given: "", want: "box-1", hostname: host},
		{name: "blank falls back to the hostname", given: "  ", want: "box-1", hostname: host},
		{name: "no hostname", given: "", want: "unknown", hostname: boom},
		{name: "blank hostname", given: "", want: "unknown", hostname: func() (string, error) { return " ", nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := holderID(tc.given, tc.hostname)
			suffix, ok := strings.CutPrefix(got, tc.want+"/")
			if !ok || len(suffix) != 16 {
				t.Fatalf("holderID(%q) = %q, want %q and a 16-character suffix", tc.given, got, tc.want+"/")
			}
		})
	}
}

// Two replicas on one machine derive their identity from the same hostname, so the half godwit draws
// is the only thing keeping them apart.
func TestNewHolderIsDrawnPerCall(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for range 1000 {
		h := NewHolder("")
		if seen[h] {
			t.Fatalf("NewHolder repeated %q", h)
		}
		seen[h] = true
	}
}

func TestSchedulerDefaultsAHolder(t *testing.T) {
	t.Parallel()
	a := Config{}.withDefaults().Holder
	b := Config{}.withDefaults().Holder
	if a == "" || a == b {
		t.Fatalf("holders = %q, %q: a scheduler given none must draw its own", a, b)
	}
}

func expireLease(t *testing.T, pool *pgxpool.Pool, id string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE cp_leases SET expires_at = now() - interval '1 second' WHERE run_id = $1`, id); err != nil {
		t.Fatal(err)
	}
}

// Both replicas answer to one hostname, which is what the holder used to be: replica A could then extend,
// and on Finish delete, the lease replica B had legitimately taken off it.
func TestOneHostnameIsNotOneLease(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, pool := newStore(t)
	if err := s.RegisterTarget(ctx, "app", "plain", map[string]string{"dsn": "x"}); err != nil {
		t.Fatal(err)
	}
	const id = "77777777-0000-0000-0000-00000000000a"
	queueRun(t, s, id, goodFiles())

	a, b := NewHolder("box-1"), NewHolder("box-1")
	if _, ok, err := s.Claim(ctx, a, time.Minute); err != nil || !ok {
		t.Fatalf("claim by A = %v, %v", ok, err)
	}
	expireLease(t, pool, id)
	if _, ok, err := s.Claim(ctx, b, time.Minute); err != nil || !ok {
		t.Fatalf("claim by B = %v, %v", ok, err)
	}

	if err := s.Heartbeat(ctx, id, a, time.Minute); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("replica A extended a lease replica B holds: err = %v", err)
	}
	if err := s.Heartbeat(ctx, id, b, time.Minute); err != nil {
		t.Fatalf("replica B holds the lease and cannot beat: %v", err)
	}
}

// The same two replicas around a run in flight: A stalls past its TTL, B claims the run, and A comes
// back. A must discover the lease is gone and leave the run — and B's lease — alone.
func TestAStalledReplicaOnTheSameHostGivesTheRunUp(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, pool := newStore(t)
	a, _ := newScheduler(t, s, Config{Holder: NewHolder("box-1"), TTL: 200 * time.Millisecond})
	b := NewScheduler(s, map[string]creds.Provider{"plain": plainProvider{}}, PGEngine{}, Policies(),
		Config{Holder: NewHolder("box-1"), TTL: time.Minute}, testLog)

	const id = "77777777-0000-0000-0000-00000000000b"
	queueRun(t, s, id, sleepFiles("5"))
	runA, ok, err := s.Claim(ctx, a.cfg.Holder, a.cfg.TTL)
	if err != nil || !ok {
		t.Fatalf("claim by A = %v, %v", ok, err)
	}
	expireLease(t, pool, id)
	if _, ok, err := s.Claim(ctx, b.cfg.Holder, b.cfg.TTL); err != nil || !ok {
		t.Fatalf("claim by B = %v, %v", ok, err)
	}

	a.execute(ctx, runA)

	after, err := s.Run(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != StateRunning {
		t.Fatalf("run = %q/%q: a replica that lost the lease must not write the run", after.State, after.Error)
	}
	if err := s.Heartbeat(ctx, id, b.cfg.Holder, time.Minute); err != nil {
		t.Fatalf("replica B holds the lease and cannot beat: %v", err)
	}
}
