package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Roles of people who may log in to the web interface (ADR 0003).
const (
	RoleAdmin   = "admin"   // everything
	RoleAnalyst = "analyst" // all statistics, also per person; no changes
	RoleViewer  = "viewer"  // aggregated statistics only
)

// Roles lists the roles from the most to the least privileged.
var Roles = []string{RoleAdmin, RoleAnalyst, RoleViewer}

func roleRank(r string) int {
	switch r {
	case RoleAdmin:
		return 3
	case RoleAnalyst:
		return 2
	case RoleViewer:
		return 1
	}
	return 0
}

// ValidRole reports whether r is a known role.
func ValidRole(r string) bool { return roleRank(r) > 0 }

// RoleAtLeast reports whether role have includes the rights of role need.
func RoleAtLeast(have, need string) bool {
	return roleRank(have) > 0 && roleRank(have) >= roleRank(need)
}

// Errors of the access management.
var (
	ErrExists    = errors.New("already exists")
	ErrLastAdmin = errors.New("the last admin can be neither removed nor downgraded")
)

// WebUser is a person who may log in to the web interface.
type WebUser struct {
	SlackUserID string
	Role        string
	DisplayName string
	AddedBy     string
	AddedAt     time.Time
	LastLoginAt time.Time
}

const webUserCols = `slack_user_id, role, display_name, added_by, added_at, last_login_at`

func scanWebUser(r scanner) (*WebUser, error) {
	var u WebUser
	var added, last int64
	err := r.Scan(&u.SlackUserID, &u.Role, &u.DisplayName, &u.AddedBy, &added, &last)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	u.AddedAt, u.LastLoginAt = fromUnix(added), fromUnix(last)
	return &u, nil
}

// GetWebUser returns a person with web access or ErrNotFound.
func (s *Store) GetWebUser(ctx context.Context, workspaceID, userID string) (*WebUser, error) {
	return scanWebUser(s.db.QueryRowContext(ctx, `SELECT `+webUserCols+` FROM web_users WHERE workspace_id = ? AND slack_user_id = ?`,
		workspaceID, userID))
}

// ListWebUsers returns everybody with web access, admins first.
func (s *Store) ListWebUsers(ctx context.Context, workspaceID string) ([]WebUser, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+webUserCols+` FROM web_users WHERE workspace_id = ?
		ORDER BY CASE role WHEN 'admin' THEN 0 WHEN 'analyst' THEN 1 ELSE 2 END, lower(display_name), slack_user_id`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WebUser
	for rows.Next() {
		u, err := scanWebUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

// AddWebUser grants web access. It returns ErrExists if the person already has it.
func (s *Store) AddWebUser(ctx context.Context, workspaceID string, u WebUser, actor string, now time.Time) error {
	if !ValidRole(u.Role) {
		return errors.New("invalid role")
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO web_users (workspace_id, slack_user_id, role, display_name, added_by, added_at)
			VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`, workspaceID, u.SlackUserID, u.Role, u.DisplayName, actor, unix(now))
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrExists
		}
		return audit(ctx, tx, workspaceID, actor, "web_user.add", map[string]any{"user": u.SlackUserID, "role": u.Role}, now)
	})
}

// SetWebUserRole changes the role of a person. changed is false if the role
// was already set.
func (s *Store) SetWebUserRole(ctx context.Context, workspaceID, userID, role, actor string, now time.Time) (changed bool, err error) {
	if !ValidRole(role) {
		return false, errors.New("invalid role")
	}
	err = s.tx(ctx, func(tx *sql.Tx) error {
		u, err := scanWebUser(tx.QueryRowContext(ctx, `SELECT `+webUserCols+` FROM web_users WHERE workspace_id = ? AND slack_user_id = ?`,
			workspaceID, userID))
		if err != nil {
			return err
		}
		if u.Role == role {
			return nil
		}
		if u.Role == RoleAdmin {
			if err := ensureOtherAdmin(ctx, tx, workspaceID, userID); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE web_users SET role = ? WHERE workspace_id = ? AND slack_user_id = ?`,
			role, workspaceID, userID); err != nil {
			return err
		}
		changed = true
		return audit(ctx, tx, workspaceID, actor, "web_user.set_role", map[string]any{"user": userID, "from": u.Role, "to": role}, now)
	})
	return changed, err
}

// RemoveWebUser revokes web access and ends all sessions of the person.
func (s *Store) RemoveWebUser(ctx context.Context, workspaceID, userID, actor string, now time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		u, err := scanWebUser(tx.QueryRowContext(ctx, `SELECT `+webUserCols+` FROM web_users WHERE workspace_id = ? AND slack_user_id = ?`,
			workspaceID, userID))
		if err != nil {
			return err
		}
		if u.Role == RoleAdmin {
			if err := ensureOtherAdmin(ctx, tx, workspaceID, userID); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM web_users WHERE workspace_id = ? AND slack_user_id = ?`, workspaceID, userID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM web_sessions WHERE workspace_id = ? AND slack_user_id = ?`, workspaceID, userID); err != nil {
			return err
		}
		return audit(ctx, tx, workspaceID, actor, "web_user.remove", map[string]any{"user": userID, "role": u.Role}, now)
	})
}

