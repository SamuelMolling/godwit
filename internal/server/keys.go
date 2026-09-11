package server

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/SamuelMolling/godwit/internal/controlplane"
	"github.com/SamuelMolling/godwit/internal/creds"
)

type targetStore interface {
	ListTargets(ctx context.Context, since time.Time) ([]controlplane.TargetSummary, error)
	Target(ctx context.Context, name string) (string, map[string]string, error)
	RegisterTarget(ctx context.Context, name, provider string, config map[string]string) error
}

func settleKeys(ctx context.Context, store targetStore, keys creds.Keyring, log *slog.Logger) {
	targets, err := store.ListTargets(ctx, time.Time{})
	if err != nil {
		log.Warn("static target keys not checked", "error", err)

		return
	}
	var stranded []string
	for _, t := range targets {
		if t.Provider != creds.ProviderStatic {
			continue
		}
		if !keys.Configured() {
			stranded = append(stranded, t.Name)

			continue
		}
		reseal(ctx, store, keys, t.Name, log)
	}
	if len(stranded) > 0 {
		log.Warn("static targets are sealed and no key is configured; every run on them will fail",
			"targets", strings.Join(stranded, ","))
	}
}

func reseal(ctx context.Context, store targetStore, keys creds.Keyring, name string, log *slog.Logger) {
	provider, config, err := store.Target(ctx, name)
	if err != nil {
		log.Warn("static target not read", "target", name, "error", err)

		return
	}
	if !keys.NeedsReseal(config[creds.DSNKey]) {
		return
	}
	dsn, err := keys.Open(ctx, config[creds.DSNKey])
	if err != nil {
		log.Warn("static target is sealed under another key and was left alone", "target", name, "error", err)

		return
	}
	sealed, err := keys.Seal(ctx, dsn)
	if err != nil {
		log.Warn("static target not resealed", "target", name, "error", err)

		return
	}
	config[creds.DSNKey] = sealed
	if err := store.RegisterTarget(ctx, name, provider, config); err != nil {
		log.Warn("static target not resealed", "target", name, "error", err)

		return
	}
	log.Info("static target resealed", "target", name, "key", keys.Describe())
}
