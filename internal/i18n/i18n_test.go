package i18n

import (
	"maps"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

var placeholder = regexp.MustCompile(`\{[a-z_]+\}`)

// Every language must provide exactly the keys of the default language,
// with the same placeholders, so that no text silently falls back to a key.
func TestCatalogsComplete(t *testing.T) {
	base := catalogs[DefaultLanguage]
	if len(base) == 0 {
		t.Fatal("default language missing")
	}
	for lang, m := range catalogs {
		for k, v := range base {
			got, ok := m[k]
			if !ok {
				t.Errorf("%s: missing key %q", lang, k)
				continue
			}
			want := placeholder.FindAllString(v, -1)
			have := placeholder.FindAllString(got, -1)
			slices.Sort(want)
			slices.Sort(have)
			if !slices.Equal(want, have) {
				t.Errorf("%s: %q has placeholders %v, want %v", lang, k, have, want)
			}
		}
		for k := range m {
			if _, ok := base[k]; !ok {
				t.Errorf("%s: unknown key %q", lang, k)
			}
		}
	}
	if !slices.Equal(Languages(), slices.Sorted(maps.Keys(catalogs))) || !slices.Contains(Languages(), "en") {
		t.Errorf("languages = %v", Languages())
	}
}

// Go renders month and weekday names and AM/PM in layouts in English only,
// so languages not based on English must use numeric layouts. Variants such
// as "pirate_en" are based on the language after the underscore.
func TestLayoutsNumericOutsideEnglish(t *testing.T) {
	names := regexp.MustCompile(`Jan|Mon|PM|pm|MST`)
	for lang, m := range catalogs {
		if _, base, _ := strings.Cut(lang, "_"); lang == "en" || base == "en" {
			continue
		}
		for k, v := range m {
			if strings.HasPrefix(k, "layout.") && names.MatchString(v) {
				t.Errorf("%s: %s = %q renders English names", lang, k, v)
			}
		}
	}
}

// Every language comes with a nerd and a pirate variant, and every variant
// belongs to an existing language.
func TestFunVariants(t *testing.T) {
	for lang := range catalogs {
		style, base, isVariant := strings.Cut(lang, "_")
		if !isVariant {
			for _, style := range []string{"nerd", "pirate"} {
				if _, ok := catalogs[style+"_"+lang]; !ok {
					t.Errorf("%s: missing %s_%s", lang, style, lang)
				}
			}
			continue
		}
		if _, ok := catalogs[base]; !ok || (style != "nerd" && style != "pirate") {
			t.Errorf("%s: unknown variant", lang)
		}
	}
}

func TestNew(t *testing.T) {
	if tr, err := New(" EN "); err != nil || tr.Lang() != "en" {
		t.Errorf("New(EN) = %v, %v", tr, err)
	}
	if _, err := New("xx"); err == nil {
		t.Error("unknown language accepted")
	}
}

func TestT(t *testing.T) {
	tr := MustNew("de")
	if got := tr.T("summary.responses", "answered", 4, "total", 6); got != "*Rückmeldungen:* 4 von 6 Mitarbeitern" {
		t.Errorf("T = %q", got)
	}
	if got := tr.T("no.such.key"); got != "no.such.key" {
		t.Errorf("missing key = %q", got)
	}
}

func TestFormats(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	ts := time.Date(2026, 10, 23, 8, 5, 0, 0, loc)
	ranges := [][2]time.Time{
		{time.Date(2026, 10, 19, 0, 0, 0, 0, loc), time.Date(2026, 10, 23, 0, 0, 0, 0, loc)},
		{time.Date(2026, 9, 28, 0, 0, 0, 0, loc), time.Date(2026, 10, 2, 0, 0, 0, 0, loc)},
		{time.Date(2025, 12, 29, 0, 0, 0, 0, loc), time.Date(2026, 1, 2, 0, 0, 0, 0, loc)},
	}
	cases := []struct {
		lang, dateTime, time string
		ranges               [3]string
	}{
		{"de", "Fr, 23.10. 08:05 Uhr", "08:05 Uhr", [3]string{"19.–23.10.2026", "28.09.–02.10.2026", "29.12.2025–02.01.2026"}},
		{"en", "Fri, Oct 23, 8:05 AM", "8:05 AM", [3]string{"Oct 19–23, 2026", "Sep 28 – Oct 2, 2026", "Dec 29, 2025 – Jan 2, 2026"}},
		{"ja", "10月23日（金）08:05", "08:05", [3]string{"2026年10月19日～23日", "2026年9月28日～10月2日", "2025年12月29日～2026年1月2日"}},
	}
	for _, c := range cases {
		tr := MustNew(c.lang)
		if got := tr.DateTime(ts); got != c.dateTime {
			t.Errorf("%s DateTime = %q, want %q", c.lang, got, c.dateTime)
		}
		if got := tr.Time(ts); got != c.time {
			t.Errorf("%s Time = %q, want %q", c.lang, got, c.time)
		}
		for i, r := range ranges {
			if got := tr.DateRange(r[0], r[1]); got != c.ranges[i] {
				t.Errorf("%s DateRange = %q, want %q", c.lang, got, c.ranges[i])
			}
		}
	}
	if got := MustNew("en").Weekday(time.Sunday); got != "Sunday" {
		t.Errorf("Weekday = %q", got)
	}
}
