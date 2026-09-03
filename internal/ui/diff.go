package ui

import (
	"net/http"
	"regexp"

	"connectrpc.com/connect"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/gen/godwit/v1/godwitv1connect"
)

var diffNameRe = regexp.MustCompile(`^[a-z0-9_]+$`)

// diffStatus lists the refusals the form itself explains; every other code is the error page.
var diffStatus = map[connect.Code]int{
	connect.CodeInvalidArgument:    http.StatusBadRequest,
	connect.CodeFailedPrecondition: http.StatusPreconditionFailed,
	connect.CodeUnimplemented:      http.StatusNotImplemented,
}

type diffView struct {
	Target     string
	Schema     string
	Name       string
	Ran        bool
	Changed    bool
	Up         string
	Down       string
	UpFile     string
	DownFile   string
	Statements []*godwitv1.PlannedStatement
	Observed   *godwitv1.PlanObservation
	Drift      string
}

func diffName(s string) string {
	if !diffNameRe.MatchString(s) {
		return "changes"
	}

	return s
}

func (h *Handler) diffForm(w http.ResponseWriter, r *http.Request) {
	p, _, err := h.frame(r.Context(), r, "diff")
	if err != nil {
		h.fail(w, p, err)

		return
	}
	p.Diff = &diffView{Target: r.URL.Query().Get("target"), Name: diffName(r.URL.Query().Get("name"))}
	h.render(w, http.StatusOK, "diff.html", p)
}

func (h *Handler) diffRun(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p, _, err := h.frame(ctx, r, "diff")
	if err != nil {
		h.fail(w, p, err)

		return
	}
	v := &diffView{Target: r.FormValue("target"), Schema: r.FormValue("schema"), Name: diffName(r.FormValue("name"))}
	p.Diff = v
	resp, err := call(ctx, godwitv1connect.GodwitServiceDiffProcedure,
		&godwitv1.DiffRequest{Target: v.Target, Schema: v.Schema, Base: godwitv1.DiffBase_DIFF_BASE_LIVE}, h.svc.Diff)
	if err != nil {
		status, shown := diffStatus[connect.CodeOf(err)]
		if !shown {
			h.fail(w, p, err)

			return
		}
		p.Error = reason(err)
		h.render(w, status, "diff.html", p)

		return
	}
	v.Ran, v.Changed = true, resp.UpSql != ""
	v.Up, v.Down, v.Statements, v.Observed, v.Drift = resp.UpSql, resp.DownSql, resp.Statements, resp.Observed, resp.Drift
	if v.Changed {
		prefix := h.now().UTC().Format("20060102150405") + "_" + v.Name
		v.UpFile, v.DownFile = prefix+".up.sql", prefix+".down.sql"
	}
	h.render(w, http.StatusOK, "diff.html", p)
}
