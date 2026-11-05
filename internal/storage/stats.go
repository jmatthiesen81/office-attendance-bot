package storage

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"officebot/internal/domain"
)

// Daily status of a person (ADR 0003).
const (
	StatusOffice   = "office"
	StatusRemote   = "remote"
	StatusAway     = "away"
	StatusNoAnswer = "no_answer"
)

// AttendanceDay is the status of one person on one work day of a closed poll.
type AttendanceDay struct {
	SlackUserID string
	Name        string
	Day         domain.Date
	Status      string
	// Planned is the status of the first calculation, i.e. at the deadline.
	Planned string
}

// Corrected reports whether an admin changed the status after the deadline.
func (a AttendanceDay) Corrected() bool { return a.Status != a.Planned }

// DirtyPoll is a closed poll whose daily status must be recalculated.
type DirtyPoll struct {
	Poll  domain.Poll
	Dirty int64
}

// DirtyClosedPolls returns the closed polls with changes since their daily
// status was last calculated.
func (s *Store) DirtyClosedPolls(ctx context.Context, workspaceID string) ([]DirtyPoll, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+pollCols+`, stats_dirty FROM polls
		WHERE workspace_id = ? AND status = 'closed' AND stats_dirty > 0 ORDER BY week_start`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DirtyPoll
	for rows.Next() {
		var d DirtyPoll
		var ws, we, dl, ca int64
		p := &d.Poll
		if err := rows.Scan(&p.ID, &p.WorkspaceID, &p.ISOYear, &p.ISOWeek, &ws, &we, &dl, &p.Status, &ca,
			&p.SummaryChannelID, &p.SummaryMessageTS, &p.SummaryVersion, &p.SummarySyncedVersion, &d.Dirty); err != nil {
			return nil, err
		}
		p.WeekStart, p.WeekEnd, p.Deadline, p.CreatedAt = fromUnix(ws), fromUnix(we), fromUnix(dl), fromUnix(ca)
		out = append(out, d)
	}
	return out, rows.Err()
}

// ReplaceAttendanceWeek stores the daily status of a poll's week. The planned
// status of existing rows is kept; rows of people no longer in the poll are
// removed. Afterwards the poll is marked clean unless it changed meanwhile
// (seen is the dirty counter read before the calculation).
func (s *Store) ReplaceAttendanceWeek(ctx context.Context, workspaceID string, pollID int64, isoWeek string, days []AttendanceDay, seen int64, now time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS keep_days (slack_user_id TEXT, day TEXT)`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM keep_days`); err != nil {
			return err
		}
		for _, d := range days {
			if _, err := tx.ExecContext(ctx, `INSERT INTO attendance_days (workspace_id, slack_user_id, day, status, planned_status, iso_week, display_name, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT (workspace_id, slack_user_id, day) DO UPDATE SET
					status = excluded.status, iso_week = excluded.iso_week, display_name = excluded.display_name, updated_at = excluded.updated_at
				WHERE (status, iso_week, display_name) IS NOT (excluded.status, excluded.iso_week, excluded.display_name)`,
				workspaceID, d.SlackUserID, d.Day.String(), d.Status, d.Status, isoWeek, d.Name, unix(now)); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO keep_days VALUES (?, ?)`, d.SlackUserID, d.Day.String()); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM attendance_days WHERE workspace_id = ? AND iso_week = ?
			AND NOT EXISTS (SELECT 1 FROM keep_days k WHERE k.slack_user_id = attendance_days.slack_user_id AND k.day = attendance_days.day)`,
			workspaceID, isoWeek); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE polls SET stats_dirty = 0 WHERE id = ? AND stats_dirty = ?`, pollID, seen)
		return err
	})
}

// AttendanceDays returns the daily status from from to to (both included),
// ordered by person and day. An empty userID returns everybody.
func (s *Store) AttendanceDays(ctx context.Context, workspaceID, userID string, from, to domain.Date) ([]AttendanceDay, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT slack_user_id, display_name, day, status, planned_status FROM attendance_days
		WHERE workspace_id = ? AND day BETWEEN ? AND ? AND (? = '' OR slack_user_id = ?)
		ORDER BY slack_user_id, day`, workspaceID, from.String(), to.String(), userID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AttendanceDay
	for rows.Next() {
		var a AttendanceDay
		var day string
		if err := rows.Scan(&a.SlackUserID, &a.Name, &day, &a.Status, &a.Planned); err != nil {
			return nil, err
		}
		if a.Day, err = domain.ParseDate(day); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AttendanceRange returns the first and last day with daily status; ok is
// false if there is none.
func (s *Store) AttendanceRange(ctx context.Context, workspaceID string) (first, last domain.Date, ok bool, err error) {
	var minDay, maxDay sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT MIN(day), MAX(day) FROM attendance_days WHERE workspace_id = ?`, workspaceID).
		Scan(&minDay, &maxDay); err != nil {
		return first, last, false, err
	}
	if !minDay.Valid {
		return first, last, false, nil
	}
	if first, err = domain.ParseDate(minDay.String); err != nil {
		return first, last, false, err
	}
	last, err = domain.ParseDate(maxDay.String)
	return first, last, err == nil, err
}

// --------------------------------------------------------------- checked days

// VerifyDay marks a day as checked: the status of everybody asked on that
// day matches actual attendance.
func (s *Store) VerifyDay(ctx context.Context, workspaceID string, day domain.Date, actor string, now time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO verified_days (workspace_id, day, verified_by, verified_at) VALUES (?, ?, ?, ?)
			ON CONFLICT (workspace_id, day) DO UPDATE SET verified_by = excluded.verified_by, verified_at = excluded.verified_at`,
			workspaceID, day.String(), actor, unix(now)); err != nil {
			return err
		}
		return audit(ctx, tx, workspaceID, actor, "day.verify", map[string]any{"day": day.String()}, now)
	})
}

