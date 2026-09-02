package controlplane

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// GetTS implements notify.TSStore; a missing key yields empty strings.
func (s *Store) GetTS(ctx context.Context, kind, key string) (string, string, error) {
	var channel, ts string
	err := s.pool.QueryRow(ctx,
		`SELECT channel, ts FROM cp_notifications WHERE kind = $1 AND key = $2`,
		kind, key).Scan(&channel, &ts)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("get notification ts: %w", err)
	}

	return channel, ts, nil
}

// PutTS implements notify.TSStore.
func (s *Store) PutTS(ctx context.Context, kind, key, channel, ts string) error {
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO cp_notifications (kind, key, channel, ts) VALUES ($1, $2, $3, $4)
		 ON CONFLICT (kind, key) DO UPDATE SET channel = EXCLUDED.channel, ts = EXCLUDED.ts`,
		kind, key, channel, ts); err != nil {
		return fmt.Errorf("put notification ts: %w", err)
	}

	return nil
}
