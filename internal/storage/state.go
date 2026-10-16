package storage

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// StateRenderedVersion is the app_state key of the bot version that last
// re-rendered the Slack messages.
const StateRenderedVersion = "rendered_version"

// GetState returns a value of the bot's own state, or "" if it is not set.
func (s *Store) GetState(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM app_state WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// SetState stores a value of the bot's own state.
func (s *Store) SetState(ctx context.Context, key, value string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO app_state (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`, key, value, unix(now))
	return err
}
