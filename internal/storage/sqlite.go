// Package storage persists polls, participants, responses and outbox jobs in SQLite.
package storage

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver

	"officebot/internal/domain"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// ErrNotFound is returned when a requested row does not exist.
var ErrNotFound = errors.New("not found")

// Store is the SQLite-backed repository.
type Store struct {
	db *sql.DB
}

// Open opens (and creates) the database and applies pending migrations.
func Open(ctx context.Context, path string) (*Store, error) {
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, fmt.Errorf("create database directory: %w", err)
		}
	}
	q := url.Values{}
	for _, p := range []string{"journal_mode(WAL)", "busy_timeout(5000)", "foreign_keys(1)", "synchronous(NORMAL)"} {
		q.Add("_pragma", p)
	}
	q.Set("_txlock", "immediate")
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	// A single connection serialises all access within this process and
	// avoids SQLITE_BUSY between our own goroutines. busy_timeout covers
	// concurrent access from the CLI.
	db.SetMaxOpenConns(1)
	db.SetConnMaxIdleTime(0)
	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	if path != ":memory:" {
		_ = os.Chmod(path, 0o600)
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Backup writes a consistent copy of the database to path (overwriting it).
// It is safe while the bot is running.
func (s *Store) Backup(ctx context.Context, path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	_, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, path)
	return err
}

