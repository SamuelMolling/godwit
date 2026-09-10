package ui

import (
	"context"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"connectrpc.com/connect"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/gen/godwit/v1/godwitv1connect"
	"github.com/SamuelMolling/godwit/internal/controlplane"
	"github.com/SamuelMolling/godwit/internal/engine"
)

var diffNameRe = regexp.MustCompile(`^[a-z0-9_]+$`)

// diffStatus lists the refusals the form itself explains; every other code is the error page.
var diffStatus = map[connect.Code]int{
	connect.CodeInvalidArgument:    http.StatusBadRequest,
	connect.CodeFailedPrecondition: http.StatusPreconditionFailed,
	connect.CodeUnimplemented:      http.StatusNotImplemented,
}

const (
	fromNewest = "auto"
	fromPlan   = "plan"
	fromRun    = "run"
	fromPaste  = "paste"
)

// repeatables is where the page took the R__ pairs it sent, and how they compare with what the target recorded.
type repeatables struct {
	Source   string
	Kind     string
	From     string
	Recorded []string
	Sent     []string
	Stale    []string
	Missing  []string
	Unknown  []string
	Bodies   map[string]string
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
	Objects    []string
	Rep        *repeatables
}

func diffName(s string) string {
	if !diffNameRe.MatchString(s) {
		return "changes"
	}

	return s
}

func diffSource(s string) string {
	if s == fromPlan || s == fromRun || s == fromPaste {
		return s
	}

	return fromNewest
}

func (h *Handler) diffForm(w http.ResponseWriter, r *http.Request) {
	p, err := h.frame(r.Context(), r, "diff")
	if err != nil {
		h.fail(w, p, err)

		return
	}
	p.Diff = &diffView{
		Target: r.URL.Query().Get("target"), Name: diffName(r.URL.Query().Get("name")),
		Rep: &repeatables{Source: fromNewest},
	}
	h.render(w, http.StatusOK, "diff.html", p)
}

func (h *Handler) diffRun(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p, err := h.frame(ctx, r, "diff")
	if err != nil {
		h.fail(w, p, err)

		return
	}
	v := &diffView{Target: r.FormValue("target"), Schema: r.FormValue("schema"), Name: diffName(r.FormValue("name"))}
	v.Rep = &repeatables{Source: diffSource(r.FormValue("files")), Bodies: pastedBodies(r)}
	p.Diff = v
	files, err := h.supply(ctx, v)
	if err != nil {
		h.refuse(w, p, err)

		return
	}
	resp, err := call(ctx, godwitv1connect.GodwitServiceDiffProcedure, &godwitv1.DiffRequest{
		Target: v.Target, Schema: v.Schema, Base: godwitv1.DiffBase_DIFF_BASE_LIVE, Files: files,
	}, h.svc.Diff)
	if err != nil {
		h.refuse(w, p, err)

		return
	}
	v.Ran, v.Changed = true, resp.UpSql != ""
	v.Up, v.Down, v.Statements, v.Observed = resp.UpSql, resp.DownSql, resp.Statements, resp.Observed
	v.Drift, v.Objects = resp.Drift, resp.RepeatableObjects
	if v.Changed {
		prefix := h.now().UTC().Format("20060102150405") + "_" + v.Name
		v.UpFile, v.DownFile = prefix+".up.sql", prefix+".down.sql"
	}
	h.render(w, http.StatusOK, "diff.html", p)
}

// refuse keeps a refusal the author can act on the form; anything else is a service failure.
func (h *Handler) refuse(w http.ResponseWriter, p page, err error) {
	status, shown := diffStatus[connect.CodeOf(err)]
	if !shown {
		h.fail(w, p, err)

		return
	}
	p.Error = reason(err)
	h.render(w, status, "diff.html", p)
}

// supply resolves the R__ pairs a target that records repeatables needs; godwit cannot see the repository.
func (h *Handler) supply(ctx context.Context, v *diffView) ([]*godwitv1.MigrationFile, error) {
	st, err := call(ctx, godwitv1connect.GodwitServiceGetTargetStatusProcedure,
		&godwitv1.GetTargetStatusRequest{Target: v.Target}, h.svc.GetTargetStatus)
	if err != nil {
		return nil, err
	}
	recorded := map[string]string{}
	for _, a := range st.Applied {
		if a.Repeatable {
			recorded[a.Name] = a.Checksum
			v.Rep.Recorded = append(v.Rep.Recorded, a.Name)
		}
	}
	slices.Sort(v.Rep.Recorded)
	if len(recorded) == 0 {
		return nil, nil
	}
	files, err := h.repeatFiles(ctx, v.Target, v.Rep)
	if err != nil {
		return nil, err
	}
	compare(v.Rep, recorded, files)

	return filesToProto(files), nil
}