// UnverifyDay removes the "checked" mark of a day.
func (s *Store) UnverifyDay(ctx context.Context, workspaceID string, day domain.Date, actor string, now time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM verified_days WHERE workspace_id = ? AND day = ?`, workspaceID, day.String())
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return audit(ctx, tx, workspaceID, actor, "day.unverify", map[string]any{"day": day.String()}, now)
	})
}

// VerifiedDay is a checked day.
type VerifiedDay struct {
	By string
	At time.Time
}

// VerifiedDays returns the checked days from from to to, by day ("2026-11-02").
func (s *Store) VerifiedDays(ctx context.Context, workspaceID string, from, to domain.Date) (map[string]VerifiedDay, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT day, verified_by, verified_at FROM verified_days WHERE workspace_id = ? AND day BETWEEN ? AND ?`,
		workspaceID, from.String(), to.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]VerifiedDay{}
	for rows.Next() {
		var day string
		var v VerifiedDay
		var at int64
		if err := rows.Scan(&day, &v.By, &at); err != nil {
			return nil, err
		}
		v.At = fromUnix(at)
		out[day] = v
	}
	return out, rows.Err()
}

// ------------------------------------------------------------- monthly stats

// MonthlyStats is the stored aggregate of one month, without personal data.
type MonthlyStats struct {
	Month  string // "2026-11"
	Data   []byte // JSON, see package stats
	Frozen bool
}

// SaveMonthlyStats stores the aggregate of a month. A frozen month is only
// overwritten when freeze is set, i.e. right before daily data is deleted.
func (s *Store) SaveMonthlyStats(ctx context.Context, workspaceID, month string, data []byte, freeze bool, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO monthly_stats (workspace_id, month, data, frozen, computed_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (workspace_id, month) DO UPDATE SET data = excluded.data, frozen = MAX(frozen, excluded.frozen), computed_at = excluded.computed_at
		WHERE frozen = 0 OR excluded.frozen = 1`, workspaceID, month, string(data), b2i(freeze), unix(now))
	return err
}

// ListMonthlyStats returns the stored months from fromMonth to toMonth.
func (s *Store) ListMonthlyStats(ctx context.Context, workspaceID, fromMonth, toMonth string) ([]MonthlyStats, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT month, data, frozen FROM monthly_stats WHERE workspace_id = ? AND month BETWEEN ? AND ? ORDER BY month`,
		workspaceID, fromMonth, toMonth)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MonthlyStats
	for rows.Next() {
		var m MonthlyStats
		var data string
		var frozen int
		if err := rows.Scan(&m.Month, &data, &frozen); err != nil {
			return nil, err
		}
		m.Data, m.Frozen = []byte(data), frozen != 0
		out = append(out, m)
	}
	return out, rows.Err()
}