func ensureOtherAdmin(ctx context.Context, tx *sql.Tx, workspaceID, userID string) error {
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM web_users WHERE workspace_id = ? AND role = 'admin' AND slack_user_id <> ?`,
		workspaceID, userID).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return ErrLastAdmin
	}
	return nil
}

// RecordWebLogin stores the time of a login and the current Slack name.
func (s *Store) RecordWebLogin(ctx context.Context, workspaceID, userID, displayName string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE web_users SET last_login_at = ?, display_name = CASE WHEN ? <> '' THEN ? ELSE display_name END
		WHERE workspace_id = ? AND slack_user_id = ?`, unix(now), displayName, displayName, workspaceID, userID)
	return err
}

// ------------------------------------------------------------------ sessions

// WebSession is a logged-in browser. Only the hash of the session ID is stored.
type WebSession struct {
	IDHash      string
	SlackUserID string
	CSRFToken   string
	CreatedAt   time.Time
	LastSeenAt  time.Time
	ExpiresAt   time.Time
}

// CreateWebSession stores a new session.
func (s *Store) CreateWebSession(ctx context.Context, workspaceID string, sess WebSession) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO web_sessions (id_hash, workspace_id, slack_user_id, csrf_token, created_at, last_seen_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, sess.IDHash, workspaceID, sess.SlackUserID, sess.CSRFToken,
		unix(sess.CreatedAt), unix(sess.LastSeenAt), unix(sess.ExpiresAt))
	return err
}

// GetWebSession returns a session that is neither expired nor idle for
// longer than idle, and records the activity. It returns ErrNotFound otherwise.
func (s *Store) GetWebSession(ctx context.Context, workspaceID, idHash string, idle time.Duration, now time.Time) (*WebSession, error) {
	var sess WebSession
	var created, seen, expires int64
	err := s.db.QueryRowContext(ctx, `UPDATE web_sessions SET last_seen_at = ?
		WHERE id_hash = ? AND workspace_id = ? AND expires_at > ? AND last_seen_at > ?
		RETURNING id_hash, slack_user_id, csrf_token, created_at, last_seen_at, expires_at`,
		unix(now), idHash, workspaceID, unix(now), unix(now.Add(-idle))).
		Scan(&sess.IDHash, &sess.SlackUserID, &sess.CSRFToken, &created, &seen, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	sess.CreatedAt, sess.LastSeenAt, sess.ExpiresAt = fromUnix(created), fromUnix(seen), fromUnix(expires)
	return &sess, nil
}

// DeleteWebSession ends a session.
func (s *Store) DeleteWebSession(ctx context.Context, idHash string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM web_sessions WHERE id_hash = ?`, idHash)
	return err
}

// CreateLoginState stores the state and nonce of a started login.
func (s *Store) CreateLoginState(ctx context.Context, stateHash, nonce, returnTo string, expires time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO web_login_states (state_hash, nonce, return_to, expires_at) VALUES (?, ?, ?, ?)`,
		stateHash, nonce, returnTo, unix(expires))
	return err
}

// ConsumeLoginState returns and deletes a login state. Each state can be used
// once; expired states are not found.
func (s *Store) ConsumeLoginState(ctx context.Context, stateHash string, now time.Time) (nonce, returnTo string, err error) {
	var valid bool
	err = s.db.QueryRowContext(ctx, `DELETE FROM web_login_states WHERE state_hash = ? RETURNING nonce, return_to, expires_at > ?`,
		stateHash, unix(now)).Scan(&nonce, &returnTo, &valid)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !valid) {
		return "", "", ErrNotFound
	}
	if err != nil {
		return "", "", err
	}
	return nonce, returnTo, nil
}

// DeleteExpiredWebState removes expired sessions and login states.
func (s *Store) DeleteExpiredWebState(ctx context.Context, idle time.Duration, now time.Time) (int64, error) {
	var total int64
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`DELETE FROM web_sessions WHERE expires_at <= ? OR last_seen_at <= ?`, []any{unix(now), unix(now.Add(-idle))}},
		{`DELETE FROM web_login_states WHERE expires_at <= ?`, []any{unix(now)}},
	} {
		res, err := s.db.ExecContext(ctx, q.sql, q.args...)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
	}
	return total, nil
}

// ----------------------------------------------------------------- audit log

// AuditEntry is one recorded change.
type AuditEntry struct {
	ID      int64
	At      time.Time
	Actor   string
	Action  string
	Details map[string]any
}

func audit(ctx context.Context, q queryer, workspaceID, actor, action string, details map[string]any, now time.Time) error {
	b, err := json.Marshal(details)
	if err != nil {
		return err
	}
	_, err = q.ExecContext(ctx, `INSERT INTO audit_log (workspace_id, at, actor, action, details) VALUES (?, ?, ?, ?, ?)`,
		workspaceID, unix(now), actor, action, string(b))
	return err
}

// Audit records a change made by actor (a Slack user ID or "cli").
func (s *Store) Audit(ctx context.Context, workspaceID, actor, action string, details map[string]any, now time.Time) error {
	return audit(ctx, s.db, workspaceID, actor, action, details, now)
}

// ListAudit returns audit entries, newest first.
func (s *Store) ListAudit(ctx context.Context, workspaceID string, limit, offset int) ([]AuditEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, at, actor, action, details FROM audit_log WHERE workspace_id = ?
		ORDER BY at DESC, id DESC LIMIT ? OFFSET ?`, workspaceID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var at int64
		var details string
		if err := rows.Scan(&e.ID, &at, &e.Actor, &e.Action, &details); err != nil {
			return nil, err
		}
		e.At = fromUnix(at)
		if err := json.Unmarshal([]byte(details), &e.Details); err != nil {
			e.Details = map[string]any{"raw": details}
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
