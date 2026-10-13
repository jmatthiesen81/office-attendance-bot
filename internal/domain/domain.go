// Package domain contains the core types shared between persistence,
// business logic and rendering.
package domain

import "time"

// NumDays is the number of selectable office days (Monday–Friday).
const NumDays = 5

// Days is a selection of office days, index 0 = Monday … 4 = Friday.
type Days [NumDays]bool

// DayKeys are the stable option values used in Slack checkboxes.
var DayKeys = [NumDays]string{"mon", "tue", "wed", "thu", "fri"}

// DayWeekdays maps day indexes to weekdays.
var DayWeekdays = [NumDays]time.Weekday{time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday}

// DayIndex returns the index for an option value; ok is false for unknown keys.
func DayIndex(key string) (int, bool) {
	for i, k := range DayKeys {
		if k == key {
			return i, true
		}
	}
	return 0, false
}

// Count returns the number of selected days.
func (d Days) Count() int {
	n := 0
	for _, v := range d {
		if v {
			n++
		}
	}
	return n
}

// Names returns name(weekday) for every selected day, in week order.
func (d Days) Names(name func(time.Weekday) string) []string {
	var parts []string
	for i, v := range d {
		if v {
			parts = append(parts, name(DayWeekdays[i]))
		}
	}
	return parts
}

// Poll status values.
const (
	PollOpen   = "open"
	PollClosed = "closed"
)

// Participant delivery status values.
const (
	DeliveryPending     = "pending"
	DeliverySent        = "sent"
	DeliveryFailed      = "failed"
	DeliveryUnreachable = "unreachable"
	DeliverySkipped     = "skipped"
)

// Poll is one weekly attendance poll.
type Poll struct {
	ID                   int64
	WorkspaceID          string
	ISOYear              int
	ISOWeek              int
	WeekStart            time.Time // Monday 00:00 in poll timezone
	WeekEnd              time.Time // Friday 00:00 in poll timezone
	Deadline             time.Time
	Status               string
	CreatedAt            time.Time
	SummaryChannelID     string
	SummaryMessageTS     string
	SummaryVersion       int64
	SummarySyncedVersion int64
}

// IsAcceptingResponses reports whether responses may still be saved at now.
func (p *Poll) IsAcceptingResponses(now time.Time) bool {
	return p.Status == PollOpen && now.Before(p.Deadline)
}

// Participant is a snapshot entry: one person asked in one poll.
type Participant struct {
	ID                  int64
	PollID              int64
	SlackUserID         string
	DisplayName         string
	DMChannelID         string
	DMMessageTS         string
	DMGeneration        int // bumped on every re-requested DM
	DeliveryStatus      string
	LastDeliveryAttempt time.Time
	ReminderCount       int
}

// Response is a saved answer. A response with no days selected is a valid
// answer ("not in the office this week").
type Response struct {
	PollID      int64
	SlackUserID string
	Days        Days
	SubmittedAt time.Time
	UpdatedAt   time.Time
	Revision    int
}
