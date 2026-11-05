# Web interface and statistics

The bot can optionally serve a web interface for administration and statistics. People log in with their Slack account ("Sign in with Slack"). Without `WEB_LISTEN_ADDR` the bot behaves exactly as before. Background and decisions: [ADR 0003](adr/0003-web-interface-and-statistics.md). 🇩🇪 Deutsch: [../de/weboberflaeche.md](../de/weboberflaeche.md)

## Contents

1. [What it offers](#what-it-offers)
2. [Setup](#setup)
3. [Access and roles](#access-and-roles)
4. [Day check: actual attendance](#day-check-actual-attendance)
5. [Statistics](#statistics)
6. [Retention and deletion](#retention-and-deletion)
7. [Security](#security)
8. [Privacy and co-determination](#privacy-and-co-determination)

---

## What it offers

| Page        | Role  | Content                                                                                            |
| ----------- | ----- | -------------------------------------------------------------------------------------------------- |
| Overview    | all   | Running polls: who answered, delivery status, outbox (like `officebot status`)                     |
| Statistics  | all   | Office occupancy, desks needed, distributions; per person for analysts and admins only (see below) |
| Day check   | admin | Reconcile the plan with actual attendance per day and mark the day as checked                      |
| Weeks       | admin | Correct responses of a week (optionally with a thread notice), re-render DMs and summary           |
| Time off    | admin | Enter and delete time off (like `officebot away`)                                                  |
| Closed days | admin | Public holidays and company holidays; they count in no statistic                                   |
| Languages   | admin | A person's language for DMs (like `officebot lang`)                                                |
| Access      | admin | Who may log in and with which role                                                                 |
| Retention   | admin | Retention periods and deleting daily data per year, cut-off date or person                         |
| Audit log   | admin | Every change with who made it and when                                                             |

The interface speaks English and German. English is the default; the **EN | DE** switch at the top right (also on the login page) changes the language and is remembered in the browser for a year. All changes use the same rules as the CLI and reach Slack through the running bot within 30 seconds.

## Setup

### 1. Extend the Slack app

In the app settings at <https://api.slack.com/apps>:

1. **OAuth & Permissions** → **Redirect URLs**: add `https://officebot.example.com/auth/callback` (your `WEB_BASE_URL` plus `/auth/callback`).
2. **OAuth & Permissions** → **User Token Scopes**: add `openid` and `profile`.
3. **Basic Information** → **App Credentials**: copy the **Client ID** and **Client Secret**.

The manifest equivalent and a step-by-step guide are in [Creating the Slack app, step 9](slack-app-setup.md#step-9-optional-sign-in-with-slack-for-the-web-interface). The app does not need to be reinstalled for this; the bot token stays the same.

### 2. Configure

In `.env` (complete templates: [dotenv.md](dotenv.md)):

```dotenv
WEB_LISTEN_ADDR=:8080
WEB_BASE_URL=https://officebot.example.com
# For the Caddy reverse proxy (profile "web")
OFFICEBOT_DOMAIN=officebot.example.com
ACME_EMAIL=it@example.com
```

In `.env.local` (secret):

```dotenv
SLACK_CLIENT_ID=1234567890.1234567890
SLACK_CLIENT_SECRET=...
```

| Variable                 | Default | Meaning                                                                                                              |
| ------------------------ | ------- | -------------------------------------------------------------------------------------------------------------------- |
| `WEB_LISTEN_ADDR`        | empty   | Address of the web server in the container, e.g. `:8080`. Empty = no web interface.                                  |
| `WEB_BASE_URL`           | –       | Public address, scheme and host only, e.g. `https://officebot.example.com`. `http://` is only allowed for localhost. |
| `SLACK_CLIENT_ID`        | –       | Client ID of the Slack app                                                                                           |
| `SLACK_CLIENT_SECRET`    | –       | Client secret of the Slack app (`.env.local`); masked in the log                                                     |
| `WEB_SESSION_TTL`        | `12h`   | Maximum lifetime of a login                                                                                          |
| `WEB_SESSION_IDLE`       | `2h`    | Logout after this time without activity                                                                              |
| `WEB_STATS_PER_PERSON`   | `false` | `true`: analysts and admins see and export statistics per person                                                     |
| `STATS_RETENTION_MONTHS` | `24`    | Retention of the daily status per person; `0` = no automatic deletion                                                |
| `OFFICE_DESKS`           | empty   | Number of desks available today; shown as a reference in the statistics                                              |

If the web interface is enabled and `WEB_BASE_URL` or the client credentials are missing or invalid, the bot does not start and says why.

### 3. Reverse proxy and certificate

The bot itself speaks plain HTTP. `compose.yaml` contains an optional **Caddy** that terminates TLS. It starts only with the profile `web`:

```bash
docker compose --profile web up -d
```

| `CADDY_TLS`      | When                                                | Certificate                                                                                               |
| ---------------- | --------------------------------------------------- | --------------------------------------------------------------------------------------------------------- |
| `auto` (default) | Ports 80 and 443 are reachable from the internet    | Caddy obtains and renews it from Let's Encrypt (HTTP-01 on port 80 or TLS-ALPN-01 on port 443)            |
| `files`          | Another public port, internal-only access, wildcard | An external service issues it via DNS-01 and writes `fullchain.pem` and `privkey.pem` to `CADDY_CERT_DIR` |

More Compose variables (in `.env`):

| Variable           | Default           | Meaning                                                              |
| ------------------ | ----------------- | -------------------------------------------------------------------- |
| `OFFICEBOT_DOMAIN` | –                 | Host name of the interface; Caddy fails to start without it          |
| `CADDY_TLS`        | `auto`            | `auto` or `files` (selects the Caddyfile embedded in `compose.yaml`) |
| `ACME_EMAIL`       | empty             | Optional contact address for the Let's Encrypt account (`auto`)      |
| `PROXY_HTTP_PORT`  | `80`              | Host port for HTTP                                                   |
| `PROXY_HTTPS_PORT` | `443`             | Host port for HTTPS                                                  |
| `CADDY_CERT_DIR`   | `./volumes/certs` | Directory with `fullchain.pem` and `privkey.pem` (`files`)           |

Notes:

- Let's Encrypt always checks port 80 or 443. `auto` works with other host ports only if a firewall or router forwards 80/443 from outside to them.
- With a public port other than 443, that port belongs in `WEB_BASE_URL` and in the redirect URL of the Slack app, e.g. `https://officebot.example.com:8443`.
- With `files`, the external service must reload Caddy after every renewal, because Caddy does not pick up changed certificate files by itself:
  ```bash
  docker exec officebot-caddy caddy reload --config /tmp/Caddyfile --force
  ```
- Caddy's certificates (`auto`) live in `./volumes/caddy/` and must be kept.
- Another reverse proxy instead of Caddy: leave out the profile and forward to `officebot:8080` in the Compose network.

### 4. Grant the first access

Nobody can log in until the first admin has been added on the console:

```bash
docker compose exec officebot /officebot web-user add --user U0123ABC          # admin
docker compose exec officebot /officebot web-user add --user U0456DEF --role viewer
docker compose exec officebot /officebot web-user list
```

Afterwards, admins can also manage access on the **Access** page.

### Development without Slack

With `DRY_RUN=true` and `WEB_BASE_URL=http://localhost:8080`, the login page offers a development login for every person with web access, without Slack. It is not available in any other configuration.

## Access and roles

| Role      | May                                                                                            |
| --------- | ---------------------------------------------------------------------------------------------- |
| `admin`   | Everything: administration, corrections, access, retention, statistics per person (if enabled) |
| `analyst` | Everything a viewer may, plus statistics and export per person (if enabled); changes nothing   |
| `viewer`  | Overview and the aggregated statistics                                                         |

CLI:

| Command                                                    | Effect                                     |
| ---------------------------------------------------------- | ------------------------------------------ |
| `web-user list`                                            | Everybody with access, role, last login    |
| `web-user add --user U [--role admin\|analyst\|viewer]`    | Grant access (default: admin)              |
| `web-user set-role --user U --role admin\|analyst\|viewer` | Change the role                            |
| `web-user remove --user U`                                 | Revoke access and end all sessions at once |

The last admin can be neither removed nor downgraded, on the CLI or in the web interface. Role changes and revoked access apply at once, also to running sessions. Every change is written to the audit log.

## Day check: actual attendance

The poll records **planned** attendance. On the **Day check** page an admin reconciles a day with actual attendance:

- Every person asked on that day is listed as office, remote, away or no answer.
- Changing a status corrects the response (office/remote) or enters time off for that day (away). Past days are allowed here.
- Someone without an answer gets one for the whole week with the first correction; their other days then count as remote.
- **Mark the day as checked** confirms that the status of everybody on that day is correct. On the CLI: `officebot attendance verify --date 2026-11-04` (`--undo` removes the mark).

The status at the deadline is kept as the plan. Every later change counts as a correction; the share of corrections on checked days shows how reliable the unchecked days are.

## Statistics

The bot keeps a daily status per person and work day of every closed poll (table `attendance_days`): office if the day is checked in the response, else away if time off covers it, else remote if the person answered, else no answer. Open weeks appear only after their deadline.

The **Statistics** page shows for a range of months:

- **Reliability:** share of checked days, response rate (warning below 90 %), deviation from the plan. Unchecked days reflect planned attendance. The filter **Checked days only** restricts everything to actual attendance.
- **Occupancy:** work days, average, median, P90, P95 and maximum of people in the office; how many desks would have been enough on 95 %, 90 % and all days, compared with `OFFICE_DESKS`.
- **Charts:** people in the office per day, average per weekday, share of days covered by *n* desks, office days per person and month, longest time without office per person. The last two are anonymous distributions.
- **Per month:** work days, office person-days, average, maximum, response rate, checked days.
- **Per person** (analysts and admins, only with `WEB_STATS_PER_PERSON=true`): office days and rate, remote, away, no answer, last office day, current and longest period without office with its breakdown by reason.
- **Methodology:** a definition of every figure.

Closed days (public holidays, company holidays) count in no figure. A period without office ignores closed days and days on which the person was not asked.

Exports: **CSV without names** (`/stats.csv`, monthly figures) for everybody with access; **CSV per person** (`/stats/persons.csv`) for analysts and admins with `WEB_STATS_PER_PERSON=true`.

## Retention and deletion

| Data                                            | Kept                                                                            |
| ----------------------------------------------- | ------------------------------------------------------------------------------- |
| Polls, responses, time off, Slack refs          | `RETENTION_WEEKS`                                                               |
| Daily status per person (`attendance_days`)     | `STATS_RETENTION_MONTHS` (default 24), whole months; `0` = only manual deletion |
| Monthly figures without names (`monthly_stats`) | Indefinitely                                                                    |

Before daily data is deleted, the monthly figures of the affected months are saved and frozen, so the statistics without names stay complete. Such months are marked in the statistics.

Manual deletion, with a preview on the **Retention** page or on the CLI:

```bash
officebot stats purge --year 2024              # shows what would be deleted
officebot stats purge --year 2024 --yes        # deletes
officebot stats purge --before 2025-01-01 --yes
officebot stats purge --user U0123ABC --yes    # one person, e.g. after leaving
```

`--user` also deletes the person's participation in stored polls (with responses), their time off and language choice, so that the daily data cannot be recalculated.

A high `RETENTION_WEEKS` pays off mainly once: when the web interface is introduced, the daily status is calculated from all weeks still in the database.

## Security

- Login with Slack (OpenID Connect): single-use `state` and `nonce`, checks of issuer, audience, expiry, nonce and workspace.
- Only people listed in the access management get a session. Sessions are stored as hashes; the cookie is `HttpOnly`, `Secure`, `SameSite=Lax` (`__Host-` prefix with https).
- CSRF token and same-origin check on every change; strict Content Security Policy without JavaScript; rate limit on the login.
- The bot's port is reachable only within the Compose network.

## Privacy and co-determination

Evaluating attendance per person is processing of personal data and can count as a technical means of monitoring performance and behavior (§ 87 (1) no. 6 BetrVG). Therefore:

- Statistics per person are **off by default** (`WEB_STATS_PER_PERSON`) and visible to analysts and admins only. Every view and export is audited.
- The case for shared desks can be made with the statistics without names.
- Daily data per person has a fixed retention period; only figures without names are kept indefinitely.
- Inform the team and involve the works council and data protection officer before introducing it.
