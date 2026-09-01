// Package server assembles and runs the godwit service.
package server

import (
	"context"
	"errors"
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
	"github.com/SamuelMolling/godwit/internal/notify"
)

// Config assembles one godwit service instance.
type Config struct {
	Listen        string
	StoreDSN      string
	MasterKey     []byte
	Tokens        []string
	Holder        string
	Scheduler     controlplane.Config
	DriftInterval time.Duration
	WebhookURL    string
	// SkipValidation disables the scratch-database admission check.
	SkipValidation bool
	Log            *slog.Logger
	// OnReady receives the bound address once the listener is up (tests).
	OnReady func(addr net.Addr)
}

// Run migrates the store, starts the scheduler and serves the API until ctx ends.
func Run(ctx context.Context, cfg Config) error {
	pool, err := pgxpool.New(ctx, cfg.StoreDSN)
	if err != nil {
		return err
	}
	defer pool.Close()

	if err := controlplane.Migrate(ctx, pool); err != nil {
		return err
	}
	store := controlplane.NewStore(pool)

	cfg.Scheduler.Holder = cfg.Holder
	eng := controlplane.PGEngine{}
	sched := controlplane.NewScheduler(store, creds.Registry(cfg.MasterKey),
		eng, controlplane.Immediate{}, cfg.Scheduler, cfg.Log)
	go sched.Run(ctx)

	var notifier notify.Notifier = notify.None{}
	if cfg.WebhookURL != "" {
		notifier = notify.Webhook{URL: cfg.WebhookURL}
	}
	drift := controlplane.NewDriftMonitor(store, sched, eng, notifier, cfg.DriftInterval, cfg.Log)
	go drift.Run(ctx)

	var validator api.Validator
	if !cfg.SkipValidation {
		validator = controlplane.NewValidator(pool, store, func() string {
			return strings.ReplaceAll(uuid.NewString(), "-", "")
		})
	}

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	if cfg.OnReady != nil {
		cfg.OnReady(ln.Addr())
	}

	srv := &http.Server{
		Handler:           api.Handler(api.NewServer(store, drift, validator, cfg.MasterKey), cfg.Tokens),
		ReadHeaderTimeout: 10 * time.Second,
		Protocols:         h2cProtocols(),
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	return serve(srv, ln)
}

func serve(srv *http.Server, ln net.Listener) error {
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}

	return nil
}

// h2cProtocols enables unencrypted HTTP/2 alongside HTTP/1 (gRPC needs h2).
func h2cProtocols() *http.Protocols {
	p := new(http.Protocols)
	p.SetHTTP1(true)
	p.SetUnencryptedHTTP2(true)

	return p
}
