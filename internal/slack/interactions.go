package slack

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	goslack "github.com/slack-go/slack"

	"officebot/internal/domain"
)

// Interaction is a parsed button click. TeamID and UserID come from the
// Slack-authenticated payload; PollID, ChannelID and MessageTS are only
// lookup keys and must be validated against the database by the handler.
// Days is only set for save clicks, Away for away/undo clicks, Language for
// the submitted language form, the remaining fields only for form interactions.
type Interaction struct {
	TeamID    string
	UserID    string
	PollID    int64
	ChannelID string
	MessageTS string
	Days      domain.Days
	Away      bool

	TriggerID string      // opens a form
	ViewID    string      // the open form; for a click in a form, the form it came from
	AbsenceID int64       // entry to delete
	Language  string      // chosen language code or LanguageDefault
	Start     domain.Date // submitted form; zero if not chosen
	End       domain.Date
}

// ErrMissingState means the payload did not contain the checkbox state.
var ErrMissingState = errors.New("checkbox state missing in interaction payload")

// findAction returns the block action with the given action ID, if any.
func findAction(cb *goslack.InteractionCallback, actionID string) *goslack.BlockAction {
	if cb.Type != goslack.InteractionTypeBlockActions {
		return nil
	}
	for _, a := range cb.ActionCallback.BlockActions {
		if a.ActionID == actionID {
			return a
		}
	}
	return nil
}

// parseButton extracts the common fields of a button whose value is a poll
// ID and whose block ID starts with blockPrefix:<pollID>.
func parseButton(cb *goslack.InteractionCallback, action *goslack.BlockAction, blockPrefix string) (Interaction, error) {
	var req Interaction
	pollID, err := strconv.ParseInt(action.Value, 10, 64)
	if err != nil || pollID <= 0 {
		return req, fmt.Errorf("invalid poll reference %q", action.Value)
	}
	if want := fmt.Sprintf("%s:%d", blockPrefix, pollID); action.BlockID != want && !strings.HasPrefix(action.BlockID, want+":") {
		return req, fmt.Errorf("block %q does not belong to poll %d", action.BlockID, pollID)
	}
	req = Interaction{
		TeamID:    cb.Team.ID,
		UserID:    cb.User.ID,
		PollID:    pollID,
		ChannelID: cb.Container.ChannelID,
		MessageTS: cb.Container.MessageTs,
	}
	if req.ChannelID == "" {
		req.ChannelID = cb.Channel.ID
	}
	if req.MessageTS == "" {
		req.MessageTS = cb.Message.Timestamp
	}
	if req.TeamID == "" || req.UserID == "" {
		return req, errors.New("interaction without team or user")
	}
	return req, nil
}

// ParseSave extracts a save click from a block_actions payload. ok is false
// for interactions that are not a save click (e.g. ticking a checkbox), which
// must be acknowledged but never persisted.
func ParseSave(cb *goslack.InteractionCallback) (req Interaction, ok bool, err error) {
	action := findAction(cb, ActionSave)
	if action == nil {
		return req, false, nil
	}
	if req, err = parseButton(cb, action, "days"); err != nil {
		return req, true, err
	}
	if cb.BlockActionState == nil {
		return req, true, ErrMissingState
	}
	state, found := cb.BlockActionState.Values[action.BlockID][ActionDays]
	if !found {
		return req, true, ErrMissingState
	}
	for _, opt := range state.SelectedOptions {
		i, known := domain.DayIndex(opt.Value)
		if !known {
			return req, true, fmt.Errorf("unknown day option %q", opt.Value)
		}
		req.Days[i] = true
	}
	return req, true, nil
}

// ParseRequestDM extracts a click on "Abfrage an mich senden" in the channel
// summary. ok is false for other interactions.
func ParseRequestDM(cb *goslack.InteractionCallback) (req Interaction, ok bool, err error) {
	action := findAction(cb, ActionRequestDM)
	if action == nil {
		return req, false, nil
	}
	req, err = parseButton(cb, action, "request")
	return req, true, err
}

// ParseAway extracts a click on "I'm away this week" (Away = true) or on
// "Undo" (Away = false) in the poll DM. ok is false for other interactions.
func ParseAway(cb *goslack.InteractionCallback) (req Interaction, ok bool, err error) {
	action := findAction(cb, ActionAway)
	if action == nil {
		if action = findAction(cb, ActionUndoAway); action == nil {
			return req, false, nil
		}
	}
	req, err = parseButton(cb, action, "away")
	req.Away = action.ActionID == ActionAway
	return req, true, err
}

