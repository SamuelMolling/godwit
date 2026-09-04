package engine

import (
	"errors"
	"fmt"
	"slices"
)

// ErrCheckpointGap marks a database parked below a checkpoint whose collapsed migrations are no longer in
// the directory: the checkpoint can neither run on it nor be recorded truthfully.
var ErrCheckpointGap = errors.New("checkpoint cannot be applied")

// ShapeCheckpoint decides what the newest checkpoint in plans does on a database whose newest applied
// version is newest. With no history it runs and the versions it collapses are recorded without running;
// on every other database it is itself recorded, because their schema already holds what it carries.
func ShapeCheckpoint(plans []Plan, newest int64) ([]Plan, error) {
	cp, ok := newestCheckpointOf(plans)
	if !ok {
		return plans, nil
	}
	out := slices.Clone(plans)
	if newest == 0 {
		for i := range out {
			if out[i].Migration.Collapses(cp) {
				out[i].MarkOnly = true
			}
		}

		return out, nil
	}
	if newest < cp.Through && !slices.ContainsFunc(out, func(p Plan) bool {
		return !p.Migration.Repeatable && p.Migration.Version == cp.Through
	}) {
		return nil, fmt.Errorf(
			"%w: %s collapses history through %014d, the newest applied version is %014d and %014d is not in the "+
				"migration directory; restore the migrations below the checkpoint, or baseline at it",
			ErrCheckpointGap, cp.ID(), cp.Through, newest, cp.Through)
	}
	for i := range out {
		if out[i].Migration.Checkpoint && out[i].Migration.Version <= cp.Version {
			out[i].MarkOnly = true
		}
	}

	return out, nil
}

// CheckpointNote says what a plan does with a checkpoint, for the pull-request comment.
func CheckpointNote(p Plan, collapsed int) string {
	if p.MarkOnly {
		return fmt.Sprintf("checkpoint recorded, not run: the target is already past version %014d", p.Migration.Through)
	}

	return fmt.Sprintf("checkpoint: builds the schema through version %014d and records the %d migration(s) below it",
		p.Migration.Through, collapsed)
}

// Collapsed counts the migrations in plans that cp subsumes.
func Collapsed(plans []Plan, cp Migration) int {
	n := 0
	for _, p := range plans {
		if p.Migration.Collapses(cp) {
			n++
		}
	}

	return n
}

func newestCheckpointOf(plans []Plan) (Migration, bool) {
	migs := make([]Migration, 0, len(plans))
	for _, p := range plans {
		migs = append(migs, p.Migration)
	}

	return NewestCheckpoint(migs)
}
