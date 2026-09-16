package ui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/internal/controlplane"
)

type eventsFail struct {
	*stub
}

func (e *eventsFail) ListDriftEvents(context.Context, *connect.Request[godwitv1.ListDriftEventsRequest]) (*connect.Response[godwitv1.ListDriftEventsResponse], error) {
	return nil, connect.NewError(connect.CodeUnavailable, errors.New("events down"))
}

func listed(name string) string { return ">" + name + "</a>" }

func before(t *testing.T, body, first, second string) {
	t.Helper()
	i, j := strings.Index(body, first), strings.Index(body, second)
	if i < 0 || j < 0 || i > j {
		t.Fatalf("%q at %d must come before %q at %d", first, i, second, j)
	}
}

func fleetOfTargets(n int) *stub {
	s := &stub{}
	for i := range n {
		sum := &godwitv1.TargetSummary{Name: fmt.Sprintf("shard-%03d", i), Provider: "static"}
		if i%40 == 7 {
			sum.UnresolvedDrift = true
			s.events = append(s.events, &godwitv1.DriftEvent{
				Id: int64(i), Target: sum.Name, DetectedAt: at(time.Hour),
				Diff: "+ column public.orders.settled_at timestamp with time zone null=YES default=<none>",
			})
		}
		if i%150 == 3 {
			sum.AttentionRuns = 1
		}
		s.sums = append(s.sums, sum)
	}

	return s
}

func TestDriftOpensOnWhatDrifted(t *testing.T) {
	t.Parallel()
	s := fixture()
	h := newUI(s, Config{})

	rec := do(h, http.MethodGet, "/ui/drift", nil)
	want(t, rec, http.StatusOK, "1 of 2 targets", listed("app"), `class="pill drifted">drifted`,
		`drifted <span class="cnt">1`, `all <span class="cnt">2`, "1–1 of 1 drifted · 2 targets registered", "Page 1 of 1")
	absent(t, rec, listed("billing"), "Accept as baseline")
	for _, c := range s.calls {
		if c == "ListDriftEvents" {
			t.Fatalf("the overview must answer from the target summaries, not the capped event window: %v", s.calls)
		}
	}

	all := do(h, http.MethodGet, "/ui/drift?scope=all", nil)
	want(t, all, http.StatusOK, "All targets", "1 of 2 drifted", listed("app"), listed("billing"), `class="pill succeeded">clean`, "1–2 of 2 · 2 targets registered")
	before(t, all.Body.String(), listed("app"), listed("billing"))

	rec = do(h, http.MethodGet, "/ui/drift?scope=all&q=BILL", nil)
	want(t, rec, http.StatusOK, listed("billing"), "1–1 of 1 matching “BILL”", `href="/ui/drift?scope=all"`)
	absent(t, rec, listed("app"))

	want(t, do(h, http.MethodGet, "/ui/drift?q=nothing", nil), http.StatusOK,
		"No drifted target matches “nothing”", "2 targets are registered")
	want(t, do(h, http.MethodGet, "/ui/drift?scope=all&q=nothing", nil), http.StatusOK, "No target matches “nothing”")

	s.sums = []*godwitv1.TargetSummary{{Name: "app"}}
	want(t, do(h, http.MethodGet, "/ui/drift", nil), http.StatusOK, "Nothing has drifted", "All 1 targets match")
	want(t, do(newUI(&stub{}, Config{}), http.MethodGet, "/ui/drift", nil), http.StatusOK, "No targets yet")
	want(t, do(newUI(&stub{err: errBoom}, Config{}), http.MethodGet, "/ui/drift", nil), http.StatusBadGateway, "boom")
}

