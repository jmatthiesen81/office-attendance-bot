-- Web interface (ADR 0003): who may log in, sessions, login states, audit log.
CREATE TABLE web_users (
    workspace_id  TEXT    NOT NULL,
    slack_user_id TEXT    NOT NULL,
    role          TEXT    NOT NULL CHECK (role IN ('admin', 'viewer')),
    display_name  TEXT    NOT NULL DEFAULT '',
    added_by      TEXT    NOT NULL DEFAULT '',
    added_at      INTEGER NOT NULL,
    last_login_at INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (workspace_id, slack_user_id)
);

CREATE TABLE web_sessions (
    id_hash       TEXT    PRIMARY KEY,
    workspace_id  TEXT    NOT NULL,
    slack_user_id TEXT    NOT NULL,
    csrf_token    TEXT    NOT NULL,
    created_at    INTEGER NOT NULL,
    last_seen_at  INTEGER NOT NULL,
    expires_at    INTEGER NOT NULL
);

CREATE INDEX web_sessions_user ON web_sessions (workspace_id, slack_user_id);

CREATE TABLE web_login_states (
    state_hash TEXT    PRIMARY KEY,
    nonce      TEXT    NOT NULL,
    return_to  TEXT    NOT NULL DEFAULT '',
    expires_at INTEGER NOT NULL
);

CREATE TABLE audit_log (
    id           INTEGER PRIMARY KEY,
    workspace_id TEXT    NOT NULL,
    at           INTEGER NOT NULL,
    actor        TEXT    NOT NULL,
    action       TEXT    NOT NULL,
    details      TEXT    NOT NULL DEFAULT '{}'
);

CREATE INDEX audit_log_at ON audit_log (workspace_id, at);

-- Statistics (ADR 0003): daily status per person, derived from closed polls.
-- planned_status is the status of the first calculation, i.e. the plan at
-- the deadline; a different status means an admin corrected it.
CREATE TABLE attendance_days (
    workspace_id   TEXT    NOT NULL,
    slack_user_id  TEXT    NOT NULL,
    day            TEXT    NOT NULL,
    status         TEXT    NOT NULL CHECK (status IN ('office', 'remote', 'away', 'no_answer')),
    planned_status TEXT    NOT NULL CHECK (planned_status IN ('office', 'remote', 'away', 'no_answer')),
    iso_week       TEXT    NOT NULL,
    display_name   TEXT    NOT NULL DEFAULT '',
    updated_at     INTEGER NOT NULL,
    PRIMARY KEY (workspace_id, slack_user_id, day)
);

CREATE INDEX attendance_days_day ON attendance_days (workspace_id, day);
CREATE INDEX attendance_days_week ON attendance_days (workspace_id, iso_week);

CREATE TABLE verified_days (
    workspace_id TEXT    NOT NULL,
    day          TEXT    NOT NULL,
    verified_by  TEXT    NOT NULL,
    verified_at  INTEGER NOT NULL,
    PRIMARY KEY (workspace_id, day)
);

CREATE TABLE office_closed_days (
    workspace_id TEXT    NOT NULL,
    day          TEXT    NOT NULL,
    reason       TEXT    NOT NULL DEFAULT '',
    created_by   TEXT    NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL,
    PRIMARY KEY (workspace_id, day)
);

-- Monthly figures without personal data, kept indefinitely. frozen months
-- are no longer recalculated, because daily data of them was deleted.
CREATE TABLE monthly_stats (
    workspace_id TEXT    NOT NULL,
    month        TEXT    NOT NULL,
    data         TEXT    NOT NULL,
    frozen       INTEGER NOT NULL DEFAULT 0,
    computed_at  INTEGER NOT NULL,
    PRIMARY KEY (workspace_id, month)
);

-- stats_dirty counts changes since attendance_days was last calculated for
-- a poll. Every change that bumps the summary version (responses, closing,
-- participants) counts, as do time-off changes of participants.
ALTER TABLE polls ADD COLUMN stats_dirty INTEGER NOT NULL DEFAULT 0;

UPDATE polls SET stats_dirty = 1 WHERE status = 'closed';

CREATE TRIGGER polls_stats_dirty AFTER UPDATE OF summary_version ON polls
WHEN NEW.summary_version <> OLD.summary_version
BEGIN
    UPDATE polls SET stats_dirty = stats_dirty + 1 WHERE id = NEW.id;
END;

-- Absence dates are calendar days, week_start/week_end instants; a margin of
-- two days on both sides covers every time zone.
CREATE TRIGGER absences_stats_dirty_insert AFTER INSERT ON absences
BEGIN
    UPDATE polls SET stats_dirty = stats_dirty + 1
    WHERE workspace_id = NEW.workspace_id
      AND week_start <= CAST(strftime('%s', NEW.end_date) AS INTEGER) + 2 * 86400
      AND week_end >= CAST(strftime('%s', NEW.start_date) AS INTEGER) - 2 * 86400
      AND id IN (SELECT poll_id FROM participants WHERE slack_user_id = NEW.slack_user_id);
END;

CREATE TRIGGER absences_stats_dirty_delete AFTER DELETE ON absences
BEGIN
    UPDATE polls SET stats_dirty = stats_dirty + 1
    WHERE workspace_id = OLD.workspace_id
      AND week_start <= CAST(strftime('%s', OLD.end_date) AS INTEGER) + 2 * 86400
      AND week_end >= CAST(strftime('%s', OLD.start_date) AS INTEGER) - 2 * 86400
      AND id IN (SELECT poll_id FROM participants WHERE slack_user_id = OLD.slack_user_id);
END;
