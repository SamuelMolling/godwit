//go:build load

package e2e

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
)

func TestLoadBackfillAtScale(t *testing.T) {
	rows := envInt("GODWIT_LOAD_ROWS", 10_000_000)
	batch := envInt("GODWIT_LOAD_BATCH", 20_000)
	r := newRig(t, 1)
	r.addTarget("scale")
	r.mustMigrate(migrationDir(t, migration{v1, "bf", bfTable, "DROP TABLE bf;"}))

	seed := seedRows(t, r.appDSN, "bf", rows)
	tableBefore := relSize(t, r.appDSN, "bf")
	journalBefore := relSize(t, r.appDSN, "godwit.journal")
	report(t, "backfill/seed", "rows", rows, "seconds", seed.Seconds(), "table_bytes", tableBefore)

	id := r.createRun(backfillMigration(v2, batch, ""))
	rep := r.claimer(id)

	var peakRSS, journalRows, lastCursor int64
	var backwards atomic.Bool
	watch := connectDB(t, r.appDSN)
	stop := make(chan struct{})
	var sampler sync.WaitGroup
	sampler.Add(1)
	go func() {
		defer sampler.Done()
		for {
			select {
			case <-stop:
				return
			case <-time.After(time.Second):
			}
			var n, cur int64
			if err := watch.QueryRow(context.Background(),
				`SELECT count(*), coalesce(max(cursor::bigint), 0) FROM godwit.journal`).Scan(&n, &cur); err != nil {
				continue
			}
			peakRSS = max(peakRSS, rss(rep.cmd.Process.Pid))
			journalRows = max(journalRows, n)
			if cur < lastCursor {
				backwards.Store(true)
			}
			lastCursor = cur
		}
	}()

	run, elapsed := watchRun(t, r, id)
	close(stop)
	sampler.Wait()
	if backwards.Load() {
		t.Fatal("the journalled cursor went backwards while the backfill ran")
	}
	if run.State != godwitv1.RunState_RUN_STATE_SUCCEEDED {
		t.Fatalf("run %s: state %s, error %s", id, run.State, run.Error)
	}

	stale := query[int64](t, r.appDSN, `SELECT count(*) FROM bf WHERE w IS DISTINCT FROM v * 2`)
	if stale != 0 {
		t.Fatalf("%d rows left unbackfilled", stale)
	}
	if n := query[int](t, r.appDSN, `SELECT count(*) FROM godwit.journal WHERE state = 'done'`); n != 2 {
		t.Fatalf("done journal rows = %d, want 2 (the table migration and the backfill)", n)
	}
	done := query[int64](t, r.appDSN,
		`SELECT rows_done FROM godwit.journal WHERE state = 'intent' AND cursor IS NOT NULL`)
	if done != int64(rows) {
		t.Fatalf("journalled rows_done = %d, want %d", done, rows)
	}
	report(t, "backfill/apply",
		"rows", rows, "batch", batch, "seconds", elapsed.Seconds(),
		"rows_per_second", int64(float64(rows)/elapsed.Seconds()),
		"batches", rows/batch, "peak_rss_kb", peakRSS,
		"journal_rows_peak", journalRows,
		"journal_bytes_before", journalBefore,
		"journal_bytes_after", relSize(t, r.appDSN, "godwit.journal"),
		"table_bytes_after", relSize(t, r.appDSN, "bf"))
}

func TestLoadBackfillPause(t *testing.T) {
	const rows, batch = 40_000, 1_000
	const pause = 200 * time.Millisecond
	r := newRig(t, 1)
	r.addTarget("paced")
	r.mustMigrate(migrationDir(t, migration{v1, "bf", bfTable, "DROP TABLE bf;"}))
	seedRows(t, r.appDSN, "bf", rows)

	_, free := watchRun(t, r, r.createRun(backfillMigration(v2, batch, "")))
	execSQL(t, r.appDSN, "UPDATE bf SET w = NULL")
	_, paced := watchRun(t, r, r.createRun(backfillMigration(v3, batch, fmt.Sprintf(" pause=%s", pause))))

	batches := rows / batch
	floor := time.Duration(batches-1) * pause
	if paced-free < floor*8/10 {
		t.Fatalf("pause=%s over %d batches added %s, want at least %s", pause, batches, paced-free, floor*8/10)
	}
	report(t, "backfill/pause",
		"rows", rows, "batch", batch, "batches", batches, "pause", pause,
		"no_pause_seconds", free.Seconds(), "paced_seconds", paced.Seconds(),
		"added_seconds", (paced - free).Seconds(), "expected_floor_seconds", floor.Seconds())
}