func TestDriftAtFleetScale(t *testing.T) {
	t.Parallel()
	h := newUI(fleetOfTargets(300), Config{})

	rec := do(h, http.MethodGet, "/ui/drift", nil)
	want(t, rec, http.StatusOK, "8 of 300 targets", listed("shard-007"), listed("shard-287"), "1–8 of 8 drifted · 300 targets registered")
	absent(t, rec, listed("shard-008"))

	all := do(h, http.MethodGet, "/ui/drift?scope=all", nil)
	want(t, all, http.StatusOK, "1–50 of 300", "Page 1 of 6", `href="/ui/drift?page=2&amp;scope=all"`)
	absent(t, all, listed("shard-299"))
	before(t, all.Body.String(), listed("shard-287"), listed("shard-000"))
	if n := strings.Count(all.Body.String(), `<td class="id">`); n != driftPageSize {
		t.Fatalf("rendered %d rows, want %d", n, driftPageSize)
	}

	want(t, do(h, http.MethodGet, "/ui/drift?scope=all&page=2", nil), http.StatusOK,
		"51–100 of 300", "Page 2 of 6", `href="/ui/drift?scope=all"`, `href="/ui/drift?page=3&amp;scope=all"`)
	want(t, do(h, http.MethodGet, "/ui/drift?scope=all&page=99", nil), http.StatusOK, "251–300 of 300", "Page 6 of 6")
	want(t, do(h, http.MethodGet, "/ui/drift?scope=all&page=nope", nil), http.StatusOK, "1–50 of 300")

	rec = do(h, http.MethodGet, "/ui/drift?scope=all&q=shard-29", nil)
	want(t, rec, http.StatusOK, "1–10 of 10 matching “shard-29” · 300 targets registered", listed("shard-299"))
	absent(t, rec, listed("shard-000"))

	rail := do(h, http.MethodGet, "/ui/drift", nil).Body.String()
	if n := strings.Count(rail, `class="dot`); n != railTargets {
		t.Fatalf("the sidebar listed %d targets, want %d", n, railTargets)
	}
	if !strings.Contains(rail, "288 more") || !strings.Contains(rail, "/ui/targets") {
		t.Fatalf("the sidebar must say how many it left out:\n%s", rail)
	}
	if !strings.Contains(rail, `<span class="lbl">shard-003</span>`) {
		t.Fatalf("a target waiting for a human must survive the sidebar cap:\n%s", rail)
	}

	far := do(h, http.MethodGet, "/ui/drift?target=shard-299", nil).Body.String()
	if !strings.Contains(far, `<span class="lbl">shard-299</span>`) {
		t.Fatalf("the open target must survive the sidebar cap:\n%s", far)
	}
}

func TestDriftTarget(t *testing.T) {
	t.Parallel()
	s := fixture()
	h := newUI(s, Config{})

	want(t, do(h, http.MethodGet, "/ui/drift?target=app", nil), http.StatusOK,
		"app drifted from its baseline", "Detected 1 min ago", "2 recorded events",
		"&#43; column public.widgets.status text null=YES default=&#39;draft&#39;::text",
		"- index public.widgets_created_at_idx CREATE INDEX", "resolved",
		`<a href="/ui/drift">Drift</a>`, `href="/ui/targets/app"`, "Accept as baseline")
	want(t, do(h, http.MethodGet, "/ui/drift?target=billing&checked=clean", nil), http.StatusOK,
		"billing matches its baseline", "Checked just now", "public.invoices.retried_at")
	want(t, do(h, http.MethodGet, "/ui/drift?target=app&checked=drifted", nil), http.StatusOK, "confirmed by the check you just ran")
	want(t, do(h, http.MethodGet, "/ui/drift?target=ghost", nil), http.StatusOK, "No open drift event", "No drift recorded")
	if last := s.calls[len(s.calls)-1]; last != "ListDriftEvents" {
		t.Fatalf("a target's events must be asked for by target: %v", s.calls)
	}

	s.events = nil
	for i := range controlplane.DriftHistoryLimit {
		s.events = append(s.events, &godwitv1.DriftEvent{Id: int64(i), Target: "app", DetectedAt: at(time.Hour), ResolvedAt: at(time.Minute)})
	}
	want(t, do(h, http.MethodGet, "/ui/drift?target=app", nil), http.StatusOK,
		"the 100 most recent events", "older events are not in this window")

	want(t, do(newUI(&eventsFail{stub: fixture()}, Config{}), http.MethodGet, "/ui/drift?target=app", nil),
		http.StatusBadGateway, "events down")
}

func TestDriftActions(t *testing.T) {
	t.Parallel()
	s := fixture()
	h := newUI(s, Config{})

	redirect(t, do(h, http.MethodPost, "/ui/drift/app/check", nil), "/ui/drift?target=app&checked=drifted")
	s.drift = &godwitv1.CheckDriftResponse{}
	redirect(t, do(h, http.MethodPost, "/ui/drift/app/check", nil), "/ui/drift?target=app&checked=clean")
	redirect(t, do(h, http.MethodPost, "/ui/drift/app/accept", nil), "/ui/drift?target=app")
	if s.calls[len(s.calls)-1] != "AcceptBaseline:app" {
		t.Fatalf("calls = %v", s.calls)
	}
	if rec := do(h, http.MethodPost, "/ui/drift/app/explode", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("code = %d", rec.Code)
	}

	s.err = connect.NewError(connect.CodeInternal, errBoom)
	want(t, do(h, http.MethodPost, "/ui/drift/app/check", nil), http.StatusBadGateway, "boom")
	want(t, do(h, http.MethodPost, "/ui/drift/app/accept", nil), http.StatusBadGateway, "boom")
}
