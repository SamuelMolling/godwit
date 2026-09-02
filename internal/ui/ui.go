// Package ui serves the operator web UI on top of the service's own API implementation.
package ui

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"errors"
	"html/template"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/SamuelMolling/godwit/assets"
	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/gen/godwit/v1/godwitv1connect"
	"github.com/SamuelMolling/godwit/internal/api"
	"github.com/SamuelMolling/godwit/internal/controlplane"
	"github.com/SamuelMolling/godwit/internal/version"
)

//go:embed templates/*.html
var files embed.FS

// Config names the replica and, when User and Password are both set, puts every page behind HTTP basic auth.
type Config struct {
	Replica  string
	User     string
	Password string
}

// Handler renders the UI by calling svc in-process with a ui:<user> principal.
type Handler struct {
	svc  godwitv1connect.GodwitServiceHandler
	cfg  Config
	tmpl *template.Template
	mux  *http.ServeMux
	now  func() time.Time
}

// New mounts the UI under /ui/.
func New(svc godwitv1connect.GodwitServiceHandler, cfg Config) *Handler {
	h := &Handler{svc: svc, cfg: cfg, mux: http.NewServeMux(), now: time.Now}
	h.tmpl = template.Must(template.New("").Funcs(h.funcs()).ParseFS(files, "templates/*.html"))
	h.mux.HandleFunc("GET /ui/{$}", h.index)
	h.mux.HandleFunc("GET /ui/mark.svg", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = w.Write(assets.Mark)
	})
	h.mux.HandleFunc("GET /ui/runs/{id}", h.run)
	h.mux.HandleFunc("POST /ui/runs/{id}/{action}", h.runAction)
	h.mux.HandleFunc("GET /ui/drift", h.drift)
	h.mux.HandleFunc("POST /ui/drift/{target}/{action}", h.driftAction)

	return h
}

func (h *Handler) protected() bool {
	return h.cfg.User != "" && h.cfg.Password != ""
}

func digestEqual(a, b string) int {
	x, y := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))

	return subtle.ConstantTimeCompare(x[:], y[:])
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := api.AnonymousActor
	if h.protected() {
		user, pass, ok := r.BasicAuth()
		if !ok || digestEqual(user, h.cfg.User)&digestEqual(pass, h.cfg.Password) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="godwit"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)

			return
		}
		name = user
	}
	ctx := api.WithPrincipal(r.Context(), api.Principal{Name: "ui:" + name, Scope: api.ScopeOperator})
	h.mux.ServeHTTP(w, r.WithContext(ctx))
}

func state(s godwitv1.RunState) string {
	return strings.ToLower(strings.TrimPrefix(s.String(), "RUN_STATE_"))
}

func (h *Handler) funcs() template.FuncMap {
	return template.FuncMap{
		"state":  state,
		"plural": plural,
		"label":  func(s godwitv1.RunState) string { return strings.ReplaceAll(state(s), "_", " ") },
		"short":  func(id string) string { return id[:min(8, len(id))] },
		"clock": func(ts *timestamppb.Timestamp) string {
			if ts == nil {
				return "—"
			}

			return ts.AsTime().UTC().Format("15:04:05Z")
		},
		"stamp": stampOf,
		"ago": func(ts *timestamppb.Timestamp) string {
			if ts == nil {
				return "—"
			}

			return ago(h.now().Sub(ts.AsTime()))
		},
		"took": func(r *godwitv1.Run) string {
			if r.FinishedAt == nil {
				return "—"
			}

			return elapsed(r.FinishedAt.AsTime().Sub(r.CreatedAt.AsTime()))
		},
	}
}

func ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return plural(int(d/time.Minute), "min") + " ago"
	case d < 24*time.Hour:
		return plural(int(d/time.Hour), "hour") + " ago"
	default:
		return plural(int(d/(24*time.Hour)), "day") + " ago"
	}
}

func plural(n int, unit string) string {
	if n == 1 || unit == "min" {
		return strconv.Itoa(n) + " " + unit
	}

	return strconv.Itoa(n) + " " + unit + "s"
}

func elapsed(d time.Duration) string {
	switch {
	case d < time.Second:
		return strconv.Itoa(int(d/time.Millisecond)) + "ms"
	case d < time.Minute:
		return strconv.FormatFloat(d.Seconds(), 'f', 1, 64) + "s"
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + "m" + strconv.Itoa(int(d%time.Minute/time.Second)) + "s"
	default:
		return strconv.Itoa(int(d/time.Hour)) + "h" + strconv.Itoa(int(d%time.Hour/time.Minute)) + "m"
	}
}

