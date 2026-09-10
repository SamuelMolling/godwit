package report

import (
	"fmt"
	"strings"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/internal/engine"
)

// RevertPlanText is the plan godwit prints before it runs anything, and all a --dry-run prints.
func RevertPlanText(m *godwitv1.RevertRunResponse, pal Palette) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s will be reverted on %s, newest first", Count(len(m.Migrations), "migration"), m.Target)
	if m.Reverts != "" {
		b.WriteString(", undoing run " + m.Reverts)
	}
	if m.Forced {
		b.WriteString(", forced past a newer run")
	}
	b.WriteString(".")
	hazards := 0
	for _, pm := range m.Migrations {
		fmt.Fprintf(&b, "\n\n%s", pal.change(engine.DirectionDown, migrationID(pm)+" (down)  "+Count(len(pm.Statements), "statement")))
		for i, st := range pm.Statements {
			fmt.Fprintf(&b, "\n  %s", statementFacts(i, item{}, engine.Statement{NoTx: st.NoTx}, terminal, false))
			for _, l := range strings.Split(st.Sql, "\n") {
				b.WriteString("\n      " + l)
			}
			for _, h := range st.Hazards {
				hazards++
				fmt.Fprintf(&b, "\n      hazard %s: %s", h.Code, h.Detail)
			}
		}
	}
	for _, l := range m.DataLoss {
		fmt.Fprintf(&b, "\n\ndata loss: %s drops %s %s holding %d row(s)", l.Migration, l.Kind, l.Object, l.Rows)
	}
	fmt.Fprintf(&b, "\n\nPlan: 0 to apply, %d to revert, %d hazard(s) to acknowledge", len(m.Migrations), hazards)

	return b.String()
}

func migrationID(pm *godwitv1.PlannedMigration) string {
	return engine.MigrationID(pm.Version, pm.Name, pm.Repeatable)
}
