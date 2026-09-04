package engine

import (
	"context"
	"math"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"
)

const (
	backfillRows = 12000
	batchSize    = 1000

	backfillSetup = `CREATE TABLE bf (id bigint PRIMARY KEY, v int, v_new bigint, touched int NOT NULL DEFAULT 0);
INSERT INTO bf (id, v) SELECT g, g FROM generate_series(1, 12000) g`

	backfillSQL = `WITH b AS (SELECT id FROM bf WHERE id > $1 AND v_new IS DISTINCT FROM v::bigint ORDER BY id LIMIT 1000)
UPDATE bf AS t SET v_new = t.v::bigint, touched = t.touched + 1 FROM b WHERE t.id = b.id RETURNING b.id`
)

func batchPlan(sql string, spec *BatchSpec) Plan {
	return Plan{
		Migration:  Migration{Version: 1, Name: "backfill", Checksum: "c"},
		Direction:  DirectionUp,
		Statements: []Statement{{SQL: sql, Hash: hashSQL(sql), Verifier: VerifierBatch, Batch: spec}},
	}
}

func intBatchPlan() Plan {
	return batchPlan(backfillSQL, &BatchSpec{
		Key: `"id"`, KeyKind: BatchKeyInt, Size: batchSize,
		Pause: time.Millisecond, Estimate: "SELECT count(*) FROM bf",
	})
}

func setupBackfill(t *testing.T) func() *pgx.Conn {
	t.Helper()
	connect := newTestDB(t)
	if _, err := connect().Exec(context.Background(), backfillSetup); err != nil {
		t.Fatal(err)
	}

	return connect
}

func scalarInt(t *testing.T, conn *pgx.Conn, sql string) int64 {
	t.Helper()
	var n int64
	if err := conn.QueryRow(context.Background(), sql).Scan(&n); err != nil {
		t.Fatal(err)
	}

	return n
}

func intentState(t *testing.T, conn *pgx.Conn) (string, int64) {
	t.Helper()
	var cursor *string
	var done int64
	if err := conn.QueryRow(context.Background(),
		`SELECT cursor, rows_done FROM godwit.journal WHERE state = 'intent'`).Scan(&cursor, &done); err != nil {
		t.Fatal(err)
	}
	if cursor == nil {
		return "", done
	}

	return *cursor, done
}

func assertBackfilled(t *testing.T, conn *pgx.Conn) {
	t.Helper()
	if n := scalarInt(t, conn, "SELECT count(*) FROM bf WHERE v_new = v::bigint"); n != backfillRows {
		t.Fatalf("backfilled rows = %d, want %d", n, backfillRows)
	}
	if n := scalarInt(t, conn, "SELECT count(*) FROM bf WHERE touched <> 1"); n != 0 {
		t.Fatalf("%d rows touched a number of times other than once", n)
	}
}

func crashAfterBatch(conn *pgx.Conn, nth int) Option {
	seen := 0

	return WithHook(func(p HookPoint, _ int) {
		if p != HookAfterBatch {
			return
		}
		seen++
		if seen == nth {
			_ = conn.Close(context.Background())
		}
	})
}

func TestBatchBackfillCompletes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	connect := setupBackfill(t)
	conn := connect()

	var ev StatementEvent
	exec := New(conn, Options{}, WithObserver(func(e StatementEvent) { ev = e }))
	res, err := exec.Up(ctx, intBatchPlan())
	if err != nil {
		t.Fatal(err)
	}
	if res.Applied != 1 {
		t.Fatalf("res = %+v", res)
	}
	if ev.RowsDone != backfillRows || ev.RowsTotal != backfillRows {
		t.Fatalf("rows done = %d, total = %d", ev.RowsDone, ev.RowsTotal)
	}
	if ev.Batches != backfillRows/batchSize+1 {
		t.Fatalf("batches = %d", ev.Batches)
	}
	assertBackfilled(t, conn)

	cursor, done := intentState(t, conn)
	if cursor != "12000" || done != backfillRows {
		t.Fatalf("journal cursor = %q, rows_done = %d", cursor, done)
	}
	if n := scalarInt(t, conn, `SELECT count(*) FROM godwit.journal WHERE state = 'done'`); n != 1 {
		t.Fatalf("done journal rows = %d", n)
	}
}

