// Package limits holds the bounds, decided by configuration, that a request is admitted within.
package limits

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// Defaults, sized against each other so none of the three stops a directory the others admit; docs/run/security.md#admission-limits argues them.
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
	RequestBytes int
	Migrations   int
	Files        int
	FileBytes    int
	HeavyCalls   int
	HeavyWait    time.Duration
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

// CheckListing applies the file, size, aggregate and migration bounds to names and sizes alone, before any body is fetched.
func (l Limits) CheckListing(in []Listed) error {
	l = l.WithDefaults()
	if len(in) > l.Files {
		return fmt.Errorf("too many migration files: %d, limit %d", len(in), l.Files)
	}
	migrations, budget := 0, l.Budget()
	for _, f := range in {
		if len(f.Name) > maxNameBytes {
			return fmt.Errorf("migration file name is %d bytes, limit %d", len(f.Name), maxNameBytes)
		}
		if f.Size > l.FileBytes {
			return fmt.Errorf("migration file %s is %d bytes, limit %d", f.Name, f.Size, l.FileBytes)
		}
		if err := budget.Charge(f.Size); err != nil {
			return err
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

// Budget is the aggregate byte bound of one submitted set, charged as its bodies arrive so an oversized set is refused before it is all held.
type Budget struct {
	mu    sync.Mutex
	limit int
	used  int
}

// Budget returns a fresh aggregate-byte budget for one submitted set.
func (l Limits) Budget() *Budget {
	return &Budget{limit: l.WithDefaults().RequestBytes}
}

// Charge adds n bytes to what the set has taken and refuses as soon as the total is over the bound.
func (b *Budget) Charge(n int) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.used += n
	if b.used > b.limit {
		return fmt.Errorf("migration files are over the %d bytes a request may hold in total; "+
			"split the directory across pull requests or raise --max-request-bytes", b.limit)
	}

	return nil
}
