package controlplane

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/SamuelMolling/godwit/internal/engine"
)

// Outcomes a journalled statement is reported under.
const (
	OutcomeDone       = "done"
	OutcomeFailed     = "failed"
	OutcomeInFlight   = "in flight"
	OutcomeNotReached = "not reached"
)

// MigrationStatement is one statement of an attempt: what the target journalled, and the SQL the run executed.
type MigrationStatement struct {
	engine.JournalStatement
	SQL      string
	NoTx     bool
	Verifier string
	Outcome  string
}

// MigrationAttempt is one godwit.runs row on the target with its statements.
type MigrationAttempt struct {
	engine.JournalRun
	Statements []MigrationStatement
}

// MigrationJournal is what one target recorded about one migration, from its own journal and the control plane's ledger.
type MigrationJournal struct {
	Target       string
	Migration    string
	Attempts     []MigrationAttempt
	RunID        string
	AppliedAt    time.Time
	Adopted      bool
	RecordedOnly bool
	Unreachable  string
	BodiesSwept  bool
}

// LedgerEntry is what the control plane holds about one migration on one target, with the file bodies of the run that carried it.
type LedgerEntry struct {
	RunID     string
	AppliedAt time.Time
	Applied   bool
	Adopted   bool
	Expansion *Expansion
	UpSQL     string
	DownSQL   string
}

// CarriedBy returns the run that applied the migration on the target, or the newest that carried its file when none did.
func (s *Store) CarriedBy(ctx context.Context, target, migration string) (LedgerEntry, error) {
	var e LedgerEntry
	var appliedAt *time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT r.id::text, a.applied_at, a.run_id IS NOT NULL, coalesce(a.adopted, false),
			coalesce(a.expansion, r.expansions -> $2), u.body, coalesce(d.body, '')
		FROM cp_runs r
		JOIN cp_run_files u ON u.run_id = r.id AND u.name = $2 || '.up.sql'
		LEFT JOIN cp_run_files d ON d.run_id = r.id AND d.name = $2 || '.down.sql'
		LEFT JOIN cp_run_applied a ON a.run_id = r.id AND a.migration = $2
		WHERE r.target = $1
		ORDER BY (a.run_id IS NOT NULL) DESC, r.seq DESC LIMIT 1`, target, migration).
		Scan(&e.RunID, &appliedAt, &e.Applied, &e.Adopted, &e.Expansion, &e.UpSQL, &e.DownSQL)
	if errors.Is(err, pgx.ErrNoRows) {
		return LedgerEntry{}, ErrNotFound
	}
	if err != nil {
		return LedgerEntry{}, fmt.Errorf("find the run that carried %s: %w", migration, err)
	}
	if appliedAt != nil {
		e.AppliedAt = *appliedAt
	}

	return e, nil
}

// Journal reads one migration's statement journal off the target and joins it to the SQL the applying run stored.
func (i *Inspector) Journal(ctx context.Context, target, migration string) (MigrationJournal, error) {
	provider, config, err := i.sched.store.Target(ctx, target)
	if err != nil {
		return MigrationJournal{}, err
	}
	out := MigrationJournal{Target: target, Migration: migration}
	entry, err := i.sched.store.CarriedBy(ctx, target, migration)
	switch {
	case errors.Is(err, ErrNotFound):
	case err != nil:
		return MigrationJournal{}, err
	default:
		out.RunID, out.AppliedAt, out.Adopted = entry.RunID, entry.AppliedAt, entry.Adopted
	}
	tg, err := i.sched.resolve(ctx, target, provider, config)
	if err != nil {
		out.Unreachable = err.Error()

		return out, nil
	}
	version, repeatable := migrationKey(migration)
	runs, err := i.sched.engine.Journal(ctx, tg.dsn, version, repeatable)
	if err != nil {
		return MigrationJournal{}, err
	}
	out.RecordedOnly = len(runs) == 0 && entry.Applied && !out.Adopted
	out.Attempts, out.BodiesSwept = attemptsOf(runs, migration, entry)

	return out, nil
}

func migrationKey(migration string) (int64, string) {
	if name, ok := strings.CutPrefix(migration, engine.RepeatablePrefix); ok {
		return 0, name
	}
	version, _ := versionOf(migration)

	return version, ""
}

func attemptsOf(runs []engine.JournalRun, migration string, entry LedgerEntry) ([]MigrationAttempt, bool) {
	up, down := executedStatements(migration, entry)
	swept := false
	out := make([]MigrationAttempt, 0, len(runs))
	for _, r := range runs {
		plan := up
		if r.Direction == string(engine.DirectionDown) {
			plan = down
		}
		swept = swept || len(plan) == 0
		out = append(out, MigrationAttempt{JournalRun: r, Statements: statementsOf(r, plan)})
	}

	return out, swept
}

// executedStatements rebuilds both sides from the bodies the applying run stored, frozen expansion included, so a statement's hash matches the journal's.
func executedStatements(migration string, entry LedgerEntry) (up, down []engine.Statement) {
	if entry.UpSQL == "" {
		return nil, nil
	}
	files := map[string]string{migration + ".up.sql": entry.UpSQL}
	if entry.DownSQL != "" {
		files[migration+".down.sql"] = entry.DownSQL
	}
	exps := map[string]Expansion{}
	if entry.Expansion != nil {
		exps[migration] = *entry.Expansion
	}

	return sideOf(files, engine.DirectionUp, exps), sideOf(files, engine.DirectionDown, exps)
}

func sideOf(files map[string]string, dir engine.Direction, exps map[string]Expansion) []engine.Statement {
	plans, err := PlansFromFiles(files, dir)
	if err != nil {
		return nil
	}
	if dir == engine.DirectionUp {
		if plans, err = ExpandUp(plans, exps); err != nil {
			return nil
		}
	}
	if len(plans) == 0 {
		return nil
	}

	return plans[0].Statements
}

func statementsOf(r engine.JournalRun, plan []engine.Statement) []MigrationStatement {
	journalled := map[int]engine.JournalStatement{}
	lastDone := -1
	for _, st := range r.Statements {
		journalled[st.Index] = st
		if st.DoneAt != nil && st.Index > lastDone {
			lastDone = st.Index
		}
	}
	out := make([]MigrationStatement, 0, max(r.StmtCount, len(r.Statements)))
	for i := range max(r.StmtCount, len(r.Statements)) {
		st := MigrationStatement{JournalStatement: journalled[i], Outcome: outcomeOf(r, i, lastDone)}
		st.Index = i
		if i < len(plan) && (st.Hash == "" || st.Hash == plan[i].Hash) {
			st.SQL, st.NoTx, st.Verifier = plan[i].SQL, plan[i].NoTx, string(plan[i].Verifier)
		}
		out = append(out, st)
	}

	return out
}

func outcomeOf(r engine.JournalRun, i, lastDone int) string {
	switch {
	case i <= lastDone:
		return OutcomeDone
	case i == lastDone+1 && r.State == "failed":
		return OutcomeFailed
	case i == lastDone+1 && r.State == "running":
		return OutcomeInFlight
	default:
		return OutcomeNotReached
	}
}
