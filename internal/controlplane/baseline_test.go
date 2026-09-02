package controlplane

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SamuelMolling/godwit/internal/engine"
)

func baselineMigrations(t *testing.T) []engine.Migration {
	t.Helper()
	plans, err := PlansFromFiles(goodFiles(), engine.DirectionUp)
	if err != nil {
		t.Fatal(err)
	}
	migs := make([]engine.Migration, 0, len(plans))
	for _, p := range plans {
		migs = append(migs, p.Migration)
	}

	return migs
}

func TestBaselinerRecordsRunAndReplaysHistory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, pool := newStore(t)
	sched, targetDSN := newScheduler(t, s, Config{Holder: "h"})
	execTarget(t, targetDSN, upBody)
	const id = "bbbbbbbb-0000-0000-0000-000000000010"

	b := NewBaseliner(sched)
	if err := b.Baseline(ctx, id, "app", baselineMigrations(t), Provenance{CreatedBy: "ops"}); err != nil {
		t.Fatal(err)
	}
	run, err := s.Run(ctx, id)
	if err != nil || run.Kind != KindBaseline || run.State != StateSucceeded || run.FinishedAt == nil || run.Rollout != RolloutDirect ||
		run.Provenance.CreatedBy != "ops" {
		t.Fatalf("run = %+v, err = %v", run, err)
	}
	files, err := s.RunFiles(ctx, id)
	if err != nil || len(files) != 2 || files["20260901120000_t.up.sql"] != upBody {
		t.Fatalf("files = %v, err = %v", files, err)
	}
	if _, err := s.SnapshotFor(ctx, "app"); err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	next, err := buildPlans([]engine.Migration{{
		Version: 20260901120001, Name: "idx",
		UpSQL: "CREATE INDEX t_id ON t (id);", DownSQL: "DROP INDEX t_id;",
	}}, engine.DirectionUp)
	if err != nil {
		t.Fatal(err)
	}
	if err := NewValidator(pool, s, func() string { return "basehist" }).Validate(ctx, "app", next); err != nil {
		t.Fatalf("validate after baseline: %v", err)
	}

	queueRun(t, s, "bbbbbbbb-0000-0000-0000-000000000011", goodFiles())
	sched.Tick(ctx)
	if r := waitState(t, s, "bbbbbbbb-0000-0000-0000-000000000011", StateSucceeded); r.Kind != KindMigrate {
		t.Fatalf("kind = %q", r.Kind)
	}

	err = b.Baseline(ctx, "bbbbbbbb-0000-0000-0000-000000000012", "app", baselineMigrations(t), Provenance{CreatedBy: "ops"})
	if !errors.Is(err, engine.ErrAlreadyMigrated) {
		t.Fatalf("second baseline err = %v", err)
	}
}

func TestBaselinerErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newStore(t)
	sched, _ := newScheduler(t, s, Config{Holder: "h"})
	b := NewBaseliner(sched)
	migs := baselineMigrations(t)

	if err := b.Baseline(ctx, "x", "ghost", migs, Provenance{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown target err = %v", err)
	}
	if err := s.RegisterTarget(ctx, "broken", "plain", map[string]string{"dsn": "postgres://nobody@127.0.0.1:1/x"}); err != nil {
		t.Fatal(err)
	}
	if err := b.Baseline(ctx, "x", "broken", migs, Provenance{}); err == nil || !strings.Contains(err.Error(), "connect target") {
		t.Fatalf("unreachable target err = %v", err)
	}

	const id = "bbbbbbbb-0000-0000-0000-000000000020"
	if err := b.Baseline(ctx, id, "app", migs, Provenance{}); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterTarget(ctx, "other", "plain", map[string]string{"dsn": newDatabase(t, "tg")}); err != nil {
		t.Fatal(err)
	}
	if err := b.Baseline(ctx, id, "other", migs, Provenance{}); err == nil || !strings.Contains(err.Error(), "create baseline") {
		t.Fatalf("duplicate run id err = %v", err)
	}
}
