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

const maxBackoff = 5 * time.Minute

var transientClasses = []string{"08", "53", "57"}

var transientCodes = map[string]bool{
	"40001": true, "40P01": true, "55P03": true, "58000": true, "58030": true,
}

var permanentCodes = map[string]bool{"57P04": true}

func transientCode(code string) bool {
	if permanentCodes[code] {
		return false
	}
	if transientCodes[code] {
		return true
	}
	for _, class := range transientClasses {
		if strings.HasPrefix(code, class) {
			return true
		}
	}

	return false
}

func transient(err error) bool {
	_, ok := classify(err)

	return ok
}

func classify(err error) (string, bool) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code, transientCode(pgErr.Code)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ReasonTimeout, true
	}
	var netErr net.Error
	if errors.As(err, &netErr) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, pgconn.ErrConnClosed) {
		return ReasonNetwork, true
	}

	return "", false
}

func failureDetail(err error) string {
	if transient(err) {
		return "transient: " + err.Error()
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return "sql: " + err.Error()
	}

	return err.Error()
}

func backoff(base time.Duration, attempts int, jitter func() float64) time.Duration {
	d := base
	for i := 1; i < attempts && d < maxBackoff; i++ {
		d *= 2
	}
	d = min(d, maxBackoff)

	return time.Duration(float64(d) * (0.8 + 0.4*jitter()))
}

func defaultJitter() float64 { return rand.Float64() }

func retryDetail(err error, wait time.Duration) string {
	return fmt.Sprintf("%s (retry in %s)", failureDetail(err), wait.Round(100*time.Millisecond))
}
