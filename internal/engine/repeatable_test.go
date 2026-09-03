package engine

import (
	"context"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

const (
	viewV1 = "CREATE OR REPLACE VIEW stats AS SELECT 1 AS n;"
	viewV2 = "CREATE OR REPLACE VIEW stats AS SELECT 2 AS n;"
	viewV3 = "CREATE OR REPLACE VIEW stats AS SELECT 3 AS n;"
	dropV  = "DROP VIEW IF EXISTS stats;"
)

func repeatable(t *testing.T, up string) Migration {
	t.Helper()
	migs, err := LoadFS(fstest.MapFS{
		"R__stats.up.sql":   &fstest.MapFile{Data: []byte(up)},
		"R__stats.down.sql": &fstest.MapFile{Data: []byte(dropV)},
	})
	if err != nil {
		t.Fatal(err)
	}

	return migs[0]
}

func TestLoadDirRepeatable(t *testing.T) {
	t.Parallel()

	migs, err := LoadDir(writeFiles(t, map[string]string{
		"R__stats.up.sql":                  viewV1,
		"R__stats.down.sql":                dropV,
		"R__audit.up.sql":                  "CREATE OR REPLACE VIEW audit AS SELECT 1;",
		"R__audit.down.sql":                "DROP VIEW IF EXISTS audit;",
		"20260901130000_b.up.sql":          "CREATE TABLE b (id int);",
		"20260901130000_b.down.sql":        "DROP TABLE b;",
		"20260901120000_create_a.up.sql":   "CREATE TABLE a (id int);",
		"20260901120000_create_a.down.sql": "DROP TABLE a;",
	}))
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, m := range migs {
		ids = append(ids, m.ID())
	}
	want := []string{"20260901120000_create_a", "20260901130000_b", "R__audit", "R__stats"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v, want %v", ids, want)
	}
	last := migs[3]
	if !last.Repeatable || last.Version != 0 || last.Name != "stats" || last.Checksum == "" ||
		last.UpFile() != "R__stats.up.sql" || last.DownFile() != "R__stats.down.sql" {
		t.Fatalf("repeatable = %+v", last)
	}
}

func TestLoadDirRepeatableErrors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		files   map[string]string
		wantErr string
	}{
		{
			name:    "uppercase name",
			files:   map[string]string{"R__Stats.up.sql": viewV1},
			wantErr: "unexpected file",
		},
		{
			name:    "missing down",
			files:   map[string]string{"R__stats.up.sql": viewV1},
			wantErr: "R__stats: missing down file",
		},
		{
			name:    "missing up",
			files:   map[string]string{"R__stats.down.sql": dropV},
			wantErr: "R__stats: missing up file",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := LoadDir(writeFiles(t, tc.files))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestCompareMigrations(t *testing.T) {
	t.Parallel()

	a := Migration{Version: 1}
	r := Migration{Name: "a", Repeatable: true}
	if CompareMigrations(a, r) >= 0 || CompareMigrations(r, a) <= 0 {
		t.Fatal("versioned must sort before repeatable")
	}
	if CompareMigrations(r, Migration{Name: "b", Repeatable: true}) >= 0 {
		t.Fatal("repeatables must sort by name")
	}
}

func applyUp(t *testing.T, conn DB, m Migration) bool {
	t.Helper()
	res, err := New(conn, Options{}).Up(context.Background(), buildPlanT(t, m, DirectionUp))
	if err != nil {
		t.Fatal(err)
	}

	return res.Skipped
}

func viewN(t *testing.T, conn DB) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(context.Background(), "SELECT n FROM stats").Scan(&n); err != nil {
		t.Fatal(err)
	}

	return n
}

func TestRepeatableAppliesReappliesAndSkips(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn := newTestDB(t)()

	if applyUp(t, conn, repeatable(t, viewV1)) {
		t.Fatal("first apply must not be skipped")
	}
	if viewN(t, conn) != 1 {
		t.Fatalf("view = %d", viewN(t, conn))
	}
	if !applyUp(t, conn, repeatable(t, viewV1)) {
		t.Fatal("unchanged content must be skipped")
	}
	if applyUp(t, conn, repeatable(t, viewV2)) {
		t.Fatal("edited content must re-apply")
	}
	if viewN(t, conn) != 2 {
		t.Fatalf("view = %d", viewN(t, conn))
	}

	var recorded string
	if err := conn.QueryRow(ctx, "SELECT checksum FROM godwit.repeatables WHERE name = 'stats'").Scan(&recorded); err != nil {
		t.Fatal(err)
	}
	if recorded != repeatable(t, viewV2).Checksum {
		t.Fatalf("recorded checksum = %s", recorded)
	}
	var versions int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM godwit.migrations").Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if versions != 0 {
		t.Fatalf("repeatables must not enter the version history: %d rows", versions)
	}
}

