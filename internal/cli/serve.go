package cli

import (
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/SamuelMolling/godwit/internal/server"
)

func newServeCmd() *cobra.Command {
	var listen, storeDSN string
	var driftInterval time.Duration
	var skipValidation bool
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the godwit control-plane service",
		Long: "Runs the godwit service: state store, scheduler, drift monitor and API.\n" +
			"Env: GODWIT_MASTER_KEY (64 hex chars), GODWIT_TOKENS (comma-separated bearer tokens),\n" +
			"GODWIT_WEBHOOK_URL (drift notifications).",
		RunE: func(cmd *cobra.Command, _ []string) error {
			key, err := hex.DecodeString(os.Getenv("GODWIT_MASTER_KEY"))
			if err != nil || len(key) != 32 {
				return fmt.Errorf("GODWIT_MASTER_KEY must be 64 hex chars (32 bytes)")
			}
			var tokens []string
			if raw := os.Getenv("GODWIT_TOKENS"); raw != "" {
				tokens = strings.Split(raw, ",")
			}
			hostname, _ := os.Hostname()

			return server.Run(cmd.Context(), server.Config{
				Listen:         listen,
				StoreDSN:       storeDSN,
				MasterKey:      key,
				Tokens:         tokens,
				Holder:         hostname,
				DriftInterval:  driftInterval,
				WebhookURL:     os.Getenv("GODWIT_WEBHOOK_URL"),
				SkipValidation: skipValidation,
				Log:            slog.New(slog.NewJSONHandler(cmd.ErrOrStderr(), nil)),
			})
		},
	}
	cmd.Flags().StringVar(&listen, "listen", ":8474", "address to serve the API on")
	cmd.Flags().StringVar(&storeDSN, "store-dsn", "", "control-plane database DSN")
	cmd.Flags().DurationVar(&driftInterval, "drift-interval", 5*time.Minute, "how often to check targets for schema drift")
	cmd.Flags().BoolVar(&skipValidation, "skip-validation", false, "disable scratch-database validation at run admission")
	_ = cmd.MarkFlagRequired("store-dsn")

	return cmd
}
