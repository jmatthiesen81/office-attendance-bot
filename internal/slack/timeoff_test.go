package slack

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"officebot/internal/domain"
	"officebot/internal/i18n"
	"officebot/internal/isoweek"
)

const awayPayload = `{
  "type": "block_actions",
  "team": {"id": "T1"},
  "user": {"id": "U1", "team_id": "T1"},
  "trigger_id": "TR1",
  "container": {"type": "message", "message_ts": "1700000000.000100", "channel_id": "DU1"},
  "channel": {"id": "DU1"},
  "actions": [{"action_id": "away_week", "block_id": "away:7", "type": "button", "value": "7", "action_ts": "1700000001.1"}]
}`

func TestParseAway(t *testing.T) {
	req, ok, err := ParseAway(parse(t, awayPayload))
	if err != nil || !ok || !req.Away || req.PollID != 7 || req.UserID != "U1" || req.MessageTS != "1700000000.000100" {
		t.Fatalf("away: %+v %v %v", req, ok, err)
	}
	undo := strings.Replace(awayPayload, `"away_week"`, `"undo_away"`, 1)
	if req, ok, err := ParseAway(parse(t, undo)); err != nil || !ok || req.Away {
		t.Fatalf("undo: %+v %v %v", req, ok, err)
	}
	if _, _, err := ParseAway(parse(t, strings.Replace(awayPayload, `"away:7"`, `"away:8"`, 1))); err == nil {
		t.Error("poll/block mismatch accepted")
	}
	if _, ok, _ := ParseSave(parse(t, awayPayload)); ok {
		t.Error("away click parsed as save")
	}
}

func TestParseOpenTimeOff(t *testing.T) {
	p := strings.Replace(awayPayload, `"action_id": "away_week"`, `"action_id": "timeoff_open"`, 1)
	req, ok, err := ParseOpenTimeOff(parse(t, p))
	if err != nil || !ok || req.TriggerID != "TR1" || req.UserID != "U1" {
		t.Fatalf("%+v %v %v", req, ok, err)
	}
	if _, _, err := ParseOpenTimeOff(parse(t, strings.Replace(p, `"trigger_id": "TR1",`, ``, 1))); err == nil {
		t.Error("click without trigger_id accepted")
	}
}

const viewActionPayload = `{
  "type": "block_actions",
  "team": {"id": "T1"},
  "user": {"id": "U1", "team_id": "T1"},
  "container": {"type": "view", "view_id": "V1"},
  "view": {"id": "V1", "callback_id": "timeoff", "hash": "h1"},
  "actions": [{"action_id": "timeoff_delete", "block_id": "b", "type": "button", "value": "42", "action_ts": "1700000001.1"}]
}`

func TestParseDeleteTimeOff(t *testing.T) {
	req, ok, err := ParseDeleteTimeOff(parse(t, viewActionPayload))
	if err != nil || !ok || req.AbsenceID != 42 || req.ViewID != "V1" || req.UserID != "U1" {
		t.Fatalf("%+v %v %v", req, ok, err)
	}
	if _, _, err := ParseDeleteTimeOff(parse(t, strings.Replace(viewActionPayload, `"timeoff"`, `"other"`, 1))); err == nil {
		t.Error("delete outside the time-off form accepted")
	}
	if _, _, err := ParseDeleteTimeOff(parse(t, strings.Replace(viewActionPayload, `"value": "42"`, `"value": "x"`, 1))); err == nil {
		t.Error("invalid absence ID accepted")
	}
}

const submitPayload = `{
  "type": "view_submission",
  "team": {"id": "T1"},
  "user": {"id": "U1", "team_id": "T1"},
  "view": {"id": "V1", "callback_id": "timeoff", "state": {"values": {
    "timeoff_start": {"start_date": {"type": "datepicker", "selected_date": "2026-10-19"}},
    "timeoff_end": {"end_date": {"type": "datepicker", "selected_date": null}}}}}
}`

func TestParseTimeOffSubmit(t *testing.T) {
	req, ok, err := ParseTimeOffSubmit(parse(t, submitPayload))
	if err != nil || !ok || req.Start != domain.NewDate(2026, 10, 19) || !req.End.IsZero() || req.UserID != "U1" {
		t.Fatalf("%+v %v %v", req, ok, err)
	}
	if _, ok, _ := ParseTimeOffSubmit(parse(t, strings.Replace(submitPayload, `"timeoff"`, `"other"`, 1))); ok {
		t.Error("foreign form parsed as time off")
	}
}

func TestPollDMAway(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/Berlin")
	w := isoweek.Week{Year: 2026, Week: 43}
	v := DMView{PollID: 7, Week: w, WeekStart: w.Monday(loc), WeekEnd: w.Day(time.Friday, loc), Away: true,
		Deadline: time.Date(2026, 10, 23, 8, 0, 0, 0, loc), Loc: loc, T: i18n.MustNew("en")}
	msg := PollDM(v)
	text := RenderPlain(msg.Blocks)
	for _, want := range []string{"You are marked as away this week", "[Not away after all]", "[🌴 Plan time off]", "Until Fri, Oct 23, 8:00 AM"} {
		if !strings.Contains(text, want) {
			t.Errorf("away DM lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "[Save]") {
		t.Errorf("away DM must hide the checkboxes:\n%s", text)
	}
	b, _ := json.Marshal(msg.Blocks)
	if !strings.Contains(string(b), `"block_id":"away:7"`) || !strings.Contains(string(b), `"action_id":"undo_away"`) {
		t.Errorf("unexpected block JSON: %s", b)
	}

	v.Away = false
	if text := RenderPlain(PollDM(v).Blocks); !strings.Contains(text, "[🌴 I'm away this week]") || !strings.Contains(text, "[Save]") {
		t.Errorf("normal DM lacks the away button:\n%s", text)
	}
	v.Away, v.Closed = true, true
	if text := RenderPlain(PollDM(v).Blocks); !strings.Contains(text, "You were away this week.") || strings.Contains(text, "[Not away after all]") {
		t.Errorf("closed away DM:\n%s", text)
	}
}

func TestTimeOffModal(t *testing.T) {
	tr := i18n.MustNew("en")
	var upcoming []domain.Absence
	for i := range 25 {
		start := domain.NewDate(2026, 11, 2).AddDays(7 * i)
		upcoming = append(upcoming, domain.Absence{ID: int64(i + 1), Start: start, End: start.AddDays(4)})
	}
	upcoming[0].End = upcoming[0].Start // single day
	m := TimeOffModal(tr, upcoming, domain.NewDate(2026, 10, 19))
	text := RenderPlain(m.Blocks.BlockSet)
	for _, want := range []string{"First day: 2026-10-19", "Last day: 2026-10-19", "Mon, Nov 2, 2026 [Delete]",
		"Mon, Nov 9, 2026 – Fri, Nov 13, 2026 [Delete]", "… and 5 more"} {
		if !strings.Contains(text, want) {
			t.Errorf("form lacks %q:\n%s", want, text)
		}
	}
	if len(m.Blocks.BlockSet) > 100 || m.CallbackID != CallbackTimeOff {
		t.Errorf("blocks=%d callback=%q", len(m.Blocks.BlockSet), m.CallbackID)
	}
	if empty := RenderPlain(TimeOffModal(tr, nil, domain.NewDate(2026, 10, 19)).Blocks.BlockSet); !strings.Contains(empty, "No time off planned.") {
		t.Errorf("empty form:\n%s", empty)
	}
}
