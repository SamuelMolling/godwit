package controlplane

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

const upBody = "CREATE TABLE t (id int);"

func goodFiles() map[string]string {
	return map[string]string{
		"20260901120000_t.up.sql":   upBody,
		"20260901120000_t.down.sql": "DROP TABLE t;",
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	t.Parallel()

	_, pool := newStore(t)
	if err := Migrate(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
}

func TestTargetLifecycle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newStore(t)

	if _, _, err := s.Target(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	if err := s.RegisterTarget(ctx, "app", "static", map[string]string{"dsn": "enc1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterTarget(ctx, "app", "kubernetes", map[string]string{"path": "/p"}); err != nil {
		t.Fatal(err)
	}
	provider, config, err := s.Target(ctx, "app")
	if err != nil || provider != "kubernetes" || config["path"] != "/p" {
		t.Fatalf("provider = %q, config = %v, err = %v", provider, config, err)
	}
}

func TestRunLifecycle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newStore(t)

	if err := s.RegisterTarget(ctx, "app", "static", map[string]string{}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRun(ctx, "11111111-1111-1111-1111-111111111111", "app", goodFiles()); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Run(ctx, "22222222-2222-2222-2222-222222222222"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	r, err := s.Run(ctx, "11111111-1111-1111-1111-111111111111")
	if err != nil || r.State != StateQueued || r.Target != "app" {
		t.Fatalf("run = %+v, err = %v", r, err)
	}

	files, err := s.RunFiles(ctx, r.ID)
	if err != nil || len(files) != 2 || files["20260901120000_t.up.sql"] != upBody {
		t.Fatalf("files = %v, err = %v", files, err)
	}

	runs, err := s.ListRuns(ctx, "")
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs = %v, err = %v", runs, err)
	}
	runs, err = s.ListRuns(ctx, "other")
	if err != nil || len(runs) != 0 {
		t.Fatalf("filtered runs = %v, err = %v", runs, err)
	}

	// Claim leases it; a second claim finds nothing (lease is live).
	claimed, ok, err := s.Claim(ctx, "h1", time.Minute)
	if err != nil || !ok || claimed.ID != r.ID || claimed.Attempts != 1 || claimed.State != StateRunning {
		t.Fatalf("claimed = %+v, ok = %v, err = %v", claimed, ok, err)
	}
	if _, ok, err := s.Claim(ctx, "h2", time.Minute); err != nil || ok {
		t.Fatalf("second claim: ok = %v, err = %v", ok, err)
	}

	if err := s.Heartbeat(ctx, r.ID, "h1", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := s.Heartbeat(ctx, r.ID, "h2", time.Minute); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("wrong holder: err = %v", err)
	}

	if err := s.Finish(ctx, r.ID, StateFailed, "boom"); err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(ctx, "22222222-2222-2222-2222-222222222222", StateFailed, "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	if err := s.Heartbeat(ctx, r.ID, "h1", time.Minute); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("lease should be gone: err = %v", err)
	}

	if err := s.Resume(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	r, _ = s.Run(ctx, r.ID)
	if r.State != StateQueued || r.Attempts != 0 || r.Error != "" {
		t.Fatalf("resumed run = %+v", r)
	}
	if err := s.Resume(ctx, r.ID); !errors.Is(err, ErrNotResumable) {
		t.Fatalf("resume queued: err = %v", err)
	}
}

func TestClaimRecoversExpiredLease(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newStore(t)

	if err := s.RegisterTarget(ctx, "app", "static", map[string]string{}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRun(ctx, "33333333-3333-3333-3333-333333333333", "app", goodFiles()); err != nil {
		t.Fatal(err)
	}

	// h1 claims with a lease that expires immediately.
	if _, ok, err := s.Claim(ctx, "h1", -time.Second); err != nil || !ok {
		t.Fatalf("ok = %v, err = %v", ok, err)
	}
	// h2 recovers the same run: state stays running, attempts increments.
	r, ok, err := s.Claim(ctx, "h2", time.Minute)
	if err != nil || !ok || r.Attempts != 2 {
		t.Fatalf("recovered = %+v, ok = %v, err = %v", r, ok, err)
	}
}

func TestClaimSerializesPerTarget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newStore(t)

	for _, target := range []string{"a", "b"} {
		if err := s.RegisterTarget(ctx, target, "static", map[string]string{}); err != nil {
			t.Fatal(err)
		}
	}
	ids := map[string]string{
		"44444444-4444-4444-4444-444444444444": "a",
		"55555555-5555-5555-5555-555555555555": "a",
		"66666666-6666-6666-6666-666666666666": "b",
	}
	for id, target := range ids {
		if err := s.CreateRun(ctx, id, target, goodFiles()); err != nil {
			t.Fatal(err)
		}
	}

	// Two claims must land on different targets; the third finds nothing.
	first, ok, err := s.Claim(ctx, "h", time.Minute)
	if err != nil || !ok {
		t.Fatal(err)
	}
	second, ok, err := s.Claim(ctx, "h", time.Minute)
	if err != nil || !ok {
		t.Fatal(err)
	}
	if ids[first.ID] == ids[second.ID] {
		t.Fatalf("both claims on target %q", ids[first.ID])
	}
	if _, ok, _ := s.Claim(ctx, "h", time.Minute); ok {
		t.Fatal("third claim should find nothing")
	}
}

func TestCreateRunUnknownTarget(t *testing.T) {
	t.Parallel()

	s, _ := newStore(t)
	err := s.CreateRun(context.Background(), "77777777-7777-7777-7777-777777777777", "ghost", goodFiles())
	if err == nil || !strings.Contains(err.Error(), "create run") {
		t.Fatalf("err = %v", err)
	}
}

func TestStoreQueryErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newStore(t)

	// Closed pool exercises every query error branch.
	s.pool.(interface{ Close() }).Close()

	if err := s.RegisterTarget(ctx, "x", "static", nil); err == nil {
		t.Fatal("want error")
	}
	if _, _, err := s.Target(ctx, "x"); err == nil {
		t.Fatal("want error")
	}
	if err := s.CreateRun(ctx, "id", "x", nil); err == nil {
		t.Fatal("want error")
	}
	if _, err := s.Run(ctx, "id"); err == nil {
		t.Fatal("want error")
	}
	if _, err := s.ListRuns(ctx, ""); err == nil {
		t.Fatal("want error")
	}
	if _, err := s.RunFiles(ctx, "id"); err == nil {
		t.Fatal("want error")
	}
	if _, _, err := s.Claim(ctx, "h", time.Minute); err == nil {
		t.Fatal("want error")
	}
	if err := s.Heartbeat(ctx, "id", "h", time.Minute); err == nil {
		t.Fatal("want error")
	}
	if err := s.Finish(ctx, "id", StateFailed, ""); err == nil {
		t.Fatal("want error")
	}
	if err := s.Resume(ctx, "id"); err == nil {
		t.Fatal("want error")
	}
}

func TestTargetBadConfigJSON(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newStore(t)

	if _, err := s.pool.Exec(ctx,
		`INSERT INTO cp_targets (name, provider, config) VALUES ('bad', 'static', '"not-an-object"')`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Target(ctx, "bad"); err == nil || !strings.Contains(err.Error(), "config") {
		t.Fatalf("err = %v", err)
	}
}
