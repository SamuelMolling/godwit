// Package limits holds the bounds, decided by configuration, that a request is admitted within.
package limits

import (
	"fmt"
	"strings"
	"time"
)

// Defaults. A directory is counted in migrations because a file count halves in practice; 2000
// of them is 4000 files and, at the 8 KiB a file RequestBytes was sized for, 32 MiB — so none of the three
// stops a directory the others admit. FileBytes holds a generated schema dump; HeavyCalls is how many
// requests may build scratch databases at once.
const (
	DefaultRequestBytes = 32 << 20
	DefaultMigrations   = 2000
	DefaultFiles        = 5000
	DefaultFileBytes    = 4 << 20
	DefaultHeavyCalls   = 4
	DefaultHeavyWait    = 30 * time.Second
	maxNameBytes        = 512
	upSuffix            = ".up.sql"
)

// Limits are the bounds applied to every request; a zero field takes its default.
type Limits struct {
	// RequestBytes caps one decoded request body.
	RequestBytes int
	// Migrations caps how many migrations one request may carry; the up half is what names one.
	Migrations int
	// Files caps how many files one request may carry, migration halves and everything else alike.
	Files int
	// FileBytes caps one migration body and one desired schema.
	FileBytes int
	// HeavyCalls caps concurrent Diff, PlanRun, CreateRun, RevertRun and Checkpoint calls.
	HeavyCalls int
	// HeavyWait is how long a call queues for a free slot before it is refused.
	HeavyWait time.Duration
}

// WithDefaults returns the bounds in force: every zero field replaced by its default.
func (l Limits) WithDefaults() Limits {
	if l.RequestBytes <= 0 {
		l.RequestBytes = DefaultRequestBytes
	}
	if l.Migrations <= 0 {
		l.Migrations = DefaultMigrations
	}
	if l.Files <= 0 {
		l.Files = DefaultFiles
	}
	if l.FileBytes <= 0 {
		l.FileBytes = DefaultFileBytes
	}
	if l.HeavyCalls <= 0 {
		l.HeavyCalls = DefaultHeavyCalls
	}
	if l.HeavyWait <= 0 {
		l.HeavyWait = DefaultHeavyWait
	}

	return l
}

// Listed is one file a caller means to send, named and sized, which is all a directory listing knows.
type Listed struct {
	Name string
	Size int
}

// CheckListing is the file, size and migration check applied to names and sizes alone, so a caller
// reading a directory over an API can refuse it before it spends requests on the bodies.
func (l Limits) CheckListing(in []Listed) error {
	l = l.WithDefaults()
	if len(in) > l.Files {
		return fmt.Errorf("too many migration files: %d, limit %d", len(in), l.Files)
	}
	migrations := 0
	for _, f := range in {
		if len(f.Name) > maxNameBytes {
			return fmt.Errorf("migration file name is %d bytes, limit %d", len(f.Name), maxNameBytes)
		}
		if f.Size > l.FileBytes {
			return fmt.Errorf("migration file %s is %d bytes, limit %d", f.Name, f.Size, l.FileBytes)
		}
		if strings.HasSuffix(f.Name, upSuffix) {
			migrations++
		}
	}
	if migrations > l.Migrations {
		return fmt.Errorf("too many migrations: %d, limit %d", migrations, l.Migrations)
	}

	return nil
}
