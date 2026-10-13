package slack

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	goslack "github.com/slack-go/slack"

	"officebot/internal/domain"
	"officebot/internal/i18n"
	"officebot/internal/isoweek"
)

// payload is a trimmed real-world block_actions payload.
const payload = `{
  "type": "block_actions",
  "team": {"id": "T1", "domain": "acme"},
  "user": {"id": "U1", "username": "anna", "team_id": "T1"},
  "container": {"type": "message", "message_ts": "1700000000.000100", "channel_id": "DU1", "is_ephemeral": false},
  "channel": {"id": "DU1", "name": "directmessage"},
  "message": {"ts": "1700000000.000100", "text": "x"},
  "state": {"values": {"days:7:2": {"days_select": {"type": "checkboxes", "selected_options": [
      {"text": {"type": "plain_text", "text": "Montag"}, "value": "mon"},
      {"text": {"type": "plain_text", "text": "Freitag"}, "value": "fri"}]}}}},
  "actions": [{"action_id": "save_days", "block_id": "days:7:2", "type": "button", "value": "7", "action_ts": "1700000001.1"}]
}`

func parse(t *testing.T, s string) *goslack.InteractionCallback {
	t.Helper()
	var cb goslack.InteractionCallback
	if err := json.Unmarshal([]byte(s), &cb); err != nil {
		t.Fatal(err)
	}
	return &cb
}

func TestParseSave(t *testing.T) {
	req, ok, err := ParseSave(parse(t, payload))
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	want := Interaction{TeamID: "T1", UserID: "U1", PollID: 7, ChannelID: "DU1", MessageTS: "1700000000.000100",
		Days: domain.Days{true, false, false, false, true}}
	if req != want {
		t.Errorf("got %+v", req)
	}
}

func TestParseSaveEmptySelection(t *testing.T) {
	p := strings.Replace(payload, `"selected_options": [
      {"text": {"type": "plain_text", "text": "Montag"}, "value": "mon"},
      {"text": {"type": "plain_text", "text": "Freitag"}, "value": "fri"}]`, `"selected_options": []`, 1)
	req, ok, err := ParseSave(parse(t, p))
	if err != nil || !ok || req.Days.Count() != 0 {
		t.Fatalf("%+v %v %v", req, ok, err)
	}
}

func TestParseCheckboxTickIsIgnored(t *testing.T) {
	p := strings.Replace(payload, `"action_id": "save_days", "block_id": "days:7:2", "type": "button"`, `"action_id": "days_select", "block_id": "days:7:2", "type": "checkboxes"`, 1)
	if _, ok, _ := ParseSave(parse(t, p)); ok {
		t.Error("checkbox interaction must not be treated as save")
	}
}

