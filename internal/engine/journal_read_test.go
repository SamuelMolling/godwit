package engine

import (
	"context"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

func nowStamp() time.Time { return time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC) }

func seedJournal(t *testing.T, conn DB) {
	t.Helper()
	ctx := context.Background()
	if err := bootstrap(ctx, conn); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `
		INSERT INTO godwit.runs (id, version, repeatable, checksum, direction, state, stmt_count, error, finished_at) VALUES
			('11111111-0000-0000-0000-000000000001', 20260901120000, NULL, 'c1', 'up', 'succeeded', 2, NULL, now()),
			('11111111-0000-0000-0000-000000000002', NULL, 'views', 'c2', 'up', 'failed', 1, 'boom', now());
		INSERT INTO godwit.journal (run_id, stmt_idx, state, sql_hash, cursor, rows_done, rows_total) VALUES
			('11111111-0000-0000-0000-000000000001', 0, 'done', 'h0', NULL, 0, NULL),
			('11111111-0000-0000-0000-000000000001', 1, 'intent', 'h1', '4200', 4200, 9000),
			('11111111-0000-0000-0000-000000000001', 1, 'done', 'h1', NULL, 0, NULL)`); err != nil {
		t.Fatal(err)
	}
}

func TestListJournal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn := newTestDB(t)()

	got, err := ListJournal(ctx, conn, 20260901120000, "")
	if err != nil || got != nil {
		t.Fatalf("fresh database: %+v, err = %v", got, err)
	}
	seedJournal(t, conn)

	got, err = ListJournal(ctx, conn, 20260901120000, "")
	if err != nil || len(got) != 1 {
		t.Fatalf("versioned = %+v, err = %v", got, err)
	}
	run := got[0]
	if run.Direction != "up" || run.State != "succeeded" || run.Error != "" || run.StmtCount != 2 || run.FinishedAt == nil {
		t.Fatalf("run = %+v", run)
	}
	if len(run.Statements) != 2 {
		t.Fatalf("statements = %+v", run.Statements)
	}
	if st := run.Statements[0]; st.Index != 0 || st.Hash != "h0" || st.DoneAt == nil || st.IntentAt != nil {
		t.Fatalf("statement 0 = %+v", st)
	}
	st := run.Statements[1]
	if st.IntentAt == nil || st.DoneAt == nil || st.Cursor != "4200" || st.RowsDone != 4200 || st.RowsTotal != 9000 {
		t.Fatalf("statement 1 = %+v", st)
	}

	reps, err := ListJournal(ctx, conn, 0, "views")
	if err != nil || len(reps) != 1 || reps[0].State != "failed" || reps[0].Error != "boom" || len(reps[0].Statements) != 0 {
		t.Fatalf("repeatable = %+v, err = %v", reps, err)
	}
}

func TestListJournalErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	probe, _ := newMockExec(t)
	probe.ExpectQuery("to_regclass").WillReturnError(errBoom)
	_, err := ListJournal(ctx, probe, 1, "")
	wantErr(t, err, "probe godwit schema")

	runs, _ := newMockExec(t)
	runs.ExpectQuery("to_regclass").WithArgs("godwit.journal").WillReturnRows(pgxmock.NewRows([]string{"present"}).AddRow(true))
	runs.ExpectQuery("FROM godwit.runs").WithArgs(int64(1)).WillReturnError(errBoom)
	_, err = ListJournal(ctx, runs, 1, "")
	wantErr(t, err, "list journalled runs")

	scan, _ := newMockExec(t)
	scan.ExpectQuery("to_regclass").WithArgs("godwit.journal").WillReturnRows(pgxmock.NewRows([]string{"present"}).AddRow(true))
	scan.ExpectQuery("FROM godwit.runs").WithArgs(int64(1)).WillReturnRows(pgxmock.NewRows(
		[]string{"id", "direction", "state", "error", "stmt_count", "started_at", "finished_at"}).
		AddRow("r", "up", "succeeded", "", 1, nowStamp(), (*string)(nil)).RowError(0, errBoom))
	_, err = ListJournal(ctx, scan, 1, "")
	wantErr(t, err, "read journalled runs")

	stmts, _ := newMockExec(t)
	stmts.ExpectQuery("to_regclass").WithArgs("godwit.journal").WillReturnRows(pgxmock.NewRows([]string{"present"}).AddRow(true))
	stmts.ExpectQuery("FROM godwit.runs").WithArgs(int64(1)).WillReturnRows(pgxmock.NewRows(
		[]string{"id", "direction", "state", "error", "stmt_count", "started_at", "finished_at"}).
		AddRow("r", "up", "succeeded", "", 1, nowStamp(), (*time.Time)(nil)))
	stmts.ExpectQuery("FROM godwit.journal").WithArgs("r").WillReturnError(errBoom)
	_, err = ListJournal(ctx, stmts, 1, "")
	wantErr(t, err, "list journal")
}

func TestListJournalStatementErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	query, _ := newMockExec(t)
	query.ExpectQuery("FROM godwit.journal").WithArgs("r").WillReturnError(errBoom)
	_, err := readJournalStatements(ctx, query, "r")
	wantErr(t, err, "list journal")

	scan, _ := newMockExec(t)
	scan.ExpectQuery("FROM godwit.journal").WithArgs("r").WillReturnRows(pgxmock.NewRows(
		[]string{"stmt_idx", "state", "sql_hash", "recorded_at", "cursor", "rows_done", "rows_total"}).
		AddRow(0, "done", "h", nowStamp(), "", int64(0), int64(0)).RowError(0, errBoom))
	_, err = readJournalStatements(ctx, scan, "r")
	wantErr(t, err, "read journal")
}