func splitCSV(s string) []string {
	var out []string
	for _, c := range strings.Split(s, ",") {
		if c = strings.TrimSpace(c); c != "" {
			out = append(out, c)
		}
	}

	return out
}

type target struct {
	Name string
	Bad  bool
}

type strip struct {
	Attention, Awaiting, Running, Queued, Succeeded int
	Oldest, Since                                   string
}

type step struct {
	Tone string
	Text string
	Who  string
	Note string
	When string
}

type planned struct {
	Version        int64
	Name           string
	Phase          string
	Applied        bool
	AlreadyApplied bool
	Effect         string
	Note           string
	Statements     int
	Hazards        []*godwitv1.PlannedHazard
}

type page struct {
	Nav       string
	Replica   string
	Version   string
	User      string
	Targets   []target
	Attention int
	Target    string
	Runs      []*godwitv1.Run
	Queue     []*godwitv1.Run
	Strip     strip
	Run       *godwitv1.Run
	Steps     []step
	Plan      *godwitv1.Plan
	Planned   []planned
	Tabs      []target
	Events    []*godwitv1.DriftEvent
	Open      *godwitv1.DriftEvent
	Checked   string
	Error     string
	Partial   bool
}

func needsHuman(r *godwitv1.Run) bool {
	return r.State == godwitv1.RunState_RUN_STATE_NEEDS_ATTENTION || r.State == godwitv1.RunState_RUN_STATE_AWAITING_CONTRACT
}

func (h *Handler) bare(r *http.Request, nav string) page {
	p := page{Nav: nav, Replica: h.cfg.Replica, Version: version.Version, Partial: r.Header.Get("HX-Request") != ""}
	if h.protected() {
		p.User = h.cfg.User
	}

	return p
}

func (h *Handler) frame(ctx context.Context, r *http.Request, nav string) (page, []*godwitv1.Run, error) {
	p := h.bare(r, nav)
	resp, err := h.svc.ListRuns(ctx, connect.NewRequest(&godwitv1.ListRunsRequest{}))
	if err != nil {
		return p, nil, err
	}
	bad := map[string]bool{}
	for _, run := range resp.Msg.Runs {
		if _, seen := bad[run.Target]; !seen {
			bad[run.Target] = false
		}
		if needsHuman(run) {
			bad[run.Target] = true
			p.Attention++
		}
	}
	for name, b := range bad {
		p.Targets = append(p.Targets, target{Name: name, Bad: b})
	}
	sort.Slice(p.Targets, func(i, j int) bool { return p.Targets[i].Name < p.Targets[j].Name })

	return p, resp.Msg.Runs, nil
}

func (h *Handler) render(w http.ResponseWriter, status int, name string, data page) {
	var buf bytes.Buffer
	if err := h.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)

		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}

func (h *Handler) fail(w http.ResponseWriter, p page, err error) {
	var cerr *connect.Error
	p.Error, p.Partial = err.Error(), false
	if errors.As(err, &cerr) {
		p.Error = cerr.Message()
	}
	status := http.StatusBadGateway
	if connect.CodeOf(err) == connect.CodeNotFound {
		status = http.StatusNotFound
	}
	h.render(w, status, "error.html", p)
}

func (h *Handler) summarize(runs []*godwitv1.Run) (strip, []*godwitv1.Run) {
	var s strip
	var queue []*godwitv1.Run
	now := h.now()
	for _, r := range runs {
		switch r.State {
		case godwitv1.RunState_RUN_STATE_NEEDS_ATTENTION:
			s.Attention++
			s.Oldest = "oldest " + ago(now.Sub(r.FinishedAt.AsTime()))
		case godwitv1.RunState_RUN_STATE_AWAITING_CONTRACT:
			s.Awaiting++
			s.Since = "since " + ago(now.Sub(r.CreatedAt.AsTime()))
		case godwitv1.RunState_RUN_STATE_RUNNING:
			s.Running++
		case godwitv1.RunState_RUN_STATE_QUEUED:
			s.Queued++
		case godwitv1.RunState_RUN_STATE_SUCCEEDED:
			if now.Sub(r.FinishedAt.AsTime()) < 24*time.Hour {
				s.Succeeded++
			}
		}
		if needsHuman(r) {
			queue = append(queue, r)
		}
	}

	return s, queue
}

func (h *Handler) index(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p, all, err := h.frame(ctx, r, "runs")
	if err != nil {
		h.fail(w, p, err)

		return
	}
	p.Target, p.Runs = r.URL.Query().Get("target"), all
	if p.Target != "" {
		p.Runs = nil
		for _, run := range all {
			if run.Target == p.Target {
				p.Runs = append(p.Runs, run)
			}
		}
	}
	p.Strip, p.Queue = h.summarize(p.Runs)
	h.render(w, http.StatusOK, "index.html", p)
}

