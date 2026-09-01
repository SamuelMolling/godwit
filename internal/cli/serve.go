package cli

import (
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/SamuelMolling/godwit/internal/server"
)

func newServeCmd() *cobra.Command {
	var listen, storeDSN string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the godwit control-plane service",
		Long: "Runs the godwit service: state store, scheduler and API.\n" +
			"Env: GODWIT_MASTER_KEY (64 hex chars), GODWIT_TOKENS (comma-separated bearer tokens).",
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
				Listen:    listen,
				StoreDSN:  storeDSN,
				MasterKey: key,
				Tokens:    tokens,
				Holder:    hostname,
				Log:       slog.New(slog.NewJSONHandler(cmd.ErrOrStderr(), nil)),
			})
		},
	}
	cmd.Flags().StringVar(&listen, "listen", ":8474", "address to serve the API on")
	cmd.Flags().StringVar(&storeDSN, "store-dsn", "", "control-plane database DSN")
	_ = cmd.MarkFlagRequired("store-dsn")

	return cmd
}
