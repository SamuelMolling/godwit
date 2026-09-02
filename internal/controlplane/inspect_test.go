package controlplane

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestInspectorStatus(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, pool := newStore(t)
	sched, _ := newScheduler(t, s, Config{Holder: "h"})
	insp := NewInspector(sched)

	st, err := insp.Status(ctx, "app")
	if err != nil || st.Provider != "plain" || len(st.Applied) != 0 || st.LastRun != nil || st.Snapshot != nil || st.OpenDrift {
		t.Fatalf("fresh target = %+v, err = %v", st, err)
	}

	const id = "cccccccc-0000-0000-0000-000000000001"
	queueRun(t, s, id, goodFiles())
	sched.Tick(ctx)
	waitState(t, s, id, StateSucceeded)
	if _, err := s.RecordDrift(ctx, "app", mustSnapshot(t, s).Fingerprint, "+ x"); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterTarget(ctx, "app", "plain", map[string]string{
		"dsn": mustTargetDSN(t, s), ConfigLockTimeout: "3s",
	}); err != nil {
		t.Fatal(err)
	}

	st, err = insp.Status(ctx, "app")
	if err != nil {
		t.Fatal(err)
	}
	if st.Target != "app" || st.Timeouts.Lock != "3s" || len(st.Applied) != 1 || st.Applied[0].Version != 20260901120000 ||
		st.Applied[0].Name != "t" || st.Applied[0].Checksum == "" || st.Applied[0].AppliedAt.IsZero() {
		t.Fatalf("status = %+v", st)
	}
	if st.LastRun == nil || st.LastRun.ID != id || st.LastRun.Kind != KindMigrate || st.LastRun.FinishedAt == nil {
		t.Fatalf("last run = %+v", st.LastRun)
	}
	if st.Snapshot == nil || st.Snapshot.RunID != id || st.Snapshot.TakenAt.IsZero() || !st.OpenDrift {
		t.Fatalf("baseline = %+v, open drift = %v", st.Snapshot, st.OpenDrift)
	}

	for _, tc := range []struct {
		drop string
		want string
	}{
		{"cp_drift_events", "check open drift"},
		{"cp_snapshots", "load snapshot"},
		{"cp_runs", "load last run"},
	} {
		if _, err := pool.Exec(ctx, "DROP TABLE "+tc.drop+" CASCADE"); err != nil {
			t.Fatal(err)
		}
		if _, err := insp.Status(ctx, "app"); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("without %s: err = %v", tc.drop, err)
		}
	}
}

func TestInspectorStatusTargetErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newStore(t)
	sched, _ := newScheduler(t, s, Config{Holder: "h"})
	insp := NewInspector(sched)

	if _, err := insp.Status(ctx, "ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown target err = %v", err)
	}
	targets := map[string]map[string]string{
		"nodsn":  {},
		"broken": {"dsn": "postgres://nobody@127.0.0.1:1/x"},
	}
	for name, config := range targets {
		if err := s.RegisterTarget(ctx, name, "plain", config); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := insp.Status(ctx, "nodsn"); err == nil || !strings.Contains(err.Error(), "missing dsn") {
		t.Fatalf("provider err = %v", err)
	}
	if _, err := insp.Status(ctx, "broken"); err == nil || !strings.Contains(err.Error(), "connect target") {
		t.Fatalf("unreachable err = %v", err)
	}
}

func mustSnapshot(t *testing.T, s *Store) Snapshot {
	t.Helper()
	snap, err := s.SnapshotFor(context.Background(), "app")
	if err != nil {
		t.Fatal(err)
	}

	return snap
}

func mustTargetDSN(t *testing.T, s *Store) string {
	t.Helper()
	_, config, err := s.Target(context.Background(), "app")
	if err != nil {
		t.Fatal(err)
	}

	return config["dsn"]
}