func (h *Handler) run(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p, _, err := h.frame(ctx, r, "runs")
	if err != nil {
		h.fail(w, p, err)

		return
	}
	resp, err := h.svc.GetRun(ctx, connect.NewRequest(&godwitv1.GetRunRequest{RunId: r.PathValue("id")}))
	if err != nil {
		h.fail(w, p, err)

		return
	}
	audit, err := h.svc.ListAudit(ctx, connect.NewRequest(&godwitv1.ListAuditRequest{RunId: resp.Msg.Run.Id}))
	if err != nil {
		h.fail(w, p, err)

		return
	}
	plan, err := h.plan(ctx, resp.Msg.Run.PlanId)
	if err != nil {
		h.fail(w, p, err)

		return
	}
	p.Run, p.Steps = resp.Msg.Run, timeline(resp.Msg.Run, audit.Msg.Entries)
	p.Plan, p.Planned = plan, plannedOf(plan)
	h.render(w, http.StatusOK, "run.html", p)
}

// plan tolerates a missing plan: retention can sweep it between GetRun and GetPlan.
func (h *Handler) plan(ctx context.Context, id string) (*godwitv1.Plan, error) {
	if id == "" {
		return nil, nil
	}
	resp, err := h.svc.GetPlan(ctx, connect.NewRequest(&godwitv1.GetPlanRequest{PlanId: id}))
	if err != nil {
		if connect.CodeOf(err) == connect.CodeNotFound {
			return nil, nil
		}

		return nil, err
	}

	return resp.Msg.Plan, nil
}

func plannedOf(p *godwitv1.Plan) []planned {
	var out []planned
	for _, m := range p.GetMigrations() {
		v := planned{
			Version: m.Version, Name: m.Name, Phase: m.Phase, Applied: m.Applied,
			AlreadyApplied: m.AlreadyApplied, Effect: m.Effect, Note: m.Note,
			Statements: len(m.Statements),
		}
		seen := map[string]bool{}
		for _, st := range m.Statements {
			for _, hz := range st.Hazards {
				if !seen[hz.Code] {
					seen[hz.Code] = true
					v.Hazards = append(v.Hazards, hz)
				}
			}
		}
		out = append(out, v)
	}

	return out
}

func timeline(run *godwitv1.Run, entries []*godwitv1.AuditEntry) []step {
	created := step{Tone: "ok", Text: "Created", Who: run.CreatedBy, When: stampOf(run.CreatedAt)}
	if run.Reverts != "" {
		created.Text = "Created as revert of " + run.Reverts[:min(8, len(run.Reverts))]
	}
	if run.Source != "" {
		created.Note = "source " + run.Source
	}
	steps := []step{created}
	parked := false
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		switch e.Action {
		case controlplane.AuditRunResume:
			steps = append(steps, step{Tone: "ok", Text: "Resumed", Who: e.Actor, When: stampOf(e.At)})
		case controlplane.AuditRunPark:
			parked = true
			steps = append(steps, step{Tone: "warn", Text: "Parked", Who: e.Actor, Note: e.Detail, When: stampOf(e.At)})
		case controlplane.AuditRunConfirm:
			steps = append(steps, step{Tone: "ok", Text: "Contract confirmed", Who: e.Actor, When: stampOf(e.At)})
		case controlplane.AuditRunReattach:
			steps = append(steps, step{Tone: "ok", Text: "Re-attached by a repeated request", Who: e.Actor, Note: e.Detail, When: stampOf(e.At)})
		}
	}
	if run.Retries > 0 {
		steps = append(steps, step{Tone: "warn", Text: plural(int(run.Retries), "transient failure") + " retried by the scheduler", When: "—"})
	}
	switch run.State {
	case godwitv1.RunState_RUN_STATE_QUEUED:
		queued := step{Text: "Queued, waiting for a replica", When: "—"}
		if run.NotBefore != nil {
			queued = step{Tone: "warn", Text: "Backing off, next attempt at " + stampOf(run.NotBefore), When: "—"}
		}
		steps = append(steps, queued)
	case godwitv1.RunState_RUN_STATE_RUNNING:
		steps = append(steps, step{Tone: "run", Text: "Running, attempt " + strconv.Itoa(int(run.Attempts)), When: "—"})
	case godwitv1.RunState_RUN_STATE_AWAITING_CONTRACT:
		steps = append(steps, step{Tone: "warn", Text: "Expand applied, waiting for contract confirmation", When: "—"})
	case godwitv1.RunState_RUN_STATE_SUCCEEDED:
		steps = append(steps, step{Tone: "ok", Text: "Succeeded", When: stampOf(run.FinishedAt)})
	case godwitv1.RunState_RUN_STATE_REVERTED:
		steps = append(steps, step{Text: "Reverted", When: stampOf(run.FinishedAt)})
	case godwitv1.RunState_RUN_STATE_FAILED:
		steps = append(steps, step{Tone: "bad", Text: "Failed", When: stampOf(run.FinishedAt)})
	case godwitv1.RunState_RUN_STATE_NEEDS_ATTENTION:
		if !parked {
			steps = append(steps, step{Tone: "bad", Text: "Stopped after " + plural(int(run.Attempts), "attempt"), When: stampOf(run.FinishedAt)})
		}
	}

	return steps
}

