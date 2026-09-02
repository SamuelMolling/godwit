// Package metrics exposes the service's Prometheus metrics.
package metrics

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/SamuelMolling/godwit/internal/version"
)

// Resume sources.
const (
	SourceReconciler = "reconciler"
	SourceManual     = "manual"
)

// Drift check results.
const (
	DriftClean    = "clean"
	DriftDrifted  = "drifted"
	DriftAccepted = "accepted"
)

// RunStat is one (target, state) bucket of runs for the runs gauges.
type RunStat struct {
	Target    string
	State     string
	Count     int
	OldestAge time.Duration
}

// Metrics is one registry's worth of godwit metrics.
type Metrics struct {
	registry *prometheus.Registry

	runs               *prometheus.Desc
	runAge             *prometheus.Desc
	resumes            *prometheus.CounterVec
	attempts           prometheus.Histogram
	heartbeatFailures  prometheus.Counter
	runDuration        *prometheus.HistogramVec
	statementDuration  *prometheus.HistogramVec
	statementFailures  *prometheus.CounterVec
	hazards            *prometheus.CounterVec
	validationFailures *prometheus.CounterVec
	driftChecks        *prometheus.CounterVec
	apiRequests        *prometheus.CounterVec
	apiDuration        *prometheus.HistogramVec
}

// New builds a Metrics set on its own registry.
func New() *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(),
		runs: prometheus.NewDesc("godwit_runs", "Runs currently in each state.",
			[]string{"target", "state"}, nil),
		runAge: prometheus.NewDesc("godwit_run_age_seconds", "Age of the oldest run in each state.",
			[]string{"target", "state"}, nil),
		resumes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "godwit_run_resumes_total", Help: "Runs resumed, by who asked for it.",
		}, []string{"target", "source"}),
		attempts: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "godwit_run_attempts", Help: "Attempts a run needed to settle.",
			Buckets: []float64{1, 2, 3, 4, 5},
		}),
		heartbeatFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "godwit_heartbeat_failures_total", Help: "Lease heartbeats that failed.",
		}),
		runDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "godwit_run_duration_seconds", Help: "Wall time of one run attempt.",
			Buckets: prometheus.ExponentialBuckets(0.1, 4, 8),
		}, []string{"target", "result"}),
		statementDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "godwit_statement_duration_seconds", Help: "Time each statement held the target.",
			Buckets: prometheus.ExponentialBuckets(0.01, 4, 8),
		}, []string{"target", "kind"}),
		statementFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "godwit_statement_failures_total", Help: "Statements that failed on the target.",
		}, []string{"target", "reason"}),
		hazards: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "godwit_hazards_total", Help: "Hazards seen at admission.",
		}, []string{"code", "acked"}),
		validationFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "godwit_validation_failures_total", Help: "Runs refused by scratch-database validation.",
		}, []string{"target"}),
		driftChecks: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "godwit_drift_checks_total", Help: "Drift checks, by outcome.",
		}, []string{"target", "result"}),
		apiRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "godwit_api_requests_total", Help: "API calls, by connect code.",
		}, []string{"method", "code"}),
		apiDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "godwit_api_request_duration_seconds", Help: "API call latency.",
			Buckets: prometheus.ExponentialBuckets(0.001, 4, 8),
		}, []string{"method"}),
	}
	buildInfo := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "godwit_build_info", Help: "Build metadata; always 1.",
	}, []string{"version", "commit"})
	buildInfo.WithLabelValues(version.Version, version.Commit).Set(1)
	m.registry.MustRegister(buildInfo, m.resumes, m.attempts, m.heartbeatFailures, m.runDuration,
		m.statementDuration, m.statementFailures, m.hazards, m.validationFailures, m.driftChecks,
		m.apiRequests, m.apiDuration)

	return m
}