func TestBatchCrashResumesFromCursor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	connect := setupBackfill(t)

	crashed := connect()
	if _, err := New(crashed, Options{}, crashAfterBatch(crashed, 5)).Up(ctx, intBatchPlan()); err == nil {
		t.Fatal("crash run must fail")
	}

	fresh := connect()
	cursor, done := intentState(t, fresh)
	if cursor != "5000" || done != 5*batchSize {
		t.Fatalf("after crash: cursor = %q, rows_done = %d", cursor, done)
	}
	if n := scalarInt(t, fresh, "SELECT count(*) FROM bf WHERE touched = 1"); n != 5*batchSize {
		t.Fatalf("rows backfilled before the crash = %d", n)
	}

	var ev StatementEvent
	res, err := New(fresh, Options{}, WithObserver(func(e StatementEvent) { ev = e })).Up(ctx, intBatchPlan())
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if res.Skipped || res.Applied != 1 {
		t.Fatalf("resume res = %+v", res)
	}
	if ev.RowsDone != backfillRows || ev.Batches != (backfillRows-5*batchSize)/batchSize+1 {
		t.Fatalf("resume rows = %d, batches = %d", ev.RowsDone, ev.Batches)
	}
	assertBackfilled(t, fresh)

	if _, done := intentState(t, fresh); done != backfillRows {
		t.Fatalf("journal rows_done = %d", done)
	}
}

func TestBatchInsideHeldPlan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	connect := setupBackfill(t)
	conn := connect()

	const contract = "CREATE TABLE contracted (id int)"
	full := intBatchPlan()
	full.Statements[0].Phase = PhaseExpand
	full.Statements = append(full.Statements, Statement{SQL: contract, Hash: hashSQL(contract), Phase: PhaseContract})
	expand := full
	expand.HoldFrom = 1

	res, err := New(conn, Options{}).Up(ctx, expand)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Held || res.Applied != 1 {
		t.Fatalf("expand res = %+v", res)
	}
	assertBackfilled(t, conn)
	if n := scalarInt(t, conn, "SELECT count(*) FROM godwit.migrations"); n != 0 {
		t.Fatalf("migrations rows before the contract phase = %d", n)
	}

	res, err = New(conn, Options{}).Up(ctx, full)
	if err != nil {
		t.Fatalf("contract phase: %v", err)
	}
	if res.Held || res.Applied != 1 {
		t.Fatalf("contract res = %+v", res)
	}
	if n := scalarInt(t, conn, "SELECT count(*) FROM godwit.migrations"); n != 1 {
		t.Fatalf("migrations rows after the contract phase = %d", n)
	}
	if n := scalarInt(t, conn, "SELECT count(*) FROM bf WHERE touched <> 1"); n != 0 {
		t.Fatalf("the contract phase re-ran the backfill on %d rows", n)
	}
}

func TestBatchRewoundCursorRepeatsNoRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	connect := setupBackfill(t)

	crashed := connect()
	if _, err := New(crashed, Options{}, crashAfterBatch(crashed, 5)).Up(ctx, intBatchPlan()); err == nil {
		t.Fatal("crash run must fail")
	}

	fresh := connect()
	if _, err := fresh.Exec(ctx, `UPDATE godwit.journal SET cursor = '2000' WHERE state = 'intent'`); err != nil {
		t.Fatal(err)
	}
	if _, err := New(fresh, Options{}).Up(ctx, intBatchPlan()); err != nil {
		t.Fatalf("resume: %v", err)
	}
	assertBackfilled(t, fresh)
}

