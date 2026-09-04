package ui

import (
	"net/http"
	"time"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/gen/godwit/v1/godwitv1connect"
)

type fleetCell struct {
	Tone string
	Text string
	Note string
}

type fleetRow struct {
	ID         string
	Checksum   string
	Repeatable bool
	Checkpoint bool
	Divergent  bool
	Cells      []fleetCell
}

type fleetData struct {
	Columns   []string
	Rows      []fleetRow
	Gaps      int
	Diverged  int
	OnlyGaps  bool
	OnlyOn    string
	GapsHref  string
	Everybody bool
}

func (h *Handler) migrations(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p, err := h.frame(ctx, r, "fleet")
	if err != nil {
		h.fail(w, p, err)

		return
	}
	q := r.URL.Query()
	p.Target = q.Get("target")
	req := &godwitv1.ListMigrationsRequest{InTarget: p.Target, NotEverywhere: q.Get("gaps") == "1"}
	resp, err := call(ctx, godwitv1connect.GodwitServiceListMigrationsProcedure, req, h.svc.ListMigrations)
	if err != nil {
		h.fail(w, p, err)

		return
	}
	p.Fleet = fleetOf(resp, p.Target, req.NotEverywhere)
	h.render(w, http.StatusOK, "migrations.html", p)
}

func fleetOf(resp *godwitv1.ListMigrationsResponse, target string, onlyGaps bool) *fleetData {
	d := &fleetData{Columns: resp.Targets, OnlyGaps: onlyGaps, OnlyOn: target, GapsHref: gapsHref(target, onlyGaps)}
	diverged := map[string]bool{}
	for _, m := range resp.Migrations {
		row := fleetRow{
			ID: m.Migration, Checksum: shortSum(m.Checksum), Repeatable: m.Repeatable,
			Checkpoint: m.Checkpoint, Divergent: m.Divergent,
		}
		cells := fleetCells(m)
		for _, t := range resp.Targets {
			row.Cells = append(row.Cells, cells[t])
		}
		d.Rows = append(d.Rows, row)
		if len(m.MissingFrom) > 0 {
			d.Gaps++
		}
		if m.Divergent {
			diverged[m.Migration] = true
		}
	}
	d.Diverged, d.Everybody = len(diverged), d.Gaps == 0 && len(d.Rows) > 0

	return d
}

func fleetCells(m *godwitv1.FleetMigration) map[string]fleetCell {
	cells := make(map[string]fleetCell, len(m.AppliedOn)+len(m.MissingFrom))
	for _, o := range m.AppliedOn {
		cell := fleetCell{Tone: "succeeded", Text: o.AppliedAt.AsTime().UTC().Format(time.DateOnly)}
		if o.CollapsedBy != "" {
			cell.Note = "recorded by " + o.CollapsedBy + ", not run"
		}
		cells[o.Target] = cell
	}
	for _, g := range m.MissingFrom {
		cells[g.Target] = gapCell(g)
	}

	return cells
}

func gapCell(g *godwitv1.MigrationGap) fleetCell {
	switch {
	case g.Holds:
		return fleetCell{Tone: "drifted", Text: "differs", Note: "applied here as " + shortSum(g.OtherChecksum)}
	case g.Behind:
		return fleetCell{Tone: "", Text: "not yet", Note: "the target has not reached this version"}
	default:
		return fleetCell{Tone: "failed", Text: "missing", Note: "the target is past this version and does not have it"}
	}
}

func shortSum(sum string) string {
	if sum == "" {
		return "unknown"
	}

	return sum[:8]
}

func gapsHref(target string, on bool) string {
	href := "/ui/migrations?"
	if target != "" {
		href += "target=" + target + "&"
	}
	if on {
		return href + "gaps=0"
	}

	return href + "gaps=1"
}
