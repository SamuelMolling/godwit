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

//go:embed app.js
var script []byte

// Config names the replica and how /ui authenticates: a basic-auth password matching one of Tokens
// resolves to that token's name and scope, and User with Password is the shared identity, which carries Scope.
// Origins, when set, are the only origins a form post may come from and the only hosts the UI answers on.
type Config struct {
	Replica  string
	Tokens   []api.Token
	User     string
	Password string
	Scope    api.Scope
	Origins  []Origin
	// AnonymousScope is what a visitor gets when neither Tokens nor User/Password can sign anyone in;
	// it defaults to AnonymousScope, never to Scope, and widening it is an explicit choice.
	AnonymousScope api.Scope
	// Anonymous serves /ui with no authentication at all, even when Tokens or User/Password could sign
	// someone in. Every visitor is then ui:anonymous with AnonymousScope.
	Anonymous bool
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
	if cfg.Scope == "" {
		cfg.Scope = api.ScopeOperator
	}
	if cfg.AnonymousScope == "" {
		cfg.AnonymousScope = AnonymousScope
	}
	h := &Handler{svc: svc, cfg: cfg, mux: http.NewServeMux(), now: time.Now}
	h.tmpl = template.Must(template.New("").Funcs(h.funcs()).ParseFS(files, "templates/*.html"))
	h.mux.HandleFunc("GET /ui/{$}", h.index)
	h.mux.HandleFunc("GET /ui/mark.svg", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = w.Write(assets.Mark)
	})
	h.mux.HandleFunc("GET /ui/app.js", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = w.Write(script)
	})
	h.mux.HandleFunc("GET /ui/runs/{id}", h.run)
	h.mux.HandleFunc("POST /ui/runs/{id}/{action}", h.runAction)
	h.mux.HandleFunc("GET /ui/plans", h.plans)
	h.mux.HandleFunc("GET /ui/plans/{id}", h.planPage)
	h.mux.HandleFunc("GET /ui/drift", h.drift)
	h.mux.HandleFunc("POST /ui/drift/{target}/{action}", h.driftAction)
	h.mux.HandleFunc("GET /ui/diff", h.diffForm)
	h.mux.HandleFunc("POST /ui/diff", h.diffRun)
	h.mux.HandleFunc("GET /ui/migrations", h.migrations)
	h.mux.HandleFunc("GET /ui/targets", h.targets)
	h.mux.HandleFunc("GET /ui/targets/{name}", h.target)

	return h
}

func (h *Handler) shared() bool {
	return h.cfg.User != "" && h.cfg.Password != ""
}

func (h *Handler) protected() bool {
	return !h.cfg.Anonymous && (len(h.cfg.Tokens) > 0 || h.shared())
}

func digestEqual(a, b string) int {
	x, y := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))

	return subtle.ConstantTimeCompare(x[:], y[:])
}

// AnonymousScope is what an unauthenticated visitor gets when the UI has no way to sign anyone in.
// It is read, never Config.Scope: a service with no credential configured must not hand out the rights
// of the identity it would have authenticated.
const AnonymousScope = api.ScopeRead

