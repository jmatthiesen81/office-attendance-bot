package storage

import (
	"context"
	"database/sql"
	"time"

	"officebot/internal/domain"
)

// ListPolls returns the latest polls of a workspace, newest first.
func (s *Store) ListPolls(ctx context.Context, workspaceID string, limit int) ([]domain.Poll, error) {
	return s.queryPolls(ctx, s.db, `SELECT `+pollCols+` FROM polls WHERE workspace_id = ? ORDER BY week_start DESC LIMIT ?`, workspaceID, limit)
}

// Person is someone the bot knows from a poll.
type Person struct {
	SlackUserID string
	Name        string
}

// KnownPeople returns everybody who took part in a stored poll, with the
// name of their latest participation, sorted by name.
func (s *Store) KnownPeople(ctx context.Context, workspaceID string) ([]Person, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT slack_user_id, display_name FROM (
			SELECT pa.slack_user_id, pa.display_name,
			       row_number() OVER (PARTITION BY pa.slack_user_id ORDER BY p.week_start DESC) AS n
			FROM participants pa JOIN polls p ON p.id = pa.poll_id WHERE p.workspace_id = ?)
		WHERE n = 1 ORDER BY lower(display_name), slack_user_id`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Person
	for rows.Next() {
		var p Person
		if err := rows.Scan(&p.SlackUserID, &p.Name); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ----------------------------------------------------------- closed days

// ClosedDay is a work day on which the office is closed (public holiday,
// company holiday). Closed days count in no statistic.
type ClosedDay struct {
	Day       domain.Date
	Reason    string
	CreatedBy string
}

// AddClosedDays marks the work days (Monday to Friday) from from to to as
// closed. Existing entries get the new reason. It returns the number of days.
func (s *Store) AddClosedDays(ctx context.Context, workspaceID string, from, to domain.Date, reason, actor string, now time.Time) (int, error) {
	n := 0
	err := s.tx(ctx, func(tx *sql.Tx) error {
		for d := from; !d.After(to); d = d.AddDays(1) {
			if wd := d.Weekday(); wd == time.Saturday || wd == time.Sunday {
				continue
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO office_closed_days (workspace_id, day, reason, created_by, created_at) VALUES (?, ?, ?, ?, ?)
				ON CONFLICT (workspace_id, day) DO UPDATE SET reason = excluded.reason`, workspaceID, d.String(), reason, actor, unix(now)); err != nil {
				return err
			}
			n++
		}
		if n == 0 {
			return nil
		}
		return audit(ctx, tx, workspaceID, actor, "closed_days.add", map[string]any{"from": from.String(), "to": to.String(), "reason": reason, "days": n}, now)
	})
	return n, err
}

// RemoveClosedDay makes a day a normal work day again.
func (s *Store) RemoveClosedDay(ctx context.Context, workspaceID string, day domain.Date, actor string, now time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM office_closed_days WHERE workspace_id = ? AND day = ?`, workspaceID, day.String())
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return audit(ctx, tx, workspaceID, actor, "closed_days.remove", map[string]any{"day": day.String()}, now)
	})
}

// ListClosedDays returns the closed days from from to to (both included).
func (s *Store) ListClosedDays(ctx context.Context, workspaceID string, from, to domain.Date) ([]ClosedDay, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT day, reason, created_by FROM office_closed_days
		WHERE workspace_id = ? AND day BETWEEN ? AND ? ORDER BY day`, workspaceID, from.String(), to.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ClosedDay
	for rows.Next() {
		var c ClosedDay
		var day string
		if err := rows.Scan(&day, &c.Reason, &c.CreatedBy); err != nil {
			return nil, err
		}
		if c.Day, err = domain.ParseDate(day); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
