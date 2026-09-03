package ui

import (
	"net/http"
	"sort"
	"strconv"
	"strings"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/gen/godwit/v1/godwitv1connect"
)

type plansData struct {
	ID      string
	Pruned  bool
	Tone    string
	State   string
	Filters []planFilter
	Rows    []planRow
	Detail  []planMigration
}

type planRow struct {
	*godwitv1.Plan
	Tone    string
	Pending int
	Total   int
}

type planFilter struct {
	Name  string
	Count int
	On    bool
	Href  string
}

type planStmt struct {
	SQL     string
	NoTx    bool
	Batch   string
	Hazards []*godwitv1.PlannedHazard
}

type planPhase struct {
	Name  string
	Stmts []planStmt
}

type planMigration struct {
	planned
	Expanded   bool
	Directives []string
	Notes      []string
	Phases     []planPhase
}

var planStates = []string{"ready", "bound", "superseded"}

func planTone(state string) string {
	switch state {
	case "bound":
		return "succeeded"
	case "ready":
		return "queued"
	default:
		return ""
	}
}

func rowOf(p *godwitv1.Plan) planRow {
	row := planRow{Plan: p, Tone: planTone(p.State), Total: len(p.Migrations)}
	for _, m := range p.Migrations {
		if !m.Applied {
			row.Pending++
		}
	}

	return row
}

func filterHref(target, state string) string {
	var q []string
	if state != "" {
		q = append(q, "state="+state)
	}
	if target != "" {
		q = append(q, "target="+target)
	}
	if len(q) == 0 {
		return "/ui/plans"
	}

	return "/ui/plans?" + strings.Join(q, "&")
}

func filtersOf(counts map[string]int, target, on string) []planFilter {
	total := 0
	for _, n := range counts {
		total += n
	}
	out := []planFilter{{Name: "all", Count: total, On: on == "", Href: filterHref(target, "")}}
	for _, s := range planStates {
		out = append(out, planFilter{Name: s, Count: counts[s], On: on == s, Href: filterHref(target, s)})
	}

	return out
}

func batchLine(b *godwitv1.PlannedBatch) string {
	if b == nil {
		return ""
	}
	line := "batch over " + b.Key + " (" + b.Kind + "), " + strconv.Itoa(int(b.Size)) + " rows per transaction"
	if b.Pause != "" {
		line += ", pausing " + b.Pause
	}

	return line
}

func phasesOf(m *godwitv1.PlannedMigration) []planPhase {
	var out []planPhase
	for _, st := range m.Statements {
		name := st.Phase
		if name == "" {
			name = m.Phase
		}
		if len(out) == 0 || out[len(out)-1].Name != name {
			out = append(out, planPhase{Name: name})
		}
		last := &out[len(out)-1]
		last.Stmts = append(last.Stmts, planStmt{SQL: st.Sql, NoTx: st.NoTx, Batch: batchLine(st.Batch), Hazards: st.Hazards})
	}

	return out
}

func planMigrations(p *godwitv1.Plan) []planMigration {
	base := plannedOf(p)
	out := make([]planMigration, 0, len(base))
	for i, m := range p.Migrations {
		out = append(out, planMigration{
			planned: base[i], Expanded: m.Expanded, Directives: m.Directives, Notes: m.Notes, Phases: phasesOf(m),
		})
	}

	return out
}

// planTargets fans out: ListPlans takes one target and refuses an empty one.
func planTargets(p page) []string {
	if p.Target != "" {
		return []string{p.Target}
	}
	out := make([]string, 0, len(p.Targets))
	for _, t := range p.Targets {
		out = append(out, t.Name)
	}

	return out
}

func (h *Handler) plans(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p, _, err := h.frame(ctx, r, "plans")
	if err != nil {
		h.fail(w, p, err)

		return
	}
	p.Target = r.URL.Query().Get("target")
	d := &plansData{State: r.URL.Query().Get("state")}
	counts := map[string]int{}
	for _, name := range planTargets(p) {
		resp, err := call(ctx, godwitv1connect.GodwitServiceListPlansProcedure,
			&godwitv1.ListPlansRequest{Target: name}, h.svc.ListPlans)
		if err != nil {
			h.fail(w, p, err)

			return
		}
		for _, plan := range resp.Plans {
			counts[plan.State]++
			if d.State == "" || d.State == plan.State {
				d.Rows = append(d.Rows, rowOf(plan))
			}
		}
	}
	sort.SliceStable(d.Rows, func(i, j int) bool {
		return d.Rows[i].CreatedAt.AsTime().After(d.Rows[j].CreatedAt.AsTime())
	})
	d.Filters, p.Plans = filtersOf(counts, p.Target, d.State), d
	h.render(w, http.StatusOK, "plans.html", p)
}

func (h *Handler) planPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p, _, err := h.frame(ctx, r, "plans")
	if err != nil {
		h.fail(w, p, err)

		return
	}
	id := r.PathValue("id")
	plan, err := h.plan(ctx, id)
	if err != nil {
		h.fail(w, p, err)

		return
	}
	d := &plansData{ID: id, Pruned: plan == nil}
	if plan != nil {
		p.Plan, p.Target = plan, plan.Target
		d.Tone, d.Detail = planTone(plan.State), planMigrations(plan)
	}
	p.Plans = d
	h.render(w, http.StatusOK, "plan.html", p)
}
