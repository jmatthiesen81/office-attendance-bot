package domain

import (
	"testing"
	"time"
)

func TestDate(t *testing.T) {
	d, err := ParseDate("2026-10-30")
	if err != nil || d != NewDate(2026, 10, 30) || d.String() != "2026-10-30" {
		t.Fatalf("ParseDate = %v, %v", d, err)
	}
	if got := d.AddDays(3); got.String() != "2026-11-02" || !got.After(d) || !d.Before(got) {
		t.Errorf("AddDays = %v", got)
	}
	if _, err := ParseDate("30.10.2026"); err == nil {
		t.Error("invalid date accepted")
	}
	// DateOf uses the calendar day in the time's own location.
	loc, _ := time.LoadLocation("Europe/Berlin")
	late := time.Date(2026, 10, 18, 23, 30, 0, 0, time.UTC) // Monday 01:30 in Berlin
	if DateOf(late.In(loc)) != NewDate(2026, 10, 19) || DateOf(late) != NewDate(2026, 10, 18) {
		t.Error("DateOf ignores the location")
	}
}

func TestAway(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/Berlin")
	p := &Poll{WeekStart: time.Date(2026, 10, 19, 0, 0, 0, 0, loc)}
	week := p.Dates(loc)
	if week[0] != NewDate(2026, 10, 19) || week[4] != NewDate(2026, 10, 23) {
		t.Fatalf("week = %v", week)
	}
	abs := []Absence{
		{SlackUserID: "U1", Start: NewDate(2026, 10, 17), End: NewDate(2026, 10, 21)},
		{SlackUserID: "U1", Start: NewDate(2026, 10, 22), End: NewDate(2026, 10, 23)},
		{SlackUserID: "U2", Start: NewDate(2026, 10, 19), End: NewDate(2026, 10, 22)},
	}
	if !AwayAllWeek(abs, "U1", week) {
		t.Error("U1 is away all week (two adjacent absences)")
	}
	if AwayAllWeek(abs, "U2", week) || !AwayOn(abs, "U2", week[1]) || AwayOn(abs, "U2", week[4]) {
		t.Error("U2 is away Monday to Thursday only")
	}
	if AwayOn(abs, "U3", week[0]) {
		t.Error("U3 has no absence")
	}
}