func (h *Handler) repeatFiles(ctx context.Context, target string, rep *repeatables) (map[string]string, error) {
	switch rep.Source {
	case fromPaste:
		rep.Kind, rep.From = fromPaste, "the boxes below"

		return pastedFiles(rep.Bodies), nil
	case fromRun:
		return h.runSnapshot(ctx, target, rep)
	case fromPlan:
		return h.planSnapshot(ctx, target, rep)
	}
	files, err := h.planSnapshot(ctx, target, rep)
	if err != nil || len(files) > 0 {
		return files, err
	}

	return h.runSnapshot(ctx, target, rep)
}

// planSnapshot takes the files stored with the target's newest plan; retention can sweep it under the listing.
func (h *Handler) planSnapshot(ctx context.Context, target string, rep *repeatables) (map[string]string, error) {
	rep.Kind = fromPlan
	list, err := call(ctx, godwitv1connect.GodwitServiceListPlansProcedure,
		&godwitv1.ListPlansRequest{Target: target, Limit: 1}, h.svc.ListPlans)
	if err != nil {
		return nil, err
	}
	if len(list.Plans) == 0 {
		rep.From = "no plan is stored for " + target

		return nil, nil
	}
	p := list.Plans[0]
	resp, err := call(ctx, godwitv1connect.GodwitServiceGetPlanProcedure,
		&godwitv1.GetPlanRequest{PlanId: p.Id, IncludeFiles: true}, h.svc.GetPlan)
	if err != nil {
		if connect.CodeOf(err) != connect.CodeNotFound {
			return nil, err
		}
		rep.From = "plan " + short(p.Id) + " was swept by retention before it could be read"

		return nil, nil
	}
	files := repeatablesOnly(resp.Files)
	rep.From = "plan " + short(p.Id) + " (" + p.State + "), stored " + stampOf(p.CreatedAt)
	if len(files) == 0 {
		rep.From += ", which carried no repeatable files"
	}

	return files, nil
}

func (h *Handler) runSnapshot(ctx context.Context, target string, rep *repeatables) (map[string]string, error) {
	rep.Kind = fromRun
	list, err := call(ctx, godwitv1connect.GodwitServiceListRunsProcedure,
		&godwitv1.ListRunsRequest{Target: target}, h.svc.ListRuns)
	if err != nil {
		return nil, err
	}
	var last *godwitv1.Run
	for _, run := range list.Runs {
		if run.State == godwitv1.RunState_RUN_STATE_SUCCEEDED {
			last = run

			break
		}
	}
	if last == nil {
		rep.From = "no run has succeeded on " + target

		return nil, nil
	}
	resp, err := call(ctx, godwitv1connect.GodwitServiceGetRunProcedure,
		&godwitv1.GetRunRequest{RunId: last.Id, IncludeFiles: true}, h.svc.GetRun)
	if err != nil {
		return nil, err
	}
	files := repeatablesOnly(resp.Files)
	rep.From = "run " + short(last.Id) + ", which succeeded " + stampOf(last.FinishedAt)
	if len(files) == 0 {
		rep.From += " carrying no repeatable files"
	}

	return files, nil
}

func repeatablesOnly(files []*godwitv1.MigrationFile) map[string]string {
	out := map[string]string{}
	for _, f := range files {
		if strings.HasPrefix(f.Name, engine.RepeatablePrefix) {
			out[f.Name] = f.Body
		}
	}

	return out
}

func pastedBodies(r *http.Request) map[string]string {
	out := map[string]string{}
	for key, values := range r.Form {
		if name, ok := strings.CutPrefix(key, "body."); ok && strings.TrimSpace(values[0]) != "" {
			out[name] = values[0]
		}
	}

	return out
}

// pastedFiles pairs each body with a placeholder down side: the loader wants a pair, a diff never runs one.
func pastedFiles(bodies map[string]string) map[string]string {
	out := map[string]string{}
	for name, body := range bodies {
		out[engine.RepeatablePrefix+name+".up.sql"] = body
		out[engine.RepeatablePrefix+name+".down.sql"] = "-- a diff never runs a down side\n"
	}

	return out
}

// compare reports the snapshot against what the target recorded; a set the loader refuses is left to Diff.
func compare(rep *repeatables, recorded map[string]string, files map[string]string) {
	migs, err := controlplane.MigrationsFromFiles(files)
	if err != nil {
		return
	}
	for _, m := range migs {
		rep.Sent = append(rep.Sent, m.Name)
		sum, ok := recorded[m.Name]
		switch {
		case !ok:
			rep.Unknown = append(rep.Unknown, m.Name)
		case sum != m.Checksum:
			rep.Stale = append(rep.Stale, m.Name)
		}
	}
	for _, name := range rep.Recorded {
		if !slices.Contains(rep.Sent, name) {
			rep.Missing = append(rep.Missing, name)
		}
	}
	slices.Sort(rep.Sent)
}

func filesToProto(files map[string]string) []*godwitv1.MigrationFile {
	out := make([]*godwitv1.MigrationFile, 0, len(files))
	for _, name := range slices.Sorted(maps.Keys(files)) {
		out = append(out, &godwitv1.MigrationFile{Name: name, Body: files[name]})
	}

	return out
}
