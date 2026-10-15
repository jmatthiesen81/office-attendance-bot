# Office Attendance Bot

A small, self-contained Slack app for weekly office attendance planning.

- At the configured time (default: **Monday 07:00 Europe/Berlin**), every member of the target group receives a private message with checkboxes for Monday to Friday and a **Speichern** (*Save*) button.
- Answers are accepted until the deadline (default: **Friday 08:00** of the polled week) and can be changed as often as needed.
- The results channel contains **exactly one summary message per calendar week**, which is updated via `chat.update` on every change.
- When someone changes an answer they already saved, the bot replies in the summary's thread with a short summary, e.g. "Alex changed their attendance: no longer coming in on Wednesday, but on Thursday instead." Several changes in quick succession are combined into one reply.
- Optionally, people who have not answered yet receive reminders.
- Slack messages are in English (default), German, French, Spanish, Italian, Portuguese, Dutch, Danish, Swedish, Norwegian (Bokmål), Polish, Czech, Turkish, Ukrainian or Japanese, configurable via `BOT_LANGUAGE`. For fun, every language also exists in nerd and pirate speak: `nerd_<lang>` and `pirate_<lang>`, e.g. `nerd_de` or `pirate_en`.
- **Language per person**: with **🌐 Language** (in the DM, the summary and the time-off form), everyone chooses the language of their own DM, reminders and replies. The channel summary stays in `BOT_LANGUAGE`. Optionally, the bot uses a person's Slack language until they choose one (`BOT_LANGUAGE_FROM_SLACK`).
- Below the summary, a **📩 Abfrage an mich senden** (*send the poll to me*) button re-sends the DM to anyone who missed or lost it. People who joined the target group after the weekly start can use it to add themselves.
- **Time off**: people who are away for a whole week get neither the poll nor reminders, and the summary lists them as away. They enter it with **🌴 I'm away this week** in the DM, with the **🌴 Plan time off** form (in the DM and the summary), or an admin does it via the CLI.

Technology: Go, Slack Socket Mode (no public HTTP endpoint), SQLite (WAL), a single Docker container. External Go dependencies: only `slack-go/slack` and `modernc.org/sqlite`.

