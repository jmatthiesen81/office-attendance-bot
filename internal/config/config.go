// Package config loads and validates the configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"officebot/internal/i18n"
	"officebot/internal/isoweek"
)

// TimeOfDay is a wall-clock time.
type TimeOfDay struct {
	Hour, Minute int
}

func (t TimeOfDay) String() string { return fmt.Sprintf("%02d:%02d", t.Hour, t.Minute) }

// Config holds the complete application configuration.
type Config struct {
	SlackBotToken string
	SlackAppToken string

	SummaryChannelID      string
	ParticipantsChannelID string
	UserIDs               []string
	ExcludeUserIDs        []string

	PollWeekday     time.Weekday
	PollTime        TimeOfDay
	Location        *time.Location
	DeadlineWeekday time.Weekday
	DeadlineTime    TimeOfDay

	ReminderEnabled  bool
	ReminderWeekdays []time.Weekday
	ReminderTime     TimeOfDay
	ReminderMaxCount int

	DatabasePath    string
	HealthFile      string
	LogLevel        slog.Level
	DryRun          bool
	RetentionWeeks  int
	SummaryMaxNames int
	// SummaryProjectLink shows a link to the project repository below the
	// summary footer.
	SummaryProjectLink bool
	// SummaryChangeNotices announces changed responses as thread replies
	// under the summary. Changes within SummaryChangeNoticeGrace after a
	// person's first answer count as part of it and are not announced.
	// Changes of one person within SummaryChangeNoticeDelay after the first
	// one are combined into one reply.
	SummaryChangeNotices     bool
	SummaryChangeNoticeDelay time.Duration
	SummaryChangeNoticeGrace time.Duration

	// Translator renders all texts sent to Slack in the configured language.
	// It is the team default: people can choose their own language for
	// everything only they see (ADR 0002).
	Translator *i18n.Translator
	// LanguageFromSlack uses a person's Slack language when they have not
	// chosen one and a catalog exists for it.
	LanguageFromSlack bool
}

// Load reads the configuration from the process environment.
func Load() (*Config, error) { return LoadFrom(os.Getenv) }

// LoadForSetup reads the configuration like Load, but does not require the
// summary channel and target group. Setup tools such as list-users use it,
// because they help to find exactly these values.
func LoadForSetup() (*Config, error) { return load(os.Getenv, false) }

// LoadFrom reads the configuration using the given lookup function.
func LoadFrom(get func(string) string) (*Config, error) { return load(get, true) }

func load(get func(string) string, requireTargets bool) (*Config, error) {
	p := parser{get: get, requireTargets: requireTargets}
	c := &Config{
		SlackBotToken:            p.str("SLACK_BOT_TOKEN", ""),
		SlackAppToken:            p.str("SLACK_APP_TOKEN", ""),
		SummaryChannelID:         p.str("SLACK_SUMMARY_CHANNEL_ID", ""),
		ParticipantsChannelID:    p.str("SLACK_PARTICIPANTS_CHANNEL_ID", ""),
		UserIDs:                  p.list("SLACK_USER_IDS"),
		ExcludeUserIDs:           p.list("SLACK_EXCLUDE_USER_IDS"),
		PollWeekday:              p.weekday("POLL_WEEKDAY", "MONDAY"),
		PollTime:                 p.timeOfDay("POLL_TIME", "07:00"),
		Location:                 p.location("POLL_TIMEZONE", "Europe/Berlin"),
		DeadlineWeekday:          p.weekday("POLL_DEADLINE_WEEKDAY", "FRIDAY"),
		DeadlineTime:             p.timeOfDay("POLL_DEADLINE_TIME", "08:00"),
		ReminderEnabled:          p.boolean("REMINDER_ENABLED", true),
		ReminderWeekdays:         p.weekdays("REMINDER_WEEKDAY", "TUESDAY"),
		ReminderTime:             p.timeOfDay("REMINDER_TIME", "09:00"),
		ReminderMaxCount:         p.integer("REMINDER_MAX_COUNT", 1),
		DatabasePath:             p.str("DATABASE_PATH", "/data/officebot.db"),
		HealthFile:               p.str("HEALTH_FILE", "/tmp/officebot.health"),
		LogLevel:                 p.logLevel("LOG_LEVEL", "info"),
		DryRun:                   p.boolean("DRY_RUN", false),
		RetentionWeeks:           p.integer("RETENTION_WEEKS", 26),
		SummaryMaxNames:          p.integer("SUMMARY_MAX_NAMES", 40),
		SummaryProjectLink:       p.boolean("SUMMARY_PROJECT_LINK", true),
		SummaryChangeNotices:     p.boolean("SUMMARY_CHANGE_NOTICES", true),
		SummaryChangeNoticeDelay: p.duration("SUMMARY_CHANGE_NOTICE_DELAY", 2*time.Minute),
		SummaryChangeNoticeGrace: p.duration("SUMMARY_CHANGE_NOTICE_GRACE", 10*time.Minute),
		Translator:               p.language("BOT_LANGUAGE", i18n.DefaultLanguage),
		LanguageFromSlack:        p.boolean("BOT_LANGUAGE_FROM_SLACK", false),
	}
	c.validate(&p)
	if len(p.errs) > 0 {
		return nil, fmt.Errorf("invalid configuration:\n  %w", errors.Join(p.errs...))
	}
	return c, nil
}

