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

// transientClasses are the SQLSTATE classes a later attempt can clear on its own: connection exceptions
// (class 08), insufficient resources (class 53 — a full disk, an exhausted memory or connection budget)
// and operator intervention (class 57 — a cancelled statement, a restart, a shutdown).
var transientClasses = []string{"08", "53", "57"}

// transientCodes are the transient codes outside those classes: the two concurrency retries, the lock
// timeout, and the system errors that come from the machine rather than the data (class 58 is not
// transient as a whole — 58P01 and 58P02 are a missing or duplicated file, which is corruption).
var transientCodes = map[string]bool{
	"40001": true, "40P01": true, "55P03": true, "58000": true, "58030": true,
}

// permanentCodes are the exceptions inside the transient classes: a dropped database is not coming back
// on its own, and retrying against it only delays the page.
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

// Transient reports whether err is a failure that a later attempt can reasonably clear on its own.
func Transient(err error) bool {
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
