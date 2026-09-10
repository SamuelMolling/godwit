package server

import (
	"context"
	"crypto"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/SamuelMolling/godwit/internal/api"
	"github.com/SamuelMolling/godwit/internal/controlplane"
	"github.com/SamuelMolling/godwit/internal/githubapp"
	"github.com/SamuelMolling/godwit/internal/metrics"
)

// GitHubApp configures the App webhook listener, which is its own listener rather than a path on the API's.
type GitHubApp struct {
	Addr          string
	Secret        string
	AppID         string
	PrivateKeyPEM string
	APIBaseURL    string
	MaxBodyBytes  int
	MaxAge        time.Duration
	Associations  []string
	Reaction      string
	// Workers bounds how many accepted commands the App carries out at once.
	Workers int
	OnReady func(addr net.Addr)
}

const (
	deliveryRetentionFactor = 4
	minDeliveryRetention    = 24 * time.Hour
)

func deliveryRetention(maxAge time.Duration) time.Duration {
	return max(minDeliveryRetention, deliveryRetentionFactor*maxAge)
}

func (g GitHubApp) enabled() bool { return g.Addr != "" }

func (g GitHubApp) credential() (crypto.Signer, error) {
	if !g.enabled() {
		return nil, nil
	}
	if g.Secret == "" {
		return nil, errors.New("--github-webhook-addr needs a webhook secret (GODWIT_GITHUB_WEBHOOK_SECRET): " +
			"an unverified endpoint is an open one")
	}
	if g.AppID == "" {
		return nil, errors.New("--github-webhook-addr needs a github app id (GODWIT_GITHUB_APP_ID)")
	}

	return parsePrivateKey(g.PrivateKeyPEM)
}

func (g GitHubApp) receiver(cfg Config, key crypto.Signer, store *controlplane.Store, svc *api.Server,
	m *metrics.Metrics, log *slog.Logger,
) (*githubapp.Receiver, *githubapp.Worker, error) {
	client := &githubapp.Client{
		BaseURL: g.APIBaseURL,
		AppID:   g.AppID,
		Signer:  key,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
		Now:     time.Now,
	}
	worker := githubapp.NewWorker(githubapp.WorkerConfig{
		API:       client,
		Service:   svc,
		Runs:      store,
		Limits:    cfg.Limits,
		PublicURL: cfg.PublicURL,
		Workers:   g.Workers,
		Log:       log,
	})
	r, err := githubapp.New(githubapp.Config{
		Secret:       g.Secret,
		MaxBodyBytes: g.MaxBodyBytes,
		MaxAge:       g.MaxAge,
		Associations: g.Associations,
		Reaction:     g.Reaction,
		Store:        githubapp.Adapt(store),
		API:          client,
		Runner:       worker,
		Record:       m.WebhookDelivered,
		Log:          log,
	})
	if err != nil {
		return nil, nil, err
	}

	return r, worker, nil
}

func parsePrivateKey(text string) (crypto.Signer, error) {
	block, _ := pem.Decode([]byte(text))
	if block == nil {
		return nil, errors.New("github app private key is not PEM (GODWIT_GITHUB_PRIVATE_KEY or GODWIT_GITHUB_PRIVATE_KEY_FILE)")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("github app private key: %w", err)
	}
	signer, ok := parsed.(crypto.Signer)
	if !ok {
		return nil, errors.New("github app private key cannot sign")
	}

	return signer, nil
}

func serveWebhook(cfg Config, key crypto.Signer, store *controlplane.Store, svc *api.Server,
	m *metrics.Metrics, log *slog.Logger,
) (func(context.Context), error) {
	if !cfg.GitHub.enabled() {
		return func(context.Context) {}, nil
	}
	receiver, worker, err := cfg.GitHub.receiver(cfg, key, store, svc, m, log)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", cfg.GitHub.Addr)
	if err != nil {
		worker.Stop(context.Background())

		return nil, err
	}
	srv := &http.Server{
		Handler:           receiver.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	log.Info("github webhook listening", "addr", ln.Addr().String())
	if cfg.GitHub.OnReady != nil {
		cfg.GitHub.OnReady(ln.Addr())
	}
	go func() { _ = srv.Serve(ln) }()

	// The listener closes first, so nothing new is accepted while what was accepted is carried out.
	return func(ctx context.Context) {
		_ = srv.Shutdown(ctx)
		worker.Stop(ctx)
	}, nil
}
