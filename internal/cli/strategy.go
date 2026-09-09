package cli

import (
	"fmt"
	"slices"
	"strings"

	"github.com/SamuelMolling/godwit/internal/engine"
)

const strategyHeading = "how this will run"

type runShape struct {
	statements int
	noTx       int
	batched    []engine.Statement
	asserts    int
	locked     int
	locks      []engine.Hazard
}

func (r planReport) shape() runShape {
	var s runShape
	for _, p := range r.items {
		if p.skipped {
			continue
		}
		for _, st := range p.Statements {
			s.statements++
			switch {
			case st.Assert != nil:
				s.asserts++
			case st.Batch != nil:
				s.batched = append(s.batched, st)
			case st.NoTx:
				s.noTx++
			}
			s.takeLocks(st)
		}
	}

	return s
}

func (s *runShape) takeLocks(st engine.Statement) {
	before := len(s.locks)
	for _, h := range st.Hazards {
		same := func(o engine.Hazard) bool { return o.Code == h.Code && o.Object == h.Object }
		if h.HoldsLock() && !slices.ContainsFunc(s.locks, same) {
			s.locks = append(s.locks, h)
		}
	}
	if len(s.locks) > before {
		s.locked++
	}
}

func (r planReport) strategy(m markup) []string {
	if !r.live {
		return nil
	}
	s := r.shape()
	if s.statements == 0 {
		return nil
	}
	out := []string{s.order()}
	if l := s.outsideTx(m); l != "" {
		out = append(out, l)
	}
	if l := s.batches(); l != "" {
		out = append(out, l)
	}
	if l := s.checks(); l != "" {
		out = append(out, l)
	}
	out = append(out, s.lockLine(m))
	if r.pauses() {
		out = append(out, "The run does not go through in one go: with "+m.code("expand-contract")+" it stops after the"+
			" expand half, waits in "+m.code("awaiting_contract")+" holding no lock and no transaction, and runs the"+
			" contract half only once "+m.code("godwit confirm")+" releases it.")
	}

	return append(out, s.onFailure())
}

func (s runShape) order() string {
	if s.statements == 1 {
		return "The one statement commits with its own journal row on the target, so a run that fails or is killed" +
			" part way is resumed at it rather than replaying the migration from its first line."
	}

	return fmt.Sprintf("%d statements run one at a time, in the order this report lists them, each committing with"+
		" its own journal row on the target. A run that fails or is killed part way is resumed at the first"+
		" statement that never committed, rather than replaying the migration from its first line.", s.statements)
}

func (s runShape) outsideTx(m markup) string {
	if s.noTx == 0 {
		return "Every statement here runs inside a transaction, so one that fails leaves nothing of itself behind."
	}

	return fmt.Sprintf("%d of them cannot run inside a transaction, because PostgreSQL refuses %s there: an index"+
		" built or dropped %s, a %s, a concurrent matview refresh. PostgreSQL cannot roll %s back, so godwit writes"+
		" an intent row before each one and, when a run comes back to it, asks the database what the statement left"+
		" rather than running it a second time.",
		s.noTx, agree(s.noTx, "it", "them"), m.code("CONCURRENTLY"), m.code("VACUUM"),
		agree(s.noTx, "it", "them"))
}

func (s runShape) batches() string {
	if len(s.batched) == 0 {
		return ""
	}
	var walks []string
	for _, st := range s.batched {
		b := st.Batch
		if w := fmt.Sprintf("by %s in batches of %d rows%s", b.Key, b.Size, pauseSuffix(b.Pause)); !slices.Contains(walks, w) {
			walks = append(walks, w)
		}
	}

	return fmt.Sprintf("%s %s not run as one statement at all: godwit walks the table %s and commits each batch, so"+
		" no single transaction holds a lock or a snapshot over the whole table, and the cursor it journals is where"+
		" a killed run picks the backfill up.",
		count(len(s.batched), "statement"), agree(len(s.batched), "does", "do"), strings.Join(walks, "; "))
}

func (s runShape) checks() string {
	if s.asserts == 0 {
		return ""
	}

	return fmt.Sprintf("%s %s nothing: they are conditions the migration declared, evaluated where they stand, and"+
		" the run stops there if one does not hold.",
		count(s.asserts, "statement"), agree(s.asserts, "changes", "change"))
}

func (s runShape) lockLine(m markup) string {
	base := "Every statement sets the target's " + m.code("lock_timeout") + " before it runs, so one that cannot take" +
		" its lock gives up and fails the run instead of queueing in front of every query behind it."
	if len(s.locks) == 0 {
		return base
	}
	parts := make([]string, 0, len(s.locks))
	for _, h := range s.locks {
		part := h.Short() + " (" + h.Code + ")"
		if h.Object != "" {
			part = m.code(h.Object) + ": " + part
		}
		parts = append(parts, part)
	}

	return fmt.Sprintf("%s %s a lock the rest of the application queues behind while %s: %s. How long that matters is"+
		" how long the statement itself takes, which grows with the table. %s",
		count(s.locked, "statement"), agree(s.locked, "holds", "hold"),
		agree(s.locked, "it runs", "they run"), strings.Join(parts, "; "), base)
}

func agree(n int, singular, plural string) string {
	if n == 1 {
		return singular
	}

	return plural
}

func (s runShape) onFailure() string {
	if s.noTx == 0 {
		return "If a statement fails, that statement is rolled back and the run stops there; the statements before it" +
			" stay committed and the migrations already finished stay applied. Fix the migration and run it again."
	}

	return "If a statement fails, the run stops there; what committed before it stays committed. An index built" +
		" CONCURRENTLY that failed leaves an INVALID index behind, and the next run of this plan finds it, drops it" +
		" and builds it again."
}
