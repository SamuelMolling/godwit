package controlplane

import (
	"context"
	"testing"
)

func TestNotificationTS(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newStore(t)

	if ch, ts, err := s.GetTS(ctx, "run", "r1"); err != nil || ch != "" || ts != "" {
		t.Fatalf("missing key = %q %q, err = %v", ch, ts, err)
	}
	if err := s.PutTS(ctx, "run", "r1", "C1", "1.1"); err != nil {
		t.Fatal(err)
	}
	if err := s.PutTS(ctx, "drift", "drift:app", "C1", "2.2"); err != nil {
		t.Fatal(err)
	}
	if err := s.PutTS(ctx, "run", "r1", "C2", "3.3"); err != nil {
		t.Fatal(err)
	}
	if ch, ts, err := s.GetTS(ctx, "run", "r1"); err != nil || ch != "C2" || ts != "3.3" {
		t.Fatalf("upserted = %q %q, err = %v", ch, ts, err)
	}
	if ch, ts, err := s.GetTS(ctx, "drift", "drift:app"); err != nil || ch != "C1" || ts != "2.2" {
		t.Fatalf("drift = %q %q, err = %v", ch, ts, err)
	}
}
