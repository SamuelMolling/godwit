package admission

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/SamuelMolling/godwit/internal/controlplane"
	"github.com/SamuelMolling/godwit/internal/engine"
)

func TestCheckHazards(t *testing.T) {
	t.Parallel()
	g, _ := newGate(t)

	m := engine.Migration{Version: 1, Name: "d", UpSQL: "DROP TABLE x;", DownSQL: "SELECT 1;"}
	p, err := engine.BuildPlan(m, engine.DirectionUp)
	if err != nil {
		t.Fatal(err)
	}
	none := controlplane.AppliedSet{}
	if err := g.checkHazards([]engine.Plan{p}, none, nil); err == nil || !strings.Contains(err.Error(), "H002") {
		t.Fatalf("err = %v", err)
	}
	if err := g.checkHazards([]engine.Plan{p}, none, []string{"H002"}); err != nil {
		t.Fatalf("acked: %v", err)
	}
	held := controlplane.AppliedSet{Versions: []int64{1}}
	if err := g.checkHazards([]engine.Plan{p}, held, nil); err != nil {
		t.Fatalf("applied: %v", err)
	}
	mark := p
	mark.MarkOnly = true
	if err := g.checkHazards([]engine.Plan{mark}, none, nil); err != nil {
		t.Fatalf("mark-only: %v", err)
	}
	down, err := engine.BuildPlan(engine.Migration{Version: 1, Name: "d", UpSQL: "SELECT 1;", DownSQL: "DROP TABLE x;"}, engine.DirectionDown)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.checkHazards([]engine.Plan{down}, held, nil); err == nil || !strings.Contains(err.Error(), "H002") {
		t.Fatalf("down: %v", err)
	}
}

func TestCheckOrder(t *testing.T) {
	t.Parallel()
	g, _ := newGate(t)

	plan := func(v int64) engine.Plan {
		p, err := engine.BuildPlan(engine.Migration{Version: v, Name: "m", UpSQL: "SELECT 1;", DownSQL: "SELECT 1;"}, engine.DirectionUp)
		if err != nil {
			t.Fatal(err)
		}

		return p
	}

	if err := g.checkOrder("app", []engine.Plan{plan(2)}, nil, false); err != nil {
		t.Fatalf("empty history: %v", err)
	}
	if err := g.checkOrder("app", []engine.Plan{plan(1), plan(3), plan(4)}, []int64{1, 3}, false); err != nil {
		t.Fatalf("applied and newer: %v", err)
	}
	err := g.checkOrder("app", []engine.Plan{plan(1), plan(2), plan(3)}, []int64{1, 3}, false)
	if !errors.Is(err, ErrRefused) || !errors.Is(err, errOutOfOrder) ||
		!strings.Contains(err.Error(), "out-of-order migrations 2: newest applied version on app is 3") {
		t.Fatalf("behind: %v", err)
	}
	if err := g.checkOrder("app", []engine.Plan{plan(2)}, []int64{1, 3}, true); err != nil {
		t.Fatalf("allowed: %v", err)
	}
}

func TestAdmitStoreError(t *testing.T) {
	t.Parallel()
	g, mock := newGate(t)

	mock.ExpectQuery("SELECT provider, coalesce\\(credential_store, ..\\), config FROM cp_targets").WithArgs("ghost").WillReturnError(pgx.ErrNoRows)
	if _, err := g.Admit(context.Background(), "ghost", nil, nil, false, false, ""); !errors.Is(err, controlplane.ErrNotFound) {
		t.Fatalf("unknown target: %v", err)
	}
	expectTarget(mock)
	mock.ExpectQuery("SELECT DISTINCT left").WithArgs("app").WillReturnError(errors.New("down"))
	if _, err := g.Admit(context.Background(), "app", nil, nil, false, false, ""); err == nil || errors.Is(err, ErrRefused) {
		t.Fatalf("a store failure is not a refusal: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
