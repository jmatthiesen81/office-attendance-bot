# Vorlagen für `.env` und `.env.local.example`

Vollständiger Soll-Stand der beiden Dateien, zum Übernehmen in die echten Dateien. Er wird bei jeder Änderung an den Umgebungsvariablen aktualisiert. Die Kommentare in den Dateien sind wie im Repository auf Englisch, damit beide Sprachfassungen denselben Dateiinhalt zeigen. Die Bedeutung aller Variablen steht unter [Konfiguration](README.md#konfiguration). 🇬🇧 English: [../en/dotenv.md](../en/dotenv.md)

## `.env`

Gemeinsame, nicht geheime Standardwerte. Diese Datei wird committet, also niemals Tokens eintragen.

```dotenv
# --- Poll schedule -------------------------------------------------------
# When the poll is sent (weekday MONDAY…SUNDAY, time HH:MM, IANA time zone).
# A poll sent on MONDAY asks about the current week, a poll sent on any
# other day about the next week (starting the following Monday).
POLL_WEEKDAY=MONDAY
POLL_TIME=07:00
POLL_TIMEZONE=Europe/Berlin
# Answers are accepted until this moment of the polled week.
POLL_DEADLINE_WEEKDAY=FRIDAY
POLL_DEADLINE_TIME=08:00

# --- Reminders -----------------------------------------------------------
REMINDER_ENABLED=true
# One or more weekdays, comma separated (e.g. TUESDAY,THURSDAY).
# Only reminders between the poll start and the deadline are sent.
REMINDER_WEEKDAY=TUESDAY
REMINDER_TIME=09:00
# Maximum number of reminders per person and week
REMINDER_MAX_COUNT=1

# --- Language ------------------------------------------------------------
# Language of all Slack messages:
# en, de, fr, es, it, pt, nl, da, sv, nb, pl, cs, tr, uk, ja.
# For fun, every language also exists as nerd_<lang> and pirate_<lang>,
# e.g. nerd_de or pirate_en.
# This is the team default: everyone can choose their own language for
# their DMs with the "Language" button; the channel summary stays in this one.
BOT_LANGUAGE=en
# Use a person's Slack language for their DMs if they have not chosen one
# and the bot speaks it.
BOT_LANGUAGE_FROM_SLACK=false

# --- Operation -----------------------------------------------------------
DATABASE_PATH=/data/officebot.db
LOG_LEVEL=info
# Delete polls older than this many weeks
RETENTION_WEEKS=26
# Maximum names shown per list in the summary before "… and N more"
SUMMARY_MAX_NAMES=40
# Show a small link to the project on GitHub below the summary footer
SUMMARY_PROJECT_LINK=true
# Reply in the summary thread when someone changes a saved answer
SUMMARY_CHANGE_NOTICES=true
# Changes of one person within this time after the first one become one reply
SUMMARY_CHANGE_NOTICE_DELAY=2m
# Corrections within this time after a person's first answer are not announced
SUMMARY_CHANGE_NOTICE_GRACE=10m
# Re-render the delivered DMs and the summary at the first start of a new
# version, so that an update shows at once (by hand: officebot refresh)
REFRESH_MESSAGES_ON_UPDATE=false
DRY_RUN=false

# --- Web interface and statistics (optional, see web-interface.md) ------
# Address of the web server in the container; empty = no web interface.
WEB_LISTEN_ADDR=
# Public address (scheme and host), also the base of the Slack redirect URL.
WEB_BASE_URL=
# Maximum lifetime of a login and logout after inactivity
WEB_SESSION_TTL=12h
WEB_SESSION_IDLE=2h
# Let admins see and export statistics per person (check with the works
# council and data protection first)
WEB_STATS_PER_PERSON=false
# Keep the daily status per person this many months; 0 = delete manually only.
# Monthly figures without names are kept indefinitely.
STATS_RETENTION_MONTHS=24
# Number of desks available today, shown as a reference (empty = unknown)
OFFICE_DESKS=

# --- Docker Compose ------------------------------------------------------
# UID/GID the container runs as (= owner of ./volumes/officebot); see `id -u` / `id -g`.
# Compose substitutes ${...} in compose.yaml only from this file, so change
# these two values here (or export them in your shell), not in .env.local.
OFFICEBOT_UID=1000
OFFICEBOT_GID=1000
# Reverse proxy for the web interface (profile "web":
# docker compose --profile web up -d). Host name of the interface:
OFFICEBOT_DOMAIN=
# auto = Let's Encrypt via ports 80/443, files = certificate from CADDY_CERT_DIR
CADDY_TLS=auto
# Contact address for the Let's Encrypt account (auto)
ACME_EMAIL=
PROXY_HTTP_PORT=80
PROXY_HTTPS_PORT=443
# Directory with fullchain.pem and privkey.pem (files)
CADDY_CERT_DIR=./volumes/certs
```

## `.env.local.example`

Vorlage für die lokale, nicht committete `.env.local` (`cp .env.local.example .env.local`). Werte hier überschreiben `.env`.

```dotenv
# --- Slack credentials ---------------------------------------------------
# Required unless DRY_RUN=true: Bot User OAuth Token (xoxb-…) and
# app-level token with connections:write (xapp-…).
SLACK_BOT_TOKEN=xoxb-...
SLACK_APP_TOKEN=xapp-...

# --- Summary channel -----------------------------------------------------
# Channel for the weekly summary (the bot must be a member)
SLACK_SUMMARY_CHANNEL_ID=C123456

# --- Target group --------------------------------------------------------
# Set exactly ONE of the two options:
# A) all active human members of this channel are asked
SLACK_PARTICIPANTS_CHANNEL_ID=
# B) comma-separated list of user IDs (find them with `officebot list-users`)
SLACK_USER_IDS=U123,U456,U789
# Never ask these people (optional, comma separated)
SLACK_EXCLUDE_USER_IDS=

# --- Web interface (optional) --------------------------------------------
# Client ID and secret of the Slack app (Basic Information → App Credentials)
SLACK_CLIENT_ID=
SLACK_CLIENT_SECRET=

# --- Local overrides of .env (optional) ----------------------------------
# DRY_RUN=true
# POLL_TIME=10:30
# BOT_LANGUAGE=de
```
