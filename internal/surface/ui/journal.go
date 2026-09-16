package ui

import (
	"net/http"

	"google.golang.org/protobuf/types/known/timestamppb"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/gen/godwit/v1/godwitv1connect"
	"github.com/SamuelMolling/godwit/internal/controlplane"
	"github.com/SamuelMolling/godwit/internal/engine"
)

type journalStmt struct {
	Index    int32
	SQL      string
	Hash     string
	Outcome  string
	Tone     string
	Kind     string
	NoTx     bool
	Verifier string
	IntentAt *timestamppb.Timestamp
	DoneAt   *timestamppb.Timestamp
	Took     string
	Rows     string
	Cursor   string
	Error    string
}

type journalAttempt struct {
	ID         string
	Direction  string
	State      string
	Tone       string
	Error      string
	StartedAt  *timestamppb.Timestamp
	FinishedAt *timestamppb.Timestamp
	Count      int
	Done       int
	Placed     bool
	Stmts      []journalStmt
}

type journalData struct {
	Migration    string
	Attempts     []journalAttempt
	Newest       *journalAttempt
	RunID        string
	AppliedAt    *timestamppb.Timestamp
	Adopted      bool
	RecordedOnly bool
	Unreachable  string
	BodiesSwept  bool
}

func (h *Handler) migration(w http.ResponseWriter, r *http.Request) {
	ctx, name := r.Context(), r.PathValue("name")
	p, err := h.frame(ctx, r, "targets")
	if err != nil {
		h.fail(w, p, err)

		return
	}
	p.Target = name
	resp, err := call(ctx, godwitv1connect.GodwitServiceGetMigrationJournalProcedure,
		&godwitv1.GetMigrationJournalRequest{Target: name, Migration: r.PathValue("migration")}, h.svc.GetMigrationJournal)
	if err != nil {
		h.fail(w, p, err)

		return
	}
	p.Journal = journalOf(resp)
	h.render(w, http.StatusOK, "migration.html", p)
}

func journalOf(resp *godwitv1.GetMigrationJournalResponse) *journalData {
	d := &journalData{
		Migration: resp.Migration, RunID: resp.RunId, AppliedAt: resp.AppliedAt, Adopted: resp.Adopted,
		RecordedOnly: resp.RecordedOnly, Unreachable: resp.Unreachable, BodiesSwept: resp.BodiesSwept,
	}
	for _, run := range resp.Runs {
		d.Attempts = append(d.Attempts, attemptOf(run))
	}
	if len(d.Attempts) > 0 {
		d.Newest = &d.Attempts[0]
	}

	return d
}

func attemptOf(run *godwitv1.MigrationJournalRun) journalAttempt {
	a := journalAttempt{
		ID: run.Id, Direction: run.Direction, State: run.State, Tone: attemptTone(run.State),
		Error: run.Error, StartedAt: run.StartedAt, FinishedAt: run.FinishedAt, Count: len(run.Statements),
	}
	previous := run.StartedAt
	for _, st := range run.Statements {
		s := stmtOf(st, previous)
		if st.DoneAt != nil {
			previous, a.Done = st.DoneAt, a.Done+1
		}
		if s.Outcome == controlplane.OutcomeFailed {
			s.Error, a.Placed = run.Error, true
		}
		a.Stmts = append(a.Stmts, s)
	}

	return a
}

func stmtOf(st *godwitv1.JournalStatement, previous *timestamppb.Timestamp) journalStmt {
	s := journalStmt{
		Index: st.Index, SQL: st.Sql, Hash: st.SqlHash, Outcome: st.Outcome, Tone: stmtTone(st.Outcome),
		Kind: "tx", NoTx: st.NoTx, Verifier: st.Verifier, IntentAt: st.IntentAt, DoneAt: st.DoneAt, Cursor: st.Cursor,
	}
	switch {
	case st.Verifier == string(engine.VerifierBatch):
		s.Kind = "batch"
	case st.NoTx:
		s.Kind = "no-tx"
	}
	if st.IntentAt != nil {
		previous = st.IntentAt
	}
	if st.DoneAt != nil && previous != nil {
		s.Took = elapsed(st.DoneAt.AsTime().Sub(previous.AsTime()))
	}
	if st.RowsTotal > 0 {
		s.Rows = thousands(st.RowsDone) + " of " + thousands(st.RowsTotal) + " rows"
	} else if st.RowsDone > 0 {
		s.Rows = thousands(st.RowsDone) + " rows"
	}

	return s
}

func attemptTone(state string) string {
	switch state {
	case "succeeded":
		return "succeeded"
	case "failed":
		return "failed"
	default:
		return "running"
	}
}

func stmtTone(outcome string) string {
	switch outcome {
	case controlplane.OutcomeDone:
		return "ok"
	case controlplane.OutcomeFailed:
		return "bad"
	case controlplane.OutcomeInFlight:
		return "run"
	default:
		return ""
	}
}
