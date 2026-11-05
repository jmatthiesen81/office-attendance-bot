# ADR 0003: Web interface for administration and statistics

- **Status:** Accepted, implemented on 2026-11-05
- **Date:** 2026-11-05

## Context

Today the bot is operated entirely through Slack and the CLI. Admin tasks such as time off (`away`), languages (`lang`), corrections after the deadline (`attendance`), `refresh`, `resync` and `status` exist only on the server console. People without access to the server cannot do them.

The bot should also answer the question **whether, and to what extent, shared desks are enough**. That needs figures that hold up in front of the team:

- how many people are in the office per day, with peaks and distribution,
- how many office days each person has per month,
- how long colleagues were not in the office.

Starting point in the code:

- The bot runs in **Socket Mode** (`internal/slack/socket.go`). It has no HTTP port and no public endpoint.
- All CLI actions call service methods (`cmd/officebot/main.go`, `internal/poll`). A web interface can use the same methods.
- The raw data lives in SQLite: `polls`, `participants`, `responses` (five day columns per person and week) and `absences`.
- `RETENTION_WEEKS` (default 26) deletes old weeks including their responses (`Service.Cleanup`). With today's tables, statistics can go back half a year at most.
- Responses are initially **planned** attendance, given by the deadline. Admins already reconcile them with actual attendance via `attendance` (see commit `9200ec5`), including past days. So far, however, the bot does not record which days were checked this way. Checked and unchecked figures therefore cannot be told apart.

## Decision

The bot gets an optional web interface in the same process. Login uses **"Sign in with Slack" (OpenID Connect)**. Who may log in is set via the CLI first, and afterwards also in the web interface. For reliable statistics, the bot writes its own table with the status of every person on every work day. That table has its own, longer retention period.

### 1. Web interface

- New package `internal/web`, built with the standard library only: `net/http`, `html/template` and `embed` for templates and CSS. There are no new dependencies and no CDN.
- The server only starts if `WEB_LISTEN_ADDR` is set. Without it, the bot behaves exactly as it does today.
- It runs as another goroutine in the `run` loop and shuts down cleanly with the rest of the bot (`http.Server.Shutdown`).
- TLS terminates at the reverse proxy (section 9). The bot's port is reachable only within the Compose network, not on the host.
- Charts are rendered as SVG on the server. No JavaScript is needed, and the CSP can be `default-src 'self'`.
- The interface is available in German and English. It uses the existing `i18n.Translator` and the language the person chose in the bot ([ADR 0002](0002-per-user-language.md)).

### 2. Sign in with Slack (OpenID Connect)