// Handler serves the registry in the Prometheus text format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// WatchRuns computes the runs gauges from stats on every scrape.
func (m *Metrics) WatchRuns(stats func(ctx context.Context) ([]RunStat, error)) {
	m.registry.MustRegister(runsCollector{m: m, stats: stats})
}

type runsCollector struct {
	m     *Metrics
	stats func(ctx context.Context) ([]RunStat, error)
}

// Describe implements prometheus.Collector.
func (c runsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.m.runs
	ch <- c.m.runAge
}

// Collect implements prometheus.Collector.
func (c runsCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stats, err := c.stats(ctx)
	if err != nil {
		return
	}
	for _, s := range stats {
		ch <- prometheus.MustNewConstMetric(c.m.runs, prometheus.GaugeValue, float64(s.Count), s.Target, s.State)
		ch <- prometheus.MustNewConstMetric(c.m.runAge, prometheus.GaugeValue, s.OldestAge.Seconds(), s.Target, s.State)
	}
}

// RunClaimed records a claim; a second attempt means the reconciler resumed the run.
func (m *Metrics) RunClaimed(target string, attempts int) {
	if attempts > 1 {
		m.resumes.WithLabelValues(target, SourceReconciler).Inc()
	}
}

// RunResumed records an operator resume.
func (m *Metrics) RunResumed(target string) {
	m.resumes.WithLabelValues(target, SourceManual).Inc()
}

// RunFinished records how one attempt settled.
func (m *Metrics) RunFinished(target, state string, attempts int, d time.Duration) {
	m.attempts.Observe(float64(attempts))
	m.runDuration.WithLabelValues(target, state).Observe(d.Seconds())
}

// HeartbeatFailed records a lost heartbeat.
func (m *Metrics) HeartbeatFailed() {
	m.heartbeatFailures.Inc()
}

// Statement records one statement's execution on a target.
func (m *Metrics) Statement(target, kind string, d time.Duration, err error) {
	m.statementDuration.WithLabelValues(target, kind).Observe(d.Seconds())
	if err != nil {
		m.statementFailures.WithLabelValues(target, failureReason(err)).Inc()
	}
}

func failureReason(err error) string {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return "error"
	}
	switch pgErr.Code {
	case "55P03":
		return "lock_timeout"
	case "57014":
		return "statement_timeout"
	default:
		return "sqlstate_" + pgErr.Code
	}
}

// Hazard records one hazard seen at admission.
func (m *Metrics) Hazard(code string, acked bool) {
	m.hazards.WithLabelValues(code, map[bool]string{true: "true", false: "false"}[acked]).Inc()
}

// ValidationFailed records a run refused by validation.
func (m *Metrics) ValidationFailed(target string) {
	m.validationFailures.WithLabelValues(target).Inc()
}

// DriftChecked records one drift check outcome.
func (m *Metrics) DriftChecked(target, result string) {
	m.driftChecks.WithLabelValues(target, result).Inc()
}

// Interceptor counts and times every API call.
func (m *Metrics) Interceptor() connect.Interceptor {
	return interceptor{m: m}
}

type interceptor struct {
	m *Metrics
}

func (i interceptor) observe(spec connect.Spec, start time.Time, err error) {
	method := spec.Procedure[strings.LastIndex(spec.Procedure, "/")+1:]
	code := "ok"
	if err != nil {
		code = connect.CodeOf(err).String()
	}
	i.m.apiRequests.WithLabelValues(method, code).Inc()
	i.m.apiDuration.WithLabelValues(method).Observe(time.Since(start).Seconds())
}

// WrapUnary implements connect.Interceptor.
func (i interceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		start := time.Now()
		resp, err := next(ctx, req)
		i.observe(req.Spec(), start, err)

		return resp, err
	}
}

// WrapStreamingClient implements connect.Interceptor.
func (interceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

// WrapStreamingHandler implements connect.Interceptor.
func (i interceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		start := time.Now()
		err := next(ctx, conn)
		i.observe(conn.Spec(), start, err)

		return err
	}
}