// userOf returns the authenticated team and user of an interaction.
func userOf(cb *goslack.InteractionCallback) (Interaction, error) {
	req := Interaction{TeamID: cb.Team.ID, UserID: cb.User.ID, ChannelID: cb.Container.ChannelID, MessageTS: cb.Container.MessageTs}
	if req.ChannelID == "" {
		req.ChannelID = cb.Channel.ID
	}
	if req.TeamID == "" || req.UserID == "" {
		return req, errors.New("interaction without team or user")
	}
	return req, nil
}

// ParseOpenTimeOff extracts a click on "Plan time off" (poll DM or channel
// summary). The form is always about the clicking user's own time off.
func ParseOpenTimeOff(cb *goslack.InteractionCallback) (req Interaction, ok bool, err error) {
	if findAction(cb, ActionTimeOff) == nil {
		return req, false, nil
	}
	if req, err = userOf(cb); err != nil {
		return req, true, err
	}
	req.TriggerID = cb.TriggerID
	if req.TriggerID == "" {
		return req, true, errors.New("time-off click without trigger_id")
	}
	return req, true, nil
}

// ParseDeleteTimeOff extracts a click on "Delete" in the time-off form.
func ParseDeleteTimeOff(cb *goslack.InteractionCallback) (req Interaction, ok bool, err error) {
	action := findAction(cb, ActionTimeOffDelete)
	if action == nil {
		return req, false, nil
	}
	if req, err = userOf(cb); err != nil {
		return req, true, err
	}
	if cb.View.ID == "" || cb.View.CallbackID != CallbackTimeOff {
		return req, true, errors.New("delete click outside the time-off form")
	}
	req.ViewID = cb.View.ID
	if req.AbsenceID, err = strconv.ParseInt(action.Value, 10, 64); err != nil || req.AbsenceID <= 0 {
		return req, true, fmt.Errorf("invalid absence reference %q", action.Value)
	}
	return req, true, nil
}

// ParseTimeOffSubmit extracts the submitted time-off form. Dates that were
// not chosen stay zero; the handler reports them as validation errors.
func ParseTimeOffSubmit(cb *goslack.InteractionCallback) (req Interaction, ok bool, err error) {
	if cb.Type != goslack.InteractionTypeViewSubmission || cb.View.CallbackID != CallbackTimeOff {
		return req, false, nil
	}
	if req, err = userOf(cb); err != nil {
		return req, true, err
	}
	req.ViewID = cb.View.ID
	pick := func(block, action string) (domain.Date, error) {
		if cb.View.State == nil {
			return domain.Date{}, nil
		}
		v := cb.View.State.Values[block][action].SelectedDate
		if v == "" {
			return domain.Date{}, nil
		}
		return domain.ParseDate(v)
	}
	if req.Start, err = pick(TimeOffStartBlock, TimeOffStartAction); err != nil {
		return req, true, err
	}
	req.End, err = pick(TimeOffEndBlock, TimeOffEndAction)
	return req, true, err
}

// ParseOpenLanguage extracts a click on "Language" (poll DM, channel summary
// or time-off form). ViewID is set if the click came from a form, on top of
// which the language form is pushed.
func ParseOpenLanguage(cb *goslack.InteractionCallback) (req Interaction, ok bool, err error) {
	if findAction(cb, ActionLanguage) == nil {
		return req, false, nil
	}
	if req, err = userOf(cb); err != nil {
		return req, true, err
	}
	req.TriggerID = cb.TriggerID
	if cb.Container.Type == "view" {
		req.ViewID = cb.View.ID
	}
	if req.TriggerID == "" {
		return req, true, errors.New("language click without trigger_id")
	}
	return req, true, nil
}

// ParseLanguageSubmit extracts the submitted language form. The value is only
// checked for presence; the handler checks that the language exists.
func ParseLanguageSubmit(cb *goslack.InteractionCallback) (req Interaction, ok bool, err error) {
	if cb.Type != goslack.InteractionTypeViewSubmission || cb.View.CallbackID != CallbackLanguage {
		return req, false, nil
	}
	if req, err = userOf(cb); err != nil {
		return req, true, err
	}
	req.ViewID = cb.View.ID
	if cb.View.State != nil {
		req.Language = cb.View.State.Values[LanguageBlock][LanguageAction].SelectedOption.Value
	}
	if req.Language == "" {
		return req, true, errors.New("language form without selection")
	}
	return req, true, nil
}
