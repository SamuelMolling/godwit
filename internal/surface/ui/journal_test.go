package ui

import (
	"context"
	"html/template"
	"net/http"
	"testing"
	"time"

	"connectrpc.com/connect"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/internal/controlplane"
)

const (
	createOrders = `CREATE TABLE public.orders (id bigserial PRIMARY KEY, customer_id bigint NOT NULL REFERENCES public.customers (id), placed_at timestamptz NOT NULL DEFAULT now(), total_cents bigint NOT NULL DEFAULT 0, archived boolean NOT NULL DEFAULT false)`
	ordersIndex  = `CREATE INDEX CONCURRENTLY orders_customer_placed_idx ON public.orders (customer_id, placed_at DESC) WHERE NOT archived`
	backfillSQL  = `WITH b AS (SELECT "id" FROM "public"."orders" WHERE "id" > $1::bigint AND total_cents IS DISTINCT FROM total::bigint ORDER BY "id" LIMIT 5000) UPDATE "public"."orders" AS t SET "total_cents" = t."total"::bigint FROM b WHERE t."id" = b."id" RETURNING b."id"`
	lockError    = `sql: statement 1 of 20260901120000_orders (up): exec: ERROR: canceling statement due to lock timeout (SQLSTATE 55P03)`
)

func journalFixture() *godwitv1.GetMigrationJournalResponse {
	return &godwitv1.GetMigrationJournalResponse{
		Target: "app", Migration: "20260901120000_orders", RunId: "7f1c9a2e-0000-4000-8000-00000000abcd",
		AppliedAt: at(90 * time.Minute),
		Runs: []*godwitv1.MigrationJournalRun{{
			Id: "b2c3d4e5-0000-4000-8000-0000000012ab", Direction: "up", State: "succeeded", StatementCount: 3,
			StartedAt: at(92 * time.Minute), FinishedAt: at(90 * time.Minute),
			Statements: []*godwitv1.JournalStatement{
				{
					Index: 0, Sql: createOrders, SqlHash: "9f2b7c1d4e5a", Outcome: controlplane.OutcomeDone,
					DoneAt: at(91*time.Minute + 59*time.Second),
				},
				{
					Index: 1, Sql: ordersIndex, SqlHash: "1a2b3c4d5e6f", Outcome: controlplane.OutcomeDone, NoTx: true,
					Verifier: "create_index_concurrently",
					IntentAt: at(91*time.Minute + 59*time.Second), DoneAt: at(90*time.Minute + 42*time.Second),
				},
				{
					Index: 2, Sql: backfillSQL, SqlHash: "abcdef123456", Outcome: controlplane.OutcomeDone,
					Verifier: "batch", IntentAt: at(90*time.Minute + 42*time.Second), DoneAt: at(90 * time.Minute),
					RowsDone: 1200000, RowsTotal: 1200000, Cursor: "1200000",
				},
			},
		}},
	}
}

func failedJournal() *godwitv1.GetMigrationJournalResponse {
	j := journalFixture()
	run := j.Runs[0]
	run.State, run.Error = "failed", lockError
	run.Statements[1].Outcome, run.Statements[1].DoneAt = controlplane.OutcomeFailed, nil
	run.Statements[2].Outcome = controlplane.OutcomeNotReached
	run.Statements[2].IntentAt, run.Statements[2].DoneAt = nil, nil
	run.Statements[2].RowsDone, run.Statements[2].RowsTotal, run.Statements[2].Cursor = 0, 0, ""

	return j
}

type journalFail struct{ *stub }

func (journalFail) GetMigrationJournal(context.Context, *connect.Request[godwitv1.GetMigrationJournalRequest]) (*connect.Response[godwitv1.GetMigrationJournalResponse], error) {
	return nil, errBoom
}

func TestMigrationPageShowsEveryStatement(t *testing.T) {
	t.Parallel()
	s := fixture()
	s.journal = journalFixture()
	h := newUI(s, Config{Replica: "godwit-0"})

	rec := do(h, http.MethodGet, "/ui/targets/app/migrations/20260901120000_orders", nil)
	want(t, rec, http.StatusOK,
		"20260901120000_orders", createOrders, ordersIndex, template.HTMLEscapeString(backfillSQL),
		`<a href="/ui/targets/app">app</a>`,
		`href="/ui/runs/7f1c9a2e-0000-4000-8000-00000000abcd"`,
		"verifier create_index_concurrently", "· batch ·",
		"1,200,000 of 1,200,000 rows", "cursor", "no-tx", "3 of 3 statements journalled done",
		`<span class="pill succeeded lg">succeeded</span>`)
	absent(t, rec, "hash only", "No text")
}

