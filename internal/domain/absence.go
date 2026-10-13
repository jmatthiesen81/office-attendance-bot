package domain

import (
	"fmt"
	"time"
)

// DeliveryAway marks a participant whose poll DM was not sent because they
// are away for the whole polled week.
const DeliveryAway = "away"

// Date is a calendar day without time of day or time zone.
type Date struct{ t time.Time } // always 00:00 UTC

// NewDate returns the given calendar day.
func NewDate(year int, month time.Month, day int) Date {
	return Date{time.Date(year, month, day, 0, 0, 0, 0, time.UTC)}
}

// DateOf returns the calendar day of t in t's location.
func DateOf(t time.Time) Date { return NewDate(t.Year(), t.Month(), t.Day()) }

// ParseDate parses "2026-10-19".
func ParseDate(s string) (Date, error) {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		return Date{}, fmt.Errorf("invalid date %q (expected YYYY-MM-DD)", s)
	}
	return Date{t}, nil
}

func (d Date) String() string { return d.t.Format(time.DateOnly) }

// IsZero reports whether d is unset.
func (d Date) IsZero() bool { return d.t.IsZero() }

// AddDays returns the day n days later (n may be negative).
func (d Date) AddDays(n int) Date { return Date{d.t.AddDate(0, 0, n)} }

// Before reports whether d is earlier than o.
func (d Date) Before(o Date) bool { return d.t.Before(o.t) }

// After reports whether d is later than o.
func (d Date) After(o Date) bool { return d.t.After(o.t) }

// Weekday returns the day of the week.
func (d Date) Weekday() time.Weekday { return d.t.Weekday() }

// In returns 00:00 of the day in loc.
func (d Date) In(loc *time.Location) time.Time {
	return time.Date(d.t.Year(), d.t.Month(), d.t.Day(), 0, 0, 0, 0, loc)
}

// Absence is time off of a person: whole days from Start to End, both included.
type Absence struct {
	ID          int64
	SlackUserID string
	Start       Date
	End         Date
	CreatedBy   string
}

// Covers reports whether the person is away on d.
func (a Absence) Covers(d Date) bool { return !d.Before(a.Start) && !d.After(a.End) }

// AwayOn reports whether one of the absences of userID covers d.
func AwayOn(absences []Absence, userID string, d Date) bool {
	for _, a := range absences {
		if a.SlackUserID == userID && a.Covers(d) {
			return true
		}
	}
	return false
}

// AwayAllWeek reports whether userID is away on every office day of the week.
func AwayAllWeek(absences []Absence, userID string, week [NumDays]Date) bool {
	for _, d := range week {
		if !AwayOn(absences, userID, d) {
			return false
		}
	}
	return true
}

// Dates returns the calendar days of the poll's office days in loc.
func (p *Poll) Dates(loc *time.Location) [NumDays]Date {
	start := DateOf(p.WeekStart.In(loc))
	var out [NumDays]Date
	for i := range out {
		out[i] = start.AddDays(i)
	}
	return out
}
