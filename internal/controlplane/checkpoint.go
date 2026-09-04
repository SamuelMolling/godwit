package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/SamuelMolling/godwit/internal/engine"
)

// ErrCheckpoint marks a checkpoint godwit refuses to generate.
var ErrCheckpoint = errors.New("checkpoint")

// checkpointSearchPath is pinned because generation has no target whose search_path it could mirror, and
// an unpinned "$user" resolves to the journal schema whenever the scratch role shares its name.
const checkpointSearchPath = "public"

// Checkpoint is a generated checkpoint file: the schema a prefix of the migration directory produces.
type Checkpoint struct {
	Version int64
	Name    string
	Through int64
	// Covers is what the checkpoint collapses, oldest first.
	Covers []string
	// Body is the whole file, directive header included.
	Body string
}

// UpFile is the name the checkpoint is written under.
func (c Checkpoint) UpFile() string {
	return engine.MigrationID(c.Version, c.Name, false) + ".up.sql"
}

// Checkpointer generates a checkpoint by replaying the files on a scratch database and rendering the
// schema they left behind as DDL.
type Checkpointer struct {
	scratch *Scratch
	newID   func() string
	// Expander renders `-- godwit:` directives below the checkpoint against the scratch catalog.
	Expander *Expander
}

// NewCheckpointer wires a Checkpointer over the scratch connection.
func NewCheckpointer(scratch *Scratch, newID func() string) *Checkpointer {
	return &Checkpointer{scratch: scratch, newID: newID, Expander: NewExpander()}
}

// Generate collapses every version of files at or below at (zero takes the newest) into one checkpoint
// named name. It replays those versions on a scratch database, renders that schema as DDL, and refuses
// unless replaying the DDL alone reproduces the same schema fingerprint.
func (c *Checkpointer) Generate(ctx context.Context, files map[string]string, at int64, name string, now time.Time) (Checkpoint, error) {
	migs, err := MigrationsFromFiles(files)
	if err != nil {
		return Checkpoint{}, fmt.Errorf("%w: %w", ErrMigrationFiles, err)
	}
	collapsed, through, err := collapsedSet(migs, at)
	if err != nil {
		return Checkpoint{}, err
	}
	factory := &scratchFactory{scratch: c.scratch, newID: c.newID, searchPath: checkpointSearchPath}
	replayed, done, err := c.replay(ctx, factory, collapsed)
	if err != nil {
		return Checkpoint{}, err
	}
	defer done()

	empty, err := factory.Create(ctx)
	if err != nil {
		return Checkpoint{}, err
	}
	defer func() { _ = empty.Close(ctx) }()
	ddl, err := renderForEmptyDatabase(ctx, empty.ConnPool, replayed.pool, factory)
	if err != nil {
		return Checkpoint{}, fmt.Errorf("%w: render the schema as DDL: %w", ErrCheckpoint, err)
	}
	if err := c.verify(ctx, factory, ddl, replayed.definition); err != nil {
		return Checkpoint{}, err
	}

	cp := Checkpoint{Version: checkpointVersion(migs, now), Name: name, Through: through}
	for _, m := range collapsed {
		cp.Covers = append(cp.Covers, m.ID())
	}
	cp.Body = checkpointBody(cp, ddl)

	return cp, nil
}

func collapsedSet(migs []engine.Migration, at int64) ([]engine.Migration, int64, error) {
	if cp, ok := engine.NewestCheckpoint(migs); ok && (at == 0 || cp.Version <= at) {
		return nil, 0, fmt.Errorf("%w: %s already collapses history through %014d; a checkpoint is generated over the migrations above it",
			ErrCheckpoint, cp.ID(), cp.Through)
	}
	var out []engine.Migration
	for _, m := range migs {
		if !m.Repeatable && (at == 0 || m.Version <= at) {
			out = append(out, m)
		}
	}
	if len(out) < 2 {
		return nil, 0, fmt.Errorf("%w: %d versioned migration(s) at or below %014d; a checkpoint needs at least two to be worth its file",
			ErrCheckpoint, len(out), at)
	}

	return out, out[len(out)-1].Version, nil
}

type replayedSchema struct {
	pool       *sql.DB
	definition string
}

