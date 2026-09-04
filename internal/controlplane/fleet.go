package controlplane

import (
	"cmp"
	"context"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/SamuelMolling/godwit/internal/engine"
)

// FleetFilter narrows the fleet view; a zero filter answers over every registered target.
type FleetFilter struct {
	Targets       []string
	FromVersion   int64
	ToVersion     int64
	NotEverywhere bool
	In            string
	NotIn         string
}

// FleetOn is a target standing on one migration under the entry's checksum.
type FleetOn struct {
	Target    string
	AppliedAt time.Time
	RunID     string
	// CollapsedBy names the checkpoint that recorded the migration on this target without running it.
	CollapsedBy string
}

// FleetGap is a target the entry does not stand on, with what separates it from the ones that do.
type FleetGap struct {
	Target        string
	NewestVersion int64
	// Behind marks a target that has not reached the migration yet, as opposed to one that skipped it.
	Behind bool
	// Holds marks a target standing on the same migration under other content.
	Holds         bool
	OtherChecksum string
}

// FleetMigration is one migration as one content: a version standing under two checksums is two entries.
type FleetMigration struct {
	Migration  string
	Version    int64
	Name       string
	Repeatable bool
	Checkpoint bool
	Checksum   string
	On         []FleetOn
	Missing    []FleetGap
	Divergent  bool
}

// Fleet is the cross-target answer to "which of my databases has this migration".
type Fleet struct {
	Targets    []string
	Migrations []FleetMigration
}

// FleetMigrations reports which targets hold each migration and which do not. It reads the ledger and the
// bodies the runs carried, and opens no connection to any target, so it answers while one is unreachable.
func (s *Store) FleetMigrations(ctx context.Context, f FleetFilter) (Fleet, error) {
	targets, err := s.fleetTargets(ctx, f)
	if err != nil {
		return Fleet{}, err
	}
	rows, err := s.standingLedger(ctx, targets)
	if err != nil {
		return Fleet{}, err
	}

	return Fleet{Targets: targets, Migrations: fleetOf(rows, targets, f)}, nil
}

func (s *Store) fleetTargets(ctx context.Context, f FleetFilter) ([]string, error) {
	// A nil slice reaches PostgreSQL as NULL, which matches nothing rather than everything.
	want := append([]string{}, f.Targets...)
	rows, err := s.pool.Query(ctx,
		`SELECT name FROM cp_targets WHERE cardinality($1::text[]) = 0 OR name = ANY($1) ORDER BY name`, want)
	if err != nil {
		return nil, fmt.Errorf("list fleet targets: %w", err)
	}
	var out []string
	var name string
	if _, err := pgx.ForEachRow(rows, []any{&name}, func() error {
		out = append(out, name)

		return nil
	}); err != nil {
		return nil, fmt.Errorf("read fleet targets: %w", err)
	}
	for _, want := range slices.Concat(f.Targets, []string{f.In, f.NotIn}) {
		if want != "" && !slices.Contains(out, want) {
			return nil, fmt.Errorf("target %q: %w", want, ErrNotFound)
		}
	}

	return out, nil
}

type ledgerRow struct {
	Target    string
	Migration string
	AppliedAt time.Time
	RunID     string
	Checksum  string
	Body      string
}