func TestBatchCrashBeforeFirstBatch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	connect := setupBackfill(t)

	crashed := connect()
	exec := New(crashed, Options{}, WithHook(func(p HookPoint, _ int) {
		if p == HookAfterIntent {
			_ = crashed.Close(ctx)
		}
	}))
	if _, err := exec.Up(ctx, intBatchPlan()); err == nil {
		t.Fatal("crash run must fail")
	}

	fresh := connect()
	if cursor, done := intentState(t, fresh); cursor != "" || done != 0 {
		t.Fatalf("cursor = %q, rows_done = %d", cursor, done)
	}
	if _, err := New(fresh, Options{}).Up(ctx, intBatchPlan()); err != nil {
		t.Fatalf("resume: %v", err)
	}
	assertBackfilled(t, fresh)
}

func TestBatchStatementChangeRefusesResume(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	connect := setupBackfill(t)

	crashed := connect()
	if _, err := New(crashed, Options{}, crashAfterBatch(crashed, 2)).Up(ctx, intBatchPlan()); err == nil {
		t.Fatal("crash run must fail")
	}

	edited := batchPlan(backfillSQL+" -- edited", intBatchPlan().Statements[0].Batch)
	if _, err := New(connect(), Options{}).Up(ctx, edited); err == nil ||
		!strings.Contains(err.Error(), "changed since run") {
		t.Fatalf("err = %v", err)
	}
}

func TestBatchPauseIsCancellable(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	connect := setupBackfill(t)

	plan := batchPlan(backfillSQL, &BatchSpec{Key: `"id"`, KeyKind: BatchKeyInt, Size: batchSize, Pause: time.Hour})
	exec := New(connect(), Options{}, WithHook(func(p HookPoint, _ int) {
		if p == HookAfterBatch {
			cancel()
		}
	}))
	if _, err := exec.Up(ctx, plan); err == nil || !strings.Contains(err.Error(), "pause between batches") {
		t.Fatalf("err = %v", err)
	}
}

func TestBatchAppliesTimeoutsPerBatch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	connect := newTestDB(t)
	conn := connect()
	if _, err := conn.Exec(ctx,
		`CREATE TABLE tt (id int PRIMARY KEY, seen text);
		 INSERT INTO tt (id) SELECT g FROM generate_series(1, 3) g`); err != nil {
		t.Fatal(err)
	}

	sql := `WITH b AS (SELECT id FROM tt WHERE id > $1::bigint AND seen IS NULL ORDER BY id LIMIT 2)
UPDATE tt AS t SET seen = current_setting('lock_timeout') FROM b WHERE t.id = b.id RETURNING b.id`
	plan := batchPlan(sql, &BatchSpec{Key: `"id"`, KeyKind: BatchKeyInt, Size: 2})
	if _, err := New(conn, Options{LockTimeout: 1500 * time.Millisecond}).Up(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if n := scalarInt(t, conn, `SELECT count(*) FROM tt WHERE seen = '1500ms'`); n != 3 {
		t.Fatalf("rows carrying the batch lock_timeout = %d", n)
	}
}

func TestBatchUUIDAndTextKeys(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		kind    string
		setup   string
		sql     string
		wantCur string
	}{
		{
			name: "uuid",
			kind: BatchKeyUUID,
			setup: `CREATE TABLE ub (id uuid PRIMARY KEY, done bool);
				INSERT INTO ub (id) VALUES
					('00000000-0000-0000-0000-000000000001'),
					('00000000-0000-0000-0000-000000000002'),
					('00000000-0000-0000-0000-000000000003')`,
			sql: `WITH b AS (SELECT id FROM ub WHERE id > $1 AND done IS NULL ORDER BY id LIMIT 2)
UPDATE ub AS t SET done = true FROM b WHERE t.id = b.id RETURNING b.id`,
			wantCur: "00000000-0000-0000-0000-000000000003",
		},
		{
			name: "text",
			kind: BatchKeyText,
			setup: `CREATE TABLE tb (id text PRIMARY KEY, done bool);
				INSERT INTO tb (id) VALUES ('a'), ('b'), ('c')`,
			sql: `WITH b AS (SELECT id FROM tb WHERE id > $1 AND done IS NULL ORDER BY id LIMIT 2)
UPDATE tb AS t SET done = true FROM b WHERE t.id = b.id RETURNING b.id`,
			wantCur: "c",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			connect := newTestDB(t)
			conn := connect()
			if _, err := conn.Exec(ctx, tc.setup); err != nil {
				t.Fatal(err)
			}

			plan := batchPlan(tc.sql, &BatchSpec{Key: `"id"`, KeyKind: tc.kind, Size: 2})
			if _, err := New(conn, Options{}).Up(ctx, plan); err != nil {
				t.Fatal(err)
			}
			cursor, done := intentState(t, conn)
			if cursor != tc.wantCur || done != 3 {
				t.Fatalf("cursor = %q, rows_done = %d", cursor, done)
			}
		})
	}
}

