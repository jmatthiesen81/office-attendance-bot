package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"officebot/internal/domain"
)

// queueChangeNotice records baseline as the selection before an unannounced
// change and schedules its change_notice job at due. If a notice is already
// pending, its older baseline is kept and nothing else happens. The job's
// dedupe key is the revision of the baseline, which only ever grows.
func queueChangeNotice(ctx context.Context, tx *sql.Tx, pollID int64, userID string, baseline *domain.Response, due, now time.Time) error {
	d := baseline.Days
	res, err := tx.ExecContext(ctx, `INSERT INTO change_notices (poll_id, slack_user_id, monday, tuesday, wednesday, thursday, friday, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (poll_id, slack_user_id) DO NOTHING`,
		pollID, userID, b2i(d[0]), b2i(d[1]), b2i(d[2]), b2i(d[3]), b2i(d[4]), unix(now))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil
	}
	return enqueueChangeNotice(ctx, tx, pollID, userID, baseline.Revision, due, now)
}

func enqueueChangeNotice(ctx context.Context, tx *sql.Tx, pollID int64, userID string, revision int, due, now time.Time) error {
	_, err := tx.ExecContext(ctx, enqueueSQL+` SELECT ?, poll_id, id, ?, ?, ? FROM participants WHERE poll_id = ? AND slack_user_id = ?`+onConflict(JobChangeNotice),
		JobChangeNotice, fmt.Sprint(revision), unix(due), unix(now), pollID, userID)
	return err
}

// GetChangeNotice returns the baseline of a pending change notice, or
// found = false if there is none.
func (s *Store) GetChangeNotice(ctx context.Context, pollID int64, userID string) (baseline domain.Days, found bool, err error) {
	var d [domain.NumDays]int
	err = s.db.QueryRowContext(ctx, `SELECT monday, tuesday, wednesday, thursday, friday FROM change_notices WHERE poll_id = ? AND slack_user_id = ?`,
		pollID, userID).Scan(&d[0], &d[1], &d[2], &d[3], &d[4])
	if errors.Is(err, sql.ErrNoRows) {
		return baseline, false, nil
	}
	if err != nil {
		return baseline, false, err
	}
	for i := range d {
		baseline[i] = d[i] != 0
	}
	return baseline, true, nil
}

// FinishChangeNotice completes a pending change notice that announced the
// response at revision. If the response changed again meanwhile, the
// announced state becomes the new baseline and another notice is scheduled
// at due; otherwise the notice is removed.
func (s *Store) FinishChangeNotice(ctx context.Context, pollID int64, userID string, announced *domain.Response, due, now time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		cur, err := getResponse(ctx, tx, pollID, userID)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		if cur == nil || announced == nil || cur.Revision == announced.Revision {
			_, err := tx.ExecContext(ctx, `DELETE FROM change_notices WHERE poll_id = ? AND slack_user_id = ?`, pollID, userID)
			return err
		}
		d := announced.Days
		if _, err := tx.ExecContext(ctx, `UPDATE change_notices SET monday = ?, tuesday = ?, wednesday = ?, thursday = ?, friday = ?, created_at = ?
			WHERE poll_id = ? AND slack_user_id = ?`, b2i(d[0]), b2i(d[1]), b2i(d[2]), b2i(d[3]), b2i(d[4]), unix(now), pollID, userID); err != nil {
			return err
		}
		return enqueueChangeNotice(ctx, tx, pollID, userID, announced.Revision, due, now)
	})
}