func (c *Config) validate(p *parser) {
	if !c.DryRun {
		if !strings.HasPrefix(c.SlackBotToken, "xoxb-") {
			p.fail("SLACK_BOT_TOKEN must be set and start with xoxb-")
		}
		if !strings.HasPrefix(c.SlackAppToken, "xapp-") {
			p.fail("SLACK_APP_TOKEN must be set and start with xapp-")
		}
	}
	if c.SummaryChannelID == "" && p.requireTargets {
		p.fail("SLACK_SUMMARY_CHANNEL_ID is required")
	}
	switch {
	case c.ParticipantsChannelID == "" && len(c.UserIDs) == 0 && p.requireTargets:
		p.fail("either SLACK_PARTICIPANTS_CHANNEL_ID or SLACK_USER_IDS must be set")
	case c.ParticipantsChannelID != "" && len(c.UserIDs) > 0:
		p.fail("set only one of SLACK_PARTICIPANTS_CHANNEL_ID and SLACK_USER_IDS")
	}
	if c.ReminderMaxCount < 0 {
		p.fail("REMINDER_MAX_COUNT must be >= 0")
	}
	if c.RetentionWeeks < 1 {
		p.fail("RETENTION_WEEKS must be >= 1")
	}
	if c.SummaryMaxNames < 1 {
		p.fail("SUMMARY_MAX_NAMES must be >= 1")
	}
	if c.SummaryChangeNoticeDelay < 0 {
		p.fail("SUMMARY_CHANGE_NOTICE_DELAY must be >= 0")
	}
	if c.SummaryChangeNoticeGrace < 0 {
		p.fail("SUMMARY_CHANGE_NOTICE_GRACE must be >= 0")
	}
	if c.DatabasePath == "" {
		p.fail("DATABASE_PATH is required")
	}
	if c.Location != nil && !c.DeadlineAfterStart() {
		p.fail("POLL_DEADLINE_WEEKDAY/POLL_DEADLINE_TIME must lie after POLL_WEEKDAY/POLL_TIME (deadline is relative to the polled week)")
	}
}

// DeadlineAfterStart reports whether the deadline of a polled week comes
// after the moment the poll is started.
func (c *Config) DeadlineAfterStart() bool {
	w := isoweek.Week{Year: 2026, Week: 10}
	return c.Deadline(w).After(c.StartTime(w))
}

// TargetWeekOffset returns which week a poll started on PollWeekday asks
// about: a start on Monday, the first day of the work week, asks about the
// current week (0), a start on any other day about the next one (1).
func (c *Config) TargetWeekOffset() int {
	if c.PollWeekday == time.Monday {
		return 0
	}
	return 1
}

// StartTime returns the scheduled poll start for the polled week w.
func (c *Config) StartTime(w isoweek.Week) time.Time {
	return w.Add(-c.TargetWeekOffset()).At(c.PollWeekday, c.PollTime.Hour, c.PollTime.Minute, c.Location)
}

// Deadline returns the response deadline for the polled week w.
func (c *Config) Deadline(w isoweek.Week) time.Time {
	return w.At(c.DeadlineWeekday, c.DeadlineTime.Hour, c.DeadlineTime.Minute, c.Location)
}

