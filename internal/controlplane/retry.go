package controlplane

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// Failure classes reported in notifications and metrics.
const (
	ReasonNetwork = "network"
	ReasonTimeout = "timeout"
)

// MaxBackoff caps the wait between transient retries.
const MaxBackoff = 5 * time.Minute

var transientCodes = map[string]bool{
	"40001": true, "40P01": true, "55P03": true, "57014": true,
	"53300": true, "57P01": true, "57P02": true, "57P03": true,
}

// Transient reports whether err is a failure that a later attempt can reasonably clear on its own.
func Transient(err error) bool {
	_, ok := classify(err)

	return ok
}

func classify(err error) (string, bool) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code, transientCodes[pgErr.Code] || strings.HasPrefix(pgErr.Code, "08")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ReasonTimeout, true
	}
	var netErr net.Error
	if errors.As(err, &netErr) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return ReasonNetwork, true
	}

	return "", false
}

// FailureDetail prefixes err with its class so a reader knows whether to page someone.
func FailureDetail(err error) string {
	if Transient(err) {
		return "transient: " + err.Error()
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return "sql: " + err.Error()
	}

	return err.Error()
}

// Backoff is the wait before retrying after the given number of attempts: base doubled per attempt, capped, with ±20% jitter.
func Backoff(base time.Duration, attempts int, jitter func() float64) time.Duration {
	d := base
	for i := 1; i < attempts && d < MaxBackoff; i++ {
		d *= 2
	}
	d = min(d, MaxBackoff)

	return time.Duration(float64(d) * (0.8 + 0.4*jitter()))
}

func defaultJitter() float64 { return rand.Float64() }

func retryDetail(err error, wait time.Duration) string {
	return fmt.Sprintf("%s (retry in %s)", FailureDetail(err), wait.Round(100*time.Millisecond))
}
