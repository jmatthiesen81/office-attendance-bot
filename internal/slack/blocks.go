package slack

import (
	"fmt"
	"time"

	goslack "github.com/slack-go/slack"

	"officebot/internal/domain"
	"officebot/internal/i18n"
	"officebot/internal/isoweek"
)

// Action IDs of the interactive elements.
const (
	ActionDays      = "days_select"
	ActionSave      = "save_days"
	ActionRequestDM = "request_dm" // button in the channel summary

	ActionAway          = "away_week"      // poll DM: away for the whole week
	ActionUndoAway      = "undo_away"      // poll DM: not away after all
	ActionTimeOff       = "timeoff_open"   // poll DM and summary: open the time-off form
	ActionTimeOffDelete = "timeoff_delete" // time-off form: delete one entry
)

// ModalView is a modal such as the time-off form.
type ModalView = goslack.ModalViewRequest

// IDs of the time-off form.
const (
	CallbackTimeOff    = "timeoff"
	TimeOffStartBlock  = "timeoff_start"
	TimeOffStartAction = "start_date"
	TimeOffEndBlock    = "timeoff_end"
	TimeOffEndAction   = "end_date"
)

// maxTimeOffEntries limits the list in the time-off form (modals allow 100 blocks).
const maxTimeOffEntries = 20

func timeOffButton(tr *i18n.Translator) *goslack.ButtonBlockElement {
	return goslack.NewButtonBlockElement(ActionTimeOff, "open", plain(tr.T("timeoff.button")))
}

// RequestDMBlock holds the buttons below the channel summary: anyone can
// (re)request their poll DM or plan their time off.
func RequestDMBlock(tr *i18n.Translator, pollID int64) goslack.Block {
	btn := goslack.NewButtonBlockElement(ActionRequestDM, fmt.Sprint(pollID), plain(tr.T("summary.request_dm")))
	return goslack.NewActionBlock(fmt.Sprintf("request:%d", pollID), btn, timeOffButton(tr))
}

// DMView is everything needed to render a participant's poll DM.
type DMView struct {
	PollID    int64
	Week      isoweek.Week
	WeekStart time.Time
	WeekEnd   time.Time
	Deadline  time.Time
	NextWeek  bool
	Response  *domain.Response // nil = not answered yet
	Closed    bool
	Away      bool // away for the whole week
	Loc       *time.Location
	T         *i18n.Translator
}

// daysBlockID changes with every saved revision. Slack clients keep local
// checkbox state per block_id; a new ID forces them to show initial_options,
// i.e. the persisted selection, after an update.
func daysBlockID(pollID int64, revision int) string {
	return fmt.Sprintf("days:%d:%d", pollID, revision)
}

func (v DMView) title() string {
	return v.T.T("dm.title", "week", v.Week.Week, "range", v.T.DateRange(v.WeekStart.In(v.Loc), v.WeekEnd.In(v.Loc)))
}

func (v DMView) weekPhrase() string {
	if v.NextWeek {
		return v.T.T("dm.week.next")
	}
	return v.T.T("dm.week.current")
}

func (v DMView) dateTime(t time.Time) string { return v.T.DateTime(t.In(v.Loc)) }

// SelectionText renders a saved selection, e.g. "Mo, Mi" or "kein Bürotag".
func SelectionText(tr *i18n.Translator, d domain.Days) string {
	if d.Count() == 0 {
		return tr.T("dm.no_office")
	}
	return tr.List(d.Names(tr.WeekdayShort))
}

func mrkdwn(s string) *goslack.TextBlockObject {
	return goslack.NewTextBlockObject(goslack.MarkdownType, s, false, false)
}
func plain(s string) *goslack.TextBlockObject {
	return goslack.NewTextBlockObject(goslack.PlainTextType, s, true, false)
}

