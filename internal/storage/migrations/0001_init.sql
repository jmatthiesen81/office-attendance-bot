-- All timestamps are stored as Unix seconds (UTC).

CREATE TABLE polls (
    id                     INTEGER PRIMARY KEY,
    workspace_id           TEXT    NOT NULL,
    iso_year               INTEGER NOT NULL,
    iso_week               INTEGER NOT NULL,
    week_start             INTEGER NOT NULL,
    week_end               INTEGER NOT NULL,
    deadline_at            INTEGER NOT NULL,
    status                 TEXT    NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'closed')),
    created_at             INTEGER NOT NULL,
    summary_channel_id     TEXT    NOT NULL,
    summary_message_ts     TEXT    NOT NULL DEFAULT '',
    -- summary_version is bumped on every change that affects the channel
    -- summary; summary_synced_version is the version last written to Slack.
    summary_version        INTEGER NOT NULL DEFAULT 1,
    summary_synced_version INTEGER NOT NULL DEFAULT 0,
    UNIQUE (workspace_id, iso_year, iso_week)
);

CREATE TABLE participants (
    id                    INTEGER PRIMARY KEY,
    poll_id               INTEGER NOT NULL REFERENCES polls (id) ON DELETE CASCADE,
    slack_user_id         TEXT    NOT NULL,
    display_name          TEXT    NOT NULL DEFAULT '',
    dm_channel_id         TEXT    NOT NULL DEFAULT '',
    dm_message_ts         TEXT    NOT NULL DEFAULT '',
    delivery_status       TEXT    NOT NULL DEFAULT 'pending',
    last_delivery_attempt INTEGER NOT NULL DEFAULT 0,
    reminder_count        INTEGER NOT NULL DEFAULT 0,
    UNIQUE (poll_id, slack_user_id)
);

CREATE TABLE responses (
    poll_id       INTEGER NOT NULL REFERENCES polls (id) ON DELETE CASCADE,
    slack_user_id TEXT    NOT NULL,
    monday        INTEGER NOT NULL DEFAULT 0,
    tuesday       INTEGER NOT NULL DEFAULT 0,
    wednesday     INTEGER NOT NULL DEFAULT 0,
    thursday      INTEGER NOT NULL DEFAULT 0,
    friday        INTEGER NOT NULL DEFAULT 0,
    submitted_at  INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL,
    revision      INTEGER NOT NULL DEFAULT 1,
    PRIMARY KEY (poll_id, slack_user_id),
    FOREIGN KEY (poll_id, slack_user_id) REFERENCES participants (poll_id, slack_user_id) ON DELETE CASCADE
);

CREATE TABLE outbox_jobs (
    id              INTEGER PRIMARY KEY,
    job_type        TEXT    NOT NULL,
    poll_id         INTEGER NOT NULL REFERENCES polls (id) ON DELETE CASCADE,
    participant_id  INTEGER NOT NULL REFERENCES participants (id) ON DELETE CASCADE,
    -- dedupe_key distinguishes intentional repetitions (e.g. reminder rounds).
    dedupe_key      TEXT    NOT NULL DEFAULT '',
    status          TEXT    NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'running', 'done', 'failed')),
    attempts        INTEGER NOT NULL DEFAULT 0,
    next_attempt_at INTEGER NOT NULL,
    last_error      TEXT    NOT NULL DEFAULT '',
    created_at      INTEGER NOT NULL,
    UNIQUE (job_type, participant_id, dedupe_key)
);

CREATE INDEX outbox_jobs_due ON outbox_jobs (status, next_attempt_at);

CREATE TABLE reminder_rounds (
    poll_id      INTEGER NOT NULL REFERENCES polls (id) ON DELETE CASCADE,
    scheduled_at INTEGER NOT NULL,
    PRIMARY KEY (poll_id, scheduled_at)
);