// Ping checks database availability.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	files, err := fs.Glob(migrationFS, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(files)
	for _, f := range files {
		version := strings.TrimSuffix(filepath.Base(f), ".sql")
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, version).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			continue
		}
		body, err := migrationFS.ReadFile(f)
		if err != nil {
			return err
		}
		err = s.tx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, string(body)); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`, version, time.Now().Unix())
			return err
		})
		if err != nil {
			return fmt.Errorf("apply migration %s: %w", version, err)
		}
	}
	return nil
}

func (s *Store) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func unix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func fromUnix(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.Unix(v, 0).UTC()
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ---------------------------------------------------------------- polls

const pollCols = `id, workspace_id, iso_year, iso_week, week_start, week_end, deadline_at, status, created_at,
	summary_channel_id, summary_message_ts, summary_version, summary_synced_version`

type scanner interface{ Scan(dest ...any) error }

func scanPoll(r scanner) (*domain.Poll, error) {
	var p domain.Poll
	var ws, we, dl, ca int64
	err := r.Scan(&p.ID, &p.WorkspaceID, &p.ISOYear, &p.ISOWeek, &ws, &we, &dl, &p.Status, &ca,
		&p.SummaryChannelID, &p.SummaryMessageTS, &p.SummaryVersion, &p.SummarySyncedVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	p.WeekStart, p.WeekEnd, p.Deadline, p.CreatedAt = fromUnix(ws), fromUnix(we), fromUnix(dl), fromUnix(ca)
	return &p, nil
}

func (s *Store) queryPolls(ctx context.Context, q queryer, query string, args ...any) ([]domain.Poll, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Poll
	for rows.Next() {
		p, err := scanPoll(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// GetPoll returns a poll by ID.
func (s *Store) GetPoll(ctx context.Context, id int64) (*domain.Poll, error) {
	return scanPoll(s.db.QueryRowContext(ctx, `SELECT `+pollCols+` FROM polls WHERE id = ?`, id))
}

// GetPollByWeek returns the poll of a workspace and ISO week.
func (s *Store) GetPollByWeek(ctx context.Context, workspaceID string, year, week int) (*domain.Poll, error) {
	return scanPoll(s.db.QueryRowContext(ctx, `SELECT `+pollCols+` FROM polls WHERE workspace_id = ? AND iso_year = ? AND iso_week = ?`,
		workspaceID, year, week))
}

// OpenPolls returns all polls with status open.
func (s *Store) OpenPolls(ctx context.Context) ([]domain.Poll, error) {
	return s.queryPolls(ctx, s.db, `SELECT `+pollCols+` FROM polls WHERE status = 'open' ORDER BY week_start`)
}

// PollsNeedingSummarySync returns polls whose channel summary is missing or outdated.
func (s *Store) PollsNeedingSummarySync(ctx context.Context) ([]domain.Poll, error) {
	return s.queryPolls(ctx, s.db, `SELECT `+pollCols+` FROM polls
		WHERE summary_message_ts = '' OR summary_version > summary_synced_version ORDER BY week_start`)
}

// CreatePoll creates a poll with its participant snapshot and enqueues one
// send_dm job per participant, all in one transaction. If a poll for the
// same workspace and week already exists nothing is changed and created is false.
// Older polls that are still open are closed.
func (s *Store) CreatePoll(ctx context.Context, p *domain.Poll, participants []domain.Participant, now time.Time) (created bool, err error) {
	err = s.tx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `INSERT INTO polls (workspace_id, iso_year, iso_week, week_start, week_end, deadline_at, status, created_at, summary_channel_id)
			VALUES (?, ?, ?, ?, ?, ?, 'open', ?, ?)
			ON CONFLICT (workspace_id, iso_year, iso_week) DO NOTHING RETURNING id`,
			p.WorkspaceID, p.ISOYear, p.ISOWeek, unix(p.WeekStart), unix(p.WeekEnd), unix(p.Deadline), unix(now), p.SummaryChannelID).Scan(&p.ID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil // already exists
		}
		if err != nil {
			return err
		}
		created = true
		p.Status, p.CreatedAt, p.SummaryVersion = domain.PollOpen, now.UTC().Truncate(time.Second), 1
		for _, part := range participants {
			var pid int64
			if err := tx.QueryRowContext(ctx, `INSERT INTO participants (poll_id, slack_user_id, display_name, slack_locale) VALUES (?, ?, ?, ?) RETURNING id`,
				p.ID, part.SlackUserID, part.DisplayName, part.SlackLocale).Scan(&pid); err != nil {
				return err
			}
			if err := enqueue(ctx, tx, JobSendDM, p.ID, pid, "", now); err != nil {
				return err
			}
		}
		_, err = closePolls(ctx, tx, `workspace_id = ? AND week_start < ?`, now, p.WorkspaceID, unix(p.WeekStart))
		return err
	})
	return created, err
}

// CloseDuePolls closes all open polls whose deadline has passed and
// enqueues DM updates so the closed state becomes visible.
func (s *Store) CloseDuePolls(ctx context.Context, now time.Time) (closed []int64, err error) {
	err = s.tx(ctx, func(tx *sql.Tx) error {
		closed, err = closePolls(ctx, tx, `deadline_at <= ?`, now, unix(now))
		return err
	})
	return closed, err
}

func closePolls(ctx context.Context, tx *sql.Tx, where string, now time.Time, args ...any) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, `UPDATE polls SET status = 'closed', summary_version = summary_version + 1
		WHERE status = 'open' AND `+where+` RETURNING id`, args...)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, id := range ids {
		if err := enqueueForPoll(ctx, tx, JobUpdateDM, id, `dm_message_ts <> ''`, "", now); err != nil {
			return nil, err
		}
		// Undelivered DMs are pointless now.
		if _, err := tx.ExecContext(ctx, `UPDATE outbox_jobs SET status = 'done', last_error = 'poll closed'
			WHERE poll_id = ? AND job_type IN (?, ?) AND status = 'pending'`, id, JobSendDM, JobReminderDM); err != nil {
			return nil, err
		}
	}
	return ids, nil
}

// SetSummaryMessage stores the timestamp of the channel summary message.
func (s *Store) SetSummaryMessage(ctx context.Context, pollID int64, ts string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE polls SET summary_message_ts = ? WHERE id = ?`, ts, pollID)
	return err
}

