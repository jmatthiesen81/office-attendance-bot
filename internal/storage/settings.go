package storage

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// UserLanguage is one stored language choice.
type UserLanguage struct {
	SlackUserID string
	Language    string
	UpdatedAt   time.Time
}

// GetUserLanguage returns the language a person chose, or "" if they chose none.
func (s *Store) GetUserLanguage(ctx context.Context, workspaceID, userID string) (string, error) {
	var lang string
	err := s.db.QueryRowContext(ctx, `SELECT language FROM user_settings WHERE workspace_id = ? AND slack_user_id = ?`,
		workspaceID, userID).Scan(&lang)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return lang, err
}

// SetUserLanguage stores the language a person chose.
func (s *Store) SetUserLanguage(ctx context.Context, workspaceID, userID, lang string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO user_settings (workspace_id, slack_user_id, language, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (workspace_id, slack_user_id) DO UPDATE SET language = excluded.language, updated_at = excluded.updated_at`,
		workspaceID, userID, lang, unix(now))
	return err
}

// ResetUserLanguage forgets the language a person chose. changed reports
// whether there was one.
func (s *Store) ResetUserLanguage(ctx context.Context, workspaceID, userID string) (changed bool, err error) {
	// The language is the only setting so far; with more, this becomes an UPDATE.
	res, err := s.db.ExecContext(ctx, `DELETE FROM user_settings WHERE workspace_id = ? AND slack_user_id = ?`,
		workspaceID, userID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ListUserLanguages returns all stored language choices of a workspace.
func (s *Store) ListUserLanguages(ctx context.Context, workspaceID string) ([]UserLanguage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT slack_user_id, language, updated_at FROM user_settings
		WHERE workspace_id = ? AND language <> '' ORDER BY slack_user_id`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UserLanguage
	for rows.Next() {
		var u UserLanguage
		var upd int64
		if err := rows.Scan(&u.SlackUserID, &u.Language, &upd); err != nil {
			return nil, err
		}
		u.UpdatedAt = fromUnix(upd)
		out = append(out, u)
	}
	return out, rows.Err()
}

// SlackLocale returns the Slack locale stored with the person's latest
// participation, or "" if there is none.
func (s *Store) SlackLocale(ctx context.Context, workspaceID, userID string) (string, error) {
	var locale string
	err := s.db.QueryRowContext(ctx, `SELECT pa.slack_locale FROM participants pa JOIN polls p ON p.id = pa.poll_id
		WHERE p.workspace_id = ? AND pa.slack_user_id = ? ORDER BY p.week_start DESC LIMIT 1`, workspaceID, userID).Scan(&locale)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return locale, err
}

// DeleteUserSettingsBefore removes the settings of people who changed them
// before t and take part in no stored poll. Old polls are deleted after the
// retention period, so this forgets people who left the target group.
func (s *Store) DeleteUserSettingsBefore(ctx context.Context, t time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM user_settings WHERE updated_at < ? AND NOT EXISTS
		(SELECT 1 FROM participants pa JOIN polls p ON p.id = pa.poll_id
		 WHERE p.workspace_id = user_settings.workspace_id AND pa.slack_user_id = user_settings.slack_user_id)`, unix(t))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
