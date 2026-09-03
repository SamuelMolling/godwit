package controlplane

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stripe/pg-schema-diff/pkg/diff"

	"github.com/SamuelMolling/godwit/internal/engine"
)

const desiredDDL = `
CREATE TABLE t (id int, status text NOT NULL DEFAULT 'new');
CREATE TABLE orders (id bigint PRIMARY KEY, t_id int);
CREATE INDEX orders_t_id_idx ON orders (t_id);
`

func newDiffer(t *testing.T, history HistoryReplayer) (*Differ, *Store, string) {
	t.Helper()
	s, pool := newStore(t)
	sched, targetDSN := newScheduler(t, s, Config{Holder: "h"})
	targetDSN += "&search_path=public"
	if err := s.RegisterTarget(context.Background(), "app", "plain", map[string]string{"dsn": targetDSN}); err != nil {
		t.Fatal(err)
	}
	var seq atomic.Int64
	newID := func() string { return fmt.Sprintf("%s%d", strings.ToLower(t.Name()), seq.Add(1)) }

	return NewDiffer(pool, sched, history, newID), s, targetDSN
}

func execDSN(t *testing.T, dsn, sql string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, sql); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func TestDifferBothDirections(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d, s, targetDSN := newDiffer(t, nil)
	d.history = NewValidator(d.pool, s, d.newID)
	queueRun(t, s, "dddddddd-0000-0000-0000-000000000001", goodFiles())
	d.sched.Tick(ctx)
	waitState(t, s, "dddddddd-0000-0000-0000-000000000001", StateSucceeded)

	out, err := d.Diff(ctx, "app", desiredDDL)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Observed.Applied) != 1 || out.Observed.SearchPath != "public" || len(out.Drift) != 0 {
		t.Fatalf("observed = %+v, drift = %q", out.Observed, out.Drift)
	}
	for _, want := range []string{`ADD COLUMN "status"`, `CREATE TABLE "public"."orders"`, `CREATE INDEX CONCURRENTLY orders_t_id_idx`} {
		if !strings.Contains(out.UpSQL, want) {
			t.Fatalf("up missing %q:\n%s", want, out.UpSQL)
		}
	}
	for _, want := range []string{`DROP TABLE "public"."orders"`, `DROP COLUMN "status"`} {
		if !strings.Contains(out.DownSQL, want) {
			t.Fatalf("down missing %q:\n%s", want, out.DownSQL)
		}
	}
	for _, sql := range []string{out.UpSQL, out.DownSQL} {
		if strings.Contains(sql, "godwit") || strings.Contains(sql, "\n\n") {
			t.Fatalf("sql:\n%s", sql)
		}
		if _, err := engine.BuildPlan(engine.Migration{Name: "x", UpSQL: sql}, engine.DirectionUp); err != nil {
			t.Fatal(err)
		}
	}

	execDSN(t, targetDSN, strings.ReplaceAll(out.UpSQL, "CONCURRENTLY ", ""))
	same, err := d.Diff(ctx, "app", desiredDDL)
	if err != nil || same.UpSQL != "" || same.DownSQL != "" {
		t.Fatalf("after apply: up = %q, down = %q, err = %v", same.UpSQL, same.DownSQL, err)
	}
	if len(same.Drift) < 2 || !strings.Contains(same.Drift[0], "+ column public.orders.id") {
		t.Fatalf("drift = %q", same.Drift)
	}
}

func TestDifferSkipsDriftWithoutHistory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d, _, targetDSN := newDiffer(t, nil)
	execDSN(t, targetDSN, "CREATE TABLE t (id int)")

	out, err := d.Diff(ctx, "app", "CREATE TABLE t (id int);")
	if err != nil || out.UpSQL != "" || out.Drift != nil || len(out.Observed.Applied) != 0 {
		t.Fatalf("out = %+v, err = %v", out, err)
	}
}

type stubHistory struct {
	val Validation
	err error
}

func (h stubHistory) Validate(context.Context, string, []engine.Plan, string) (Validation, error) {
	return h.val, h.err
}

func TestDifferErrors(t *testing.T) {
	ctx := context.Background()
	d, s, _ := newDiffer(t, stubHistory{err: errBoom})

	if _, err := d.Diff(ctx, "ghost", desiredDDL); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown target err = %v", err)
	}
	if err := s.RegisterTarget(ctx, "broken", "plain", map[string]string{"dsn": "postgres://nobody@127.0.0.1:1/x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Diff(ctx, "broken", desiredDDL); err == nil || !strings.Contains(err.Error(), "connect target") {
		t.Fatalf("unreachable err = %v", err)
	}
	if _, err := d.Diff(ctx, "app", desiredDDL); !errors.Is(err, errBoom) {
		t.Fatalf("history err = %v", err)
	}
	d.history = nil

	parseDSN = func(string) (*pgx.ConnConfig, error) { return nil, errBoom }
	if _, err := d.Diff(ctx, "app", desiredDDL); !errors.Is(err, errBoom) || !strings.Contains(err.Error(), "parse target dsn") {
		t.Fatalf("parse err = %v", err)
	}
	parseDSN = pgx.ParseConfig

	if _, err := d.pool.Exec(ctx, "CREATE DATABASE godwit_diff_dup"); err != nil {
		t.Fatal(err)
	}
	dup := NewDiffer(d.pool, d.sched, nil, func() string { return "dup" })
	if _, err := dup.Diff(ctx, "app", desiredDDL); err == nil || !strings.Contains(err.Error(), "create scratch database") {
		t.Fatalf("scratch err = %v", err)
	}

	if _, err := d.Diff(ctx, "app", "CREATE TABLE t (id nosuchtype);"); !errors.Is(err, ErrDesiredSchema) {
		t.Fatalf("ddl err = %v", err)
	}

	var calls int
	generatePlan = func(context.Context, diff.SchemaSource, diff.SchemaSource, ...diff.PlanOpt) (diff.Plan, error) {
		calls++
		if calls == 2 {
			return diff.Plan{}, errBoom
		}

		return diff.Plan{Statements: []diff.Statement{{DDL: "SELECT 1"}}}, nil
	}
	defer func() { generatePlan = diff.Generate }()
	if _, err := d.Diff(ctx, "app", desiredDDL); !errors.Is(err, errBoom) || !strings.Contains(err.Error(), "diff desired to live") {
		t.Fatalf("down err = %v", err)
	}
	calls = 1
	if _, err := d.Diff(ctx, "app", desiredDDL); !errors.Is(err, errBoom) || !strings.Contains(err.Error(), "diff live to desired") {
		t.Fatalf("up err = %v", err)
	}
	if err := (&scratchFactory{}).Close(); err != nil {
		t.Fatal(err)
	}
	quietLog{}.Errorf("dropped")
	quietLog{}.Warnf("dropped")
}
