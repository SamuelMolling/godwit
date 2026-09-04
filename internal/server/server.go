// Package server assembles and runs the godwit service.
package server

import (
	"cmp"
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
	"github.com/SamuelMolling/godwit/internal/ui"
	"github.com/SamuelMolling/godwit/internal/version"
)

// Config assembles one godwit service instance.
type Config struct {
	Listen   string
	StoreDSN string
	// ScratchDSN is the PostgreSQL validation and diff create their throwaway databases on; empty runs
	// them on the store server with the store's own credentials.
	ScratchDSN string
	// ScratchTemplate is the database scratch databases are cloned from; empty means template0.
	ScratchTemplate string
	// Keys seals and opens the DSNs of static targets; the zero value holds no key, and a deployment
	// whose targets all use vault or kubernetes runs with it.
	Keys creds.Keyring
	// Tokens are bearer token specs, "name:scope:secret"; a bare secret is an admin token named anonymous.
	Tokens []string
	// Holder is this replica's identity in leases, log lines and the UI; empty takes controlplane.NewHolder.
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
	// StoreMaxConns caps the API pool against the store; zero takes the default. It wins over
	// pool_max_conns in the DSN.
	StoreMaxConns int
	// Limits are the API admission bounds; a zero field takes its default.
	Limits api.Limits
	// SkipValidation disables the scratch-database admission check.
	SkipValidation bool
	RequirePlan    bool
	// PlanTTL is how long a stored plan stays bindable; zero keeps plans forever.
	PlanTTL time.Duration
	// PlanRetention is how long bound and superseded plans are kept; zero keeps them forever.
	PlanRetention time.Duration
	// UI serves the operator web UI under /ui/. Any Tokens secret is accepted as the basic-auth password;
	// UIUser and UIPassword add a shared identity whose rights are UIScope (default operator).
	UI         bool
	UIUser     string
	UIPassword string
	UIScope    string
	// UIOrigins are the scheme://host[:port] origins /ui is reached at; empty compares the browser's Origin with the request Host.
	UIOrigins []string
	Log       *slog.Logger
	// OnReady receives the bound address once the listener is up.
	OnReady func(addr net.Addr)
}

// DefaultStoreMaxConns bounds the pool the API handlers, the scheduler, the drift monitor, the validator
// and the scratch factory share. Unsized, pgx defaults it to max(4, NumCPU), which is both unpredictable
// across nodes and small enough that a burst of Diff calls leaves the scheduler waiting on Acquire.
const DefaultStoreMaxConns = 20

func openPool(ctx context.Context, dsn string, maxConns int) (*pgxpool.Pool, error) {
	pcfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	pcfg.MaxConns = int32(maxConns)

	return pgxpool.NewWithConfig(ctx, pcfg)
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
	if (cfg.UIUser == "") != (cfg.UIPassword == "") {
		return errors.New("ui user and ui password must be set together")
	}
	uiScope := api.ScopeOperator
	if cfg.UIScope != "" {
		s, err := api.ParseScope(cfg.UIScope)
		if err != nil {
			return fmt.Errorf("ui scope: %w", err)
		}
		uiScope = s
	}
	origins, err := ui.ParseOrigins(cfg.UIOrigins)
	if err != nil {
		return err
	}
	tokens, err := api.ParseTokens(cfg.Tokens)
	if err != nil {
		return err
	}
	pool, err := openPool(ctx, cfg.StoreDSN, cmp.Or(cfg.StoreMaxConns, DefaultStoreMaxConns))
	if err != nil {
		return err
	}
	defer pool.Close()

	cfg.Holder = cmp.Or(cfg.Holder, controlplane.NewHolder(""))
	log := cfg.Log.With("replica", cfg.Holder, "build", version.Version)
	if len(tokens) == 0 {
		log.Warn("no tokens configured; every caller is anonymous with scope admin")
	}
	applied, err := controlplane.Migrate(ctx, pool)
	if err != nil {
		return err
	}
	log.Info("store migrated", "applied", applied)
	store := controlplane.NewStore(pool)
	settleKeys(ctx, store, cfg.Keys, log)

	scratch, closeScratch, err := newScratch(ctx, cfg, pool, log)
	if err != nil {
		return err
	}
	defer closeScratch()

	m := metrics.New()
	m.WatchRuns(store.RunStats)

	notifier, closeNotifier := newNotifier(cfg, store, log, m.Notified)
	defer closeNotifier()

	cfg.Scheduler.Holder = cfg.Holder
	eng := controlplane.PGEngine{Metrics: m, Log: log}
	sched := controlplane.NewScheduler(store, creds.Registry(cfg.Keys),
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
		v := controlplane.NewValidator(scratch, store, newID)
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

	apiSrv := api.NewServer(store, drift, validator, cfg.Keys)
	apiSrv.Metrics = m
	apiSrv.Log = log
	apiSrv.Notifier = notifier
	apiSrv.Baseliner = controlplane.NewBaseliner(sched)
	apiSrv.Reconciler = controlplane.NewReconciler(sched)
	apiSrv.Inspector = controlplane.NewInspector(sched)
	apiSrv.Differ = controlplane.NewDiffer(scratch, sched, history, newID)
	apiSrv.Checkpointer = controlplane.NewCheckpointer(scratch, newID)
	apiSrv.RequirePlan, apiSrv.PlanTTL = cfg.RequirePlan, cfg.PlanTTL
	apiSrv.Limits = cfg.Limits
	handler := api.Handler(apiSrv, tokens)
	if cfg.UI {
		if cfg.UIUser == "" && len(tokens) == 0 {
			log.Warn("ui enabled without basic auth", "anonymous_scope", string(ui.AnonymousScope))
		}
		mux := http.NewServeMux()
		mux.Handle("/", handler)
		mux.Handle("/ui/", ui.New(apiSrv, ui.Config{
			Replica: cfg.Holder, Tokens: tokens, User: cfg.UIUser, Password: cfg.UIPassword,
			Scope: uiScope, Origins: origins,
		}))
		handler = mux
	}
	// No ReadTimeout or WriteTimeout: both are per-stream in HTTP/2 and per-connection in HTTP/1, and
	// WatchRun holds a response open for the length of a run. Request bodies are bounded by size instead.
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
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