func TestParseRejectsManipulation(t *testing.T) {
	cases := map[string][2]string{
		"unknown day":         {`"value": "fri"`, `"value": "sat"`},
		"poll/block mismatch": {`"value": "7", "action_ts"`, `"value": "8", "action_ts"`},
		"bad poll id":         {`"value": "7", "action_ts"`, `"value": "x", "action_ts"`},
	}
	for name, r := range cases {
		if _, _, err := ParseSave(parse(t, strings.Replace(payload, r[0], r[1], 1))); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	noState := strings.Replace(payload, `"days:7:2": {"days_select"`, `"other": {"days_select"`, 1)
	if _, _, err := ParseSave(parse(t, noState)); !errors.Is(err, ErrMissingState) {
		t.Errorf("missing state: %v", err)
	}
}

func TestPollDMRoundTrip(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/Berlin")
	w := isoweek.Week{Year: 2026, Week: 43}
	v := DMView{PollID: 7, Week: w, WeekStart: w.Monday(loc), WeekEnd: w.Day(time.Friday, loc),
		Deadline: time.Date(2026, 10, 23, 8, 0, 0, 0, loc), Loc: loc, T: i18n.MustNew("de"),
		Response: &domain.Response{Days: domain.Days{false, true}, Revision: 2, UpdatedAt: time.Date(2026, 10, 19, 9, 42, 0, 0, loc)}}
	msg := PollDM(v)
	text := RenderPlain(msg.Blocks)
	for _, want := range []string{"# 🏢 Büroplanung – KW 43 (19.–23.10.2026)", "[ ] Montag", "[x] Dienstag", "[Speichern]",
		"✅ Gespeichert (Mo, 19.10. 09:42 Uhr): *Di*", "bis Fr, 23.10. 08:00 Uhr"} {
		if !strings.Contains(text, want) {
			t.Errorf("DM lacks %q:\n%s", want, text)
		}
	}
	// the action block ID encodes poll and revision, matching ParseSave's check
	b, _ := json.Marshal(msg.Blocks)
	if !strings.Contains(string(b), `"block_id":"days:7:2"`) || !strings.Contains(string(b), `"value":"7"`) {
		t.Errorf("unexpected block JSON: %s", b)
	}

	v.Closed = true
	closed := RenderPlain(PollDM(v).Blocks)
	if strings.Contains(closed, "[Speichern]") || !strings.Contains(closed, "geschlossen") || !strings.Contains(closed, "*Di*") {
		t.Errorf("closed DM:\n%s", closed)
	}
}

func TestPollDMEnglish(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/Berlin")
	w := isoweek.Week{Year: 2026, Week: 43}
	v := DMView{PollID: 7, Week: w, WeekStart: w.Monday(loc), WeekEnd: w.Day(time.Friday, loc), NextWeek: true,
		Deadline: time.Date(2026, 10, 23, 8, 0, 0, 0, loc), Loc: loc, T: i18n.MustNew("en"),
		Response: &domain.Response{Days: domain.Days{true, false, true}, Revision: 1, UpdatedAt: time.Date(2026, 10, 19, 9, 42, 0, 0, loc)}}
	text := RenderPlain(PollDM(v).Blocks)
	for _, want := range []string{"# 🏢 Office planning – week 43 (Oct 19–23, 2026)", "in the office next week?", "[x] Monday",
		"[ ] Tuesday", "[Save]", "✅ Saved (Mon, Oct 19, 9:42 AM): *Mon, Wed*", "until Fri, Oct 23, 8:00 AM"} {
		if !strings.Contains(text, want) {
			t.Errorf("DM lacks %q:\n%s", want, text)
		}
	}
}

func TestErrorClassification(t *testing.T) {
	if !IsPermanent(goslack.SlackErrorResponse{Err: "not_in_channel"}) || IsPermanent(errors.New("timeout")) {
		t.Error("IsPermanent")
	}
	if d, ok := RetryAfter(&goslack.RateLimitedError{RetryAfter: 3 * time.Second}); !ok || d != 3*time.Second {
		t.Error("RetryAfter")
	}
	if !IsMessageGone(goslack.SlackErrorResponse{Err: "message_not_found"}) {
		t.Error("IsMessageGone")
	}
}

func TestRecentSet(t *testing.T) {
	r := newRecentSet(2)
	if !r.add("a") || r.add("a") || !r.add("b") || !r.add("c") || !r.add("a") {
		t.Error("recent set semantics")
	}
}

const requestPayload = `{
  "type": "block_actions",
  "team": {"id": "T1"},
  "user": {"id": "U9", "team_id": "T1"},
  "container": {"type": "message", "message_ts": "1700000000.000200", "channel_id": "CSUM"},
  "channel": {"id": "CSUM"},
  "actions": [{"action_id": "request_dm", "block_id": "request:7", "type": "button", "value": "7", "action_ts": "1700000001.2"}]
}`

func TestParseRequestDM(t *testing.T) {
	req, ok, err := ParseRequestDM(parse(t, requestPayload))
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	want := Interaction{TeamID: "T1", UserID: "U9", PollID: 7, ChannelID: "CSUM", MessageTS: "1700000000.000200"}
	if req != want {
		t.Errorf("got %+v", req)
	}
	// A save click is not a DM request and vice versa.
	if _, ok, _ := ParseRequestDM(parse(t, payload)); ok {
		t.Error("save click parsed as DM request")
	}
	if _, ok, _ := ParseSave(parse(t, requestPayload)); ok {
		t.Error("DM request parsed as save click")
	}
	bad := strings.Replace(requestPayload, `"value": "7"`, `"value": "8"`, 1)
	if _, _, err := ParseRequestDM(parse(t, bad)); err == nil {
		t.Error("poll/block mismatch accepted")
	}
}

func TestRequestDMBlockRoundTrip(t *testing.T) {
	b, _ := json.Marshal(RequestDMBlock(i18n.MustNew("de"), 7))
	if !strings.Contains(string(b), `"block_id":"request:7"`) || !strings.Contains(string(b), `"action_id":"request_dm"`) {
		t.Errorf("unexpected block JSON: %s", b)
	}
}