// LogValue keeps secrets out of logs.
func (c *Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("summary_channel", c.SummaryChannelID),
		slog.String("participants_channel", c.ParticipantsChannelID),
		slog.Int("user_ids", len(c.UserIDs)),
		slog.String("poll", fmt.Sprintf("%s %s %s offset=%d", c.PollWeekday, c.PollTime, c.Location, c.TargetWeekOffset())),
		slog.String("language", c.Translator.Lang()),
		slog.Bool("language_from_slack", c.LanguageFromSlack),
		slog.String("deadline", fmt.Sprintf("%s %s", c.DeadlineWeekday, c.DeadlineTime)),
		slog.Bool("reminder", c.ReminderEnabled),
		slog.String("change_notices", changeNotices(c)),
		slog.String("database", c.DatabasePath),
		slog.Bool("dry_run", c.DryRun),
		slog.String("bot_token", mask(c.SlackBotToken)),
		slog.String("app_token", mask(c.SlackAppToken)),
	)
}

func changeNotices(c *Config) string {
	if !c.SummaryChangeNotices {
		return "off"
	}
	return fmt.Sprintf("delay=%s grace=%s", c.SummaryChangeNoticeDelay, c.SummaryChangeNoticeGrace)
}

func mask(s string) string {
	if s == "" {
		return ""
	}
	return "***"
}

type parser struct {
	get            func(string) string
	errs           []error
	requireTargets bool
}

func (p *parser) fail(format string, args ...any) {
	p.errs = append(p.errs, fmt.Errorf(format, args...))
}

func (p *parser) str(key, def string) string {
	if v := strings.TrimSpace(p.get(key)); v != "" {
		return v
	}
	return def
}

func (p *parser) list(key string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range strings.Split(p.get(key), ",") {
		v = strings.TrimSpace(v)
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

func (p *parser) integer(key string, def int) int {
	v := p.str(key, "")
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		p.fail("%s: invalid integer %q", key, v)
		return def
	}
	return n
}

func (p *parser) boolean(key string, def bool) bool {
	v := p.str(key, "")
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		p.fail("%s: invalid boolean %q", key, v)
		return def
	}
	return b
}

func (p *parser) duration(key string, def time.Duration) time.Duration {
	v := p.str(key, "")
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		p.fail("%s: invalid duration %q (e.g. 90s, 2m)", key, v)
		return def
	}
	return d
}

var weekdayNames = map[string]time.Weekday{
	"MONDAY": time.Monday, "TUESDAY": time.Tuesday, "WEDNESDAY": time.Wednesday,
	"THURSDAY": time.Thursday, "FRIDAY": time.Friday, "SATURDAY": time.Saturday, "SUNDAY": time.Sunday,
}

func parseWeekday(s string) (time.Weekday, bool) {
	wd, ok := weekdayNames[strings.ToUpper(strings.TrimSpace(s))]
	return wd, ok
}

func (p *parser) weekday(key, def string) time.Weekday {
	v := p.str(key, def)
	wd, ok := parseWeekday(v)
	if !ok {
		p.fail("%s: invalid weekday %q (use MONDAY…SUNDAY)", key, v)
	}
	return wd
}

func (p *parser) weekdays(key, def string) []time.Weekday {
	var out []time.Weekday
	seen := map[time.Weekday]bool{}
	for _, v := range strings.Split(p.str(key, def), ",") {
		wd, ok := parseWeekday(v)
		if !ok {
			p.fail("%s: invalid weekday %q (use MONDAY…SUNDAY, comma separated)", key, v)
			continue
		}
		if !seen[wd] {
			seen[wd] = true
			out = append(out, wd)
		}
	}
	return out
}

func (p *parser) timeOfDay(key, def string) TimeOfDay {
	v := p.str(key, def)
	t, err := time.Parse("15:04", v)
	if err != nil {
		p.fail("%s: invalid time %q (use HH:MM)", key, v)
		return TimeOfDay{}
	}
	return TimeOfDay{Hour: t.Hour(), Minute: t.Minute()}
}

func (p *parser) location(key, def string) *time.Location {
	v := p.str(key, def)
	loc, err := time.LoadLocation(v)
	if err != nil {
		p.fail("%s: unknown time zone %q", key, v)
		return nil
	}
	return loc
}

func (p *parser) language(key, def string) *i18n.Translator {
	v := p.str(key, def)
	t, err := i18n.New(v)
	if err != nil {
		p.fail("%s: %v", key, err)
		return i18n.MustNew(i18n.DefaultLanguage)
	}
	return t
}

func (p *parser) logLevel(key, def string) slog.Level {
	v := p.str(key, def)
	var l slog.Level
	if err := l.UnmarshalText([]byte(v)); err != nil {
		p.fail("%s: invalid log level %q (debug, info, warn, error)", key, v)
	}
	return l
}