func stampOf(ts *timestamppb.Timestamp) string {
	if ts == nil {
		return "—"
	}

	return ts.AsTime().UTC().Format("2006-01-02 15:04:05Z")
}

func (h *Handler) runAction(w http.ResponseWriter, r *http.Request) {
	id, ctx, p := r.PathValue("id"), r.Context(), h.bare(r, "runs")
	var err error
	switch r.PathValue("action") {
	case "resume":
		_, err = h.svc.ResumeRun(ctx, connect.NewRequest(&godwitv1.ResumeRunRequest{RunId: id}))
	case "park":
		_, err = h.svc.ParkRun(ctx, connect.NewRequest(&godwitv1.ParkRunRequest{RunId: id, Reason: r.FormValue("reason")}))
	case "confirm":
		_, err = h.svc.ConfirmRollout(ctx, connect.NewRequest(&godwitv1.ConfirmRolloutRequest{RunId: id}))
	case "revert":
		var resp *connect.Response[godwitv1.RevertRunResponse]
		resp, err = h.svc.RevertRun(ctx, connect.NewRequest(&godwitv1.RevertRunRequest{
			RunId: id, AcknowledgeHazards: splitCSV(r.FormValue("ack")),
		}))
		if err == nil {
			id = resp.Msg.RunId
		}
	default:
		http.NotFound(w, r)

		return
	}
	if err != nil {
		h.fail(w, p, err)

		return
	}
	http.Redirect(w, r, "/ui/runs/"+id, http.StatusSeeOther)
}

func (h *Handler) drift(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p, _, err := h.frame(ctx, r, "drift")
	if err != nil {
		h.fail(w, p, err)

		return
	}
	events, err := h.svc.ListDriftEvents(ctx, connect.NewRequest(&godwitv1.ListDriftEventsRequest{}))
	if err != nil {
		h.fail(w, p, err)

		return
	}
	open := map[string]*godwitv1.DriftEvent{}
	for _, e := range events.Msg.Events {
		if e.ResolvedAt == nil && open[e.Target] == nil {
			open[e.Target] = e
		}
	}
	for _, t := range p.Targets {
		p.Tabs = append(p.Tabs, target{Name: t.Name, Bad: open[t.Name] != nil})
	}
	p.Target, p.Checked = r.URL.Query().Get("target"), r.URL.Query().Get("checked")
	if p.Target == "" && len(p.Tabs) > 0 {
		p.Target = p.Tabs[0].Name
	}
	p.Open = open[p.Target]
	for _, e := range events.Msg.Events {
		if e.Target == p.Target {
			p.Events = append(p.Events, e)
		}
	}
	h.render(w, http.StatusOK, "drift.html", p)
}

func (h *Handler) driftAction(w http.ResponseWriter, r *http.Request) {
	tgt, ctx, p := r.PathValue("target"), r.Context(), h.bare(r, "drift")
	dest := "/ui/drift?target=" + tgt
	switch r.PathValue("action") {
	case "check":
		resp, err := h.svc.CheckDrift(ctx, connect.NewRequest(&godwitv1.CheckDriftRequest{Target: tgt}))
		if err != nil {
			h.fail(w, p, err)

			return
		}
		dest += "&checked=clean"
		if resp.Msg.Drifted {
			dest = "/ui/drift?target=" + tgt + "&checked=drifted"
		}
	case "accept":
		if _, err := h.svc.AcceptBaseline(ctx, connect.NewRequest(&godwitv1.AcceptBaselineRequest{Target: tgt})); err != nil {
			h.fail(w, p, err)

			return
		}
	default:
		http.NotFound(w, r)

		return
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}
