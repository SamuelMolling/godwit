package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/pashagolub/pgxmock/v4"
)

func TestDropNaming(t *testing.T) {
	t.Parallel()
	qualified := Drop{Schema: "public", Table: "users", Column: "age"}
	if qualified.String() != "public.users.age" || qualified.Kind() != "column" {
		t.Fatalf("qualified = %s %s", qualified, qualified.Kind())
	}
	bare := Drop{Table: "users"}
	if bare.String() != "users" || bare.Kind() != "table" {
		t.Fatalf("bare = %s %s", bare, bare.Kind())
	}
	if got := (Loss{Drop: bare, Rows: 2}).String(); got != "table users holds 2 row(s)" {
		t.Fatalf("loss = %q", got)
	}
}

func TestPlanDropsCollectsPerMigration(t *testing.T) {
	t.Parallel()
	p := buildPlanT(t, Migration{
		Version: 1, Name: "d",
		UpSQL:   "SELECT 1;",
		DownSQL: "DROP TABLE public.a, b;\nALTER TABLE c DROP COLUMN old;\nSELECT 1;",
	}, DirectionDown)
	got := PlanDrops([]Plan{p})
	want := []Drop{{Schema: "public", Table: "a"}, {Table: "b"}, {Table: "c", Column: "old"}}
	if len(got) != 1 || len(got[p.Migration.ID()]) != 3 {
		t.Fatalf("drops = %+v", got)
	}
	for i, d := range got[p.Migration.ID()] {
		if d != want[i] {
			t.Fatalf("drop %d = %+v, want %+v", i, d, want[i])
		}
	}
}

func expectRegclass(mock pgxmock.PgxConnIface) {
	rel := "public.t"
	mock.ExpectQuery("SELECT to_regclass").WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"to_regclass"}).AddRow(&rel))
}

func expectNoRegclass(mock pgxmock.PgxConnIface) {
	var missing *string
	mock.ExpectQuery("SELECT to_regclass").WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"to_regclass"}).AddRow(missing))
}

func TestDataLossSkipsWhatTheTargetDoesNotHave(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mock, err := pgxmock.NewConn()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mock.Close(ctx) })

	expectNoRegclass(mock)
	got, err := DataLoss(ctx, mock, []Drop{{Table: "gone"}})
	if err != nil || len(got) != 0 {
		t.Fatalf("missing table = %+v, err = %v", got, err)
	}

	expectRegclass(mock)
	mock.ExpectQuery("FROM pg_attribute").WithArgs("public.t", "gone").
		WillReturnRows(pgxmock.NewRows([]string{"exists"}).AddRow(false))
	if got, err = DataLoss(ctx, mock, []Drop{{Table: "t", Column: "gone"}}); err != nil || len(got) != 0 {
		t.Fatalf("missing column = %+v, err = %v", got, err)
	}

	expectRegclass(mock)
	mock.ExpectQuery("SELECT count").WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(int64(0)))
	if got, err = DataLoss(ctx, mock, []Drop{{Schema: "public", Table: "t"}}); err != nil || len(got) != 0 {
		t.Fatalf("empty table = %+v, err = %v", got, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDataLossErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mock, err := pgxmock.NewConn()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mock.Close(ctx) })

	mock.ExpectQuery("SELECT to_regclass").WithArgs(pgxmock.AnyArg()).WillReturnError(errBoom)
	if _, err := DataLoss(ctx, mock, []Drop{{Table: "t"}}); err == nil || !strings.Contains(err.Error(), "resolve t") {
		t.Fatalf("err = %v", err)
	}

	expectRegclass(mock)
	mock.ExpectQuery("FROM pg_attribute").WithArgs("public.t", "c").WillReturnError(errBoom)
	if _, err := DataLoss(ctx, mock, []Drop{{Table: "t", Column: "c"}}); err == nil || !strings.Contains(err.Error(), "check column t.c") {
		t.Fatalf("err = %v", err)
	}

	expectRegclass(mock)
	mock.ExpectQuery("SELECT count").WillReturnError(errBoom)
	if _, err := DataLoss(ctx, mock, []Drop{{Table: "t"}}); err == nil || !strings.Contains(err.Error(), "count t") {
		t.Fatalf("err = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