// PollDM renders the private poll message, including the saved selection.
func PollDM(v DMView) Message {
	blocks := []goslack.Block{goslack.NewHeaderBlock(plain(v.title()))}

	if v.Closed {
		text := v.T.T("dm.closed")
		if v.Away {
			text += "\n" + v.T.T("dm.closed.away")
		} else if v.Response != nil {
			text += "\n" + v.T.T("dm.closed.selection", "selection", SelectionText(v.T, v.Response.Days))
		} else {
			text += "\n" + v.T.T("dm.closed.no_answer")
		}
		blocks = append(blocks, goslack.NewSectionBlock(mrkdwn(text), nil, nil))
		return Message{Text: v.T.T("dm.title.closed", "title", v.title()), Blocks: blocks}
	}

	awayBlockID := fmt.Sprintf("away:%d", v.PollID)
	if v.Away {
		undo := goslack.NewButtonBlockElement(ActionUndoAway, fmt.Sprint(v.PollID), plain(v.T.T("dm.away.undo")))
		blocks = append(blocks,
			goslack.NewSectionBlock(mrkdwn(v.T.T("dm.away")), nil, nil),
			goslack.NewActionBlock(awayBlockID, undo, timeOffButton(v.T)),
			goslack.NewContextBlock("status", mrkdwn(v.T.T("dm.away.hint", "deadline", v.dateTime(v.Deadline)))),
		)
		return Message{Text: v.title(), Blocks: blocks}
	}

	blocks = append(blocks, goslack.NewSectionBlock(mrkdwn(v.T.T("dm.question", "when", v.weekPhrase())), nil, nil))

	options := make([]*goslack.OptionBlockObject, domain.NumDays)
	var initial []*goslack.OptionBlockObject
	for i := range domain.NumDays {
		options[i] = goslack.NewOptionBlockObject(domain.DayKeys[i], plain(v.T.Weekday(domain.DayWeekdays[i])), nil)
		if v.Response != nil && v.Response.Days[i] {
			initial = append(initial, options[i])
		}
	}
	checkboxes := goslack.NewCheckboxGroupsBlockElement(ActionDays, options...)
	checkboxes.InitialOptions = initial

	save := goslack.NewButtonBlockElement(ActionSave, fmt.Sprint(v.PollID), plain(v.T.T("dm.save")))
	save.Style = goslack.StylePrimary

	revision := 0
	if v.Response != nil {
		revision = v.Response.Revision
	}
	blocks = append(blocks, goslack.NewActionBlock(daysBlockID(v.PollID, revision), checkboxes, save))

	status := v.T.T("dm.status.empty")
	if v.Response != nil {
		status = v.T.T("dm.status.saved", "time", v.dateTime(v.Response.UpdatedAt), "selection", SelectionText(v.T, v.Response.Days))
	}
	blocks = append(blocks, goslack.NewContextBlock("status",
		mrkdwn(status),
		mrkdwn(v.T.T("dm.hint", "deadline", v.dateTime(v.Deadline))),
	))
	away := goslack.NewButtonBlockElement(ActionAway, fmt.Sprint(v.PollID), plain(v.T.T("dm.away.button")))
	blocks = append(blocks, goslack.NewActionBlock(awayBlockID, away, timeOffButton(v.T)))
	return Message{Text: v.title(), Blocks: blocks}
}

// FormatDateRange formats an absence, e.g. "Mo, 19.10.2026 – Fr, 23.10.2026"
// or "Mo, 19.10.2026" for a single day.
func FormatDateRange(tr *i18n.Translator, start, end domain.Date) string {
	if start == end {
		return tr.Date(start.In(time.UTC))
	}
	return tr.T("timeoff.range", "start", tr.Date(start.In(time.UTC)), "end", tr.Date(end.In(time.UTC)))
}

// TimeOffModal renders the time-off form: a date range to add and the
// user's upcoming time off, each with a delete button.
func TimeOffModal(tr *i18n.Translator, upcoming []domain.Absence, today domain.Date) goslack.ModalViewRequest {
	date := func(block, action, label string) goslack.Block {
		dp := goslack.NewDatePickerBlockElement(action)
		dp.InitialDate = today.String()
		return goslack.NewInputBlock(block, plain(label), nil, dp)
	}
	blocks := []goslack.Block{
		goslack.NewSectionBlock(mrkdwn(tr.T("timeoff.intro")), nil, nil),
		date(TimeOffStartBlock, TimeOffStartAction, tr.T("timeoff.start")),
		date(TimeOffEndBlock, TimeOffEndAction, tr.T("timeoff.end")),
		goslack.NewDividerBlock(),
		goslack.NewSectionBlock(mrkdwn(tr.T("timeoff.list")), nil, nil),
	}
	if len(upcoming) == 0 {
		blocks = append(blocks, goslack.NewContextBlock("", mrkdwn(tr.T("timeoff.none"))))
	}
	for i, a := range upcoming {
		if i == maxTimeOffEntries {
			blocks = append(blocks, goslack.NewContextBlock("", mrkdwn(tr.T("summary.more", "count", len(upcoming)-i))))
			break
		}
		del := goslack.NewButtonBlockElement(ActionTimeOffDelete, fmt.Sprint(a.ID), plain(tr.T("timeoff.delete")))
		del.Style = goslack.StyleDanger
		blocks = append(blocks, goslack.NewSectionBlock(mrkdwn(FormatDateRange(tr, a.Start, a.End)), nil, goslack.NewAccessory(del)))
	}
	return goslack.ModalViewRequest{
		Type:       goslack.VTModal,
		CallbackID: CallbackTimeOff,
		Title:      plain(tr.T("timeoff.title")),
		Submit:     plain(tr.T("timeoff.submit")),
		Close:      plain(tr.T("timeoff.close")),
		Blocks:     goslack.Blocks{BlockSet: blocks},
	}
}

// SupersededDM replaces an old poll DM after a new one was requested. It has
// no buttons; clicks on a stale copy are rejected anyway.
func SupersededDM(v DMView) Message {
	return Message{
		Text: v.T.T("dm.title.superseded", "title", v.title()),
		Blocks: []goslack.Block{
			goslack.NewHeaderBlock(plain(v.title())),
			goslack.NewSectionBlock(mrkdwn(v.T.T("dm.superseded")), nil, nil),
		},
	}
}

// Reminder renders a reminder that points to the original poll message.
func Reminder(v DMView, permalink string) Message {
	text := v.T.T("reminder.text", "week", v.Week.Week, "deadline", v.dateTime(v.Deadline))
	if permalink != "" {
		text += fmt.Sprintf("\n<%s|%s>", permalink, v.T.T("reminder.link"))
	}
	return Message{Text: text, Blocks: []goslack.Block{goslack.NewSectionBlock(mrkdwn(text), nil, nil)}}
}
