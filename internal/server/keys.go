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

// settleKeys moves every static target onto the key in force and names the ones it cannot. It never
// fails a start-up: a target whose key is gone is that target's problem, and the replica still serves
// the rest — the call that needs it is refused, naming the key it wants.
func settleKeys(ctx context.Context, store targetStore, keys creds.Keyring, log *slog.Logger) {
	targets, err := store.ListTargets(ctx, time.Time{})
	if err != nil {
		log.Warn("static target keys not checked", "error", err)

		return
	}
	var stranded []string
	for _, t := range targets {
		if t.Provider != "static" {
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
	if !keys.NeedsReseal(config["dsn"]) {
		return
	}
	dsn, err := keys.Open(ctx, config["dsn"])
	if err != nil {
		log.Warn("static target is sealed under another key and was left alone", "target", name, "error", err)

		return
	}
	sealed, err := keys.Seal(ctx, dsn)
	if err != nil {
		log.Warn("static target not resealed", "target", name, "error", err)

		return
	}
	config["dsn"] = sealed
	if err := store.RegisterTarget(ctx, name, provider, config); err != nil {
		log.Warn("static target not resealed", "target", name, "error", err)

		return
	}
	log.Info("static target resealed", "target", name, "key", keys.Describe())
}