func TestBatchCursorHelpers(t *testing.T) {
	t.Parallel()

	if _, err := zeroCursor("float"); err == nil || !strings.Contains(err.Error(), `unsupported batch key kind "float"`) {
		t.Fatalf("err = %v", err)
	}
	if _, err := parseCursor("float", "1"); err == nil {
		t.Fatal("want an unsupported kind error")
	}
	if _, err := parseCursor(BatchKeyInt, "nope"); err == nil || !strings.Contains(err.Error(), "parse batch cursor") {
		t.Fatalf("err = %v", err)
	}
	if _, err := parseCursor(BatchKeyUUID, "nope"); err == nil || !strings.Contains(err.Error(), "parse batch cursor") {
		t.Fatalf("err = %v", err)
	}

	zero, err := zeroCursor(BatchKeyInt)
	if err != nil || zero.num != math.MinInt64 || zero.arg() != int64(math.MinInt64) {
		t.Fatalf("int zero = %+v, err = %v", zero, err)
	}
	uid, err := parseCursor(BatchKeyUUID, "00000000-0000-0000-0000-00000000000a")
	if err != nil || uid.text() != "00000000-0000-0000-0000-00000000000a" {
		t.Fatalf("uuid cursor = %+v, err = %v", uid, err)
	}
	if !uid.above(mustZero(t, BatchKeyUUID)) || mustZero(t, BatchKeyUUID).above(uid) {
		t.Fatal("uuid ordering is wrong")
	}
	txt, err := parseCursor(BatchKeyText, "m")
	if err != nil || txt.text() != "m" || txt.arg() != "m" || !txt.above(mustZero(t, BatchKeyText)) {
		t.Fatalf("text cursor = %+v, err = %v", txt, err)
	}
	if uid.arg() == nil {
		t.Fatal("uuid arg must be bindable")
	}
}

func mustZero(t *testing.T, kind string) batchCursor {
	t.Helper()
	c, err := zeroCursor(kind)
	if err != nil {
		t.Fatal(err)
	}

	return c
}

func mockBatchSpec() *BatchSpec {
	return &BatchSpec{Key: `"id"`, KeyKind: BatchKeyInt, Size: 2}
}

const mockBatchSQL = "UPDATE t SET x = 1 RETURNING id"

