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

	"github.com/SamuelMolling/godwit/internal/authz"
	"github.com/SamuelMolling/godwit/internal/controlplane"
	"github.com/SamuelMolling/godwit/internal/creds"
	"github.com/SamuelMolling/godwit/internal/limits"
	"github.com/SamuelMolling/godwit/internal/metrics"
	"github.com/SamuelMolling/godwit/internal/notify"
	"github.com/SamuelMolling/godwit/internal/surface/api"
	"github.com/SamuelMolling/godwit/internal/surface/ui"
	"github.com/SamuelMolling/godwit/internal/version"
)

// Config assembles one godwit service instance.
type Config struct {
	Listen           string
	StoreDSN         string
	ScratchDSN       string
	ScratchTemplate  string
	Keys             creds.Keyring
	Tokens           []string
	Holder           string
	Scheduler        controlplane.Config
	DriftInterval    time.Duration
	WebhookURL       string
	SlackToken       string
	SlackChannel     string
	SlackMode        string
	SlackURL         string
	PublicURL        string
	Notifier         notify.Notifier
	StoreMaxConns    int
	Limits           limits.Limits
	SkipValidation   bool
	RequirePlan      bool
	PlanTTL          time.Duration
	PlanRetention    time.Duration
	GitHub           GitHubApp
	UI               bool
	UIUser           string
	UIPassword       string
	UIScope          string
	UIAnonymousScope string
	UIOrigins        []string
	ShutdownTimeout  time.Duration
	Log              *slog.Logger
	OnReady          func(addr net.Addr)
}

// DefaultStoreMaxConns sizes the pool every component shares; unsized, pgx takes max(4, NumCPU) and a burst of Diff calls leaves the scheduler waiting on Acquire.
const DefaultStoreMaxConns = 20

// DefaultShutdownTimeout sits under the 30 seconds Kubernetes, ECS and systemd default their kill delay to, so the process exits on its own first.
const DefaultShutdownTimeout = 20 * time.Second

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
	if cfg.UIAnonymousScope != "" && cfg.UIUser != "" {
		return errors.New("--ui-anonymous-scope serves /ui unauthenticated; GODWIT_UI_USER then " +
			"configures an identity nobody is ever asked for")
	}
	uiScope := authz.ScopeOperator
	if cfg.UIScope != "" {
		s, err := authz.ParseScope(cfg.UIScope)
		if err != nil {
			return fmt.Errorf("ui scope: %w", err)
		}
		uiScope = s
	}
	var anonScope authz.Scope
	if cfg.UIAnonymousScope != "" {
		s, err := authz.ParseScope(cfg.UIAnonymousScope)
		if err != nil {
			return fmt.Errorf("ui anonymous scope: %w", err)
		}
		anonScope = s
	}
	githubKey, err := cfg.GitHub.credential()
	if err != nil {
		return err
	}
	origins, err := ui.ParseOrigins(cfg.UIOrigins)
	if err != nil {
		return err
	}
	tokens, err := authz.ParseTokens(cfg.Tokens)
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
	sched := controlplane.NewScheduler(store, creds.Registry(cfg.Keys, store.VaultStore),
		eng, controlplane.Policies(), cfg.Scheduler, log)
	sched.Metrics = m
	sched.Notifier = notifier
	// Runs outlive the signal that ends ctx: shutdown drains them, and only the grace period aborts them.
	runCtx, abortRuns := context.WithCancel(context.WithoutCancel(ctx))
	defer abortRuns()
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		sched.Run(runCtx)
	}()

	drift := controlplane.NewDriftMonitor(store, sched, eng, notifier, cfg.DriftInterval, log)
	drift.PlanRetention = cfg.PlanRetention
	if cfg.GitHub.enabled() {
		drift.DeliveryRetention = deliveryRetention(cfg.GitHub.MaxAge)
	}
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

	stopWebhook, err := serveWebhook(cfg, githubKey, store, apiSrv, m, log)
	if err != nil {
		return err
	}

	handler := api.Handler(apiSrv, tokens)
	if cfg.UI {
		switch {
		case anonScope != "":
			log.Warn("ui served without authentication", "scope", string(anonScope),
				"detail", "anyone who can reach this listener may use /ui at this scope; "+
					"actions are audited as ui:anonymous, with no identity behind them")
		case cfg.UIUser == "" && len(tokens) == 0:
			log.Warn("ui enabled without basic auth", "anonymous_scope", string(ui.AnonymousScope))
		}
		mux := http.NewServeMux()
		mux.Handle("/", handler)
		mux.Handle("/ui/", ui.New(apiSrv, ui.Config{
			Replica: cfg.Holder, Tokens: tokens, User: cfg.UIUser, Password: cfg.UIPassword,
			Scope: uiScope, Origins: origins,
			Anonymous: anonScope != "", AnonymousScope: anonScope,
		}))
		handler = mux
	}
	// No ReadTimeout or WriteTimeout: WatchRun holds a response open for the length of a run; request bodies are bounded by size instead.
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
		Protocols:         h2cProtocols(),
	}
	grace := cmp.Or(cfg.ShutdownTimeout, DefaultShutdownTimeout)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		<-ctx.Done()
		log.Info("shutting down", "grace", grace)
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), grace)
		defer cancel()
		stopWebhook(shutdownCtx)
		_ = srv.Shutdown(shutdownCtx)
		awaitRuns(shutdownCtx, sched.Stop, drained, log)
	}()

	if err := serve(srv, ln); err != nil {
		return err
	}
	<-stopped

	return nil
}

// awaitRuns holds the process open for the runs this replica claimed: their leases still beat, so abandoning them costs a whole lease TTL.
func awaitRuns(ctx context.Context, stopClaiming func(), drained <-chan struct{}, log *slog.Logger) {
	stopClaiming()
	select {
	case <-drained:
		log.Info("runs drained")
	case <-ctx.Done():
		log.Warn("grace period expired with runs still executing; their leases will requeue them")
	}
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

func h2cProtocols() *http.Protocols {
	p := new(http.Protocols)
	p.SetHTTP1(true)
	p.SetUnencryptedHTTP2(true)

	return p
}
