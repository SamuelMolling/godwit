package ui

import (
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/gen/godwit/v1/godwitv1connect"
	"github.com/SamuelMolling/godwit/internal/controlplane"
)

const (
	driftPageSize = 50
	scopeAll      = "all"
)

type driftRow struct {
	Name    string
	Drifted bool
	Waiting int32
	Run     *godwitv1.Run
}

type driftChip struct {
	Name  string
	Count int
	On    bool
	Href  string
}

type driftList struct {
	Query   string
	OnlyBad bool
	Chips   []driftChip
	Rows    []driftRow
	Matched int
	Total   int
	Drifted int
	Page    int
	Pages   int
	From    int
	To      int
	Prev    string
	Next    string
	Clear   string
}

func driftHref(query, scope string, page int) string {
	q := url.Values{}
	if query != "" {
		q.Set("q", query)
	}
	if scope != "" {
		q.Set("scope", scope)
	}
	if page > 1 {
		q.Set("page", strconv.Itoa(page))
	}
	if len(q) == 0 {
		return "/ui/drift"
	}

	return "/ui/drift?" + q.Encode()
}

func (d *driftList) scope() string {
	if d.OnlyBad {
		return ""
	}

	return scopeAll
}

func (d *driftList) window(rows []driftRow, page int) {
	d.Matched = len(rows)
	d.Pages = max(1, (len(rows)+driftPageSize-1)/driftPageSize)
	d.Page = min(max(page, 1), d.Pages)
	first := (d.Page - 1) * driftPageSize
	d.Rows = rows[first:min(first+driftPageSize, len(rows))]
	if len(d.Rows) > 0 {
		d.From, d.To = first+1, first+len(d.Rows)
	}
	if d.Page > 1 {
		d.Prev = driftHref(d.Query, d.scope(), d.Page-1)
	}
	if d.Page < d.Pages {
		d.Next = driftHref(d.Query, d.scope(), d.Page+1)
	}
}

func nameMatches(name, query string) bool {
	return strings.Contains(strings.ToLower(name), strings.ToLower(query))
}

func driftListOf(sums []*godwitv1.TargetSummary, q url.Values) *driftList {
	d := &driftList{Query: strings.TrimSpace(q.Get("q")), OnlyBad: q.Get("scope") != scopeAll, Total: len(sums)}
	var rows []driftRow
	for _, s := range sums {
		if s.UnresolvedDrift {
			d.Drifted++
		}
		if !nameMatches(s.Name, d.Query) || (d.OnlyBad && !s.UnresolvedDrift) {
			continue
		}
		rows = append(rows, driftRow{Name: s.Name, Drifted: s.UnresolvedDrift, Waiting: s.AttentionRuns, Run: s.LastRun})
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Drifted && !rows[j].Drifted })
	d.Chips = []driftChip{
		{Name: "drifted", Count: d.Drifted, On: d.OnlyBad, Href: driftHref(d.Query, "", 1)},
		{Name: "all", Count: d.Total, On: !d.OnlyBad, Href: driftHref(d.Query, scopeAll, 1)},
	}
	d.Clear = driftHref("", d.scope(), 1)
	d.window(rows, pageNo(q.Get("page")))

	return d
}

func pageNo(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 1
	}

	return n
}

func atCap(events []*godwitv1.DriftEvent) bool {
	return len(events) >= controlplane.DriftHistoryLimit
}

func recorded(events []*godwitv1.DriftEvent) string {
	if atCap(events) {
		return "the " + strconv.Itoa(len(events)) + " most recent events"
	}

	return plural(len(events), "recorded event")
}

func (h *Handler) drift(w http.ResponseWriter, r *http.Request) {
	p, err := h.frame(r.Context(), r, "drift")
	if err != nil {
		h.fail(w, p, err)

		return
	}
	if name := r.URL.Query().Get("target"); name != "" {
		h.driftTarget(w, r, p, name)

		return
	}
	p.Drift = driftListOf(p.Summaries, r.URL.Query())
	h.render(w, http.StatusOK, "drift.html", p)
}

func (h *Handler) driftTarget(w http.ResponseWriter, r *http.Request, p page, name string) {
	events, err := call(r.Context(), godwitv1connect.GodwitServiceListDriftEventsProcedure,
		&godwitv1.ListDriftEventsRequest{Target: name}, h.svc.ListDriftEvents)
	if err != nil {
		h.fail(w, p, err)

		return
	}
	p.Target, p.Checked, p.Events = name, r.URL.Query().Get("checked"), events.Events
	for _, e := range p.Events {
		if e.ResolvedAt == nil {
			p.Open = e

			break
		}
	}
	p.Locked = !p.Can["check"]
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
