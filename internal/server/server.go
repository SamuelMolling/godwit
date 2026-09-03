// Package server assembles and runs the godwit service.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/SamuelMolling/godwit/internal/api"
	"github.com/SamuelMolling/godwit/internal/controlplane"
	"github.com/SamuelMolling/godwit/internal/creds"
	"github.com/SamuelMolling/godwit/internal/metrics"
	"github.com/SamuelMolling/godwit/internal/notify"
	"github.com/SamuelMolling/godwit/internal/version"
)

// Config assembles one godwit service instance.
type Config struct {
	Listen    string
	StoreDSN  string
	MasterKey []byte
	// Tokens are bearer token specs, "name:scope:secret"; "name:secret" and a bare secret (named anonymous) are admin.
	Tokens        []string
	Holder        string
	Scheduler     controlplane.Config
	DriftInterval time.Duration
	WebhookURL    string
	SlackToken    string
	SlackChannel  string
	SlackMode     string
	// SlackURL overrides the Slack API base (tests point it at a fake).
	SlackURL  string
	PublicURL string
	// Notifier is an extra synchronous notifier called in-process (tests, embedding).
	Notifier notify.Notifier
	// SkipValidation disables the scratch-database admission check.
	SkipValidation bool
	RequirePlan    bool
	// PlanTTL is how long a stored plan stays bindable; zero keeps plans forever.
	PlanTTL time.Duration
	// PlanRetention is how long bound and superseded plans are kept; zero keeps them forever.
	PlanRetention time.Duration
	Log           *slog.Logger
	// OnReady receives the bound address once the listener is up.
	OnReady func(addr net.Addr)
}

// Run migrates the store, starts the scheduler and serves the API until ctx ends.
func Run(ctx context.Context, cfg Config) error {
	if cfg.SlackMode == "" {
		cfg.SlackMode = notify.ModeThread
	}
	if cfg.SlackMode != notify.ModeThread && cfg.SlackMode != notify.ModeEdit {
		return fmt.Errorf("slack mode %q: want %s or %s", cfg.SlackMode, notify.ModeThread, notify.ModeEdit)
	}
	if cfg.SlackToken != "" && cfg.SlackChannel == "" {
		return errors.New("slack channel is required when a slack token is set")
	}
	tokens, err := api.ParseTokens(cfg.Tokens)
	if err != nil {
		return err
	}
	pool, err := pgxpool.New(ctx, cfg.StoreDSN)
	if err != nil {
		return err
	}
	defer pool.Close()

	log := cfg.Log.With("replica", cfg.Holder, "build", version.Version)
	applied, err := controlplane.Migrate(ctx, pool)
	if err != nil {
		return err
	}
	log.Info("store migrated", "applied", applied)
	store := controlplane.NewStore(pool)

	m := metrics.New()
	m.WatchRuns(store.RunStats)

	notifier, closeNotifier := newNotifier(cfg, store, log, m.Notified)
	defer closeNotifier()

	cfg.Scheduler.Holder = cfg.Holder
	eng := controlplane.PGEngine{Metrics: m, Log: log}
	sched := controlplane.NewScheduler(store, creds.Registry(cfg.MasterKey),
		eng, controlplane.Policies(), cfg.Scheduler, log)
	sched.Metrics = m
	sched.Notifier = notifier
	go sched.Run(ctx)

	drift := controlplane.NewDriftMonitor(store, sched, eng, notifier, cfg.DriftInterval, log)
	drift.PlanRetention = cfg.PlanRetention
	go drift.Run(ctx)

	newID := func() string { return strings.ReplaceAll(uuid.NewString(), "-", "") }
	var validator api.Validator
	var history controlplane.HistoryReplayer
	if !cfg.SkipValidation {
		v := controlplane.NewValidator(pool, store, newID)
		validator, history = v, v
	}

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	log.Info("listening", "addr", ln.Addr().String(), "validation", !cfg.SkipValidation)
	if cfg.OnReady != nil {
		cfg.OnReady(ln.Addr())
	}

	apiSrv := api.NewServer(store, drift, validator, cfg.MasterKey)
	apiSrv.Metrics = m
	apiSrv.Log = log
	apiSrv.Notifier = notifier
	apiSrv.Baseliner = controlplane.NewBaseliner(sched)
	apiSrv.Inspector = controlplane.NewInspector(sched)
	apiSrv.Differ = controlplane.NewDiffer(pool, sched, history, newID)
	apiSrv.RequirePlan, apiSrv.PlanTTL = cfg.RequirePlan, cfg.PlanTTL
	srv := &http.Server{
		Handler:           api.Handler(apiSrv, tokens),
		ReadHeaderTimeout: 10 * time.Second,
		Protocols:         h2cProtocols(),
	}
	go func() {
		<-ctx.Done()
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	return serve(srv, ln)
}

func newNotifier(cfg Config, store notify.TSStore, log *slog.Logger, record func(provider, result string)) (notify.Notifier, func()) {
	var all notify.Multi
	var async []*notify.Async
	if cfg.Notifier != nil {
		all = append(all, cfg.Notifier)
	}
	if cfg.WebhookURL != "" {
		a := notify.NewAsync("webhook", notify.Webhook{URL: cfg.WebhookURL}, log, record)
		all, async = append(all, a), append(async, a)
	}
	if cfg.SlackToken != "" {
		slack := notify.Slack{
			Token: cfg.SlackToken, Channel: cfg.SlackChannel, Mode: cfg.SlackMode,
			Store: store, BaseURL: cfg.SlackURL, PublicURL: cfg.PublicURL,
		}
		a := notify.NewAsync("slack", slack, log, record)
		all, async = append(all, a), append(async, a)
		log.Info("slack notifications enabled", "channel", cfg.SlackChannel, "mode", cfg.SlackMode)
	}

	return all, func() {
		for _, a := range async {
			a.Close()
		}
	}
}

func serve(srv *http.Server, ln net.Listener) error {
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}

	return nil
}

// h2cProtocols enables unencrypted HTTP/2, which gRPC needs.
func h2cProtocols() *http.Protocols {
	p := new(http.Protocols)
	p.SetHTTP1(true)
	p.SetUnencryptedHTTP2(true)

	return p
}
