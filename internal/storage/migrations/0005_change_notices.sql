-- Pending change notice of a person: a thread reply under the channel summary
-- that announces a changed response. The row holds the selection before the
-- first change not announced yet (the baseline). Further changes until the
-- notice is posted only update the response, so the notice reports the net
-- difference between baseline and current response once.
CREATE TABLE change_notices (
    poll_id       INTEGER NOT NULL REFERENCES polls (id) ON DELETE CASCADE,
    slack_user_id TEXT    NOT NULL,
    monday        INTEGER NOT NULL,
    tuesday       INTEGER NOT NULL,
    wednesday     INTEGER NOT NULL,
    thursday      INTEGER NOT NULL,
    friday        INTEGER NOT NULL,
    created_at    INTEGER NOT NULL,
    PRIMARY KEY (poll_id, slack_user_id),
    FOREIGN KEY (poll_id, slack_user_id) REFERENCES participants (poll_id, slack_user_id) ON DELETE CASCADE
);
