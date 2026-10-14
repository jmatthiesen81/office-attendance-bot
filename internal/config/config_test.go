package config

import (
	"strings"
	"testing"
	"time"

	"officebot/internal/isoweek"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func valid() map[string]string {
	return map[string]string{
		"SLACK_BOT_TOKEN":          "xoxb-secret",
		"SLACK_APP_TOKEN":          "xapp-secret",
		"SLACK_SUMMARY_CHANNEL_ID": "C123",
		"SLACK_USER_IDS":           "U1, U2,U1",
	}
}

func TestDefaults(t *testing.T) {
	c, err := LoadFrom(env(valid()))
	if err != nil {
		t.Fatal(err)
	}
	if c.PollWeekday != time.Monday || c.PollTime != (TimeOfDay{7, 0}) || c.Location.String() != "Europe/Berlin" {
		t.Errorf("unexpected poll schedule: %v %v %v", c.PollWeekday, c.PollTime, c.Location)
	}
	if c.DeadlineWeekday != time.Friday || c.DeadlineTime != (TimeOfDay{8, 0}) {
		t.Errorf("unexpected deadline: %v %v", c.DeadlineWeekday, c.DeadlineTime)
	}
	if len(c.UserIDs) != 2 {
		t.Errorf("user ids not deduplicated: %v", c.UserIDs)
	}
	if len(c.ReminderWeekdays) != 1 || c.ReminderWeekdays[0] != time.Tuesday {
		t.Errorf("reminder weekdays: %v", c.ReminderWeekdays)
	}
	w := isoweek.Week{Year: 2026, Week: 43}
	if got := c.Deadline(w).Format("2006-01-02 15:04"); got != "2026-10-23 08:00" {
		t.Errorf("deadline = %s", got)
	}
}

func TestValidationErrors(t *testing.T) {
	cases := map[string]map[string]string{
		"missing bot token":  {"SLACK_BOT_TOKEN": ""},
		"wrong app token":    {"SLACK_APP_TOKEN": "xoxb-nope"},
		"missing summary":    {"SLACK_SUMMARY_CHANNEL_ID": ""},
		"both target modes":  {"SLACK_PARTICIPANTS_CHANNEL_ID": "C9"},
		"no target mode":     {"SLACK_USER_IDS": ""},
		"bad weekday":        {"POLL_WEEKDAY": "MONTAG"},
		"bad time":           {"POLL_TIME": "25:00"},
		"bad tz":             {"POLL_TIMEZONE": "Mars/Base"},
		"deadline too early": {"POLL_DEADLINE_WEEKDAY": "MONDAY", "POLL_TIME": "09:00"},
		"bad language":       {"BOT_LANGUAGE": "xx"},
		"bad bool":           {"REMINDER_ENABLED": "vielleicht"},
		"bad reminder days":  {"REMINDER_WEEKDAY": "TUESDAY,FOO"},
	}
	for name, override := range cases {
		m := valid()
		for k, v := range override {
			m[k] = v
		}
		if _, err := LoadFrom(env(m)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestDryRunNeedsNoTokens(t *testing.T) {
	m := valid()
	m["SLACK_BOT_TOKEN"], m["SLACK_APP_TOKEN"], m["DRY_RUN"] = "", "", "true"
	if _, err := LoadFrom(env(m)); err != nil {
		t.Fatal(err)
	}
}

func TestTargetWeekFollowsPollWeekday(t *testing.T) {
	w := isoweek.Week{Year: 2026, Week: 44}
	cases := []struct {
		weekday, start string
		offset         int
	}{
		{"MONDAY", "2026-10-26 07:00", 0},  // Monday polls the current week
		{"TUESDAY", "2026-10-20 07:00", 1}, // every other day polls the next week
		{"THURSDAY", "2026-10-22 07:00", 1},
		{"FRIDAY", "2026-10-23 07:00", 1},
		{"SUNDAY", "2026-10-25 07:00", 1},
	}
	for _, c := range cases {
		m := valid()
		m["POLL_WEEKDAY"] = c.weekday
		cfg, err := LoadFrom(env(m))
		if err != nil {
			t.Fatalf("%s: %v", c.weekday, err)
		}
		if cfg.TargetWeekOffset() != c.offset {
			t.Errorf("%s: offset = %d, want %d", c.weekday, cfg.TargetWeekOffset(), c.offset)
		}
		if got := cfg.StartTime(w).Format("2006-01-02 15:04"); got != c.start {
			t.Errorf("%s: start = %s, want %s", c.weekday, got, c.start)
		}
	}
}

func TestLanguage(t *testing.T) {
	c, err := LoadFrom(env(valid()))
	if err != nil || c.Translator.Lang() != "en" || c.LanguageFromSlack {
		t.Fatalf("default language: %v %v", c, err)
	}
	m := valid()
	m["BOT_LANGUAGE"] = "de"
	m["BOT_LANGUAGE_FROM_SLACK"] = "true"
	if c, err = LoadFrom(env(m)); err != nil || c.Translator.Lang() != "de" || !c.LanguageFromSlack {
		t.Fatalf("de: %v", err)
	}
}

func TestProjectLink(t *testing.T) {
	if c, err := LoadFrom(env(valid())); err != nil || !c.SummaryProjectLink {
		t.Fatalf("project link not on by default: %v", err)
	}
	m := valid()
	m["SUMMARY_PROJECT_LINK"] = "false"
	if c, err := LoadFrom(env(m)); err != nil || c.SummaryProjectLink {
		t.Fatalf("project link not disabled: %v", err)
	}
}

func TestSetupModeSkipsTargets(t *testing.T) {
	m := valid()
	m["SLACK_SUMMARY_CHANNEL_ID"], m["SLACK_USER_IDS"] = "", ""
	if _, err := load(env(m), false); err != nil {
		t.Errorf("setup mode: %v", err)
	}
	if _, err := LoadFrom(env(m)); err == nil {
		t.Error("normal mode must still require targets")
	}
}

func TestLogValueMasksTokens(t *testing.T) {
	c, _ := LoadFrom(env(valid()))
	if s := c.LogValue().String(); strings.Contains(s, "secret") {
		t.Errorf("token leaked: %s", s)
	}
}
