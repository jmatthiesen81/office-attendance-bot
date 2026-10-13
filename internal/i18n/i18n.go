// Package i18n provides the translations of all texts the bot sends to Slack
// and the locale-specific date and time formats. Translations live in
// locales/<lang>.json and are embedded into the binary.
package i18n

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"
	"time"
)

//go:embed locales/*.json
var localeFS embed.FS

// DefaultLanguage is used when no language is configured.
const DefaultLanguage = "en"

// Translator renders texts and dates in one language.
type Translator struct {
	lang string
	msgs map[string]string
}

var catalogs = mustLoad()

func mustLoad() map[string]map[string]string {
	files, err := fs.Glob(localeFS, "locales/*.json")
	if err != nil {
		panic(err)
	}
	out := map[string]map[string]string{}
	for _, f := range files {
		b, err := localeFS.ReadFile(f)
		if err != nil {
			panic(err)
		}
		var m map[string]string
		if err := json.Unmarshal(b, &m); err != nil {
			panic(fmt.Sprintf("i18n: %s: %v", f, err))
		}
		out[strings.TrimSuffix(path.Base(f), ".json")] = m
	}
	return out
}

// Languages returns the available language codes, sorted.
func Languages() []string {
	out := make([]string, 0, len(catalogs))
	for l := range catalogs {
		out = append(out, l)
	}
	slices.Sort(out)
	return out
}

// New returns the translator for lang ("de", "en", …).
func New(lang string) (*Translator, error) {
	lang = strings.ToLower(strings.TrimSpace(lang))
	m, ok := catalogs[lang]
	if !ok {
		return nil, fmt.Errorf("unsupported language %q (available: %s)", lang, strings.Join(Languages(), ", "))
	}
	return &Translator{lang: lang, msgs: m}, nil
}

// MustNew is like New but panics on unknown languages. Meant for tests.
func MustNew(lang string) *Translator {
	t, err := New(lang)
	if err != nil {
		panic(err)
	}
	return t
}

// Lang returns the language code.
func (t *Translator) Lang() string { return t.lang }

// T returns the text for key with {name} placeholders replaced. args are
// name/value pairs. Unknown keys are returned as is, so that a missing
// translation is visible instead of silently empty.
func (t *Translator) T(key string, args ...any) string {
	s, ok := t.msgs[key]
	if !ok {
		return key
	}
	if len(args) == 0 {
		return s
	}
	pairs := make([]string, 0, len(args))
	for i := 0; i+1 < len(args); i += 2 {
		pairs = append(pairs, "{"+fmt.Sprint(args[i])+"}", fmt.Sprint(args[i+1]))
	}
	return strings.NewReplacer(pairs...).Replace(s)
}

var weekdayKeys = [7]string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}

// Weekday returns the full name of wd, e.g. "Montag".
func (t *Translator) Weekday(wd time.Weekday) string {
	return t.T("weekday.long." + weekdayKeys[wd])
}

// WeekdayShort returns the abbreviated name of wd, e.g. "Mo".
func (t *Translator) WeekdayShort(wd time.Weekday) string {
	return t.T("weekday.short." + weekdayKeys[wd])
}

// Time formats a wall-clock time, e.g. "09:42 Uhr".
func (t *Translator) Time(tm time.Time) string {
	return t.T("format.time", "time", tm.Format(t.T("layout.time")))
}

// DateTime formats a weekday, date and time, e.g. "Fr, 23.10. 08:00 Uhr".
func (t *Translator) DateTime(tm time.Time) string {
	return t.T("format.datetime",
		"weekday", t.WeekdayShort(tm.Weekday()),
		"date", tm.Format(t.T("layout.date")),
		"time", tm.Format(t.T("layout.time")))
}

// DateRange formats the range of a week, e.g. "19.–23.10.2026",
// "28.09.–02.10.2026" or "29.12.2025–02.01.2026".
func (t *Translator) DateRange(start, end time.Time) string {
	kind := "day"
	switch {
	case start.Year() != end.Year():
		kind = "year"
	case start.Month() != end.Month():
		kind = "month"
	}
	return t.T("format.range."+kind,
		"start", start.Format(t.T("layout.range."+kind+".start")),
		"end", end.Format(t.T("layout.range."+kind+".end")))
}

// List joins items with the language's list separator, e.g. "Mo, Mi, Fr".
func (t *Translator) List(items []string) string {
	return strings.Join(items, t.T("list.separator"))
}

// Date formats a calendar day with weekday, e.g. "Mo, 19.10.2026".
func (t *Translator) Date(tm time.Time) string {
	return t.T("format.date", "weekday", t.WeekdayShort(tm.Weekday()), "date", tm.Format(t.T("layout.date_long")))
}