func TestMigrationPageLeadsWithTheFailedStatement(t *testing.T) {
	t.Parallel()
	s := fixture()
	s.journal = failedJournal()
	h := newUI(s, Config{Replica: "godwit-0"})

	rec := do(h, http.MethodGet, "/ui/targets/app/migrations/20260901120000_orders", nil)
	want(t, rec, http.StatusOK,
		lockError, ordersIndex, `<i class="bad"></i>`, "· failed", "· not reached",
		"1 of 3 statements journalled done", `<span class="pill failed lg">failed</span>`)
}

func TestMigrationPageWithoutAJournal(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		resp *godwitv1.GetMigrationJournalResponse
		want string
	}{
		{"unreachable", &godwitv1.GetMigrationJournalResponse{Unreachable: "vault: permission denied"}, "vault: permission denied"},
		{"adopted", &godwitv1.GetMigrationJournalResponse{RunId: "r1", Adopted: true}, "Nothing ran here"},
		{"recorded only", &godwitv1.GetMigrationJournalResponse{RunId: "r1", RecordedOnly: true}, "Recorded without executing"},
		{"nothing", &godwitv1.GetMigrationJournalResponse{}, "No journal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := fixture()
			s.journal = tc.resp
			h := newUI(s, Config{Replica: "godwit-0"})
			want(t, do(h, http.MethodGet, "/ui/targets/app/migrations/20260901120000_orders", nil), http.StatusOK, tc.want)
		})
	}
}

func TestMigrationPageWithoutStatementText(t *testing.T) {
	t.Parallel()
	s := fixture()
	s.journal = journalFixture()
	s.journal.BodiesSwept = true
	for _, st := range s.journal.Runs[0].Statements {
		st.Sql = ""
	}
	h := newUI(s, Config{Replica: "godwit-0"})

	rec := do(h, http.MethodGet, "/ui/targets/app/migrations/20260901120000_orders", nil)
	want(t, rec, http.StatusOK, "No text:", "9f2b7c1d", "hash only")
	absent(t, rec, createOrders)
}

func TestMigrationPageErrors(t *testing.T) {
	t.Parallel()
	s := fixture()
	s.err = errBoom
	h := newUI(s, Config{Replica: "godwit-0"})

	if rec := do(h, http.MethodGet, "/ui/targets/app/migrations/20260901120000_orders", nil); rec.Code != http.StatusBadGateway {
		t.Fatalf("code = %d", rec.Code)
	}
	h = newUI(&journalFail{stub: fixture()}, Config{Replica: "godwit-0"})
	if rec := do(h, http.MethodGet, "/ui/targets/app/migrations/20260901120000_orders", nil); rec.Code != http.StatusBadGateway {
		t.Fatalf("code = %d", rec.Code)
	}
}

func TestTargetPageLinksEveryAppliedMigration(t *testing.T) {
	t.Parallel()
	h := newUI(fixture(), Config{Replica: "godwit-0"})

	want(t, do(h, http.MethodGet, "/ui/targets/app", nil), http.StatusOK,
		`href="/ui/targets/app/migrations/`)
}

func TestRowsWithoutAnEstimate(t *testing.T) {
	t.Parallel()

	got := stmtOf(&godwitv1.JournalStatement{RowsDone: 4200}, nil)
	if got.Rows != "4,200 rows" || got.Took != "" {
		t.Fatalf("statement = %+v", got)
	}
	if tone := stmtTone(controlplane.OutcomeNotReached); tone != "" {
		t.Fatalf("tone = %q", tone)
	}
	if tone := stmtTone(controlplane.OutcomeInFlight); tone != "run" {
		t.Fatalf("tone = %q", tone)
	}
	if tone := attemptTone("running"); tone != "running" {
		t.Fatalf("tone = %q", tone)
	}
}