func TestLoadBackfillUnderWrites(t *testing.T) {
	rows := envInt("GODWIT_LOAD_WRITE_ROWS", 2_000_000)
	batch := envInt("GODWIT_LOAD_BATCH", 20_000)
	r := newRig(t, 1)
	r.addTarget("busy")
	r.mustMigrate(migrationDir(t, migration{v1, "bf", bfTable, "DROP TABLE bf;"}))
	seedRows(t, r.appDSN, "bf", rows)

	// The updater stays in a reserved band, so every seeded row outside it is untouched once the backfill starts.
	band := int64(rows / 5)
	var inserted, dirtied atomic.Int64
	stop := make(chan struct{})
	var writers sync.WaitGroup
	writers.Add(2)
	go func() {
		defer writers.Done()
		conn := connectDB(t, r.appDSN)
		for {
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
			}
			if _, err := conn.Exec(context.Background(),
				`INSERT INTO bf (v) SELECT (g % 1000)::int FROM generate_series(1, 100) g`); err == nil {
				inserted.Add(100)
			}
		}
	}()
	go func() {
		defer writers.Done()
		conn := connectDB(t, r.appDSN)
		for {
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
			}
			lo := 1 + rand.Int64N(band-100)
			if _, err := conn.Exec(context.Background(),
				`UPDATE bf SET v = v + 1 WHERE id BETWEEN $1 AND $1 + 99`, lo); err == nil {
				dirtied.Add(100)
			}
		}
	}()

	run, elapsed := watchRun(t, r, r.createRun(backfillMigration(v2, batch, "")))
	close(stop)
	writers.Wait()
	if run.State != godwitv1.RunState_RUN_STATE_SUCCEEDED {
		t.Fatalf("run: state %s, error %s", run.State, run.Error)
	}

	cursor := query[int64](t, r.appDSN,
		`SELECT cursor::bigint FROM godwit.journal WHERE state = 'intent' AND cursor IS NOT NULL`)
	done := query[int64](t, r.appDSN,
		`SELECT rows_done FROM godwit.journal WHERE state = 'intent' AND cursor IS NOT NULL`)
	stale := func(where string, args ...any) int64 {
		return query[int64](t, r.appDSN, `SELECT count(*) FROM bf WHERE w IS DISTINCT FROM v * 2 AND `+where, args...)
	}
	quiet := stale(`id > $1 AND id <= $2`, band, rows)
	report(t, "backfill/concurrent_writes",
		"seed_rows", rows, "seconds", elapsed.Seconds(),
		"inserted_during", inserted.Load(), "updated_during", dirtied.Load(),
		"final_cursor", cursor, "rows_done", done,
		"stale_in_written_band", stale(`id <= $1`, band),
		"stale_appended_after_the_last_batch", stale(`id > $1`, rows),
		"stale_in_the_quiet_band", quiet,
		"total_rows", query[int64](t, r.appDSN, `SELECT count(*) FROM bf`))
	if quiet != 0 {
		t.Fatalf("%d rows nobody wrote to were left unbackfilled", quiet)
	}
	if done < int64(rows) {
		t.Fatalf("journalled rows_done = %d, want at least the %d seeded rows", done, rows)
	}
}

func TestLoadHistoryGrowth(t *testing.T) {
	total := envInt("GODWIT_LOAD_HISTORY", 1000)
	chunk := envInt("GODWIT_LOAD_CHUNK", 100)
	r := newRig(t, 0)
	// Every request carries the whole directory: the default --max-files 2000 is a ceiling of 1000 migrations.
	r.startWith("--max-files", strconv.Itoa(4*total+8))
	r.addTarget("deep")

	dir := t.TempDir()
	base := int64(20260901000000)
	for applied := 0; applied < total; applied += chunk {
		for i := applied; i < applied+chunk; i++ {
			writeMigration(t, dir, base+int64(i), i)
		}
		admit := timed(func() { r.mustMigrate(dir) })
		report(t, "history/migrate", "history", applied+chunk, "chunk", chunk, "seconds", admit.Seconds())
		measureHistory(t, r, "curve", dir, applied+chunk)
	}
	cut := timed(func() { r.mustCLI("checkpoint", "--name", "squash", "--dir", dir) })
	record := timed(func() { r.mustMigrate(dir) })
	body := checkpointBody(t, dir)
	report(t, "history/checkpoint", "history", total,
		"generate_seconds", cut.Seconds(), "record_seconds", record.Seconds(),
		"body_bytes", len(body), "statements", strings.Count(body, ";"),
		"concurrently", strings.Count(body, "CONCURRENTLY"))
	measureHistory(t, r, "checkpointed", dir, total)
	report(t, "history/store_bytes",
		"history", total,
		"cp_run_files", relSize(t, r.storeDSN, "cp_run_files"),
		"cp_runs", relSize(t, r.storeDSN, "cp_runs"),
		"cp_run_applied", relSize(t, r.storeDSN, "cp_run_applied"),
		"cp_snapshots", relSize(t, r.storeDSN, "cp_snapshots"),
		"cp_plan_files", relSize(t, r.storeDSN, "cp_plan_files"))
}

