package engine

import (
	"context"
	"fmt"
	"hash/fnv"
)

func lockKey(dbname string) int64 {
	h := fnv.New64a()
	h.Write([]byte("godwit:" + dbname))

	return int64(h.Sum64())
}

// acquireLock takes a session advisory lock scoped to the current database.
func acquireLock(ctx context.Context, db DB) (release func(), err error) {
	var dbname string
	if err := db.QueryRow(ctx, `SELECT current_database()`).Scan(&dbname); err != nil {
		return nil, fmt.Errorf("resolve database name: %w", err)
	}
	key := lockKey(dbname)
	if _, err := db.Exec(ctx, `SELECT pg_advisory_lock($1)`, key); err != nil {
		return nil, fmt.Errorf("acquire advisory lock: %w", err)
	}

	return func() {
		_, _ = db.Exec(ctx, `SELECT pg_advisory_unlock($1)`, key)
	}, nil
}