func TestRepeatableDown(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn := newTestDB(t)()
	exec := New(conn, Options{})

	m := repeatable(t, viewV1)
	if applyUp(t, conn, m) {
		t.Fatal("apply skipped")
	}
	res, err := exec.Down(ctx, buildPlanT(t, m, DirectionDown))
	if err != nil || res.Skipped {
		t.Fatalf("down = %+v, err = %v", res, err)
	}
	var recorded int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM godwit.repeatables").Scan(&recorded); err != nil {
		t.Fatal(err)
	}
	if recorded != 0 {
		t.Fatalf("down must drop the record: %d rows", recorded)
	}
	res, err = exec.Down(ctx, buildPlanT(t, m, DirectionDown))
	if err != nil || !res.Skipped {
		t.Fatalf("second down = %+v, err = %v", res, err)
	}
}

func TestRepeatableResumesAfterCrash(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	open := newTestDB(t)
	conn := open()

	m := Migration{
		Name: "stats", Repeatable: true,
		UpSQL:   "CREATE TABLE IF NOT EXISTS marker (id int);\n" + viewV1,
		DownSQL: dropV,
	}
	m.Checksum = repeatable(t, m.UpSQL).Checksum
	plan := buildPlanT(t, m, DirectionUp)

	crash := New(conn, Options{}, WithHook(func(p HookPoint, idx int) {
		if p == HookBeforeStatement && idx == 1 {
			panic("crash")
		}
	}))
	func() {
		defer func() { _ = recover() }()
		_, _ = crash.Up(ctx, plan)
	}()

	if _, err := conn.Exec(ctx, "SELECT 1 FROM marker"); err != nil {
		t.Fatalf("first statement must be committed: %v", err)
	}
	if _, err := conn.Exec(ctx, `UPDATE godwit.runs SET state = 'failed' WHERE repeatable = 'stats'`); err != nil {
		t.Fatal(err)
	}

	resumed := open()
	res, err := New(resumed, Options{}).Up(ctx, plan)
	if err != nil || res.Skipped || res.Applied != 1 {
		t.Fatalf("resume = %+v, err = %v", res, err)
	}
	if viewN(t, resumed) != 1 {
		t.Fatalf("view = %d", viewN(t, resumed))
	}
	var runs int
	if err := resumed.QueryRow(ctx, `SELECT count(*) FROM godwit.runs WHERE repeatable = 'stats'`).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if runs != 1 {
		t.Fatalf("resume must reuse the open run: %d runs", runs)
	}
}

// An edited repeatable is a different run key, so it starts a run of its own instead of failing to resume.
func TestRepeatableEditedAfterCrashStartsNewRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	open := newTestDB(t)
	conn := open()

	first := Migration{Name: "stats", Repeatable: true, UpSQL: "SELECT 1;\n" + viewV1, DownSQL: dropV}
	first.Checksum = repeatable(t, first.UpSQL).Checksum
	crash := New(conn, Options{}, WithHook(func(p HookPoint, idx int) {
		if p == HookBeforeStatement && idx == 1 {
			panic("crash")
		}
	}))
	func() {
		defer func() { _ = recover() }()
		_, _ = crash.Up(ctx, buildPlanT(t, first, DirectionUp))
	}()
	if _, err := conn.Exec(ctx, `UPDATE godwit.runs SET state = 'failed' WHERE repeatable = 'stats'`); err != nil {
		t.Fatal(err)
	}

	next := open()
	if applyUp(t, next, repeatable(t, viewV3)) {
		t.Fatal("edited repeatable must apply")
	}
	if viewN(t, next) != 3 {
		t.Fatalf("view = %d", viewN(t, next))
	}
	var runs int
	if err := next.QueryRow(ctx, `SELECT count(*) FROM godwit.runs WHERE repeatable = 'stats'`).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if runs != 2 {
		t.Fatalf("edited content must open its own run: %d runs", runs)
	}
}