func measureHistory(t *testing.T, r *rig, phase, dir string, applied int) {
	t.Helper()
	next := filepath.Join(t.TempDir(), "next")
	if err := os.MkdirAll(next, 0o750); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		body, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(next, e.Name()), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeMigration(t, next, newestVersion(t, dir)+1, applied)

	schema := filepath.Join(t.TempDir(), "desired.sql")
	var ddl strings.Builder
	for i := range applied {
		fmt.Fprintf(&ddl, "%s\n", tableDDL(i))
	}
	if err := os.WriteFile(schema, []byte(ddl.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	plan := timed(func() { r.mustCLI("plan", "--target", r.target, "--dir", next) })
	status := timed(func() { r.mustCLI("target", "status", r.target, "--dir", dir) })
	diff := timed(func() { r.mustCLI("diff", "--target", r.target, "--dir", dir, "--schema", schema, "--dry-run") })
	report(t, "history/"+phase, "history", applied,
		"plan_seconds", plan.Seconds(), "status_seconds", status.Seconds(), "diff_seconds", diff.Seconds())
}

func checkpointBody(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		body, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), "-- godwit: checkpoint") {
			return string(body)
		}
	}
	t.Fatal("no checkpoint file in " + dir)

	return ""
}

func newestVersion(t *testing.T, dir string) int64 {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var newest int64
	for _, e := range entries {
		v, err := strconv.ParseInt(strings.SplitN(e.Name(), "_", 2)[0], 10, 64)
		if err == nil && v > newest {
			newest = v
		}
	}

	return newest
}

// Unqualified DDL lands in the scratch role's own schema, which godwit checkpoint then renders as nothing.
func tableDDL(i int) string {
	return fmt.Sprintf("CREATE TABLE public.h%04d (id bigint PRIMARY KEY, note text);", i)
}

func writeMigration(t *testing.T, dir string, version int64, i int) {
	t.Helper()
	for name, body := range map[string]string{
		fmt.Sprintf("%014d_h%04d.up.sql", version, i):   tableDDL(i),
		fmt.Sprintf("%014d_h%04d.down.sql", version, i): fmt.Sprintf("DROP TABLE public.h%04d;", i),
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLoadManyTargets(t *testing.T) {
	targets := envInt("GODWIT_LOAD_TARGETS", 24)
	replicas := envInt("GODWIT_LOAD_REPLICAS", 3)
	r := newRig(t, replicas)

	names := make([]string, targets)
	for i := range names {
		names[i] = fmt.Sprintf("t%02d", i)
		r.mustCLI("target", "add", names[i], "--provider", "static", "--dsn", createDatabase(t, r.appDB+"_"+names[i]))
	}

	client := r.client()
	ids := make([]string, targets)
	submit := timed(func() {
		var wg sync.WaitGroup
		for i, name := range names {
			wg.Add(1)
			go func() {
				defer wg.Done()
				resp, err := client.CreateRun(context.Background(), connect.NewRequest(&godwitv1.CreateRunRequest{
					Target: name,
					Files:  files(migration{v1, "u", "CREATE TABLE u (id bigint PRIMARY KEY); SELECT pg_sleep(1);", "DROP TABLE u;"}),
				}))
				if err != nil {
					t.Error(err)

					return
				}
				ids[i] = resp.Msg.RunId
			}()
		}
		wg.Wait()
	})

	var peakStoreBackends int
	drain := timed(func() {
		waitUntil(t, converge, "every run settled", func() bool {
			peakStoreBackends = max(peakStoreBackends, backends(t, r.storeDB))
			for _, id := range ids {
				if r.getRun(id).State != godwitv1.RunState_RUN_STATE_SUCCEEDED {
					return false
				}
			}

			return true
		})
	})

	claims := make([]int, 0, replicas)
	for _, rep := range r.all() {
		claims = append(claims, rep.logs.count("run claimed"))
	}
	report(t, "scale/many_targets",
		"targets", targets, "replicas", replicas,
		"submit_seconds", submit.Seconds(), "drain_seconds", drain.Seconds(),
		"total_seconds", (submit + drain).Seconds(),
		"runs_per_second", float64(targets)/(submit+drain).Seconds(),
		"claims_per_replica", fmt.Sprint(claims), "peak_store_backends", peakStoreBackends)
}
