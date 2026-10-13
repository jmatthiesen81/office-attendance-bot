# ADR 0001: Configurable work days

- **Status:** Proposed (planned for later, not implemented)
- **Date:** 2026-10-13

## Context

The bot assumes a fixed Monday-to-Friday work week:

- Answers are stored in fixed columns `monday` … `friday` (`internal/storage/migrations/0001_init.sql`, `domain.Days` with `NumDays = 5`).
- A week is identified by ISO year and ISO week number. `WeekStart` is Monday, `WeekEnd` is Friday.
- Checkboxes, the summary, the date range and reminders are all built around Mon–Fri.

This works for teams in Germany and the US, where people also work Monday to Friday. Sunday only comes first in the US calendar. It does not cover:

- work weeks from Sunday to Thursday (e.g. Israel, Saudi Arabia),
- offices used only on some days (e.g. Tue–Thu),
- four-day weeks.

The polled week follows from `POLL_WEEKDAY`: a poll started on Monday is for the current week and a poll started on any other day is for the next week. The rule assumes Monday as the first day of the work week.

## Decision (planned)

Work days become configurable as a list, not just as a start day:

```dotenv
WORK_DAYS=MONDAY,TUESDAY,WEDNESDAY,THURSDAY,FRIDAY   # default
# WORK_DAYS=SUNDAY,MONDAY,TUESDAY,WEDNESDAY,THURSDAY
# WORK_DAYS=TUESDAY,WEDNESDAY,THURSDAY
```

- The **first day in the list** starts the work week. This generalises today's rule: a poll started on the first work day is for the current week, a poll started on any other day is for the next week starting on that day.
- The list must be given in week order from the first day. It must not be empty or contain duplicates.
- Without `WORK_DAYS`, the bot behaves exactly as it does today.

### Required changes

1. **Week identity:** a week is identified by the date of its first work day (`week_start` as a local date) instead of ISO year/week. This is the only way to make weeks starting on Sunday, and weeks across the turn of the year, unambiguous. The ISO week number stays available for display where it makes sense.
2. **Schema:** the columns `monday` … `friday` are replaced by a weekday bitmask (`days INTEGER`, bit 0 = Sunday … bit 6 = Saturday). A migration converts existing answers without loss. The bitmask is keyed by weekday, not by position in the list, so that changing `WORK_DAYS` later does not shift old answers.
3. **Domain:** `domain.Days` becomes a set of `time.Weekday`. `NumDays`, `DayKeys` and `DayWeekdays` are derived from the configuration.
4. **Rendering:** checkboxes, summary sections, the date range (first to last work day) and short lists use the configured days. Weekday names for all seven days already exist in the translation files.
5. **Deadline and reminders:** `POLL_DEADLINE_WEEKDAY` and `REMINDER_WEEKDAY` refer to the week starting on its first work day instead of Monday. The "deadline after start" check stays.
6. **Time off:** "away all week" (`domain.AwayAllWeek`) currently checks Monday to Friday of the polled week. In future it checks the configured work days. Stored time off (calendar days from–to) stays unchanged.
7. **Interactions:** checkbox values remain stable keys (`mon` … `sun`). Values not in `WORK_DAYS` are rejected, as they are today.

### Relation to internationalisation

Slack texts are already translatable (`BOT_LANGUAGE`, `internal/i18n/locales/*.json`). For teams outside Germany, the following would come on top:

- **Week label:** ISO week numbers are unusual in many countries. The translation should be able to choose between "KW 43" and "Week of Oct 19".
- **Formats:** date and time layouts already exist per language. In the long run, a locale (e.g. `en-US` vs. `en-GB`) could separate language from format.

These points are independent of `WORK_DAYS` and can be implemented separately.

## Alternatives considered

- **Keep Mon–Fri only:** no work, but teams with a different work week cannot use the bot.
- **Configure only a start day (`WORK_WEEK_START`):** simpler, but it fixes five consecutive days. Offices with Tue–Thu or four-day weeks are not covered. The list covers the start day as well.
- **Store positions instead of weekdays (index 0 … n):** simpler schema, but changing `WORK_DAYS` would assign existing answers to the wrong days.

## Consequences

- A larger change across schema, domain, rendering and scheduler, including a data migration. It should only be tackled once there are actual users with a different work week.
- The week rule introduced now (first work day = current week, otherwise next week) and the translation files are designed so they can be extended without breaking changes.
- Existing installations without `WORK_DAYS` see no difference after the migration.
