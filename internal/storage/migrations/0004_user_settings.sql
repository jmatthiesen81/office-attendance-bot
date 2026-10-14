-- Personal settings of a person (ADR 0002). language is a catalog code such
-- as 'de' or 'pirate_en'; it applies to everything only this person sees.
CREATE TABLE user_settings (
    workspace_id  TEXT    NOT NULL,
    slack_user_id TEXT    NOT NULL,
    language      TEXT    NOT NULL DEFAULT '',
    updated_at    INTEGER NOT NULL,
    PRIMARY KEY (workspace_id, slack_user_id)
);

-- Slack language of the person (users.info locale, e.g. 'de-DE') when the
-- participants were resolved; used with BOT_LANGUAGE_FROM_SLACK=true.
ALTER TABLE participants ADD COLUMN slack_locale TEXT NOT NULL DEFAULT '';
