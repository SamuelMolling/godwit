package metrics

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgconn"
)

var errBoom = errors.New("boom")

func scrape(t *testing.T, m *Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	return rec.Body.String()
}

func expect(t *testing.T, body string, lines ...string) {
	t.Helper()
	for _, line := range lines {
		if !strings.Contains(body, line) {
			t.Fatalf("missing %q in:\n%s", line, body)
		}
	}
}

func TestEvents(t *testing.T) {
	t.Parallel()
	m := New()

	m.RunClaimed("app", 1)
	m.RunClaimed("app", 2)
	m.RunResumed("app")
	m.RunFinished("app", "failed", 3, 2*time.Second)
	m.HeartbeatFailed()
	m.Statement("app", "tx", time.Millisecond, nil)
	m.Statement("app", "tx", time.Millisecond, &pgconn.PgError{Code: "55P03"})
	m.Statement("app", "no_tx", time.Millisecond, &pgconn.PgError{Code: "57014"})
	m.Statement("app", "no_tx", time.Millisecond, &pgconn.PgError{Code: "42P01"})
	m.Statement("app", "tx", time.Millisecond, errBoom)
	m.Hazard("H001", true)
	m.Hazard("H002", false)
	m.ValidationFailed("app")
	m.DriftChecked("app", DriftDrifted)
	m.Notified("slack", "delivered")

	expect(t, scrape(t, m),
		`godwit_build_info{commit="none",version="dev"} 1`,
		`godwit_run_resumes_total{source="reconciler",target="app"} 1`,
		`godwit_run_resumes_total{source="manual",target="app"} 1`,
		`godwit_run_attempts_bucket{le="3"} 1`,
		`godwit_run_duration_seconds_count{result="failed",target="app"} 1`,
		`godwit_heartbeat_failures_total 1`,
		`godwit_statement_duration_seconds_count{kind="tx",target="app"} 3`,
		`godwit_statement_failures_total{reason="lock_timeout",target="app"} 1`,
		`godwit_statement_failures_total{reason="statement_timeout",target="app"} 1`,
		`godwit_statement_failures_total{reason="sqlstate_42P01",target="app"} 1`,
		`godwit_statement_failures_total{reason="error",target="app"} 1`,
		`godwit_hazards_total{acked="true",code="H001"} 1`,
		`godwit_hazards_total{acked="false",code="H002"} 1`,
		`godwit_validation_failures_total{target="app"} 1`,
		`godwit_drift_checks_total{result="drifted",target="app"} 1`,
		`godwit_notifications_total{provider="slack",result="delivered"} 1`,
	)
}

func TestWatchRuns(t *testing.T) {
	t.Parallel()

	m := New()
	m.WatchRuns(func(context.Context) ([]RunStat, error) {
		return []RunStat{{Target: "app", State: "queued", Count: 2, OldestAge: 90 * time.Second}}, nil
	})
	expect(t, scrape(t, m),
		`godwit_runs{state="queued",target="app"} 2`,
		`godwit_run_age_seconds{state="queued",target="app"} 90`,
	)

	broken := New()
	broken.WatchRuns(func(context.Context) ([]RunStat, error) { return nil, errBoom })
	if body := scrape(t, broken); strings.Contains(body, "godwit_runs{") {
		t.Fatalf("stats error must yield no samples:\n%s", body)
	}
}

type fakeConn struct {
	connect.StreamingHandlerConn
}

func (fakeConn) Spec() connect.Spec {
	return connect.Spec{Procedure: "/godwit.v1.GodwitService/WatchRun"}
}

func TestInterceptor(t *testing.T) {
	t.Parallel()
	m := New()
	i := m.Interceptor()

	unary := i.WrapUnary(func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) {
		return nil, connect.NewError(connect.CodeNotFound, errBoom)
	})
	if _, err := unary(context.Background(), connect.NewRequest(&struct{}{})); err == nil {
		t.Fatal("want error")
	}
	ok := i.WrapUnary(func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) {
		return nil, nil
	})
	if _, err := ok(context.Background(), connect.NewRequest(&struct{}{})); err != nil {
		t.Fatal(err)
	}
	stream := i.WrapStreamingHandler(func(context.Context, connect.StreamingHandlerConn) error { return nil })
	if err := stream(context.Background(), fakeConn{}); err != nil {
		t.Fatal(err)
	}
	called := false
	next := connect.StreamingClientFunc(func(context.Context, connect.Spec) connect.StreamingClientConn {
		called = true

		return nil
	})
	i.WrapStreamingClient(next)(context.Background(), connect.Spec{})
	if !called {
		t.Fatal("streaming client must pass through")
	}

	expect(t, scrape(t, m),
		`godwit_api_requests_total{code="not_found",method=""} 1`,
		`godwit_api_requests_total{code="ok",method=""} 1`,
		`godwit_api_requests_total{code="ok",method="WatchRun"} 1`,
		`godwit_api_request_duration_seconds_count{method="WatchRun"} 1`,
	)
}
