// Package isoweek implements ISO-8601 calendar weeks and DST-safe
// computation of weekly schedule occurrences.
package isoweek

import (
	"fmt"
	"time"
)

// Week identifies an ISO-8601 week.
type Week struct {
	Year int
	Week int
}

// Of returns the ISO week containing t (evaluated in t's location).
func Of(t time.Time) Week {
	y, w := t.ISOWeek()
	return Week{Year: y, Week: w}
}

// Monday returns Monday 00:00 of the week in loc.
func (w Week) Monday(loc *time.Location) time.Time {
	// January 4th is always in week 1.
	jan4 := time.Date(w.Year, time.January, 4, 0, 0, 0, 0, loc)
	offset := (int(jan4.Weekday()) + 6) % 7 // days since Monday
	return time.Date(w.Year, time.January, 4-offset+7*(w.Week-1), 0, 0, 0, 0, loc)
}

// Day returns 00:00 of the given weekday within the week in loc.
func (w Week) Day(wd time.Weekday, loc *time.Location) time.Time {
	m := w.Monday(loc)
	return time.Date(m.Year(), m.Month(), m.Day()+DayOffset(wd), 0, 0, 0, 0, loc)
}

// At returns the given weekday and wall-clock time within the week in loc.
// Using time.Date keeps the wall-clock time stable across DST changes.
func (w Week) At(wd time.Weekday, hour, minute int, loc *time.Location) time.Time {
	m := w.Monday(loc)
	return time.Date(m.Year(), m.Month(), m.Day()+DayOffset(wd), hour, minute, 0, 0, loc)
}

// Add returns the week n weeks later (n may be negative).
func (w Week) Add(n int) Week {
	m := w.Monday(time.UTC)
	return Of(time.Date(m.Year(), m.Month(), m.Day()+7*n, 12, 0, 0, 0, time.UTC))
}

// Before reports whether w is earlier than o.
func (w Week) Before(o Week) bool {
	if w.Year != o.Year {
		return w.Year < o.Year
	}
	return w.Week < o.Week
}

func (w Week) String() string { return fmt.Sprintf("%04d-W%02d", w.Year, w.Week) }

// Parse parses "2026-W43".
func Parse(s string) (Week, error) {
	var w Week
	if _, err := fmt.Sscanf(s, "%4d-W%2d", &w.Year, &w.Week); err != nil {
		return Week{}, fmt.Errorf("invalid ISO week %q (expected YYYY-Www)", s)
	}
	if w.Week < 1 || w.Week > WeeksInYear(w.Year) {
		return Week{}, fmt.Errorf("invalid ISO week %q: year %d has %d weeks", s, w.Year, WeeksInYear(w.Year))
	}
	return w, nil
}

// WeeksInYear returns 52 or 53.
func WeeksInYear(year int) int {
	_, w := time.Date(year, time.December, 28, 12, 0, 0, 0, time.UTC).ISOWeek()
	return w
}

// DayOffset returns the number of days from Monday to wd.
func DayOffset(wd time.Weekday) int { return (int(wd) + 6) % 7 }

// Prev returns the latest occurrence of weekday/hour:minute that is <= now.
func Prev(now time.Time, wd time.Weekday, hour, minute int, loc *time.Location) time.Time {
	now = now.In(loc)
	w := Of(now)
	occ := w.At(wd, hour, minute, loc)
	if occ.After(now) {
		occ = w.Add(-1).At(wd, hour, minute, loc)
	}
	return occ
}

// Next returns the earliest occurrence of weekday/hour:minute that is > now.
func Next(now time.Time, wd time.Weekday, hour, minute int, loc *time.Location) time.Time {
	now = now.In(loc)
	w := Of(now)
	occ := w.At(wd, hour, minute, loc)
	if !occ.After(now) {
		occ = w.Add(1).At(wd, hour, minute, loc)
	}
	return occ
}
