package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/internal/controlplane"
	"github.com/SamuelMolling/godwit/internal/engine"
)

func journalRequest(target, migration string) *connect.Request[godwitv1.GetMigrationJournalRequest] {
	return connect.NewRequest(&godwitv1.GetMigrationJournalRequest{Target: target, Migration: migration})
}

func TestGetMigrationJournal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := &Server{}

	if _, err := s.GetMigrationJournal(ctx, journalRequest("app", "20260901120000_t")); connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("without an inspector: %v", err)
	}
	s.Inspector = stubInspector{}
	for _, req := range []*connect.Request[godwitv1.GetMigrationJournalRequest]{
		journalRequest("", "20260901120000_t"), journalRequest("app", ""),
	} {
		if _, err := s.GetMigrationJournal(ctx, req); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Fatalf("%+v: %v", req.Msg, err)
		}
	}
	s.Inspector = stubInspector{err: errors.New("down")}
	if _, err := s.GetMigrationJournal(ctx, journalRequest("app", "20260901120000_t")); err == nil {
		t.Fatal("an inspector error must surface")
	}

	applied := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	done := applied.Add(time.Second)
	s.Inspector = stubInspector{journal: controlplane.MigrationJournal{
		Target: "app", Migration: "20260901120000_t", RunID: "r1", AppliedAt: applied, BodiesSwept: true,
		Attempts: []controlplane.MigrationAttempt{{
			JournalRun: engine.JournalRun{
				ID: "j1", Direction: "up", State: "succeeded", StmtCount: 1,
				StartedAt: applied, FinishedAt: &done,
			},
			Statements: []controlplane.MigrationStatement{{
				JournalStatement: engine.JournalStatement{Index: 0, Hash: "h", IntentAt: &applied, DoneAt: &done, RowsDone: 5},
				SQL:              "SELECT 1", NoTx: true, Verifier: "rerun", Outcome: controlplane.OutcomeDone,
			}},
		}},
	}}
	resp, err := s.GetMigrationJournal(ctx, journalRequest("app", "20260901120000_t"))
	if err != nil {
		t.Fatal(err)
	}
	m := resp.Msg
	if m.Target != "app" || m.RunId != "r1" || !m.BodiesSwept || m.AppliedAt.AsTime() != applied || len(m.Runs) != 1 {
		t.Fatalf("response = %+v", m)
	}
	run := m.Runs[0]
	if run.Id != "j1" || run.State != "succeeded" || run.StatementCount != 1 || run.FinishedAt.AsTime() != done {
		t.Fatalf("run = %+v", run)
	}
	st := run.Statements[0]
	if st.Sql != "SELECT 1" || !st.NoTx || st.Verifier != "rerun" || st.RowsDone != 5 ||
		st.IntentAt.AsTime() != applied || st.DoneAt.AsTime() != done {
		t.Fatalf("statement = %+v", st)
	}
}

func TestGetMigrationJournalWithoutTimestamps(t *testing.T) {
	t.Parallel()

	s := &Server{Inspector: stubInspector{journal: controlplane.MigrationJournal{
		Attempts: []controlplane.MigrationAttempt{{
			JournalRun: engine.JournalRun{ID: "j1", State: "running", StmtCount: 1},
			Statements: []controlplane.MigrationStatement{{Outcome: controlplane.OutcomeNotReached}},
		}},
	}}}
	resp, err := s.GetMigrationJournal(context.Background(), journalRequest("app", "20260901120000_t"))
	if err != nil {
		t.Fatal(err)
	}
	m := resp.Msg
	if m.AppliedAt != nil || m.Runs[0].FinishedAt != nil || m.Runs[0].Statements[0].DoneAt != nil ||
		m.Runs[0].Statements[0].IntentAt != nil {
		t.Fatalf("an unfinished attempt must carry no stamps: %+v", m)
	}
}