func TestRepeatableStatusAndList(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn := newTestDB(t)()
	exec := New(conn, Options{})

	if reps, err := ListRepeatables(ctx, conn); err != nil || reps != nil {
		t.Fatalf("before bootstrap = %v, err = %v", reps, err)
	}
	if applyUp(t, conn, repeatable(t, viewV1)) {
		t.Fatal("apply skipped")
	}
	reps, err := ListRepeatables(ctx, conn)
	if err != nil || len(reps) != 1 || reps[0].Name != "stats" || reps[0].AppliedAt.IsZero() {
		t.Fatalf("repeatables = %v, err = %v", reps, err)
	}

	rows, err := exec.Status(ctx, []Migration{repeatable(t, viewV1), repeatable(t, viewV2)})
	if err != nil || len(rows) != 2 {
		t.Fatalf("status = %v, err = %v", rows, err)
	}
	if !rows[0].Applied || rows[0].Drifted || rows[0].AppliedAt.IsZero() {
		t.Fatalf("unchanged row = %+v", rows[0])
	}
	if rows[1].Applied || rows[1].Drifted {
		t.Fatalf("edited row = %+v", rows[1])
	}

	unknown := repeatable(t, viewV1)
	unknown.Name = "other"
	rows, err = exec.Status(ctx, []Migration{unknown})
	if err != nil || rows[0].Applied {
		t.Fatalf("unknown repeatable = %v, err = %v", rows, err)
	}
}

func TestRepeatableMarkOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn := newTestDB(t)()

	m := repeatable(t, viewV1)
	plan := buildPlanT(t, m, DirectionUp)
	plan.MarkOnly = true
	if _, err := New(conn, Options{}).Up(ctx, plan); err != nil {
		t.Fatal(err)
	}
	var checksum string
	if err := conn.QueryRow(ctx, "SELECT checksum FROM godwit.repeatables WHERE name = 'stats'").Scan(&checksum); err != nil {
		t.Fatal(err)
	}
	if checksum != m.Checksum {
		t.Fatalf("checksum = %s", checksum)
	}
	if _, err := conn.Exec(ctx, "SELECT 1 FROM stats"); err == nil {
		t.Fatal("mark-only must not execute the statements")
	}
}

func TestListRepeatablesErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mock, exec := newMockExec(t)

	mock.ExpectQuery("SELECT to_regclass").WithArgs("godwit.repeatables").WillReturnError(errBoom)
	if _, err := ListRepeatables(ctx, mock); err == nil || !strings.Contains(err.Error(), "probe godwit schema") {
		t.Fatalf("probe err = %v", err)
	}

	mock.ExpectQuery("SELECT to_regclass").WithArgs("godwit.repeatables").
		WillReturnRows(pgxmock.NewRows([]string{"present"}).AddRow(true))
	mock.ExpectQuery("SELECT name, checksum, applied_at").WillReturnError(errBoom)
	if _, err := ListRepeatables(ctx, mock); err == nil || !strings.Contains(err.Error(), "list repeatables") {
		t.Fatalf("query err = %v", err)
	}

	mock.ExpectQuery("SELECT to_regclass").WithArgs("godwit.repeatables").
		WillReturnRows(pgxmock.NewRows([]string{"present"}).AddRow(true))
	mock.ExpectQuery("SELECT name, checksum, applied_at").WillReturnRows(
		pgxmock.NewRows([]string{"name", "checksum", "applied_at"}).AddRow("stats", "c", time.Now()).RowError(0, errBoom))
	if _, err := ListRepeatables(ctx, mock); err == nil || !strings.Contains(err.Error(), "read repeatables") {
		t.Fatalf("row err = %v", err)
	}

	expectBootstrap(mock)
	mock.ExpectQuery("SELECT version, name, checksum, applied_at").
		WillReturnRows(pgxmock.NewRows([]string{"version", "name", "checksum", "applied_at"}))
	mock.ExpectQuery("SELECT name, checksum, applied_at").WillReturnError(errBoom)
	if _, err := exec.Status(ctx, nil); err == nil || !strings.Contains(err.Error(), "list repeatables") {
		t.Fatalf("status err = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
