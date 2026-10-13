package storage

import (
	"context"
	"database/sql"
	"time"

	"officebot/internal/domain"
)

const absenceCols = `id, slack_user_id, start_date, end_date, created_by`

func scanAbsences(rows *sql.Rows) ([]domain.Absence, error) {
	defer rows.Close()
	var out []domain.Absence
	for rows.Next() {
		var a domain.Absence
		var start, end string
		if err := rows.Scan(&a.ID, &a.SlackUserID, &start, &end, &a.CreatedBy); err != nil {
			return nil, err
		}
		var err error
		if a.Start, err = domain.ParseDate(start); err != nil {
			return nil, err
		}
		if a.End, err = domain.ParseDate(end); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ListAbsences returns the absences of a workspace that end on or after
// from, ordered by user and start. An empty userID lists everybody.
func (s *Store) ListAbsences(ctx context.Context, workspaceID, userID string, from domain.Date) ([]domain.Absence, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+absenceCols+` FROM absences
		WHERE workspace_id = ? AND (? = '' OR slack_user_id = ?) AND end_date >= ?
		ORDER BY slack_user_id, start_date`, workspaceID, userID, userID, from.String())
	if err != nil {
		return nil, err
	}
	return scanAbsences(rows)
}

// AbsencesBetween returns the absences of a user that overlap [from, to].
func (s *Store) AbsencesBetween(ctx context.Context, workspaceID, userID string, from, to domain.Date) ([]domain.Absence, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+absenceCols+` FROM absences
		WHERE workspace_id = ? AND slack_user_id = ? AND end_date >= ? AND start_date <= ?
		ORDER BY start_date`, workspaceID, userID, from.String(), to.String())
	if err != nil {
		return nil, err
	}
	return scanAbsences(rows)
}

// AddAbsence stores time off of a user. Overlapping and adjacent absences of
// the same user are merged into one, which is returned.
func (s *Store) AddAbsence(ctx context.Context, workspaceID, userID string, start, end domain.Date, createdBy string, now time.Time) (merged domain.Absence, err error) {
	merged = domain.Absence{SlackUserID: userID, Start: start, End: end, CreatedBy: createdBy}
	err = s.tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `DELETE FROM absences
			WHERE workspace_id = ? AND slack_user_id = ? AND end_date >= ? AND start_date <= ?
			RETURNING `+absenceCols, workspaceID, userID, start.AddDays(-1).String(), end.AddDays(1).String())
		if err != nil {
			return err
		}
		touching, err := scanAbsences(rows)
		if err != nil {
			return err
		}
		for _, a := range touching {
			if a.Start.Before(merged.Start) {
				merged.Start = a.Start
			}
			if a.End.After(merged.End) {
				merged.End = a.End
			}
		}
		return insertAbsence(ctx, tx, workspaceID, &merged, now)
	})
	return merged, err
}

func insertAbsence(ctx context.Context, tx *sql.Tx, workspaceID string, a *domain.Absence, now time.Time) error {
	return tx.QueryRowContext(ctx, `INSERT INTO absences (workspace_id, slack_user_id, start_date, end_date, created_by, created_at)
		VALUES (?, ?, ?, ?, ?, ?) RETURNING id`,
		workspaceID, a.SlackUserID, a.Start.String(), a.End.String(), a.CreatedBy, unix(now)).Scan(&a.ID)
}

// RemoveAbsenceDays makes a user present again from start to end: absences
// inside the range are deleted, absences reaching into it are shortened or
// split. changed reports whether anything was removed.
func (s *Store) RemoveAbsenceDays(ctx context.Context, workspaceID, userID string, start, end domain.Date, now time.Time) (changed bool, err error) {
	err = s.tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `DELETE FROM absences
			WHERE workspace_id = ? AND slack_user_id = ? AND end_date >= ? AND start_date <= ?
			RETURNING `+absenceCols, workspaceID, userID, start.String(), end.String())
		if err != nil {
			return err
		}
		overlapping, err := scanAbsences(rows)
		if err != nil {
			return err
		}
		changed = len(overlapping) > 0
		for _, a := range overlapping {
			if a.Start.Before(start) {
				before := domain.Absence{SlackUserID: userID, Start: a.Start, End: start.AddDays(-1), CreatedBy: a.CreatedBy}
				if err := insertAbsence(ctx, tx, workspaceID, &before, now); err != nil {
					return err
				}
			}
			if a.End.After(end) {
				after := domain.Absence{SlackUserID: userID, Start: end.AddDays(1), End: a.End, CreatedBy: a.CreatedBy}
				if err := insertAbsence(ctx, tx, workspaceID, &after, now); err != nil {
					return err
				}
			}
		}
		return nil
	})
	return changed, err
}

// DeleteAbsence deletes one absence. With a non-empty userID only that
// user's absence is deleted, so people can never delete other people's time
// off. It returns the deleted absence or ErrNotFound.
func (s *Store) DeleteAbsence(ctx context.Context, workspaceID, userID string, id int64) (*domain.Absence, error) {
	rows, err := s.db.QueryContext(ctx, `DELETE FROM absences WHERE workspace_id = ? AND id = ? AND (? = '' OR slack_user_id = ?)
		RETURNING `+absenceCols, workspaceID, id, userID, userID)
	if err != nil {
		return nil, err
	}
	deleted, err := scanAbsences(rows)
	if err != nil {
		return nil, err
	}
	if len(deleted) == 0 {
		return nil, ErrNotFound
	}
	return &deleted[0], nil
}

// DeleteAbsencesBefore removes absences that ended before d.
func (s *Store) DeleteAbsencesBefore(ctx context.Context, d domain.Date) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM absences WHERE end_date < ?`, d.String())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// pollAbsences returns the absences of a poll's participants around the
// poll's week. The range is one day wider on both sides because week_start
// and week_end are instants in the poll time zone, which the store does not
// know; callers decide per calendar day with domain.AwayOn.
func pollAbsences(ctx context.Context, q queryer, p *domain.Poll) ([]domain.Absence, error) {
	from := domain.DateOf(p.WeekStart.UTC()).AddDays(-1)
	to := domain.DateOf(p.WeekEnd.UTC()).AddDays(1)
	rows, err := q.QueryContext(ctx, `SELECT `+absenceCols+` FROM absences
		WHERE workspace_id = ? AND end_date >= ? AND start_date <= ?
		  AND slack_user_id IN (SELECT slack_user_id FROM participants WHERE poll_id = ?)
		ORDER BY slack_user_id, start_date`, p.WorkspaceID, from.String(), to.String(), p.ID)
	if err != nil {
		return nil, err
	}
	return scanAbsences(rows)
}
