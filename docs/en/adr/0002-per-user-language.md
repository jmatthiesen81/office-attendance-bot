# ADR 0002: Per-person language for private messages

- **Status:** Proposed (planned for later, not implemented)
- **Date:** 2026-10-13

## Context

Today the bot speaks a single language, set via `BOT_LANGUAGE` (`internal/config/config.go`, `cfg.Translator`). It applies to everything:

- the poll DM, including status, hints and buttons (`slack.DMView.T`),
- reminders,
- ephemeral replies to clicks (`Service.RejectInteraction` and the `reply.*` texts),
- the time-off form (`slack.TimeOffModal`),
- the channel summary (`summary.Renderer.T`).

With 14 languages plus nerd and pirate variants (`internal/i18n/locales/`), mixed teams will likely not all want to read the same language. The channel summary is the same for everyone and has to stay in one language. Everything only one person sees, however, can be in that person's language.

The translation layer is already prepared for this. DM blocks, the form and date formats receive the `Translator` as a parameter and do not use a global language.

## Decision (planned)

Each person can choose the language the bot uses for their private messages. The channel summary stays in `BOT_LANGUAGE`.

### Which language applies

The language is determined per person in this order:

1. **The person's own choice**, stored in the database.
2. **The person's Slack language** (`locale` from `users.info`), but only if `BOT_LANGUAGE_FROM_SLACK=true` is set and a catalog exists for it. `de-DE` maps to `de`, `pt-BR` to `pt`, and so on.
3. **`BOT_LANGUAGE`** as the team default.

Step 2 is off by default. Otherwise, a German team whose members use Slack in English would suddenly get English messages after an update. Slack also does not offer many of our languages, such as Danish, Polish, Czech, Turkish and Ukrainian, and of course not nerd or pirate. So an explicit choice is needed in any case.

### Slack UI

- The poll DM gets a **"🌐 Language"** button in its actions area, next to "🌴 Plan time off".
- It opens a modal with a select menu (`static_select` with option groups):
  - "Team default (English)", to clear the person's choice,
  - the languages,
  - the nerd variants,
  - the pirate variants.
- Each language is listed in its own language: "Deutsch", "Français", "Українська", "Nerd (Deutsch)". For this, every catalog gets a new `language.name` key. With 42 entries, the list stays below Slack's limit of 100 options.
- After saving, the bot immediately re-renders the current DM of every open poll in the new language, as an `update_dm` job just like for time off. If the time-off form is open at that moment, it stays in the old language until it is opened again.
- The same button also appears in the time-off form and below the channel summary, next to "📩 Send me the poll". That way, people without a current DM can reach the setting too.

### Operations (CLI)

```sh
officebot lang list                  # all stored language choices
officebot lang set --user U123 de    # set for one person
officebot lang reset --user U123     # back to the team default
```

Like `away`, the CLI only writes to the database and enqueues `update_dm` jobs. The running bot does the sending.

### Required changes

1. **Schema:** migration `0004_user_settings.sql` with the table `user_settings(workspace_id, slack_user_id, language, updated_at)`, primary key `(workspace_id, slack_user_id)`. The table name is deliberately generic so that later personal settings fit there too.
2. **Storage:** `GetUserLanguage`, `SetUserLanguage` and `ResetUserLanguage`. For sending reminders to many people, `UserLanguages(ctx, ws, userIDs)` is added.
3. **Service:** `s.tr()` is complemented by `s.trFor(ctx, userID)`, which implements the order above. Every place that addresses a single person uses `trFor`: `sendDM`, `RefreshDM`, `sendReminder`, `RejectInteraction`, `HandleRequestDM`, the time-off handlers and `timeOffModal`. The summary keeps using `cfg.Translator`.
4. **Slack locale:** with step 2, `locale` is read along with the participants when they are resolved, and stored with the participant. The call needs `users.info` with `include_locale=true`; the existing `users:read` scope is enough. Check before implementing whether slack-go sets that parameter.
5. **Socket:** the error message for a form timeout (`slack.Socket.tr`) also needs the person's language. For this, `InteractionHandler` gets a method that returns the translator for a user ID.
6. **Robustness:** if a stored language no longer exists after an update, the next step of the order applies and the bot logs a warning. The choice itself stays stored, so it applies again after a rollback.
7. **Interactions:** new action `language_open` (button) and callback `language` (modal submit). The submit is validated synchronously, as with the time-off form. A value without a catalog is rejected.

### Out of scope

- **Time zone and formats:** times are still shown in `POLL_TIMEZONE`. Date and time formats come from the catalog of the chosen language. A per-person time zone (`tz` from Slack) would be a separate step.
- **Summary:** it stays in one language. Multilingual summaries, or one summary per language, would clutter the channel.
- **Privacy:** only one language code per person is stored. The cleanup job (`Service.Cleanup`) deletes entries of people who have not taken part in any poll for `RETENTION_WEEKS`. The privacy notes in the README are extended accordingly.

## Alternatives considered

- **Use only the Slack language:** no UI needed, but many languages and the fun variants cannot be reached this way. Also, the Slack language often differs from the language people want from the bot.
- **Mapping via an environment variable** (`BOT_USER_LANGUAGES=U123:de,U456:fr`): easy to build, but no self-service. Every change needs a restart by operations.
- **Slash command** (`/officebot-language fr`): convenient for experienced users, but it needs the `commands` scope and a manifest change. People who don't know the command can hardly find it. It can be added later.
- **App Home tab with settings:** clear and extensible. But it needs `home_tab_enabled`, the `app_home_opened` event and its own view. It pays off once there are several personal settings. The `user_settings` table is already prepared for that.

## Consequences

- Everyone reads DMs, reminders and replies in their own language. The summary stays the same for everyone.
- New languages also need the `language.name` key. The existing completeness test enforces this automatically.
- Without an explicit choice and with `BOT_LANGUAGE_FROM_SLACK=false`, the bot behaves exactly as it does today.
- Tests must cover mixed languages: two people with different languages in the same poll, a language change while a DM is open, an unknown stored language, and the CLI.