1. `GET /login` shows "Sign in with Slack".
2. `GET /auth/start` creates `state` and `nonce`. Both are random, single-use and valid for 10 minutes. `web_login_states` stores only their hash. The page then redirects to `https://slack.com/openid/connect/authorize` with these parameters:
   - `response_type=code`
   - `scope=openid profile`
   - `client_id`
   - `redirect_uri={WEB_BASE_URL}/auth/callback`
   - `state`
   - `nonce`
   - `team` (the bot's team ID)
3. `GET /auth/callback`:
   - checks `state` and marks it as used,
   - exchanges `code` on the server side at `https://slack.com/api/openid.connect.token`,
   - checks `iss = https://slack.com`, `aud = SLACK_CLIENT_ID`, `exp` and `nonce` in the ID token,
   - checks that `https://slack.com/team_id` matches the bot's workspace.

   The ID token's signature is not verified. OIDC Core §3.1.3.7 allows this because the token comes directly from the token endpoint over TLS.
4. **Authorization:** `https://slack.com/user_id` must be listed in `web_users`. If it is not, a 403 page "No access" appears and no session is created.
5. **Session:**
   - The session ID is a random 256-bit value. `web_sessions` stores only its SHA-256 hash.
   - Cookie `__Host-officebot_session` with `Secure`, `HttpOnly`, `SameSite=Lax` and `Path=/`.
   - The session expires after `WEB_SESSION_TTL` (default 12 h), or earlier after `WEB_SESSION_IDLE` without activity (default 2 h).
6. **On every request**, the session and `web_users` are read again. Role changes and revoked access take effect immediately. Revoking access also deletes all of the person's sessions.
7. **Logout** via `POST /logout`.

Further protection:

- A CSRF token per session for every `POST`, plus a check of the `Origin` header.
- Headers `Content-Security-Policy` (`default-src 'self'; frame-ancestors 'none'`), `Referrer-Policy: same-origin` and `X-Content-Type-Options: nosniff`.
- Simple rate limiting on `/auth/*`.

Changes to the Slack app (documented in `slack-app-setup.md`):

- enable "Sign in with Slack",
- add the redirect URL `{WEB_BASE_URL}/auth/callback`,
- add the user scopes `openid` and `profile`.

### 3. Who may log in

There are two roles:

| Role     | May                                                                                            |
| -------- | ---------------------------------------------------------------------------------------------- |
| `admin`  | everything: administration, corrections, access management, per-person statistics (if enabled) |
| `viewer` | only the aggregated statistics                                                                 |

**Initial access via the CLI.** Without at least one entry, nobody can log in.

```sh
officebot web-user list
officebot web-user add --user U123 [--role admin|viewer]   # default: admin
officebot web-user set-role --user U123 --role viewer
officebot web-user remove --user U123
```

**Afterwards also in the web interface** at `/access` (admins only): add people from the workspace directory (`participants.Directory`), change roles, remove people.

The same rules apply to the CLI and the web. They live centrally in the storage layer:

- The last admin can be neither removed nor downgraded.
- Every change is written to `audit_log`. The author recorded there is the Slack user ID or `cli`.

### 4. Admin pages

| Route                | Role          | Content                                                                                                                     |
| -------------------- | ------------- | --------------------------------------------------------------------------------------------------------------------------- |
| `/`                  | admin, viewer | current week: who answered, delivery status, outbox (like `status`)                                                         |
| `/weeks/{2026-W45}`  | admin         | the week's responses, correct a response (optionally with a thread notice), `refresh`, `resync`                             |
| `/days/{2026-11-05}` | admin         | day check: everyone asked for that day with their status, change a status directly, mark the day as checked (see section 5) |
| `/absences`          | admin         | list, add and remove time off (like `away`)                                                                                 |
| `/languages`         | admin         | set or reset a person's language (like `lang`)                                                                              |
| `/closed-days`       | admin         | maintain days the office is closed (see section 5)                                                                          |
| `/access`            | admin         | access management                                                                                                           |
| `/stats`             | admin, viewer | statistics (see section 6)                                                                                                  |
| `/retention`         | admin         | show retention periods, delete per-person data by year, cut-off date or person (see section 5)                              |
| `/audit`             | admin         | audit log                                                                                                                   |

All actions use the same service methods as the CLI. The same rules therefore apply, for example that time off cannot lie in the past. Changes also go through the outbox and the update worker, with DM update, updated summary and thread notice.

### 5. Data basis for reliable figures

#### Daily status per person

New table `attendance_days`, one row per person and work day on which the person was asked:

| Column          | Meaning                                               |
| --------------- | ----------------------------------------------------- |
| `workspace_id`  | workspace                                             |
| `slack_user_id` | person                                                |
| `day`           | date `YYYY-MM-DD` in `POLL_TIMEZONE`                  |
| `status`        | `office`, `remote`, `away` or `no_answer`             |
| `iso_week`      | week of the poll, e.g. `2026-W45`, for recalculations |
| `updated_at`    | last recalculation                                    |

The status is determined like this, in this order:

1. **`office`:** the day is checked in the response.
2. **`away`:** a time-off entry (`absences`) covers the day.
3. **`remote`:** the person answered but did not check the day.
4. **`no_answer`:** the person was asked but did not answer.

People who were not asked in a week, for example because they were not yet on the team, get no rows. Those days count neither for nor against them.

**When the table is written:**

- when a week closes at the deadline,
- on every later correction of a closed week (`attendance` in the CLI or web, `away remove --id`). The affected week is then recalculated.

Open weeks do not appear in the statistics. A week counts as complete only after its deadline.

**Backfill:** the migration calculates the table once from all closed weeks still in the database. That gives a history of up to `RETENTION_WEEKS` from day one.

#### Checked days

Admins reconcile planned attendance with actual attendance. Today they do this via `attendance`, in future via the day check `/days/{date}`. To make this reconciliation visible in the figures, the bot records which days have been checked:

- New table `verified_days(workspace_id, day, verified_by, verified_at)`. A day counts as checked once an admin confirms it in the day check. That also works if nothing needed correcting. On the CLI: `officebot attendance verify --date 2026-11-05`.
- A checked day means: for **everyone** asked on that day, the status matches actual attendance.
- If something about that day changes afterwards, for example a response or a time-off entry, the day stays checked and the change is written to the audit log. After the deadline, people cannot change anything themselves anyway, only admins can.
- `attendance_days` gets the column `corrected`. It shows that an admin changed the status compared with the original response. This shows how often planned and actual attendance differ.
- Days on which no admin was in the office stay unchecked. The statistics openly show what share of days is checked.

#### Closed days

New table `office_closed_days(workspace_id, day, reason)`. Admins maintain public holidays, company holidays and closures there. These days are excluded from every rate and every denominator. Without this table, October 3 for example would look like a day everyone worked remotely. An automatic holiday list is out of scope for this ADR.

#### Retention

Figures without personal data and per-person data follow different rules.

**Figures without personal data are kept indefinitely.** The table `monthly_stats` holds per month:

- work days, closed days, office person-days,
- average, median, P90, P95 and maximum of occupancy, also by weekday,
- share of checked days, share of corrected entries, response rate,
- the **anonymous distributions**: number of people per bucket of office days in the month (0, 1–2, 3–5, 6–8, 9–12, more than 12) and per bucket of the longest period without office (up to 1 week, up to 2, up to 4, up to 8, more than 8 weeks).

The bot writes `monthly_stats` at the turn of the month. It rewrites them for affected months when something changes there, and always right before per-person data is deleted. All statistics without names therefore stay complete for old months, even after the daily data behind them has been deleted. For months without daily data, the statistics fall back to `monthly_stats`. The interface marks such months, because the filter "checked days only" can no longer be applied there.

**Per-person data** (`attendance_days`) is deleted in two ways:

1. **Automatically** after `STATS_RETENTION_MONTHS` (default 24) by the existing cleanup job. With `0`, automatic deletion is off. Deletion then happens only manually.
2. **Manually** via the CLI and the web interface (`/retention`, admins only):

   ```sh
   officebot stats purge --year 2024 [--yes]            # delete one calendar year
   officebot stats purge --before 2025-01-01 [--yes]    # delete everything before a cut-off date
   officebot stats purge --user U123 [--yes]            # delete all daily data of one person
   ```

   - Without `--yes`, the command only shows what would be deleted: period, number of rows and people affected.
   - Before deleting, it writes `monthly_stats` for all affected months. If only one person is deleted, the monthly figures stay as they were calculated with that person. They are not recalculated afterwards.
   - `--user` is meant for people leaving and for erasure requests under Art. 17 GDPR. It also deletes the person's responses and time off in weeks still within `RETENTION_WEEKS`. Otherwise the daily data would reappear on the next recalculation.
   - Every call is written to the audit log, with the period or person and the number of rows deleted.

`RETENTION_WEEKS` still applies to polls, responses, time off and Slack references. After the backfill, the statistics no longer depend on it. A high value such as 106 pays off mainly once: it brings up to two years of history into `attendance_days` on rollout. It can be lowered again afterwards.

#### What the figures say

The interface states these limits explicitly with every statistic:

- **Checked:** every period shows the share of checked days. Unchecked days reflect planned attendance. A filter "checked days only" limits all statistics to actual attendance.
- **Deviation from the plan:** the share of corrected entries on checked days shows how reliable the unchecked days are likely to be.
- **Coverage:** every period shows the response rate, i.e. the share of days with a status other than `no_answer`. Below 90 %, the figures carry a warning.
- Every metric has a short definition that can be shown with a click (a "Methodology" section).

### 6. Statistics

The period is freely selectable, for example a month, a quarter or the last 12 months.

#### Office occupancy (aggregated, for `viewer` and `admin`)

- **People in the office per day:** average, median, P90, P95 and maximum, plus a timeline over all days.
- **By weekday:** average occupancy per weekday. This shows whether days such as Tuesday are crowded.
- **Sufficient desks:** a curve showing on what percentage of days *n* desks would have been enough. If `OFFICE_DESKS` is set, the statistic compares this with today's number, for example: "14 desks available, 9 would have been enough on 95 % of days".
- **Per month:** work days, office person-days, average occupancy, peak and response rate.
- **Distribution of office days per person and month, without names:** for example "7 of 20 people were in the office on 2 days or fewer, 3 on more than 12". This is the central argument for shared desks, and nobody has to be named for it.
- **Time away from the office, without names:** distribution of each person's longest period without an office day, for example "5 people were not in the office for more than 4 weeks in a row".

#### Per person (`admin` only, only with `WEB_STATS_PER_PERSON=true`)

A table with one row per person:

- **office days per month**, as a count and as a rate relative to the work days on which the person was asked and not away,
- days `remote`, `away` and `no_answer` per month,
- **last office day** and **current time without office** in work days and calendar weeks,
- **longest period without office** in the selected period, with start and end date.

This is how a **period without office** is calculated:

- It is the longest run of consecutive work days without `office`.
- Closed days and days on which the person was not asked do not break the run. They do not count towards it either.
- The run is broken down into `remote`, `away` and `no_answer`. That way, a three-week vacation does not show up as three weeks of working from home. If it contains `no_answer` days, this is shown as "including X days without an answer".

#### Export

- `/stats.csv` exports the aggregated figures without names.
- The per-person export is available only to admins with `WEB_STATS_PER_PERSON=true`. Every such export is recorded in the audit log.

### 7. Database (migration `0007_web_and_stats.sql`)

- `web_users(workspace_id, slack_user_id, role, display_name, added_by, added_at, last_login_at)`, primary key `(workspace_id, slack_user_id)`, `role` checked to be `admin` or `viewer`
- `web_sessions(id_hash PRIMARY KEY, workspace_id, slack_user_id, csrf_token, created_at, last_seen_at, expires_at)`
- `web_login_states(state_hash PRIMARY KEY, nonce, return_to, expires_at)`
- `audit_log(id, at, actor, action, details)`, `details` as JSON
- `attendance_days`, primary key `(workspace_id, slack_user_id, day)`, with indexes on `(workspace_id, day)` and `(workspace_id, iso_week)`
- `office_closed_days`, primary key `(workspace_id, day)`
- `verified_days`, primary key `(workspace_id, day)`
- `monthly_stats`, primary key `(workspace_id, month)`

The existing cleanup job removes expired sessions and login states, and `attendance_days` older than `STATS_RETENTION_MONTHS` (except with `0`). `monthly_stats` is never deleted automatically.

### 8. Configuration

| Variable                 | Default | Purpose                                                                                 |
| ------------------------ | ------- | --------------------------------------------------------------------------------------- |
| `WEB_LISTEN_ADDR`        | empty   | e.g. `:8080`. Empty means the web interface is off.                                     |
| `WEB_BASE_URL`           | –       | public `https://` address. Required if the interface is enabled.                        |
| `SLACK_CLIENT_ID`        | –       | required if the interface is enabled                                                    |
| `SLACK_CLIENT_SECRET`    | –       | required if the interface is enabled. Belongs in `.env.local` and is masked in the log. |
| `WEB_SESSION_TTL`        | `12h`   | maximum lifetime of a session                                                           |
| `WEB_SESSION_IDLE`       | `2h`    | logout after this time without activity                                                 |
| `WEB_STATS_PER_PERSON`   | `false` | enables per-person statistics and export for admins                                     |
| `STATS_RETENTION_MONTHS` | `24`    | retention of `attendance_days`. `0` turns automatic deletion off.                       |
| `OFFICE_DESKS`           | empty   | number of desks available today, as a reference value in the statistics                 |

If the interface is enabled and `WEB_BASE_URL` lacks `https://` or the client secret is missing, the bot does not start.

### 9. Running behind Caddy

The bot itself speaks plain HTTP and knows nothing about TLS. For operation, the repository ships an optional **Caddy** as a reverse proxy. It is started via the Compose profile `web` (`docker compose --profile web up -d`). Without the profile, only the bot runs. Any other proxy can then reach `officebot:8080`.

#### Two modes

| `CADDY_TLS`      | When                                                | Certificate                                                                                                                                |
| ---------------- | --------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------ |
| `auto` (default) | ports 80 and 443 are reachable from outside         | Caddy obtains and renews it itself from Let's Encrypt, via HTTP-01 (port 80) or TLS-ALPN-01 (port 443).                                    |
| `files`          | another public port, internal access only, wildcard | An external service issues the certificate via DNS-01 and puts it in `CADDY_CERT_DIR`. Caddy only reads `fullchain.pem` and `privkey.pem`. |

For HTTP-01 and TLS-ALPN-01, Let's Encrypt always checks port 80 or 443. No other port can be specified for this. `auto` therefore works with other host ports only if a firewall or router forwards 80 or 443 from outside to them. In all other cases, use `files`.

The external DNS-01 service is deliberately **not part of this project**. The same task also comes up for other services, for example GitLab and wildcard certificates. It is therefore solved in a separate project. This project expects only two things from it: the two files in the directory and a call after every renewal.

#### Implementation

- Two Caddyfiles, embedded in `compose.yaml` as environment variables of the `caddy` service (`CADDYFILE_auto`, `CADDYFILE_files`). At start, the container writes the one selected by `CADDY_TLS` to `/tmp/Caddyfile` (tmpfs) and starts Caddy with it. A server thus still needs only `compose.yaml` and the env files, no clone of the repository. In `compose.yaml`, the placeholders are written as `{$$VAR}`, so Compose leaves them to Caddy.

  ```
  # Caddyfile.auto
  {
      email {$ACME_EMAIL}
  }
  {$OFFICEBOT_DOMAIN} {
      reverse_proxy officebot:8080
  }

  # Caddyfile.files
  {$OFFICEBOT_DOMAIN} {
      tls /certs/fullchain.pem /certs/privkey.pem
      reverse_proxy officebot:8080
  }
  ```

- The `caddy` service in `compose.yaml`:
  - image `caddy:2`, `container_name: officebot-caddy`, profile `web`, `restart: unless-stopped`
  - `ports`: `${PROXY_HTTP_PORT:-80}:80` and `${PROXY_HTTPS_PORT:-443}:443`
  - bind mounts at `./volumes/caddy/data` and `./volumes/caddy/config`, no Docker volumes, as with the bot. The certificates from `auto` live there and must be kept.
  - with `files`, additionally `${CADDY_CERT_DIR:-./volumes/certs}:/certs:ro`
  - hardened like the bot: `read_only`, `cap_drop: ALL` plus `cap_add: NET_BIND_SERVICE`, `no-new-privileges`
- The bot gets no host port, only `expose: ["8080"]`. Section 1 applies accordingly: the port is reachable only within the Compose network.
- **Reload after a renewal (`files`):** Caddy does not pick up changed certificate files by itself. The external service therefore runs `docker exec officebot-caddy caddy reload --config /tmp/Caddyfile --force` after every renewal. Without `--force`, Caddy would skip the reload because the configuration is unchanged.
- If Caddy runs on a public port other than 443, that port must be part of `WEB_BASE_URL` and of the redirect URL in the Slack app, e.g. `https://officebot.example.com:8443`.

#### Compose variables

These variables are read by Compose, not by the bot. They still live in `.env` and are documented in `dotenv.md`.

| Variable           | Default           | Purpose                                                              |
| ------------------ | ----------------- | -------------------------------------------------------------------- |
| `OFFICEBOT_DOMAIN` | –                 | host name of the interface, e.g. `officebot.example.com`             |
| `CADDY_TLS`        | `auto`            | `auto` or `files`                                                    |
| `ACME_EMAIL`       | –                 | optional contact address for the Let's Encrypt account (`auto` only) |
| `PROXY_HTTP_PORT`  | `80`              | host port for HTTP                                                   |
| `PROXY_HTTPS_PORT` | `443`             | host port for HTTPS                                                  |
| `CADDY_CERT_DIR`   | `./volumes/certs` | directory with `fullchain.pem` and `privkey.pem` (`files` only)      |

### 10. Privacy and co-determination

- Evaluating attendance per person is processing of personal data. In Germany it can count as a technical means of monitoring performance and behavior (§ 87 (1) no. 6 BetrVG). The per-person view is therefore **off by default**. It appears only with `WEB_STATS_PER_PERSON=true` and only for admins.
- The case for shared desks relies on the statistics **without names** (section 6). They are sufficient for it.
- Every access to the per-person view and every per-person export is written to the audit log.
- Per-person data has a fixed, documented retention period (`STATS_RETENTION_MONTHS`). Only figures without personal data are kept indefinitely. This is easier to justify to the works council and the data protection officer than unlimited storage (storage limitation, Art. 5 (1)(e) GDPR).
- After someone leaves or requests erasure, `stats purge --user` removes the person's data.
- The README gets a section on privacy of statistics under "Known limitations": inform the team, involve the works council and the data protection officer, state the retention periods.

### 11. Implementation steps

Each step is its own commit:

1. **Foundation:** migration, storage for `web_users` (including the last-admin protection), audit log, configuration, CLI `web-user`.
2. **Server and login:** HTTP server in the `run` loop, middleware (session, role, CSRF, headers), OIDC login and logout, layout, `/access`.
3. **Admin pages:** overview, week, time off, languages, closed days, audit log.
4. **Data basis:** `attendance_days` with backfill from existing weeks, recalculation on corrections, `verified_days` with the day check `/days/{date}` and the CLI `attendance verify`, `monthly_stats` with anonymous distributions, automatic deletion, `stats purge` in the CLI and at `/retention`.
5. **Statistics:** package `internal/stats`, pages, SVG charts, CSV export, methodology.
6. **Operations and docs:** Caddy service and Caddyfiles (section 9), README de/en, `dotenv.md` de/en, `slack-app-setup.md` de/en, update the status of this ADR.

**Tests:**

- login against a fake Slack server (`httptest`): wrong team, wrong `state` or `nonce`, unknown person, access revoked during an active session
- roles and CSRF
- last-admin protection in the CLI and web
- rules for the daily status, recalculation after `attendance`, the `corrected` flag
- checked days: confirming via web and CLI, the filter "checked days only", the share of checked days
- `stats purge`: preview without `--yes`, `monthly_stats` written before deleting, statistics without names unchanged afterwards, `--user` also deletes responses and time off, `STATS_RETENTION_MONTHS=0`
- periods without office across week and month boundaries and across closed days
- percentiles and distributions with fixed test data

### Implementation notes

Where the implementation differs from or refines the plan above:

- **Recalculation via triggers:** instead of every change triggering a recalculation itself, database triggers mark a closed week as changed (`polls.stats_dirty`) as soon as its summary version or the time off of a participant changes. The scheduler and the web interface recalculate marked weeks. This covers every path, including the CLI.
- **Backfill:** the migration marks all closed weeks; the scheduler's first run calculates them. The result is the same as backfilling in the migration itself.
- **Keeping the plan:** before a closed week is corrected (`attendance`, day check, `away remove --id`), the bot calculates its daily status if that has not happened yet. A correction never ends up as the plan in `attendance_days`.
- **Day check and past time off:** "Away" in the day check enters time off for that one day, also in the past. "No answer" cannot be set there. Someone without an answer gets one for the whole week with the first correction.
- **Monthly figures:** `monthly_stats` stores a month's figures as JSON, including the histogram of daily occupancy. Percentiles and the desk curve can thus be combined exactly for any period. Months from which daily data was deleted are frozen (`frozen`) and no longer recalculated; the statistics always use the stored figures for them.
- **Retention in whole months:** `STATS_RETENTION_MONTHS` always deletes whole months, so no month has only part of its daily data.
- **Interface language:** the web interface has its own small catalog for English and German; otherwise all 45 bot catalogs would have to grow by the interface texts. English is the default; an **EN | DE** switch in the header (also on the login page) sets a cookie (`officebot_lang`, one year). The bot language of a person no longer plays a role (added on 2026-11-05).
- **Third role `analyst`** (added on 2026-11-05): sees everything a viewer sees plus the statistics and export per person (only with `WEB_STATS_PER_PERSON=true`), but changes nothing and has no access to the administration or the audit log. Intended for team leads, HR or the works council. Migration `0008` rebuilds `web_users`, because SQLite cannot change the CHECK constraint on the role.
- **Development login:** with `DRY_RUN=true` and a `WEB_BASE_URL` on localhost, people with access can log in without Slack. This allows trying the interface locally.
- **Charts:** the SVG charts show exact values as native tooltips (`<title>`), and the numbers are in tables next to them.
- **Caddy as root in the container:** Caddy runs as root inside the container so it can bind ports 80 and 443, but without any other capability (`cap_drop: ALL`, only `NET_BIND_SERVICE`). With `no-new-privileges`, a regular user would not get the file capability of the Caddy binary. The files in `./volumes/caddy/` are therefore owned by root.
- **Caddyfile from an environment variable:** a bind-mounted Caddyfile needs the file on the server; without a clone, Docker creates an empty directory in its place and Caddy does not start. Compose `configs` with inline `content` cannot be written into a read-only container and failed on the production server with `mkdirat etc/caddy/Caddyfile: file exists`. The environment variable works with every Compose version and keeps the container read-only.
- **Additionally:** `/weeks` for correcting whole weeks, a per-person export (`/stats/persons.csv`, analysts and admins with `WEB_STATS_PER_PERSON` only) and logins in the audit log (`login`, `login.denied`).

## Alternatives considered

- **Login link via Slack DM:** needs no change to the Slack app and no public HTTPS address. But people have to request a new link every time the session expires. Rejected in favor of the familiar login flow.
- **Reverse proxy with company SSO** (oauth2-proxy, Authelia): needs additional infrastructure, and the bot would have to trust headers. Permissions would live outside the bot and could not be managed in the interface.
- **Calculate statistics from `responses` only:** simpler, but limited to half a year by `RETENTION_WEEKS`. It also could not tell whether someone was on vacation or did not answer. Raising `RETENTION_WEEKS` substantially instead would keep all Slack references and DM data longer than necessary.
- **Keep per-person data indefinitely:** technically no problem, the amount of data is small. But the purpose, planning office desks, does not need daily per-person data over years. Long-term trends come from the figures without names. The data of former colleagues would also stay stored forever, and a works agreement usually requires a fixed period. Anyone who still wants to delete only manually sets `STATS_RETENTION_MONTHS=0`.
- **Statistics in the Slack App Home:** needs no web server. But Slack blocks are poorly suited for charts, periods and exports.
- **JavaScript chart library:** nicer interaction, but more dependencies and a looser CSP. Server-side SVG is enough for the charts needed.
- **TLS directly in the bot** (e.g. `autocert`): the bot would need ports 80 or 443 itself and would have to manage certificates. A proxy in front is easier to replace and keeps TLS out of the bot.
- **Traefik instead of Caddy:** Traefik supports DNS-01 with AutoDNS directly via lego. For the standard case with ports 80 and 443, however, Caddy is much simpler to configure. DNS-01 is handled by a separate service that other servers need as well (section 9).
- **Caddy with a custom AutoDNS DNS module:** there is no official module, only the library `libdns/autodns`. A custom module and a custom Caddy image would have to be maintained permanently.

## Consequences

- Administration is possible without server access. The CLI stays fully available.
- The interface needs an HTTPS address, a reverse proxy and an updated Slack app. With the profile `web` and free ports 80 and 443, the repository brings the proxy and certificate itself. Without `WEB_LISTEN_ADDR` nothing changes.
- Other ports, internal-only access or wildcard certificates require the external certificate service.
- Statistics without names, including the distributions for the shared-desk question, go back indefinitely. Per-person statistics and the filter "checked days only" go back as far as daily data exists. At the start, the history only goes back as far as weeks remain in the database.
- The figures are only as good as the response rate, the share of checked days and the maintenance of closed days. The interface shows all three openly.
- The day check turns the reconciliation admins do today per person and week on the CLI into one click per day.
- A daily status per person is additionally stored for up to 24 months. This must be agreed with the team, the works council and the data protection officer before rollout.
