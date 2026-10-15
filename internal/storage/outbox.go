package storage

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Outbox job types.
const (
	JobSendDM     = "send_dm"
	JobUpdateDM   = "update_dm"
	JobReminderDM = "reminder_dm"
	// JobChangeNotice announces a changed response in the summary thread.
	// Its dedupe key is the revision of the announced change's baseline.
	JobChangeNotice = "change_notice"
)

// JobKeyJoin is the dedupe key of the send_dm job of a late joiner.
// Automatic weekly DMs have an empty key; requested DMs (late joiners and
// re-requests, "g<generation>") have a non-empty one.
const JobKeyJoin = "join"

// Job is a pending unit of Slack work.
type Job struct {
	ID            int64
	Type          string
	PollID        int64
	ParticipantID int64
	DedupeKey     string
	// Attempts includes the current attempt (1 on the first run).
	Attempts int
}

// repeatable job types may be re-enqueued after completion; others run once.
func repeatable(jobType string) bool { return jobType == JobUpdateDM }

func enqueue(ctx context.Context, q queryer, jobType string, pollID, participantID int64, key string, now time.Time) error {
	_, err := q.ExecContext(ctx, enqueueSQL+` VALUES (?, ?, ?, ?, ?, ?)`+onConflict(jobType),
		jobType, pollID, participantID, key, unix(now), unix(now))
	return err
}

func enqueueForPoll(ctx context.Context, q queryer, jobType string, pollID int64, where, key string, now time.Time) error {
	_, err := q.ExecContext(ctx, enqueueSQL+` SELECT ?, poll_id, id, ?, ?, ? FROM participants WHERE poll_id = ? AND `+where+onConflict(jobType),
		jobType, key, unix(now), unix(now), pollID)
	return err
}

const enqueueSQL = `INSERT INTO outbox_jobs (job_type, poll_id, participant_id, dedupe_key, next_attempt_at, created_at)`

// onConflict makes enqueueing idempotent. Repeatable jobs that already ran
// (or are currently running) are reset to pending so the latest state is
// applied once more; a running job whose status changed is not marked done.
func onConflict(jobType string) string {
	if repeatable(jobType) {
		return ` ON CONFLICT (job_type, participant_id, dedupe_key) DO UPDATE SET
			status = 'pending', attempts = 0, last_error = '', next_attempt_at = excluded.next_attempt_at
			WHERE status <> 'pending'`
	}
	return ` ON CONFLICT (job_type, participant_id, dedupe_key) DO NOTHING`
}

// EnqueueJob adds a job (idempotently, see onConflict).
func (s *Store) EnqueueJob(ctx context.Context, jobType string, pollID, participantID int64, key string, now time.Time) error {
	return enqueue(ctx, s.db, jobType, pollID, participantID, key, now)
}

// ClaimDueJob marks the oldest due pending job as running and returns it.
// It returns nil when no job is due.
func (s *Store) ClaimDueJob(ctx context.Context, now time.Time) (*Job, error) {
	var j Job
	err := s.db.QueryRowContext(ctx, `UPDATE outbox_jobs SET status = 'running', attempts = attempts + 1
		WHERE id = (SELECT id FROM outbox_jobs WHERE status = 'pending' AND next_attempt_at <= ? ORDER BY next_attempt_at, id LIMIT 1)
		RETURNING id, job_type, poll_id, participant_id, dedupe_key, attempts`, unix(now)).
		Scan(&j.ID, &j.Type, &j.PollID, &j.ParticipantID, &j.DedupeKey, &j.Attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &j, nil
}

// NextJobDue returns the time of the next pending job.
func (s *Store) NextJobDue(ctx context.Context) (time.Time, bool, error) {
	var v sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MIN(next_attempt_at) FROM outbox_jobs WHERE status = 'pending'`).Scan(&v); err != nil {
		return time.Time{}, false, err
	}
	if !v.Valid {
		return time.Time{}, false, nil
	}
	return fromUnix(v.Int64), true, nil
}

// CompleteJob marks a running job as done. If the job was re-enqueued while
// running it stays pending and runs again.
func (s *Store) CompleteJob(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE outbox_jobs SET status = 'done', last_error = '' WHERE id = ? AND status = 'running'`, id)
	return err
}

// RetryJob schedules a running job for another attempt.
func (s *Store) RetryJob(ctx context.Context, id int64, next time.Time, lastErr string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE outbox_jobs SET status = 'pending', next_attempt_at = ?, last_error = ? WHERE id = ? AND status = 'running'`,
		unix(next), truncate(lastErr), id)
	return err
}

// FailJob marks a running job as permanently failed.
func (s *Store) FailJob(ctx context.Context, id int64, lastErr string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE outbox_jobs SET status = 'failed', last_error = ? WHERE id = ? AND status = 'running'`, truncate(lastErr), id)
	return err
}

// ResetRunningJobs returns jobs left running by a crash to pending.
func (s *Store) ResetRunningJobs(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE outbox_jobs SET status = 'pending' WHERE status = 'running'`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// JobStats counts jobs per status.
func (s *Store) JobStats(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT status, COUNT(*) FROM outbox_jobs GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		out[st] = n
	}
	return out, rows.Err()
}

func truncate(s string) string {
	if len(s) > 500 {
		return s[:500]
	}
	return s
}