// MarkSummarySynced records that the summary reflects at least version.
// Using MAX guarantees an older sync can never lower the synced version.
func (s *Store) MarkSummarySynced(ctx context.Context, pollID, version int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE polls SET summary_synced_version = MAX(summary_synced_version, ?) WHERE id = ?`, version, pollID)
	return err
}

// RequestSummaryResync marks the summaries of the given polls (or all open
// polls if none are given) as outdated.
func (s *Store) RequestSummaryResync(ctx context.Context, pollIDs ...int64) (int64, error) {
	var res sql.Result
	var err error
	if len(pollIDs) == 0 {
		res, err = s.db.ExecContext(ctx, `UPDATE polls SET summary_version = summary_version + 1 WHERE status = 'open'`)
	} else {
		for _, id := range pollIDs {
			if res, err = s.db.ExecContext(ctx, `UPDATE polls SET summary_version = summary_version + 1 WHERE id = ?`, id); err != nil {
				break
			}
		}
	}
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// RequestDMRefresh enqueues an update of every delivered DM of a poll, so
// that they are re-rendered from the current state.
func (s *Store) RequestDMRefresh(ctx context.Context, pollID int64, now time.Time) error {
	return enqueueForPoll(ctx, s.db, JobUpdateDM, pollID, `dm_message_ts <> ''`, "", now)
}

// DeletePollsBefore removes polls (and all dependent rows) whose week ended before t.
func (s *Store) DeletePollsBefore(ctx context.Context, t time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM polls WHERE week_end < ?`, unix(t))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DeletePoll removes one poll with all dependent rows.
func (s *Store) DeletePoll(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM polls WHERE id = ?`, id)
	return err
}

// --------------------------------------------------------- participants

const participantCols = `id, poll_id, slack_user_id, display_name, dm_channel_id, dm_message_ts,
	delivery_status, last_delivery_attempt, reminder_count, dm_generation, slack_locale`

func scanParticipant(r scanner) (*domain.Participant, error) {
	var p domain.Participant
	var last int64
	err := r.Scan(&p.ID, &p.PollID, &p.SlackUserID, &p.DisplayName, &p.DMChannelID, &p.DMMessageTS,
		&p.DeliveryStatus, &last, &p.ReminderCount, &p.DMGeneration, &p.SlackLocale)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	p.LastDeliveryAttempt = fromUnix(last)
	return &p, nil
}

// GetParticipant returns the participant of a poll by Slack user ID.
func (s *Store) GetParticipant(ctx context.Context, pollID int64, userID string) (*domain.Participant, error) {
	return scanParticipant(s.db.QueryRowContext(ctx, `SELECT `+participantCols+` FROM participants WHERE poll_id = ? AND slack_user_id = ?`, pollID, userID))
}

// GetParticipantByID returns a participant by row ID.
func (s *Store) GetParticipantByID(ctx context.Context, id int64) (*domain.Participant, error) {
	return scanParticipant(s.db.QueryRowContext(ctx, `SELECT `+participantCols+` FROM participants WHERE id = ?`, id))
}

// ListParticipants returns the participant snapshot of a poll.
func (s *Store) ListParticipants(ctx context.Context, pollID int64) ([]domain.Participant, error) {
	return listParticipants(ctx, s.db, pollID)
}

func listParticipants(ctx context.Context, q queryer, pollID int64) ([]domain.Participant, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+participantCols+` FROM participants WHERE poll_id = ? ORDER BY id`, pollID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Participant
	for rows.Next() {
		p, err := scanParticipant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// SetDMChannel stores the DM channel of a participant.
func (s *Store) SetDMChannel(ctx context.Context, participantID int64, channelID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE participants SET dm_channel_id = ? WHERE id = ?`, channelID, participantID)
	return err
}

// MarkDelivered stores the DM message reference and marks the DM as sent.
func (s *Store) MarkDelivered(ctx context.Context, participantID int64, channelID, ts string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE participants SET dm_channel_id = ?, dm_message_ts = ?, delivery_status = 'sent', last_delivery_attempt = ?
		WHERE id = ?`, channelID, ts, unix(now), participantID)
	return err
}

// SetDeliveryStatus updates the delivery status of a participant.
func (s *Store) SetDeliveryStatus(ctx context.Context, participantID int64, status string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE participants SET delivery_status = ?, last_delivery_attempt = ? WHERE id = ?`, status, unix(now), participantID)
	return err
}

