-- Web interface: third role "analyst" (statistics per person, no changes).
-- SQLite cannot change a CHECK constraint, so web_users is rebuilt.
CREATE TABLE web_users_new (
    workspace_id  TEXT    NOT NULL,
    slack_user_id TEXT    NOT NULL,
    role          TEXT    NOT NULL CHECK (role IN ('admin', 'analyst', 'viewer')),
    display_name  TEXT    NOT NULL DEFAULT '',
    added_by      TEXT    NOT NULL DEFAULT '',
    added_at      INTEGER NOT NULL,
    last_login_at INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (workspace_id, slack_user_id)
);

INSERT INTO web_users_new (workspace_id, slack_user_id, role, display_name, added_by, added_at, last_login_at)
SELECT workspace_id, slack_user_id, role, display_name, added_by, added_at, last_login_at FROM web_users;

DROP TABLE web_users;
ALTER TABLE web_users_new RENAME TO web_users;