// replay applies collapsed on a fresh scratch database, expanding any directive against the catalog the
// migrations before it left, and returns the schema they produced.
func (c *Checkpointer) replay(ctx context.Context, factory *scratchFactory, collapsed []engine.Migration) (replayedSchema, func(), error) {
	name, scratch, err := factory.create(ctx)
	if err != nil {
		return replayedSchema{}, nil, err
	}
	done := func() { _ = scratch.Close(context.WithoutCancel(ctx)) }
	conn, err := connectScratch(ctx, c.scratch.connConfig(name, checkpointSearchPath))
	if err != nil {
		done()

		return replayedSchema{}, nil, fmt.Errorf("connect scratch database: %w", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()

	def, err := c.applyCollapsed(ctx, conn, collapsed)
	if err != nil {
		done()

		return replayedSchema{}, nil, err
	}

	return replayedSchema{pool: scratch.ConnPool, definition: def}, done, nil
}

func (c *Checkpointer) applyCollapsed(ctx context.Context, conn engine.DB, collapsed []engine.Migration) (string, error) {
	v := &Validator{Expander: c.Expander}
	for _, m := range collapsed {
		p, err := engine.BuildPlan(m, engine.DirectionUp)
		if err != nil {
			return "", fmt.Errorf("%w: %w", ErrMigrationFiles, err)
		}
		if p, err = v.expandPlan(ctx, conn, p, map[string]Expansion{}, nil); err != nil {
			return "", err
		}
		if _, err := applyPlans(ctx, conn, engine.Options{}, []engine.Plan{p}, nil, engine.WithAssertProbe()); err != nil {
			return "", fmt.Errorf("%w: %s: %w", ErrMigrationFiles, m.ID(), err)
		}
	}
	def, _, err := snapshotScratch(ctx, conn)
	if err != nil {
		return "", fmt.Errorf("snapshot scratch database: %w", err)
	}

	return def, nil
}

// verify refuses a checkpoint whose DDL does not reproduce the schema the migrations produced; the
// generated file is only worth what the replay of it is. It applies the body through the executor, the
// way a run would, so a statement godwit cannot plan is refused here and not on a target.
func (c *Checkpointer) verify(ctx context.Context, factory *scratchFactory, ddl, want string) error {
	name, scratch, err := factory.create(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = scratch.Close(context.WithoutCancel(ctx)) }()
	conn, err := connectScratch(ctx, c.scratch.connConfig(name, checkpointSearchPath))
	if err != nil {
		return fmt.Errorf("connect scratch database: %w", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()

	p, err := engine.BuildPlan(engine.Migration{Version: 1, Name: "checkpoint", Checkpoint: true, UpSQL: ddl}, engine.DirectionUp)
	if err != nil {
		return fmt.Errorf("%w: the generated schema does not plan: %w", ErrCheckpoint, err)
	}
	if _, err := applyPlans(ctx, conn, engine.Options{}, []engine.Plan{p}, nil, engine.WithAssertProbe()); err != nil {
		return fmt.Errorf("%w: the generated schema does not apply: %w", ErrCheckpoint, err)
	}
	got, _, err := snapshotScratch(ctx, conn)
	if err != nil {
		return fmt.Errorf("snapshot scratch database: %w", err)
	}
	if diff := engine.DiffSchemas(want, got); len(diff) > 0 {
		return fmt.Errorf("%w: the generated schema is not the one the migrations produce:\n%s",
			ErrCheckpoint, strings.Join(diff, "\n"))
	}

	return nil
}

// checkpointVersion stamps the checkpoint now, or one above the newest file when the directory is ahead
// of the clock, so the checkpoint always sorts last.
func checkpointVersion(migs []engine.Migration, now time.Time) int64 {
	v, _ := strconv.ParseInt(now.UTC().Format("20060102150405"), 10, 64) // a formatted timestamp always parses
	for _, m := range migs {
		if !m.Repeatable && m.Version >= v {
			v = m.Version + 1
		}
	}

	return v
}

func checkpointBody(cp Checkpoint, ddl string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "-- %s %s through=%014d\n", engine.DirectiveMarker, engine.DirectiveCheckpoint, cp.Through)
	fmt.Fprintf(&b, "-- %d migrations, %s through %s.\n", len(cp.Covers), cp.Covers[0], cp.Covers[len(cp.Covers)-1])
	fmt.Fprint(&b, "-- A target that has applied any of them records this file; one with no history runs it instead of them.\n\n")
	fmt.Fprintln(&b, strings.TrimSpace(ddl))

	return b.String()
}

// Checkpoints reads the checkpoints a target's standing ledger holds, newest first.
func (s *Store) Checkpoints(ctx context.Context, target string) ([]engine.Migration, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT a.migration, u.body
		FROM cp_run_applied a
		JOIN cp_runs r ON r.id = a.run_id
		JOIN cp_run_files u ON u.run_id = a.run_id AND u.name = a.migration || '.up.sql'
		WHERE r.target = $1 AND `+standingRow+` AND u.body LIKE '%'||$2||'%'
		ORDER BY a.migration DESC`, target, engine.DirectiveMarker+" "+engine.DirectiveCheckpoint)
	if err != nil {
		return nil, fmt.Errorf("list checkpoints: %w", err)
	}
	var out []engine.Migration
	var id, body string
	if _, err := pgx.ForEachRow(rows, []any{&id, &body}, func() error {
		if m, ok := checkpointOf(id, body); ok {
			out = append(out, m)
		}

		return nil
	}); err != nil {
		return nil, fmt.Errorf("read checkpoints: %w", err)
	}

	return out, nil
}

// checkpointOf rebuilds one migration from a ledger row; a body that does not load is not a checkpoint.
func checkpointOf(id, body string) (engine.Migration, bool) {
	migs, err := MigrationsFromFiles(map[string]string{id + ".up.sql": body})
	if err != nil || len(migs) != 1 || !migs[0].Checkpoint {
		return engine.Migration{}, false
	}

	return migs[0], true
}