// RequestNewDM forgets the current DM of a participant and enqueues a new
// one. The generation counter makes the new DM distinguishable from the old
// one (metadata key and job dedupe key).
func (s *Store) RequestNewDM(ctx context.Context, participantID int64, now time.Time) (generation int, err error) {
	err = s.tx(ctx, func(tx *sql.Tx) error {
		var pollID int64
		if err := tx.QueryRowContext(ctx, `UPDATE participants
			SET dm_generation = dm_generation + 1, dm_message_ts = '', delivery_status = 'pending', last_delivery_attempt = ?
			WHERE id = ? RETURNING dm_generation, poll_id`, unix(now), participantID).Scan(&generation, &pollID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		return enqueue(ctx, tx, JobSendDM, pollID, participantID, fmt.Sprintf("g%d", generation), now)
	})
	return generation, err
}

// AddParticipant adds a late joiner to a running poll's snapshot and
// enqueues their DM. If the user is already a participant, created is false.
func (s *Store) AddParticipant(ctx context.Context, pollID int64, p domain.Participant, now time.Time) (created bool, err error) {
	err = s.tx(ctx, func(tx *sql.Tx) error {
		var id int64
		err := tx.QueryRowContext(ctx, `INSERT INTO participants (poll_id, slack_user_id, display_name, slack_locale, last_delivery_attempt) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (poll_id, slack_user_id) DO NOTHING RETURNING id`, pollID, p.SlackUserID, p.DisplayName, p.SlackLocale, unix(now)).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		created = true
		// A non-empty key marks the DM as requested: it is sent even if the
		// person is away (see JobKeyJoin).
		if err := enqueue(ctx, tx, JobSendDM, pollID, id, JobKeyJoin, now); err != nil {
			return err
		}
		// The participant count in the summary changes.
		_, err = tx.ExecContext(ctx, `UPDATE polls SET summary_version = summary_version + 1 WHERE id = ?`, pollID)
		return err
	})
	return created, err
}

// IncrementReminderCount records a sent reminder.
func (s *Store) IncrementReminderCount(ctx context.Context, participantID int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE participants SET reminder_count = reminder_count + 1 WHERE id = ?`, participantID)
	return err
}

// ------------------------------------------------------------ responses

// SaveResponse atomically inserts or updates a participant's response. The
// revision only increases (and the summary is only marked outdated) when the
// selection actually changes, so duplicate interactions are harmless.
func (s *Store) SaveResponse(ctx context.Context, pollID int64, userID string, days domain.Days, now time.Time) (resp *domain.Response, changed bool, err error) {
	return s.SaveResponseWithNotice(ctx, pollID, userID, days, now, time.Time{}, 0)
}

// SaveResponseWithNotice is SaveResponse that also schedules a change
// notice at noticeAt when an existing response changes. A zero noticeAt
// schedules none. A first answer never gets one, and neither do changes
// within grace after it: they count as corrections of the first answer.
func (s *Store) SaveResponseWithNotice(ctx context.Context, pollID int64, userID string, days domain.Days, now, noticeAt time.Time, grace time.Duration) (resp *domain.Response, changed bool, err error) {
	err = s.tx(ctx, func(tx *sql.Tx) error {
		prev, err := getResponse(ctx, tx, pollID, userID)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO responses (poll_id, slack_user_id, monday, tuesday, wednesday, thursday, friday, submitted_at, updated_at, revision)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1)
			ON CONFLICT (poll_id, slack_user_id) DO UPDATE SET
				monday = excluded.monday, tuesday = excluded.tuesday, wednesday = excluded.wednesday,
				thursday = excluded.thursday, friday = excluded.friday,
				updated_at = excluded.updated_at, revision = revision + 1
			WHERE (monday, tuesday, wednesday, thursday, friday) IS NOT
			      (excluded.monday, excluded.tuesday, excluded.wednesday, excluded.thursday, excluded.friday)`,
			pollID, userID, b2i(days[0]), b2i(days[1]), b2i(days[2]), b2i(days[3]), b2i(days[4]), unix(now), unix(now))
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		changed = n > 0
		if changed {
			if _, err := tx.ExecContext(ctx, `UPDATE polls SET summary_version = summary_version + 1 WHERE id = ?`, pollID); err != nil {
				return err
			}
			if prev != nil && !noticeAt.IsZero() && !now.Before(prev.SubmittedAt.Add(grace)) {
				if err := queueChangeNotice(ctx, tx, pollID, userID, prev, noticeAt, now); err != nil {
					return err
				}
			}
		}
		resp, err = getResponse(ctx, tx, pollID, userID)
		return err
	})
	return resp, changed, err
}