func (h *Handler) principal(r *http.Request) (api.Principal, bool) {
	if !h.protected() {
		return api.Principal{Name: "ui:" + api.AnonymousActor, Scope: h.cfg.AnonymousScope}, true
	}
	user, pass, ok := r.BasicAuth()
	if !ok {
		return api.Principal{}, false
	}
	for _, t := range h.cfg.Tokens {
		if digestEqual(pass, t.Secret) == 1 {
			return api.Principal{Name: "ui:" + t.Name, Scope: t.Scope}, true
		}
	}
	if h.shared() && digestEqual(user, h.cfg.User)&digestEqual(pass, h.cfg.Password) == 1 {
		return api.Principal{Name: "ui:" + user, Scope: h.cfg.Scope}, true
	}

	return api.Principal{}, false
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	harden(w.Header())
	if !h.knownHost(r.Host) {
		http.Error(w, "unknown host", http.StatusForbidden)

		return
	}
	if !safeMethod(r.Method) && !h.sameOrigin(r) {
		http.Error(w, "cross-site request refused", http.StatusForbidden)

		return
	}
	p, ok := h.principal(r)
	if !ok {
		w.Header().Set("WWW-Authenticate", `Basic realm="godwit"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)

		return
	}
	h.mux.ServeHTTP(w, r.WithContext(api.WithPrincipal(r.Context(), p)))
}

// call runs the scope decision the auth interceptor would have made, then the handler in process.
func call[Req, Resp any](ctx context.Context, procedure string, msg *Req,
	fn func(context.Context, *connect.Request[Req]) (*connect.Response[Resp], error),
) (*Resp, error) {
	if err := api.Authorize(procedure, api.Caller(ctx)); err != nil {
		return nil, err
	}
	resp, err := fn(ctx, connect.NewRequest(msg))
	if err != nil {
		return nil, err
	}

	return resp.Msg, nil
}

var uiActions = map[string]string{
	"resume":  godwitv1connect.GodwitServiceResumeRunProcedure,
	"park":    godwitv1connect.GodwitServiceParkRunProcedure,
	"confirm": godwitv1connect.GodwitServiceConfirmRolloutProcedure,
	"revert":  godwitv1connect.GodwitServiceRevertRunProcedure,
	"check":   godwitv1connect.GodwitServiceCheckDriftProcedure,
	"accept":  godwitv1connect.GodwitServiceAcceptBaselineProcedure,
	"diff":    godwitv1connect.GodwitServiceDiffProcedure,
}

func allowed(p api.Principal) map[string]bool {
	out := make(map[string]bool, len(uiActions))
	for name, procedure := range uiActions {
		out[name] = api.Authorize(procedure, p) == nil
	}

	return out
}

func stateActions(s godwitv1.RunState) []string {
	switch s {
	case godwitv1.RunState_RUN_STATE_NEEDS_ATTENTION, godwitv1.RunState_RUN_STATE_FAILED:
		return []string{"resume", "park"}
	case godwitv1.RunState_RUN_STATE_AWAITING_CONTRACT:
		return []string{"confirm"}
	case godwitv1.RunState_RUN_STATE_SUCCEEDED:
		return []string{"revert"}
	default:
		return nil
	}
}

func locked(s godwitv1.RunState, can map[string]bool) bool {
	offered := stateActions(s)
	for _, a := range offered {
		if can[a] {
			return false
		}
	}

	return len(offered) > 0
}

func state(s godwitv1.RunState) string {
	return strings.ToLower(strings.TrimPrefix(s.String(), "RUN_STATE_"))
}

func (h *Handler) funcs() template.FuncMap {
	return template.FuncMap{
		"state":    state,
		"plural":   plural,
		"backfill": backfillOf,
		"label":    func(s godwitv1.RunState) string { return strings.ReplaceAll(state(s), "_", " ") },
		"short":    short,
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

func short(id string) string { return id[:min(8, len(id))] }

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
	Repeatable     bool
	Phase          string
	Applied        bool
	AlreadyApplied bool
	Effect         string
	Note           string
	Statements     int
	Hazards        []*godwitv1.PlannedHazard
}

// backfill is the live view of a batched statement: Done and Batches are counted, Total is an estimate.
type backfill struct {
	Migration string
	Statement int32
	Phase     string
	Done      string
	Total     string
	Batches   string
	Percent   int
}

func backfillOf(r *godwitv1.Run) *backfill {
	p := r.GetProgress()
	if r.GetState() != godwitv1.RunState_RUN_STATE_RUNNING || p.GetBatches() == 0 {
		return nil
	}
	b := &backfill{
		Migration: p.Migration, Statement: p.Statement, Phase: p.Phase,
		Done: thousands(p.RowsDone), Batches: batchesOf(p.Batches),
	}
	if p.RowsTotal > 0 {
		b.Total = thousands(p.RowsTotal)
		b.Percent = min(100, int(p.RowsDone*100/p.RowsTotal))
	}

	return b
}

func batchesOf(n int32) string {
	if n == 1 {
		return "1 batch"
	}

	return strconv.Itoa(int(n)) + " batches"
}

func thousands(n int64) string {
	s := strconv.FormatInt(n, 10)
	var out []byte
	for i := range len(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, s[i])
	}

	return string(out)
}

type page struct {
	Nav       string
	Replica   string
	Version   string
	User      string
	Scope     string
	Can       map[string]bool
	Locked    bool
	Targets   []target
	Attention int
	Target    string
	Runs      []*godwitv1.Run
	Queue     []*godwitv1.Run
	Strip     strip
	Run       *godwitv1.Run
	Ledger    []*godwitv1.RunMigration
	Steps     []step
	Plan      *godwitv1.Plan
	Planned   []planned
	Plans     *plansData
	Tabs      []target
	Events    []*godwitv1.DriftEvent
	Open      *godwitv1.DriftEvent
	Checked   string
	Diff      *diffView
	Fleet     *fleetData
	Summaries []*godwitv1.TargetSummary
	Summary   *godwitv1.TargetSummary
	Status    *godwitv1.GetTargetStatusResponse
	Ready     []*godwitv1.Plan
	Applied   []migRow
	Pending   []migRow
	Error     string
	Partial   bool
	Anonymous bool
}

func needsHuman(r *godwitv1.Run) bool {
	return r.State == godwitv1.RunState_RUN_STATE_NEEDS_ATTENTION || r.State == godwitv1.RunState_RUN_STATE_AWAITING_CONTRACT
}

func (h *Handler) bare(r *http.Request, nav string) page {
	who := api.Caller(r.Context())
	p := page{
		Nav: nav, Replica: h.cfg.Replica, Version: version.Version, Anonymous: h.cfg.Anonymous,
		Scope: string(who.Scope), Can: allowed(who), Partial: r.Header.Get("HX-Request") != "",
	}
	if h.protected() {
		p.User = strings.TrimPrefix(who.Name, "ui:")
	}

	return p
}

func (h *Handler) frame(ctx context.Context, r *http.Request, nav string) (page, error) {
	p := h.bare(r, nav)
	resp, err := call(ctx, godwitv1connect.GodwitServiceListTargetsProcedure, &godwitv1.ListTargetsRequest{}, h.svc.ListTargets)
	if err != nil {
		return p, err
	}
	p.Summaries = resp.Targets
	for _, t := range resp.Targets {
		p.Attention += int(t.AttentionRuns)
		p.Targets = append(p.Targets, target{Name: t.Name, Bad: t.AttentionRuns > 0})
	}

	return p, nil
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

func reason(err error) string {
	var cerr *connect.Error
	if errors.As(err, &cerr) {
		return cerr.Message()
	}

	return err.Error()
}

func (h *Handler) fail(w http.ResponseWriter, p page, err error) {
	p.Error, p.Partial = reason(err), false
	status := http.StatusBadGateway
	switch connect.CodeOf(err) {
	case connect.CodeNotFound:
		status = http.StatusNotFound
	case connect.CodePermissionDenied:
		status = http.StatusForbidden
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
	p, err := h.frame(ctx, r, "runs")
	if err != nil {
		h.fail(w, p, err)

		return
	}
	resp, err := call(ctx, godwitv1connect.GodwitServiceListRunsProcedure, &godwitv1.ListRunsRequest{}, h.svc.ListRuns)
	if err != nil {
		h.fail(w, p, err)

		return
	}
	all := resp.Runs
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
	p, err := h.frame(ctx, r, "runs")
	if err != nil {
		h.fail(w, p, err)

		return
	}
	resp, err := call(ctx, godwitv1connect.GodwitServiceGetRunProcedure,
		&godwitv1.GetRunRequest{RunId: r.PathValue("id")}, h.svc.GetRun)
	if err != nil {
		h.fail(w, p, err)

		return
	}
	audit, err := call(ctx, godwitv1connect.GodwitServiceListAuditProcedure,
		&godwitv1.ListAuditRequest{RunId: resp.Run.Id}, h.svc.ListAudit)
	if err != nil {
		h.fail(w, p, err)

		return
	}
	plan, err := h.plan(ctx, resp.Run.PlanId)
	if err != nil {
		h.fail(w, p, err)

		return
	}
	p.Run, p.Steps, p.Ledger = resp.Run, timeline(resp.Run, audit.Entries), resp.Applied
	p.Plan, p.Planned = plan, plannedOf(plan)
	p.Locked = locked(resp.Run.State, p.Can)
	h.render(w, http.StatusOK, "run.html", p)
}

// plan tolerates a missing plan: retention can sweep it between GetRun and GetPlan.
func (h *Handler) plan(ctx context.Context, id string) (*godwitv1.Plan, error) {
	if id == "" {
		return nil, nil
	}
	resp, err := call(ctx, godwitv1connect.GodwitServiceGetPlanProcedure, &godwitv1.GetPlanRequest{PlanId: id}, h.svc.GetPlan)
	if err != nil {
		if connect.CodeOf(err) == connect.CodeNotFound {
			return nil, nil
		}

		return nil, err
	}

	return resp.Plan, nil
}

func plannedOf(p *godwitv1.Plan) []planned {
	var out []planned
	for _, m := range p.GetMigrations() {
		v := planned{
			Version: m.Version, Name: m.Name, Repeatable: m.Repeatable, Phase: m.Phase, Applied: m.Applied,
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
		steps = append(steps, step{Tone: "run", Text: "Running, attempt " + strconv.Itoa(int(run.Attempts)), Note: statementLine(run.GetProgress()), When: "—"})
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

// statementLine names the newest statement the run reported, which is the one it is on unless it has just finished it.
func statementLine(p *godwitv1.RunProgress) string {
	if p == nil {
		return ""
	}
	line := "statement " + strconv.Itoa(int(p.Statement)) + " of " + p.Migration
	if p.Phase != "" {
		line += " (" + p.Phase + ")"
	}

	return line
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
		_, err = call(ctx, godwitv1connect.GodwitServiceResumeRunProcedure,
			&godwitv1.ResumeRunRequest{RunId: id}, h.svc.ResumeRun)
	case "park":
		_, err = call(ctx, godwitv1connect.GodwitServiceParkRunProcedure,
			&godwitv1.ParkRunRequest{RunId: id, Reason: r.FormValue("reason")}, h.svc.ParkRun)
	case "confirm":
		_, err = call(ctx, godwitv1connect.GodwitServiceConfirmRolloutProcedure,
			&godwitv1.ConfirmRolloutRequest{RunId: id}, h.svc.ConfirmRollout)
	case "revert":
		var resp *godwitv1.RevertRunResponse
		resp, err = call(ctx, godwitv1connect.GodwitServiceRevertRunProcedure, &godwitv1.RevertRunRequest{
			RunId: id, AcknowledgeHazards: splitCSV(r.FormValue("ack")),
			Force: r.FormValue("force") != "", AllowDataLoss: r.FormValue("allow-data-loss") != "",
		}, h.svc.RevertRun)
		if err == nil {
			id = resp.RunId
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
	p, err := h.frame(ctx, r, "drift")
	if err != nil {
		h.fail(w, p, err)

		return
	}
	events, err := call(ctx, godwitv1connect.GodwitServiceListDriftEventsProcedure,
		&godwitv1.ListDriftEventsRequest{}, h.svc.ListDriftEvents)
	if err != nil {
		h.fail(w, p, err)

		return
	}
	open := map[string]*godwitv1.DriftEvent{}
	for _, e := range events.Events {
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
	p.Open, p.Locked = open[p.Target], !p.Can["check"]
	for _, e := range events.Events {
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
		resp, err := call(ctx, godwitv1connect.GodwitServiceCheckDriftProcedure,
			&godwitv1.CheckDriftRequest{Target: tgt}, h.svc.CheckDrift)
		if err != nil {
			h.fail(w, p, err)

			return
		}
		dest += "&checked=clean"
		if resp.Drifted {
			dest = "/ui/drift?target=" + tgt + "&checked=drifted"
		}
	case "accept":
		if _, err := call(ctx, godwitv1connect.GodwitServiceAcceptBaselineProcedure,
			&godwitv1.AcceptBaselineRequest{Target: tgt}, h.svc.AcceptBaseline); err != nil {
			h.fail(w, p, err)

			return
		}
	default:
		http.NotFound(w, r)

		return
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}
