package server

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/SamuelMolling/godwit/internal/controlplane"
)

// newScratch resolves where scratch databases live and refuses a configured scratch connection that can act
// outside them; without a scratch DSN they stay on the store server and every finding is only a warning.
func newScratch(ctx context.Context, cfg Config, store *pgxpool.Pool, log *slog.Logger) (*controlplane.Scratch, func(), error) {
	pool, closePool := store, func() {}
	if cfg.ScratchDSN != "" {
		p, err := pgxpool.New(ctx, cfg.ScratchDSN)
		if err != nil {
			return nil, nil, fmt.Errorf("scratch dsn: %w", err)
		}
		pool, closePool = p, p.Close
	}
	scratch := controlplane.NewScratch(pool, cfg.ScratchTemplate)
	// A shutdown arriving mid-start-up must not turn the preflight into a refusal to serve.
	probeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	findings, err := scratch.Check(probeCtx, store.Config().ConnConfig.Database)
	if err != nil {
		closePool()

		return nil, nil, err
	}
	fatal := make([]string, 0, len(findings))
	for _, f := range findings {
		if f.Fatal && cfg.ScratchDSN != "" {
			fatal = append(fatal, f.Detail)

			continue
		}
		log.Warn("scratch database is not isolated", "detail", f.Detail)
	}
	if len(fatal) > 0 {
		closePool()

		return nil, nil, fmt.Errorf("%w: %s", controlplane.ErrScratchPrivileged, strings.Join(fatal, "; "))
	}
	if cfg.ScratchDSN == "" {
		log.Warn("validation and diff execute submitted DDL on the store server with the store credentials; " +
			"set --scratch-dsn to a throwaway PostgreSQL that holds nothing")
	}

	return scratch, closePool, nil
}