// GetResponse returns the response of a user or nil if there is none.
func (s *Store) GetResponse(ctx context.Context, pollID int64, userID string) (*domain.Response, error) {
	r, err := getResponse(ctx, s.db, pollID, userID)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	return r, err
}

const responseCols = `poll_id, slack_user_id, monday, tuesday, wednesday, thursday, friday, submitted_at, updated_at, revision`

func scanResponse(r scanner) (*domain.Response, error) {
	var resp domain.Response
	var d [domain.NumDays]int
	var sub, upd int64
	err := r.Scan(&resp.PollID, &resp.SlackUserID, &d[0], &d[1], &d[2], &d[3], &d[4], &sub, &upd, &resp.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	for i := range d {
		resp.Days[i] = d[i] != 0
	}
	resp.SubmittedAt, resp.UpdatedAt = fromUnix(sub), fromUnix(upd)
	return &resp, nil
}

func getResponse(ctx context.Context, q queryer, pollID int64, userID string) (*domain.Response, error) {
	return scanResponse(q.QueryRowContext(ctx, `SELECT `+responseCols+` FROM responses WHERE poll_id = ? AND slack_user_id = ?`, pollID, userID))
}

// Snapshot is a consistent view of a poll for rendering the summary.
type Snapshot struct {
	Poll         domain.Poll
	Participants []domain.Participant
	Responses    map[string]domain.Response
	// Absences of the participants around the poll's week.
	Absences []domain.Absence
}

// PollSnapshot reads poll, participants and responses in one transaction.
func (s *Store) PollSnapshot(ctx context.Context, pollID int64) (*Snapshot, error) {
	var snap Snapshot
	err := s.tx(ctx, func(tx *sql.Tx) error {
		p, err := scanPoll(tx.QueryRowContext(ctx, `SELECT `+pollCols+` FROM polls WHERE id = ?`, pollID))
		if err != nil {
			return err
		}
		snap.Poll = *p
		if snap.Participants, err = listParticipants(ctx, tx, pollID); err != nil {
			return err
		}
		if snap.Absences, err = pollAbsences(ctx, tx, p); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT `+responseCols+` FROM responses WHERE poll_id = ?`, pollID)
		if err != nil {
			return err
		}
		defer rows.Close()
		snap.Responses = map[string]domain.Response{}
		for rows.Next() {
			r, err := scanResponse(rows)
			if err != nil {
				return err
			}
			snap.Responses[r.SlackUserID] = *r
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return &snap, nil
}

// ------------------------------------------------------------ reminders

// CreateReminderRound records the given (possibly missed) reminder rounds and,
// if at least one of them is new, enqueues one reminder per participant who
// has not answered yet, received the poll DM and is below maxCount.
// Missed rounds are thus collapsed into a single reminder.
func (s *Store) CreateReminderRound(ctx context.Context, pollID int64, rounds []time.Time, maxCount int, now time.Time) (enqueued int, err error) {
	err = s.tx(ctx, func(tx *sql.Tx) error {
		inserted := false
		var key string
		for _, r := range rounds {
			res, err := tx.ExecContext(ctx, `INSERT INTO reminder_rounds (poll_id, scheduled_at) VALUES (?, ?) ON CONFLICT DO NOTHING`, pollID, unix(r))
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n > 0 {
				inserted = true
				key = fmt.Sprint(unix(r))
			}
		}
		if !inserted {
			return nil
		}
		var before int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM outbox_jobs WHERE poll_id = ? AND job_type = ?`, pollID, JobReminderDM).Scan(&before); err != nil {
			return err
		}
		where := fmt.Sprintf(`delivery_status = 'sent' AND reminder_count < %d AND NOT EXISTS
			(SELECT 1 FROM responses r WHERE r.poll_id = participants.poll_id AND r.slack_user_id = participants.slack_user_id)`, maxCount)
		if err := enqueueForPoll(ctx, tx, JobReminderDM, pollID, where, key, now); err != nil {
			return err
		}
		var after int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM outbox_jobs WHERE poll_id = ? AND job_type = ?`, pollID, JobReminderDM).Scan(&after); err != nil {
			return err
		}
		enqueued = after - before
		return nil
	})
	return enqueued, err
}
