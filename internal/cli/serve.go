package cli

import (
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/SamuelMolling/godwit/internal/controlplane"
	"github.com/SamuelMolling/godwit/internal/server"
)

func newServeCmd() *cobra.Command {
	var listen, storeDSN, logFormat, logLevel string
	var driftInterval, leaseTTL, tickInterval time.Duration
	var maxAttempts int
	var skipValidation, requirePlan bool
	var planTTL, planRetention time.Duration
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the godwit control-plane service",
		Long: "Runs the godwit service: state store, scheduler, drift monitor and API.\n" +
			"Env: GODWIT_MASTER_KEY (64 hex chars), GODWIT_TOKENS (comma-separated name:scope:secret bearer tokens, scope read|pipeline|operator|admin; name:secret and a bare secret are admin),\n" +
			"GODWIT_WEBHOOK_URL (JSON notifications), GODWIT_SLACK_TOKEN/GODWIT_SLACK_CHANNEL/GODWIT_SLACK_MODE (Slack notifications),\n" +
			"GODWIT_PUBLIC_URL (link base for notifications), VAULT_ADDR/VAULT_TOKEN or VAULT_K8S_ROLE (vault provider),\n" +
			"GODWIT_LOG_FORMAT and GODWIT_LOG_LEVEL (defaults for --log-format and --log-level).",
		RunE: func(cmd *cobra.Command, _ []string) error {
			log, err := server.NewLogger(cmd.ErrOrStderr(), logFormat, logLevel)
			if err != nil {
				return err
			}
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
				Scheduler:      controlplane.Config{TTL: leaseTTL, Interval: tickInterval, MaxAttempts: maxAttempts},
				DriftInterval:  driftInterval,
				WebhookURL:     os.Getenv("GODWIT_WEBHOOK_URL"),
				SlackToken:     os.Getenv("GODWIT_SLACK_TOKEN"),
				SlackChannel:   os.Getenv("GODWIT_SLACK_CHANNEL"),
				SlackMode:      os.Getenv("GODWIT_SLACK_MODE"),
				PublicURL:      os.Getenv("GODWIT_PUBLIC_URL"),
				SkipValidation: skipValidation,
				RequirePlan:    requirePlan,
				PlanTTL:        planTTL,
				PlanRetention:  planRetention,
				Log:            log,
			})
		},
	}
	cmd.Flags().StringVar(&listen, "listen", ":8474", "address to serve the API on")
	cmd.Flags().StringVar(&storeDSN, "store-dsn", "", "control-plane database DSN")
	cmd.Flags().DurationVar(&driftInterval, "drift-interval", 5*time.Minute, "how often to check targets for schema drift")
	cmd.Flags().DurationVar(&leaseTTL, "lease-ttl", 30*time.Second, "how long a claimed run stays leased without a heartbeat")
	cmd.Flags().DurationVar(&tickInterval, "tick-interval", 2*time.Second, "how often the scheduler looks for runnable runs")
	cmd.Flags().IntVar(&maxAttempts, "max-attempts", 5, "claims a run may take before it parks as needs_attention")
	cmd.Flags().BoolVar(&skipValidation, "skip-validation", false, "disable scratch-database validation at run admission")
	cmd.Flags().BoolVar(&requirePlan, "require-plan", false, "refuse runs without a stored plan on every target")
	cmd.Flags().DurationVar(&planTTL, "plan-ttl", 720*time.Hour, "how long a stored plan stays bindable")
	cmd.Flags().DurationVar(&planRetention, "plan-retention", 2160*time.Hour, "how long bound and superseded plans are kept before the drift ticker deletes them")
	cmd.Flags().StringVar(&logFormat, "log-format", envOr("GODWIT_LOG_FORMAT", "json"), "log format: json or text")
	cmd.Flags().StringVar(&logLevel, "log-level", envOr("GODWIT_LOG_LEVEL", "info"), "log level: debug, info, warn or error")
	_ = cmd.MarkFlagRequired("store-dsn")

	return cmd
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}

	return fallback
}
