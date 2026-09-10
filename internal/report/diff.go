package report

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/internal/engine"
)

type diffJSON struct {
	Target     string           `json:"target"`
	Changed    bool             `json:"changed"`
	UpSQL      string           `json:"up_sql"`
	DownSQL    string           `json:"down_sql"`
	Statements []statementJSON  `json:"statements"`
	Drift      string           `json:"drift,omitempty"`
	Observed   *planObservation `json:"observed,omitempty"`
	Files      []string         `json:"files"`

	RepeatableObjects []string `json:"repeatable_objects"`
}

// WriteDiff reports the migration a diff generated, and the files it was written to.
func WriteDiff(w io.Writer, m *godwitv1.DiffResponse, files []string, schema string, asJSON bool) {
	if asJSON {
		writeDiffJSON(w, m, files)

		return
	}
	if len(m.RepeatableObjects) > 0 {
		fmt.Fprintf(w, "declared by repeatable migrations, so the desired schema keeps them: %s\n",
			strings.Join(m.RepeatableObjects, ", "))
	}
	if m.UpSql == "" {
		fmt.Fprintf(w, "no changes: %s already matches %s\n", m.Target, schema)

		return
	}
	fmt.Fprint(w, Plan{drift: m.Drift}.driftBlock("drift (the target's live schema, not its history, is the starting point):", "  ", "", "", Colors(w)))
	fmt.Fprintf(w, "%s -> %s: %d statement(s)\n", m.Target, schema, len(m.Statements))
	for i, st := range m.Statements {
		fmt.Fprintf(w, "  [%d] %-5s %s\n", i, statementMode(engine.Statement{NoTx: st.NoTx}), firstLine(st.Sql))
		for _, h := range st.Hazards {
			fmt.Fprintf(w, "        hazard %s: %s\n", h.Code, h.Detail)
			WriteRecipeText(w, "          ", h.Recipe)
		}
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "-- up")
	fmt.Fprintln(w, m.UpSql)
	fmt.Fprintln(w, "-- down")
	fmt.Fprintln(w, m.DownSql)
	for _, f := range files {
		fmt.Fprintln(w, "wrote", f)
	}
}

func writeDiffJSON(w io.Writer, m *godwitv1.DiffResponse, files []string) {
	out := diffJSON{
		Target: m.Target, Changed: m.UpSql != "", UpSQL: m.UpSql, DownSQL: m.DownSql,
		Statements: []statementJSON{}, Drift: m.Drift, Files: append([]string{}, files...),
		RepeatableObjects: append([]string{}, m.RepeatableObjects...),
	}
	for _, st := range m.Statements {
		hazards := make([]hazardJSON, 0, len(st.Hazards))
		for _, h := range st.Hazards {
			hazards = append(hazards, hazardJSON{Code: h.Code, Detail: h.Detail, Recipe: h.Recipe})
		}
		out.Statements = append(out.Statements, statementJSON{SQL: st.Sql, Mode: statementMode(engine.Statement{NoTx: st.NoTx}), Hazards: hazards})
	}
	out.Observed = observationFromProto(m.Observed)
	body, _ := json.MarshalIndent(out, "", "  ")
	fmt.Fprintln(w, string(body))
}
