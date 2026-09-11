package engine

import (
	"context"
	"fmt"
	"hash/fnv"
	"time"
)

const appName = "godwit"

// Without keepalives a black-holed replica leaves a backend holding the advisory lock for the two hours the operating system takes to give up on the socket.
const sessionSetup = `SET application_name = '` + appName + `';
SET tcp_keepalives_idle = 30;
SET tcp_keepalives_interval = 10;
SET tcp_keepalives_count = 3`

func lockKey(dbname string) int64 {
	h := fnv.New64a()
	h.Write([]byte("godwit:" + dbname))

	return int64(h.Sum64())
}

func acquireLock(ctx context.Context, db DB, wait time.Duration) (release func(), err error) {
	var dbname string
	if err := db.QueryRow(ctx, `SELECT current_database()`).Scan(&dbname); err != nil {
		return nil, fmt.Errorf("resolve database name: %w", err)
	}
	// Best effort: a target that refuses either setting still migrates, it just orphans for longer.
	_, _ = db.Exec(ctx, sessionSetup)
	key := lockKey(dbname)
	if err := waitForLock(ctx, db, key, wait); err != nil {
		return nil, fmt.Errorf("acquire advisory lock on %s%s: %w", dbname, lockHolder(ctx, db, key), err)
	}

	return func() {
		_, _ = db.Exec(ctx, `SELECT pg_advisory_unlock($1)`, key)
	}, nil
}

// lock_timeout does not cover advisory locks and statement_timeout does, and a session lock outlives the transaction that took it.
func waitForLock(ctx context.Context, db DB, key int64, wait time.Duration) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, fmt.Sprintf(
		"SET LOCAL statement_timeout = %d; SELECT pg_advisory_lock(%d)", wait.Milliseconds(), key)); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

// godwit never terminates the holder: between two statements a peer executor looks exactly like an orphan, and killing one mid-migration is worse than waiting.
func lockHolder(ctx context.Context, db DB, key int64) string {
	var pid int32
	var app, state string
	var since float64
	err := db.QueryRow(ctx, `
		SELECT a.pid, a.application_name, a.state, extract(epoch FROM now() - a.state_change)
		FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid
		WHERE l.locktype = 'advisory' AND l.granted
		  AND l.classid = $1::oid AND l.objid = $2::oid AND l.objsubid = 1
		LIMIT 1`, int64(uint32(uint64(key)>>32)), int64(uint32(key))).Scan(&pid, &app, &state, &since)
	if err != nil {
		return ""
	}

	return fmt.Sprintf(" (held by pid %d, application_name %q, %s for %.0fs)", pid, app, state, since)
}