// standingLedger reads the newest standing row per target and migration. A run whose file bodies retention
// swept leaves an empty checksum rather than dropping the migration out of the answer.
func (s *Store) standingLedger(ctx context.Context, targets []string) ([]ledgerRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT ON (r.target, a.migration) r.target, a.migration, a.applied_at, a.run_id::text,
			coalesce(encode(sha256(convert_to(u.body, 'UTF8')), 'hex'), ''),
			coalesce(CASE WHEN u.body LIKE '%'||$2||'%' THEN u.body END, '')
		FROM cp_run_applied a
		JOIN cp_runs r ON r.id = a.run_id
		LEFT JOIN cp_run_files u ON u.run_id = a.run_id AND u.name = a.migration || '.up.sql'
		WHERE r.target = ANY($1) AND `+standingRow+`
		ORDER BY r.target, a.migration, r.created_at DESC`,
		targets, engine.DirectiveMarker+" "+engine.DirectiveCheckpoint)
	if err != nil {
		return nil, fmt.Errorf("list standing migrations: %w", err)
	}
	var out []ledgerRow
	var row ledgerRow
	fields := []any{&row.Target, &row.Migration, &row.AppliedAt, &row.RunID, &row.Checksum, &row.Body}
	if _, err := pgx.ForEachRow(rows, fields, func() error {
		out = append(out, row)

		return nil
	}); err != nil {
		return nil, fmt.Errorf("read standing migrations: %w", err)
	}

	return out, nil
}

func fleetOf(rows []ledgerRow, targets []string, f FleetFilter) []FleetMigration {
	cps := collapsedBy(rows)
	newest, migrated := map[string]int64{}, map[string]bool{}
	stands := map[[2]string]string{}
	for _, r := range rows {
		version, _, _ := splitMigrationID(r.Migration)
		newest[r.Target], migrated[r.Target] = max(newest[r.Target], version), true
		stands[[2]string{r.Target, r.Migration}] = r.Checksum
	}
	entries, index := []*FleetMigration{}, map[[2]string]*FleetMigration{}
	for _, r := range rows {
		key := [2]string{r.Migration, r.Checksum}
		e, ok := index[key]
		if !ok {
			version, name, repeatable := splitMigrationID(r.Migration)
			e = &FleetMigration{
				Migration: r.Migration, Version: version, Name: name, Repeatable: repeatable,
				Checkpoint: cps.is[r.Migration], Checksum: r.Checksum,
			}
			entries = append(entries, e)
			index[key] = e
		}
		e.On = append(e.On, FleetOn{
			Target: r.Target, AppliedAt: r.AppliedAt, RunID: r.RunID,
			CollapsedBy: cps.by[[2]string{r.Target, r.Migration}],
		})
	}

	return finish(entries, targets, f, fleetState{newest: newest, migrated: migrated, stands: stands})
}

type fleetState struct {
	newest   map[string]int64
	migrated map[string]bool
	stands   map[[2]string]string
}

// finish fills each entry's gaps and divergence flag, then applies the filter and the reading order.
func finish(entries []*FleetMigration, targets []string, f FleetFilter, st fleetState) []FleetMigration {
	contents := map[string]map[string]bool{}
	for _, e := range entries {
		if e.Checksum == "" {
			continue
		}
		if contents[e.Migration] == nil {
			contents[e.Migration] = map[string]bool{}
		}
		contents[e.Migration][e.Checksum] = true
	}
	out := make([]FleetMigration, 0, len(entries))
	for _, e := range entries {
		e.Divergent = len(contents[e.Migration]) > 1
		for _, t := range targets {
			if slices.ContainsFunc(e.On, func(o FleetOn) bool { return o.Target == t }) {
				continue
			}
			e.Missing = append(e.Missing, gapOf(*e, t, st))
		}
		if f.keep(*e) {
			out = append(out, *e)
		}
	}
	slices.SortFunc(out, compareFleet)

	return out
}

func gapOf(e FleetMigration, target string, st fleetState) FleetGap {
	gap := FleetGap{Target: target, NewestVersion: st.newest[target]}
	if other, holds := st.stands[[2]string{target, e.Migration}]; holds {
		gap.Holds, gap.OtherChecksum = true, other

		return gap
	}
	gap.Behind = !st.migrated[target] || (!e.Repeatable && st.newest[target] < e.Version)

	return gap
}

func (f FleetFilter) keep(e FleetMigration) bool {
	if f.FromVersion != 0 || f.ToVersion != 0 {
		if e.Repeatable || e.Version < f.FromVersion || (f.ToVersion != 0 && e.Version > f.ToVersion) {
			return false
		}
	}
	if f.NotEverywhere && len(e.Missing) == 0 {
		return false
	}

	return (f.In == "" || standsOn(e, f.In)) && (f.NotIn == "" || !standsOn(e, f.NotIn))
}

func standsOn(e FleetMigration, target string) bool {
	return slices.ContainsFunc(e.On, func(o FleetOn) bool { return o.Target == target })
}

func compareFleet(a, b FleetMigration) int {
	return cmp.Or(cmp.Compare(fleetOrder(a), fleetOrder(b)),
		strings.Compare(a.Name, b.Name), strings.Compare(a.Checksum, b.Checksum))
}

// fleetOrder sorts versions ascending and puts the versionless repeatables after all of them.
func fleetOrder(m FleetMigration) int64 {
	if m.Repeatable {
		return math.MaxInt64
	}

	return m.Version
}

type checkpointIndex struct {
	is map[string]bool
	by map[[2]string]string
}

// collapsedBy finds, for each row, the checkpoint of the same run that recorded it without running it: a
// database with no history runs the checkpoint and records the versions below it in that same run.
func collapsedBy(rows []ledgerRow) checkpointIndex {
	idx := checkpointIndex{is: map[string]bool{}, by: map[[2]string]string{}}
	newest := map[string]engine.Migration{}
	for _, r := range rows {
		cp, ok := checkpointOf(r.Migration, r.Body)
		if !ok {
			continue
		}
		idx.is[r.Migration] = true
		if held, seen := newest[r.RunID]; !seen || cp.Through > held.Through {
			newest[r.RunID] = cp
		}
	}
	for _, r := range rows {
		cp, ok := newest[r.RunID]
		version, versioned := versionOf(r.Migration)
		if ok && versioned && r.Migration != cp.ID() && version <= cp.Through {
			idx.by[[2]string{r.Target, r.Migration}] = cp.ID()
		}
	}

	return idx
}

func splitMigrationID(id string) (int64, string, bool) {
	if name, ok := strings.CutPrefix(id, engine.RepeatablePrefix); ok {
		return 0, name, true
	}
	version, _ := versionOf(id)
	_, name, _ := strings.Cut(id, "_")

	return version, name, false
}