> **Note on the translations:** the language catalogs were created with AI and may contain mistakes, such as unusual wording, the wrong form of address or unfitting terms. This applies in particular to the nerd and pirate variants: puns, tech jargon and pirate speak do not translate literally, and in many languages they are only lightly adapted so far. Corrections are welcome, see [Development](#development).

> UI texts quoted in this document are the German texts (`BOT_LANGUAGE=de`) with an English translation. By default (`BOT_LANGUAGE=en`), the bot sends all Slack messages in English.

---

## Contents

1. [Architecture](#architecture)
2. [Setting up the Slack app](#setting-up-the-slack-app)
3. [Configuration](#configuration)
4. [Running with Docker Compose](#running-with-docker-compose)
5. [Test run](#test-run)
6. [CLI commands](#cli-commands)
7. [Backup](#backup)
8. [Troubleshooting](#troubleshooting)
9. [Development](#development)
10. [Known limitations](#known-limitations)

---

## Architecture

```text
cmd/officebot/main.go            Wiring, subcommands, health check, graceful shutdown
internal/config/                 Load and validate environment variables
internal/isoweek/                ISO 8601 weeks, DST-safe schedule computation
internal/domain/                 Shared types (Poll, Participant, Response, Days, Absence)
internal/slack/                  API interface + slack-go adapter + dry run, Block Kit, Socket Mode, interactions
internal/participants/           Determine the target group (channel or list mode, filters)
internal/poll/                   Business logic (weekly start, saving, deadline, reminders) + scheduler
internal/summary/                Channel summary: rendering + central update worker
internal/storage/                SQLite repository + embedded migrations
internal/worker/                 Outbox worker with retry/backoff
```

### Key decisions

| Decision                                 | Rationale                                                                                                                                                                                                                                                                                                                                                                |
|------------------------------------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| **Transactional outbox** (`outbox_jobs`) | The poll, the participant snapshot and one `send_dm` job per person are created in *one* transaction. After a restart, sending resumes where it stopped. Failed DMs are retried individually.                                                                                                                                                                            |
| **Idempotency via unique constraints**   | `UNIQUE(workspace_id, iso_year, iso_week)` prevents duplicate polls. `UNIQUE(job_type, participant_id, dedupe_key)` prevents duplicate jobs. Each reminder round (`reminder_rounds`) runs exactly once.                                                                                                                                                                  |
| **Message metadata against duplicates**  | DMs and the summary carry Slack message metadata. If the bot crashes after Slack accepted a message but before its timestamp was stored, the next attempt finds the existing message and does not post again.                                                                                                                                                            |
| **Versioned summary**                    | Every relevant change increments `summary_version`. A single update worker batches changes for 1.5 s, **always renders from the current database state** and then sets `summary_synced_version`. An older state can therefore never overwrite a newer one. As long as the two versions differ, the worker syncs again: with backoff, every 30 s and after every restart. |
| **Explicit save instead of auto-save**   | Checkbox clicks are only acknowledged, never persisted. Only **Speichern** (*Save*) reads the selection (`state.values`) from the payload. The `block_id` contains the revision, so Slack clients show the saved selection after the update.                                                                                                                             |
| **Database before Slack**                | An answer is committed first. If a Slack call fails afterwards, the answer is kept and the call is retried via the outbox.                                                                                                                                                                                                                                               |
| **Mentions without mass pings**          | The summary is first posted as a placeholder without mentions and immediately completed via `chat.update`. Updates do not trigger notifications, so the people listed as "Noch offen" (*still open*) are not pinged.                                                                                                                                                     |
| **A single SQLite connection**           | `MaxOpenConns=1`, WAL, `busy_timeout=5000`, `_txlock=immediate`. There is no `SQLITE_BUSY` within the process, and concurrent CLI access simply waits.                                                                                                                                                                                                                   |
| **Pure-Go SQLite**                       | No cgo, static binary, distroless image of about 15 MB.                                                                                                                                                                                                                                                                                                                  |
| **No HTTP port**                         | The health check reads a heartbeat file that the daemon writes every 15 s (process, database, Socket Mode status).                                                                                                                                                                                                                                                       |

### Flows

1. **Scheduler**: sleeps on a timer until the next relevant moment (weekly start, deadline, reminder, at most 1 h). Each tick is idempotent: it closes polls whose deadline has passed, creates the current poll if it is missing (also catching up after downtime, as long as the deadline has not passed), schedules due reminders and deletes old data.
2. **Saving**: acknowledged immediately (under 3 s). Then, asynchronously: check the workspace, take the user from the Slack-authenticated payload, use the poll ID only as a lookup key, and check that the poll is open, the deadline has not passed, the user is in the snapshot and the message is the stored DM. Then an atomic upsert (the revision only increases on a real change), update the DM and trigger the summary.
3. **Deadline**: from the deadline on, save clicks are rejected (with a notice to the person). The DMs are re-rendered without buttons as closed, and the summary shows "🔒 Abstimmung geschlossen" (*poll closed*).
4. **Requesting the DM again** (button below the summary, only while the poll is open): the click must come from the current summary message. All replies are ephemeral, i.e. only visible to the person who clicked.
   - **Participants:** the old DM is replaced by a note without buttons ("durch eine neue ersetzt", *replaced by a new one*). A new DM with the saved selection is queued via the outbox. A per-participant generation counter in the DM metadata ensures that crash recovery never adopts the old DM. At most one new DM per person every 2 minutes.
   - **Late joiners:** people who now belong to the target group (member of the participants channel, or listed in `SLACK_USER_IDS`) and pass the usual filters are added to the running poll. Everybody else gets a short explanation.
5. **Time off** (table `absences`): whole calendar days from–to, both included, in `POLL_TIMEZONE`. Overlapping and directly adjacent entries of a person are merged.
   - **Away all week** (Monday to Friday covered): the weekly DM is not sent (delivery status `away`), there are no reminders, and the summary lists the person under "Away". People who are away do not count towards the total: "4 of 5 people · 1 away".
   - **Away part of the week**: the DM arrives as usual. Reminders on days off are skipped.
   - **In the DM**: **🌴 I'm away this week** enters Monday to Friday of the polled week. The DM then replaces the checkboxes with a note and a **Not away after all** button. It gives back exactly this week, even if it lies inside a longer vacation. A previously saved selection is kept.
   - **Form** (**🌴 Plan time off** in the DM and the summary): opens only for the person who clicked. It takes a date range and lists the person's own planned time off, each with **Delete**. The last day must not be in the past and at most one year ahead. In Slack, everybody can only change their own entries.
   - **Changes while a poll is open** take effect immediately: the summary and the DM are updated. Someone whose DM was held back because of time off and who deletes it gets the DM right away. Someone who is away and clicks **Send me the poll** (also as a late joiner) gets the DM with **Not away after all**.
   - **Retention**: time off that ended more than `RETENTION_WEEKS` ago is deleted automatically.
6. **Language per person** (table `user_settings`, [ADR 0002](adr/0002-per-user-language.md)): everything only one person sees (DM, reminders, ephemeral replies, forms) is in that person's language: their own choice, otherwise their Slack language if `BOT_LANGUAGE_FROM_SLACK=true` and the bot speaks it, otherwise `BOT_LANGUAGE`. The channel summary is always in `BOT_LANGUAGE`.
   - **🌐 Language** opens a form with all languages, each listed in its own language, plus the nerd and pirate variants. "Team default" clears the choice. From the time-off form, it opens on top of it; that form keeps its language until it is opened again.
   - **After saving**, the DMs of all open polls are re-rendered in the new language right away (`update_dm` via the outbox).
   - **Robustness**: if a stored language no longer exists (e.g. after a rollback), the next step applies and the bot logs a warning. The choice stays stored.
   - **Retention**: the choice of people who have taken part in no stored poll for `RETENTION_WEEKS` is deleted automatically.
7. **Change notices in the thread** (table `change_notices`, turned off with `SUMMARY_CHANGE_NOTICES=false`): when someone changes an answer they already saved, the bot stores the selection *before* the change in the same transaction and schedules a `change_notice` job after `SUMMARY_CHANGE_NOTICE_DELAY`. A person's first answer never causes a notice, and neither do corrections within `SUMMARY_CHANGE_NOTICE_GRACE` after it (grace period, counted from the first answer).
   - **Combining**: further changes until then only update the answer. The job compares the stored selection with the current one and posts **one** thread reply under the summary, depending on the difference: days dropped, days added, both ("no longer coming in on Wednesday, but on Thursday instead"), not in the office at all any more, or in the office after all. If the changes cancel out, it posts nothing.
   - **Name instead of mention**: the notice shows the display name so the person is not pinged. It is in `BOT_LANGUAGE`, like the summary.
   - **Robustness**: if the summary does not exist yet, the job is retried with backoff. If the person changes their selection while the notice is being posted, another notice follows for that. If the bot crashes right after posting, a notice may appear twice once.

---

## Setting up the Slack app

The complete step-by-step guide is in **[Creating the Slack app](slack-app-setup.md)**. It covers the manifest, tokens, installation, inviting the bot, finding IDs and a quick check. It is written so it can be handed to a workspace admin.

In short:

1. Create the app from the manifest.
2. Create an app-level token with `connections:write` (`SLACK_APP_TOKEN`).
3. Install the app and copy the bot token (`SLACK_BOT_TOKEN`).
4. `/invite` the bot to the results channel, and in mode A also to the participants channel.

### Required OAuth scopes

| Scope                                 | Purpose                                                     |
|---------------------------------------|-------------------------------------------------------------|
| `chat:write`                          | Post DMs, reminders, the summary and thread replies         |
| `im:write`                            | Open a DM channel with a person                             |
| `im:history`                          | After a crash, check whether a DM was already sent          |
| `users:read`                          | Filter out bots, app users and deactivated accounts         |
| `channels:read` / `groups:read`       | Mode A: read the members of the participants channel        |
| `channels:history` / `groups:history` | After a crash, check whether the summary was already posted |

The `*:history` scopes are only needed for duplicate protection after a crash. Without them the bot still works. It logs a debug message and may, in rare cases, send a message twice.

The app-level token additionally needs `connections:write`. After every scope change the app must be reinstalled.

---

## Configuration

All settings come from environment variables and are validated at startup. All errors are reported at once, and startup aborts with exit code 2. Tokens never appear in logs.

Docker Compose loads two files, the second overriding the first:

| File         | In git                | Content                                                                                                                     |
|--------------|-----------------------|-----------------------------------------------------------------------------------------------------------------------------|
| `.env`       | yes                   | Shared, non-secret defaults: schedule, deadline, reminders, operation, container UID/GID                                    |
| `.env.local` | **no** (`.gitignore`) | Workspace-specific settings and secrets: tokens, channel IDs, participants, local overrides. Template: `.env.local.example` |

```bash
cp .env.local.example .env.local   # then fill in the values
```

[dotenv.md](dotenv.md) shows the complete current contents of both files, ready to copy.

`.env.local` (from `.env.local.example`):

```dotenv
# --- Slack credentials (required unless DRY_RUN=true) ----------------------
SLACK_BOT_TOKEN=xoxb-...
SLACK_APP_TOKEN=xapp-...

# Channel for the summary (the bot must be a member)
SLACK_SUMMARY_CHANNEL_ID=C123456

# Target group – set exactly ONE of the two options:
#   A) all active human members of a channel
SLACK_PARTICIPANTS_CHANNEL_ID=
#   B) explicit list of user IDs
SLACK_USER_IDS=U123,U456,U789
# optional: never ask these people
SLACK_EXCLUDE_USER_IDS=

# optional local overrides of .env, e.g.
# DRY_RUN=true
# POLL_TIME=10:30
# BOT_LANGUAGE=de
```

`.env` (shared defaults):

```dotenv
# --- Schedule ------------------------------------------------------------
POLL_WEEKDAY=MONDAY            # MONDAY = current week, any other day = next week
POLL_TIME=07:00                # HH:MM
POLL_TIMEZONE=Europe/Berlin    # IANA time zone, daylight saving time is handled
POLL_DEADLINE_WEEKDAY=FRIDAY   # deadline, relative to the polled week
POLL_DEADLINE_TIME=08:00

# --- Reminders -----------------------------------------------------------
REMINDER_ENABLED=true
REMINDER_WEEKDAY=TUESDAY       # several allowed: TUESDAY,THURSDAY
REMINDER_TIME=09:00
REMINDER_MAX_COUNT=1           # max. reminders per person and week

# --- Operation -----------------------------------------------------------
DATABASE_PATH=/data/officebot.db
LOG_LEVEL=info                 # debug, info, warn, error
RETENTION_WEEKS=26             # older polls are deleted
SUMMARY_MAX_NAMES=40           # names per list, then "… und N weitere" (… and N more)
SUMMARY_PROJECT_LINK=true      # false = no GitHub link below the summary
SUMMARY_CHANGE_NOTICES=true    # false = no change notices in the summary thread
SUMMARY_CHANGE_NOTICE_DELAY=2m # changes of one person within this time = one notice
SUMMARY_CHANGE_NOTICE_GRACE=10m # corrections this soon after the first answer = no notice
BOT_LANGUAGE=en                # language of Slack messages (see table)
BOT_LANGUAGE_FROM_SLACK=false  # true = DMs in the person's Slack language until they choose one
DRY_RUN=false                  # true = send nothing to Slack, only log

# --- Docker Compose ------------------------------------------------------
# UID/GID the container runs as (= owner of ./volumes/officebot).
# Find out with: id -u / id -g
OFFICEBOT_UID=1000
OFFICEBOT_GID=1000
```

`OFFICEBOT_UID`/`OFFICEBOT_GID` are substituted into `compose.yaml` itself. Compose reads such `${…}` values only from `.env` (or the shell), so change them in `.env`, not in `.env.local`. Never put tokens into `.env`: it is committed.

| Variable                                       | Required    | Default                 | Meaning                                                                                                                                                                                                                    |
|------------------------------------------------|-------------|-------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `SLACK_BOT_TOKEN`                              | yes¹        | –                       | Bot User OAuth Token, `xoxb-…`                                                                                                                                                                                             |
| `SLACK_APP_TOKEN`                              | yes¹        | –                       | App-level token (`connections:write`), `xapp-…`                                                                                                                                                                            |
| `SLACK_SUMMARY_CHANNEL_ID`                     | yes         | –                       | Results channel                                                                                                                                                                                                            |
| `SLACK_PARTICIPANTS_CHANNEL_ID`                | one of both | –                       | Mode A: channel members are asked                                                                                                                                                                                          |
| `SLACK_USER_IDS`                               | one of both | –                       | Mode B: comma-separated user IDs                                                                                                                                                                                           |
| `SLACK_EXCLUDE_USER_IDS`                       | no          | –                       | Excluded user IDs                                                                                                                                                                                                          |
| `POLL_WEEKDAY` / `POLL_TIME`                   | no          | `MONDAY` / `07:00`      | When the poll is sent. `MONDAY` polls the current week, any other day the next week (see below).                                                                                                                           |
| `POLL_TIMEZONE`                                | no          | `Europe/Berlin`         | Time zone for all times                                                                                                                                                                                                    |
| `POLL_DEADLINE_WEEKDAY` / `POLL_DEADLINE_TIME` | no          | `FRIDAY` / `08:00`      | Deadline within the polled week. It must lie after the poll start.                                                                                                                                                         |
| `REMINDER_ENABLED`                             | no          | `true`                  | Reminders on/off                                                                                                                                                                                                           |
| `REMINDER_WEEKDAY` / `REMINDER_TIME`           | no          | `TUESDAY` / `09:00`     | Reminder time(s). Only times between the weekly start and the deadline count.                                                                                                                                              |
| `REMINDER_MAX_COUNT`                           | no          | `1`                     | Maximum reminders per person and week                                                                                                                                                                                      |
| `DATABASE_PATH`                                | no          | `/data/officebot.db`    | SQLite file (the directory is created with mode 0700)                                                                                                                                                                      |
| `LOG_LEVEL`                                    | no          | `info`                  | `debug`, `info`, `warn`, `error`                                                                                                                                                                                           |
| `RETENTION_WEEKS`                              | no          | `26`                    | Retention of past polls                                                                                                                                                                                                    |
| `SUMMARY_MAX_NAMES`                            | no          | `40`                    | Truncation of long name lists                                                                                                                                                                                              |
| `SUMMARY_PROJECT_LINK`                         | no          | `true`                  | Small note below "Last updated" inviting people to rate the bot on GitHub; `false` hides it                                                                                                                                |
| `SUMMARY_CHANGE_NOTICES`                       | no          | `true`                  | When someone changes a saved answer, the bot replies in the summary thread with the difference (see flows); `false` turns it off                                                                                           |
| `SUMMARY_CHANGE_NOTICE_DELAY`                  | no          | `2m`                    | Wait after the first change; further changes of that person meanwhile are combined into one reply (e.g. `30s`, `5m`)                                                                                                       |
| `SUMMARY_CHANGE_NOTICE_GRACE`                  | no          | `10m`                   | Grace period after a person's first answer: corrections within it count as part of the first answer and cause no notice; `0` = none                                                                                        |
| `DRY_RUN`                                      | no          | `false`                 | No contact with Slack. All messages go to the log.                                                                                                                                                                         |
| `BOT_LANGUAGE`                                 | no          | `en`                    | Team language: `en`, `de`, `fr`, `es`, `it`, `pt`, `nl`, `da`, `sv`, `nb`, `pl`, `cs`, `tr`, `uk`, `ja`; for fun also `nerd_<lang>`, `pirate_<lang>`. Applies to the summary and to everyone who has not chosen a language |
| `BOT_LANGUAGE_FROM_SLACK`                      | no          | `false`                 | `true`: people without an own choice get their DMs in their Slack language, if the bot speaks it (`de-DE` → `de`). Read with the participants at the weekly start                                                          |
| `HEALTH_FILE`                                  | no          | `/tmp/officebot.health` | Heartbeat file for the health check                                                                                                                                                                                        |
| `OFFICEBOT_UID` / `OFFICEBOT_GID`              | no          | `1000` / `1000`         | Compose only: user the container runs as, and thus owner of the data in `./volumes/officebot`                                                                                                                              |

¹ Not needed with `DRY_RUN=true`.

**Times for testing:** the poll start, deadline and reminders are freely configurable. To test right away, set `POLL_WEEKDAY` to today and `POLL_TIME` a few minutes into the future. Unless today is Monday, the bot then polls the next week. If this week's start time has already passed, the bot catches up on startup, provided the deadline has not passed. Alternatively, `officebot start-week` starts the week immediately.

**Which week is polled?** The work week starts on Monday. A poll that starts on Monday is for the current week. A poll that starts on any other day (Tuesday to Sunday) is for the next week, starting the following Monday. The deadline always refers to the polled week. Example of a Friday poll for the following week:

```dotenv
POLL_WEEKDAY=FRIDAY
POLL_TIME=12:00
POLL_DEADLINE_WEEKDAY=MONDAY   # Monday of the polled week
POLL_DEADLINE_TIME=08:00
REMINDER_WEEKDAY=MONDAY        # reminders only count between start and deadline,
REMINDER_TIME=07:00            # the default TUESDAY would be too late here
```

---

## Running with Docker Compose

```bash
# create .env.local (see above), then:
docker compose up -d              # published image from ghcr.io (make pull)
docker compose logs -f officebot
docker compose ps                 # STATUS shows (healthy) after about 15–60 s
```

**Published image or own build:** `compose.yaml` refers to `ghcr.io/jmatthiesen81/office-attendance-bot:1`, a multi-arch image for amd64 and arm64. To run the bot, you only need `compose.yaml`, `.env`, `.env.local` and the `volumes/officebot/` directory; no clone of the repository. The `:1` tag gets fixes and new features, but no breaking changes. For a fixed version, use `:1.2.3`; for the latest untested state, `:edge` (see [Development](#development)). Updates: `docker compose up -d`. Because of `pull_policy: always`, Compose pulls the newest image for the tag every time. With `docker compose up -d --build` (`make up`), Compose builds from source instead. This also happens automatically if the image cannot be pulled. `docker compose exec officebot /officebot version` shows the running version.

Container properties:

- Multi-stage build, runtime image `distroless/static:nonroot` (about 15 MB). Under Compose it runs as `OFFICEBOT_UID:OFFICEBOT_GID` (default 1000:1000), never as root.
- Read-only root file system, `/tmp` as tmpfs, all capabilities dropped, `no-new-privileges`.
- **No Docker volumes:** all data lives visibly in the project directory under `./volumes/officebot/` (bind mount to `/data`). It holds `officebot.db` with its `-wal`/`-shm` files (database file mode 0600). The image deliberately declares no `VOLUME`, so Docker never creates anonymous volumes. The directory is tracked via `.gitkeep`; its contents are excluded by `.gitignore`. Recommended: `chmod 700 volumes/officebot`.
- `restart: unless-stopped`. `SIGTERM` triggers a graceful shutdown: saves and jobs in progress are completed first (`stop_grace_period: 30s`).
- Health check `officebot healthcheck`. It reports "unhealthy" if the heartbeat is older than 60 s, the database is unavailable, or Socket Mode has been disconnected for more than 2 minutes. If Socket Mode stays disconnected for more than 15 minutes, the bot exits and the restart policy starts it again.

---

## Test run

### Locally without Slack (dry run)

```bash
docker build -t officebot .
mkdir -p volumes/officebot-dry
docker run -d --name officebot-dry -u "$(id -u):$(id -g)" -v "$PWD/volumes/officebot-dry:/data" \
  -e DRY_RUN=true -e SLACK_SUMMARY_CHANNEL_ID=CSUM -e SLACK_USER_IDS=U1,U2,U3 officebot

docker logs -f officebot-dry      # shows the poll start, all DMs and the summary as text

E="-e DRY_RUN=true -e SLACK_SUMMARY_CHANNEL_ID=CSUM -e SLACK_USER_IDS=U1,U2,U3"
docker exec $E officebot-dry /officebot simulate-save --user U1 --days mon,wed
docker exec $E officebot-dry /officebot simulate-save --user U2 --days ""   # "no office day"
docker exec $E officebot-dry /officebot simulate-request --user U3          # click "Abfrage an mich senden"
docker exec $E officebot-dry /officebot status
docker restart officebot-dry      # creates no new poll and no new DMs
```

### With real Slack

1. For the first test, set `SLACK_USER_IDS` to your own user ID and use a test channel as `SLACK_SUMMARY_CHANNEL_ID`.
2. `docker compose up -d --build`. If this week's configured start time has already passed, the DM arrives immediately. Otherwise run `docker compose exec officebot /officebot start-week`.
3. Tick days in the DM and click **Speichern** (*Save*). The DM shows "✅ Gespeichert" (*saved*), and the summary in the channel is updated after about 1–2 s.
4. Change the selection and save again: no new channel message appears, only an update.
5. Test the deadline: set `POLL_DEADLINE_WEEKDAY`/`POLL_DEADLINE_TIME` shortly into the future and restart. Afterwards the DM shows "🔒 geschlossen" (*closed*).
6. Reset the week and test again: `docker compose exec officebot /officebot delete-week --week 2026-W42 --yes`. This only deletes the database records; the old Slack messages remain.

---

## CLI commands

All commands run inside the container with the same configuration: `docker compose exec officebot /officebot <command>`. They only write to the database. The running bot does the sending within 30 s. This way only one process ever talks to Slack, and no duplicate messages are created.

| Command                                            | Effect                                                                            |
|----------------------------------------------------|-----------------------------------------------------------------------------------|
| `run`                                              | Start the bot (default)                                                           |
| `start-week [--week 2026-W43]`                     | Start the poll now. If it already exists, nothing happens.                        |
| `resync [--week 2026-W43]`                         | Rewrite the summary (without `--week`: all open polls)                            |
| `status`                                           | Open polls, answers, delivery status, outbox                                      |
| `list-users [--channel C…] [--all]`                | List users with IDs and print a ready-to-paste `SLACK_USER_IDS=` line (read-only) |
| `delete-week --week W --yes`                       | Delete a week's poll from the database (for testing)                              |
| `simulate-save --user U --days mon,wed [--week W]` | Only with `DRY_RUN=true`: simulate an answer                                      |
| `simulate-request --user U [--week W]`             | Only with `DRY_RUN=true`: simulate a click on "Abfrage an mich senden"            |
| `away list [--user U]`                             | List upcoming time off (with IDs)                                                 |
| `away add --user U --from D --to D`                | Add time off (`YYYY-MM-DD`, both days included)                                   |
| `away remove --user U --from D --to D`             | Remove days from time off (shortens or splits entries)                            |
| `away remove --id N`                               | Delete one entry                                                                  |
| `lang list`                                        | List the language choices of people                                               |
| `lang set --user U <language>`                     | Set a person's language, e.g. `de` or `pirate_en`; open DMs are re-rendered       |
| `lang reset --user U`                              | Back to the team default                                                          |
| `backup <file>`                                    | Consistent copy of the database (`VACUUM INTO`), also while running               |
| `migrate`                                          | Apply migrations and exit                                                         |
| `healthcheck`                                      | Health check probe                                                                |
| `version`                                          | Print the version (`dev` for local builds without a version)                      |

---

## Backup

The database is the single source of truth. The state can be fully reconstructed without the Slack messages; `resync` rewrites all summaries.

```bash
make backup
# equivalent to:
docker compose exec officebot /officebot backup /data/officebot-20261013-1000.db
mv volumes/officebot/officebot-20261013-1000.db backup/
```

`VACUUM INTO` creates a consistent copy even while the bot is running (WAL). The file appears directly in `./volumes/officebot/`. Simply copying `officebot.db` without `-wal`/`-shm` while the bot is running is **not** safe. With the container stopped (`docker compose stop`), however, you can simply copy the whole `volumes/officebot/` directory.

**Restore:** `docker compose stop`, place the backup as `volumes/officebot/officebot.db`, remove the old `officebot.db-wal`/`-shm` next to it, then `docker compose start` and run `resync`.

---

## Troubleshooting

| Symptom / log                                          | Cause and fix                                                                                                                       |
|--------------------------------------------------------|-------------------------------------------------------------------------------------------------------------------------------------|
| `invalid configuration: …`                             | Check the variables. All errors are listed at once.                                                                                 |
| `slack auth.test … invalid_auth`                       | `SLACK_BOT_TOKEN` is wrong or the app is not installed                                                                              |
| `socket mode: invalid app token`                       | `SLACK_APP_TOKEN` is wrong or lacks `connections:write`                                                                             |
| `missing_scope`                                        | Add the scope (see above) and **reinstall the app**                                                                                 |
| `not_in_channel` / `channel_not_found` for the summary | Invite the bot to the results channel and check the channel ID                                                                      |
| `list members of C…` fails                             | Mode A: the bot must be a member of the participants channel. A private channel needs `groups:read`.                                |
| `no participants found`                                | The target group is empty (only bots or deactivated users?). The bot retries every minute.                                          |
| Clicking Save has no effect                            | Is *Interactivity* enabled? Is the socket connected (`docker compose ps` → healthy)? Check the logs with `LOG_LEVEL=debug`.         |
| `poll DM cannot be delivered … unreachable`            | The person is deactivated or unreachable. They appear in the summary under "⚠️ nicht zustellbar" (*undeliverable*).                 |
| `job failed, retrying`                                 | Temporary error (network, 429). The bot retries with exponential backoff (5 s … 30 min, max. 8 attempts) and honours `Retry-After`. |
| The summary was deleted                                | It is re-posted automatically on the next sync (or immediately with `resync`)                                                       |
| No poll after downtime                                 | The poll is only caught up while the week's deadline has not passed. Otherwise use `start-week --week …` for a later week.          |

Useful: `docker compose exec officebot /officebot status` shows the delivery status, reminders and answer per person as well as the outbox statistics.

---

## Development

Go does not need to be installed locally. Tests and linting run in the official image:

```bash
make test     # go test -race ./...
make vet      # gofmt + go vet
```

The tests cover: ISO weeks and year boundaries (incl. 53-week years and DST), time off (merging, shortening and splitting, no DM and no reminder, form validation, only own entries deletable), configuration validation, participant filters, multiple and empty selections, repeated and duplicate answers, protection against manipulated interactions, the deadline, caught-up and idempotent weekly starts, concurrent changes (race detector), outbox retries with backoff and `Retry-After`, recovery after a lost Slack response, a deleted summary, reminders and formatting including large teams.

Translations: all texts the bot sends to Slack live in `internal/i18n/locales/<language>.json`. Date and time formats are stored there as Go layouts (`layout.*`). To add a language, copy `en.json` and translate it; the file name is the value for `BOT_LANGUAGE`. Add `nerd_<language>.json` and `pirate_<language>.json` for the nerd and pirate variants. Go always renders month and weekday names in layouts in English, so all non-English languages use numeric date formats (nerd variants: ISO 8601). Tests check that every language contains all keys with the same placeholders and uses no English names in its layouts. The catalogs were translated with AI and have not been reviewed by native speakers. Please make improvements directly in the respective JSON file; for the nerd and pirate variants, a tone of their own is explicitly welcome.

Architecture decisions are recorded in [adr/](adr/), e.g. [ADR 0001: Configurable work days](adr/0001-configurable-work-days.md) and [ADR 0002: Per-person language](adr/0002-per-user-language.md).

Releases: for pull requests, the workflow `.github/workflows/release.yml` runs gofmt, `go vet`, the tests and a test build without publishing anything. Every push to a version branch such as `1.0` publishes the current state as `edge` and `sha-<commit>` once the tests pass. This is meant for trying things out on a test server, not for production. `officebot version` shows `edge-<commit>` there. A Git tag with the version number without a `v` prefix (e.g. `1.0.0`) publishes a release:

```bash
git tag 1.0.0 && git push origin 1.0.0   # → tags 1.0.0, 1.0, 1 and latest
```

Pre-releases such as `1.1.0-rc.1` only get their own tag, and `0.x` versions get no major tag. The image includes an SBOM and build provenance. After the first release, the package is private on GitHub. Set it to **Public** once under *Package settings* → *Change visibility*.

New migrations: add a file with the next free number, e.g. `internal/storage/migrations/0004_….sql`. It is applied once, in a transaction, at startup.

---

## Known limitations

- **Single instance**: the scheduler, outbox and update worker assume that exactly one container is running (no distributed locks).
- **Privacy of time off**: the bot stores vacation and absence periods with the Slack user ID in its database. Anyone away for a whole week is listed as "Away" in the summary channel, visible to everyone in it. The bot never asks for a reason. Inform the team beforehand and, where applicable, align with your works council or data protection officer.
- **Privacy of the language choice**: per person, only the language code is stored, plus the Slack language (`locale`) with the participants of a poll. Both are deleted with the retention period.
- **Time off and closed weeks**: later changes to time off only update open polls. Closed summaries stay as they are.
- **Single workspace**: interactions from other workspaces are discarded. Org-level Enterprise Grid installations are not supported.
- **Unsaved ticks**: if someone ticks days without saving, Slack keeps showing these ticks locally. Only the line "✅ Gespeichert: …" (*saved*) below the checkboxes is binding.
- **Participant snapshot**: people who join the participants channel after the weekly start are not asked automatically. They can add themselves to the running week with the button below the summary, otherwise they are asked from the next week on. People who leave remain listed for the current week.
- **Reminders**: if the bot crashes exactly between sending a reminder and storing the counter, a reminder may arrive twice. For poll DMs and the summary, the metadata check prevents such duplicates.
- **Lost DMs** are not re-sent automatically, but anyone can request a new one with the button below the summary. Deleted summaries are re-posted automatically.
- **Long lists** are truncated per section after `SUMMARY_MAX_NAMES` names ("… und N weitere", *… and N more*). The counts per day remain complete.
- "Letzte Aktualisierung" (*last updated*) only changes when the content changes.