// FirstMonthlyStats returns the earliest stored month, or "".
func (s *Store) FirstMonthlyStats(ctx context.Context, workspaceID string) (string, error) {
	var m sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT MIN(month) FROM monthly_stats WHERE workspace_id = ?`, workspaceID).Scan(&m)
	return m.String, err
}

// ------------------------------------------------------------------- purge

// PurgeScope selects daily data to delete: a date range, or one person
// (UserID) with all their data.
type PurgeScope struct {
	From, To domain.Date // both included; zero From = from the beginning
	UserID   string
}

// PurgePreview describes what a purge would delete.
type PurgePreview struct {
	Rows      int
	People    int
	First     domain.Date
	Last      domain.Date
	Responses int // only for a person: responses still within RETENTION_WEEKS
	Absences  int // only for a person
}

func (p PurgeScope) where() (string, []any) {
	if p.UserID != "" {
		return `slack_user_id = ?`, []any{p.UserID}
	}
	from := "0000-00-00"
	if !p.From.IsZero() {
		from = p.From.String()
	}
	return `day BETWEEN ? AND ?`, []any{from, p.To.String()}
}

// PreviewPurge counts what Purge would delete.
func (s *Store) PreviewPurge(ctx context.Context, workspaceID string, scope PurgeScope) (PurgePreview, error) {
	var p PurgePreview
	where, args := scope.where()
	var first, last sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*), COUNT(DISTINCT slack_user_id), MIN(day), MAX(day) FROM attendance_days
		WHERE workspace_id = ? AND `+where, append([]any{workspaceID}, args...)...).Scan(&p.Rows, &p.People, &first, &last); err != nil {
		return p, err
	}
	if first.Valid {
		p.First, _ = domain.ParseDate(first.String)
		p.Last, _ = domain.ParseDate(last.String)
	}
	if scope.UserID != "" {
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM responses r JOIN polls p ON p.id = r.poll_id WHERE p.workspace_id = ? AND r.slack_user_id = ?`,
			workspaceID, scope.UserID).Scan(&p.Responses); err != nil {
			return p, err
		}
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM absences WHERE workspace_id = ? AND slack_user_id = ?`,
			workspaceID, scope.UserID).Scan(&p.Absences); err != nil {
			return p, err
		}
	}
	return p, nil
}

// Purge deletes daily data. For a person it also deletes their participation
// in stored polls (with responses, notices and jobs), their time off and
// language choice, so the data cannot be recalculated; summaries of open
// polls they took part in are re-rendered. Months must be frozen before.
func (s *Store) Purge(ctx context.Context, workspaceID string, scope PurgeScope, actor string, now time.Time) (rows int64, err error) {
	if scope.UserID == "" && scope.To.IsZero() {
		return 0, errors.New("purge without range or person")
	}
	err = s.tx(ctx, func(tx *sql.Tx) error {
		where, args := scope.where()
		res, err := tx.ExecContext(ctx, `DELETE FROM attendance_days WHERE workspace_id = ? AND `+where, append([]any{workspaceID}, args...)...)
		if err != nil {
			return err
		}
		rows, _ = res.RowsAffected()
		details := map[string]any{"rows": rows}
		if scope.UserID != "" {
			details["user"] = scope.UserID
			if _, err := tx.ExecContext(ctx, `UPDATE polls SET summary_version = summary_version + 1
				WHERE workspace_id = ? AND status = 'open' AND id IN (SELECT poll_id FROM participants WHERE slack_user_id = ?)`,
				workspaceID, scope.UserID); err != nil {
				return err
			}
			for _, q := range []string{
				`DELETE FROM participants WHERE slack_user_id = ? AND poll_id IN (SELECT id FROM polls WHERE workspace_id = ?)`,
				`DELETE FROM absences WHERE slack_user_id = ? AND workspace_id = ?`,
				`DELETE FROM user_settings WHERE slack_user_id = ? AND workspace_id = ?`,
			} {
				if _, err := tx.ExecContext(ctx, q, scope.UserID, workspaceID); err != nil {
					return err
				}
			}
		} else {
			details["to"] = scope.To.String()
			if !scope.From.IsZero() {
				details["from"] = scope.From.String()
			}
		}
		return audit(ctx, tx, workspaceID, actor, "stats.purge", details, now)
	})
	return rows, err
}
