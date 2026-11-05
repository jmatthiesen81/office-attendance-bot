package web

import (
	"fmt"
	"strconv"
	"time"

	"officebot/internal/isoweek"
)

func itoa(n int) string { return strconv.Itoa(n) }

func weekKey(year, week int) string { return isoweek.Week{Year: year, Week: week}.String() }

var weekdaysShort = map[string][7]string{
	"de": {"So", "Mo", "Di", "Mi", "Do", "Fr", "Sa"},
	"en": {"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"},
}

func weekdayShort(lang string, wd time.Weekday) string { return weekdaysShort[lang][wd] }

var monthNames = map[string][12]string{
	"de": {"Januar", "Februar", "März", "April", "Mai", "Juni", "Juli", "August", "September", "Oktober", "November", "Dezember"},
	"en": {"January", "February", "March", "April", "May", "June", "July", "August", "September", "October", "November", "December"},
}

// monthName formats "2026-11" as "November 2026".
func monthName(lang, month string) string {
	t, err := time.Parse("2006-01", month)
	if err != nil {
		return month
	}
	return fmt.Sprintf("%s %d", monthNames[lang][t.Month()-1], t.Year())
}

// shortDate formats a day without year: "02.11." or "Nov 2".
func shortDate(lang string, t time.Time) string {
	if lang == "de" {
		return t.Format("02.01.")
	}
	return t.Format("Jan 2")
}

// fmtDate formats a day: "02.11.2026" or "2026-11-02".
func fmtDate(lang string, t time.Time) string {
	if t.IsZero() {
		return "–"
	}
	if lang == "de" {
		return t.Format("02.01.2006")
	}
	return t.Format("2006-01-02")
}
