-- Time off of a person: whole calendar days from start_date to end_date, both
-- included, as YYYY-MM-DD in the poll time zone (text compares like dates).
-- People who are away for a whole polled week get no poll DM and no reminders.
CREATE TABLE absences (
    id            INTEGER PRIMARY KEY,
    workspace_id  TEXT    NOT NULL,
    slack_user_id TEXT    NOT NULL,
    start_date    TEXT    NOT NULL,
    end_date      TEXT    NOT NULL,
    -- Slack user ID of the person who entered it, or 'cli'.
    created_by    TEXT    NOT NULL DEFAULT '',
    created_at    INTEGER NOT NULL,
    CHECK (start_date <= end_date)
);

CREATE INDEX absences_user ON absences (workspace_id, slack_user_id, end_date);
