-- Small key/value state of the bot itself, e.g. the version whose rendering
-- the Slack messages show (refresh after an update).
CREATE TABLE app_state (
    key        TEXT    PRIMARY KEY,
    value      TEXT    NOT NULL,
    updated_at INTEGER NOT NULL
);
