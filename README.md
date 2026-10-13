# Office Attendance Bot

A small, self-contained Slack app for weekly office attendance planning.

- Every week (default **Monday 07:00 Europe/Berlin**) each member of the target group gets a private message to tick the days they will be in the office and click **Speichern** (*Save*).
- Answers can be changed until the deadline (default **Friday 08:00** of the polled week).
- The results channel holds **exactly one summary message per week**, updated in place on every change.
- Optional reminders go to everyone who has not answered yet.
- Messages in English (default), German, French, Spanish, Italian, Portuguese, Dutch, Danish, Swedish, Norwegian, Polish, Czech, Turkish or Ukrainian – each also, for fun, in nerd and pirate speak, e.g. `nerd_de`, `pirate_en` (`BOT_LANGUAGE`).
- Missed or lost the DM? A button below the summary sends it again, and people who joined the team after the weekly start can add themselves.
- On vacation? People mark time off in Slack (or an admin via the CLI). Whoever is away all week gets no poll or reminders and is listed as away in the summary.

Go · Slack Socket Mode (no public endpoint) · SQLite · one Docker container · no paid services.

## Documentation

Full documentation (architecture, Slack app setup, all configuration options, CLI, backup, troubleshooting, limitations):

- 🇬🇧 English: [docs/en/README.md](docs/en/README.md) · [Creating the Slack app](docs/en/slack-app-setup.md)
- 🇩🇪 Deutsch: [docs/de/README.md](docs/de/README.md) · [Slack-App anlegen](docs/de/slack-app-setup.md)

## Quick start

1. **Create the Slack app**: follow the step-by-step guide [docs/en/slack-app-setup.md](docs/en/slack-app-setup.md) (🇩🇪 [docs/de/slack-app-setup.md](docs/de/slack-app-setup.md)). It takes about 10 minutes and can be handed to a workspace admin.
2. **Configure.** Shared defaults (schedule, deadline, reminders, container UID/GID) live in the committed `.env`. Tokens, channel IDs and participants go into `.env.local`, which is ignored by git and overrides `.env`:

   ```bash
   cp .env.local.example .env.local   # then fill in tokens, channel and participants
   ```

   All options are described under [Configuration](docs/en/README.md#configuration).

3. **Start:**

   ```bash
   docker compose up -d --build
   docker compose logs -f officebot
   ```

   Data is stored in `./volumes/officebot/` (bind mount, no Docker volumes).

To try it without Slack, use the [dry run](docs/en/README.md#test-run).

## Common commands

```bash
docker compose exec officebot /officebot status       # polls, answers, delivery state
docker compose exec officebot /officebot start-week   # start this week's poll now (idempotent)
docker compose exec officebot /officebot resync       # rewrite the channel summary
make backup                                           # consistent database backup into ./backup
make test                                             # go test -race (runs in Docker, no local Go needed)
```

See [CLI commands](docs/en/README.md#cli-commands) for the full list.