func TestBatchErrorPaths(t *testing.T) {
	t.Parallel()

	prelude := func(mock pgxmock.PgxConnIface) {
		expectLock(mock)
		expectBootstrap(mock)
		expectNotApplied(mock)
		expectNewRun(mock)
	}
	expectIntent := func(mock pgxmock.PgxConnIface) {
		mock.ExpectExec("INSERT INTO godwit.journal").
			WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
			WillReturnResult(pgxmock.NewResult("INSERT", 1))
	}
	expectTimeouts := func(mock pgxmock.PgxConnIface) {
		mock.ExpectExec("SET LOCAL lock_timeout").WillReturnResult(pgxmock.NewResult("SET", 0))
		mock.ExpectExec("SET LOCAL statement_timeout").WillReturnResult(pgxmock.NewResult("SET", 0))
	}
	keyRows := func(vals ...any) *pgxmock.Rows {
		rows := pgxmock.NewRows([]string{"id"})
		for _, v := range vals {
			rows.AddRow(v)
		}

		return rows
	}

	cases := []struct {
		name    string
		spec    *BatchSpec
		setup   func(mock pgxmock.PgxConnIface)
		wantErr string
	}{
		{
			name:    "unsupported key kind",
			spec:    &BatchSpec{KeyKind: "float", Size: 2},
			setup:   func(mock pgxmock.PgxConnIface) { prelude(mock) },
			wantErr: `unsupported batch key kind "float"`,
		},
		{
			name:    "size not positive",
			spec:    &BatchSpec{KeyKind: BatchKeyInt},
			setup:   func(mock pgxmock.PgxConnIface) { prelude(mock) },
			wantErr: "batch size must be positive",
		},
		{
			name: "estimate fails",
			spec: &BatchSpec{KeyKind: BatchKeyInt, Size: 2, Estimate: "SELECT est"},
			setup: func(mock pgxmock.PgxConnIface) {
				prelude(mock)
				mock.ExpectQuery("SELECT est").WillReturnError(errBoom)
			},
			wantErr: "estimate rows",
		},
		{
			name: "intent insert fails",
			spec: mockBatchSpec(),
			setup: func(mock pgxmock.PgxConnIface) {
				prelude(mock)
				mock.ExpectExec("INSERT INTO godwit.journal").
					WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
					WillReturnError(errBoom)
			},
			wantErr: "journal intent for statement 0",
		},
		{
			name: "begin fails",
			spec: mockBatchSpec(),
			setup: func(mock pgxmock.PgxConnIface) {
				prelude(mock)
				expectIntent(mock)
				mock.ExpectBegin().WillReturnError(errBoom)
			},
			wantErr: "begin batch",
		},
		{
			name: "set timeouts fails",
			spec: mockBatchSpec(),
			setup: func(mock pgxmock.PgxConnIface) {
				prelude(mock)
				expectIntent(mock)
				mock.ExpectBegin()
				mock.ExpectExec("SET LOCAL lock_timeout").WillReturnError(errBoom)
				mock.ExpectRollback()
			},
			wantErr: "set timeouts",
		},
		{
			name: "batch statement fails",
			spec: mockBatchSpec(),
			setup: func(mock pgxmock.PgxConnIface) {
				prelude(mock)
				expectIntent(mock)
				mock.ExpectBegin()
				expectTimeouts(mock)
				mock.ExpectQuery(regexp.QuoteMeta(mockBatchSQL)).WithArgs(pgxmock.AnyArg()).WillReturnError(errBoom)
				mock.ExpectRollback()
			},
			wantErr: "exec",
		},
		{
			name: "key scan fails",
			spec: mockBatchSpec(),
			setup: func(mock pgxmock.PgxConnIface) {
				prelude(mock)
				expectIntent(mock)
				mock.ExpectBegin()
				expectTimeouts(mock)
				mock.ExpectQuery(regexp.QuoteMeta(mockBatchSQL)).WithArgs(pgxmock.AnyArg()).WillReturnRows(keyRows("no"))
				mock.ExpectRollback()
			},
			wantErr: "scan batch key",
		},
		{
			name: "rows error",
			spec: mockBatchSpec(),
			setup: func(mock pgxmock.PgxConnIface) {
				prelude(mock)
				expectIntent(mock)
				mock.ExpectBegin()
				expectTimeouts(mock)
				mock.ExpectQuery(regexp.QuoteMeta(mockBatchSQL)).WithArgs(pgxmock.AnyArg()).
					WillReturnRows(keyRows(int64(1)).CloseError(errBoom))
				mock.ExpectRollback()
			},
			wantErr: "read batch",
		},
		{
			name: "cursor update fails",
			spec: mockBatchSpec(),
			setup: func(mock pgxmock.PgxConnIface) {
				prelude(mock)
				expectIntent(mock)
				mock.ExpectBegin()
				expectTimeouts(mock)
				mock.ExpectQuery(regexp.QuoteMeta(mockBatchSQL)).WithArgs(pgxmock.AnyArg()).WillReturnRows(keyRows(int64(1)))
				mock.ExpectExec("UPDATE godwit.journal SET cursor").
					WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
					WillReturnError(errBoom)
				mock.ExpectRollback()
			},
			wantErr: "journal batch of statement 0",
		},
		{
			name: "batch commit fails",
			spec: mockBatchSpec(),
			setup: func(mock pgxmock.PgxConnIface) {
				prelude(mock)
				expectIntent(mock)
				mock.ExpectBegin()
				expectTimeouts(mock)
				mock.ExpectQuery(regexp.QuoteMeta(mockBatchSQL)).WithArgs(pgxmock.AnyArg()).WillReturnRows(keyRows(int64(1)))
				mock.ExpectExec("UPDATE godwit.journal SET cursor").
					WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
					WillReturnResult(pgxmock.NewResult("UPDATE", 1))
				mock.ExpectCommit().WillReturnError(errBoom)
				mock.ExpectRollback()
			},
			wantErr: "commit batch",
		},
		{
			name: "done insert fails",
			spec: mockBatchSpec(),
			setup: func(mock pgxmock.PgxConnIface) {
				prelude(mock)
				expectIntent(mock)
				mock.ExpectBegin()
				expectTimeouts(mock)
				mock.ExpectQuery(regexp.QuoteMeta(mockBatchSQL)).WithArgs(pgxmock.AnyArg()).WillReturnRows(keyRows(int64(1)))
				mock.ExpectExec("UPDATE godwit.journal SET cursor").
					WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
					WillReturnResult(pgxmock.NewResult("UPDATE", 1))
				mock.ExpectCommit()
				mock.ExpectExec("INSERT INTO godwit.journal").
					WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
					WillReturnError(errBoom)
			},
			wantErr: "journal done for statement 0",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mock, exec := newMockExec(t)
			tc.setup(mock)
			expectMarkFailed(mock)
			_, err := exec.Up(context.Background(), batchPlan(mockBatchSQL, tc.spec))
			wantErr(t, err, tc.wantErr)
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestReconcileBatchKind(t *testing.T) {
	t.Parallel()

	done, err := reconcile(context.Background(), nil, Statement{Verifier: VerifierBatch})
	if err != nil || done {
		t.Fatalf("batch reconcile = %v, %v", done, err)
	}
}

func TestBatchReportsProgressPerBatch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn := setupBackfill(t)()

	var partials []StatementEvent
	var final StatementEvent
	exec := New(conn, Options{}, WithObserver(func(e StatementEvent) {
		if e.Partial {
			partials = append(partials, e)

			return
		}
		final = e
	}))
	if _, err := exec.Up(ctx, intBatchPlan()); err != nil {
		t.Fatal(err)
	}
	if len(partials) != final.Batches {
		t.Fatalf("partials = %d, batches = %d", len(partials), final.Batches)
	}
	if partials[0].RowsDone != batchSize || partials[0].RowsTotal != backfillRows {
		t.Fatalf("first report = %d of %d rows", partials[0].RowsDone, partials[0].RowsTotal)
	}
	for i, p := range partials {
		if p.Migration != final.Migration || p.Index != final.Index || p.Batches != i+1 {
			t.Fatalf("report %d = %+v", i, p)
		}
		if i > 0 && p.RowsDone < partials[i-1].RowsDone {
			t.Fatalf("report %d went backwards: %d", i, p.RowsDone)
		}
	}
	if last := partials[len(partials)-1]; last.RowsDone != final.RowsDone {
		t.Fatalf("last report = %d rows, final = %d", last.RowsDone, final.RowsDone)
	}
}
