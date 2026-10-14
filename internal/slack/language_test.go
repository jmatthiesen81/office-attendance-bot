package slack

import (
	"strings"
	"testing"

	goslack "github.com/slack-go/slack"

	"officebot/internal/domain"
	"officebot/internal/i18n"
)

func TestParseOpenLanguage(t *testing.T) {
	p := strings.Replace(awayPayload, `"action_id": "away_week"`, `"action_id": "language_open"`, 1)
	req, ok, err := ParseOpenLanguage(parse(t, p))
	if err != nil || !ok || req.TriggerID != "TR1" || req.UserID != "U1" || req.ViewID != "" {
		t.Fatalf("from DM: %+v %v %v", req, ok, err)
	}
	inForm := strings.Replace(viewActionPayload, `"action_id": "timeoff_delete"`, `"action_id": "language_open"`, 1)
	inForm = strings.Replace(inForm, `"user": {"id": "U1", "team_id": "T1"},`, `"user": {"id": "U1", "team_id": "T1"}, "trigger_id": "TR2",`, 1)
	if req, ok, err := ParseOpenLanguage(parse(t, inForm)); err != nil || !ok || req.ViewID != "V1" || req.TriggerID != "TR2" {
		t.Fatalf("from time-off form: %+v %v %v", req, ok, err)
	}
	if _, ok, _ := ParseOpenTimeOff(parse(t, p)); ok {
		t.Error("language click parsed as time off")
	}
}

const languageSubmitPayload = `{
  "type": "view_submission",
  "team": {"id": "T1"},
  "user": {"id": "U1", "team_id": "T1"},
  "view": {"id": "V9", "callback_id": "language", "state": {"values": {
    "language": {"language_select": {"type": "static_select", "selected_option": {"value": "pirate_de", "text": {"type": "plain_text", "text": "Pirat (Deutsch)"}}}}}}}
}`

func TestParseLanguageSubmit(t *testing.T) {
	req, ok, err := ParseLanguageSubmit(parse(t, languageSubmitPayload))
	if err != nil || !ok || req.Language != "pirate_de" || req.UserID != "U1" || req.ViewID != "V9" {
		t.Fatalf("%+v %v %v", req, ok, err)
	}
	if _, ok, _ := ParseTimeOffSubmit(parse(t, languageSubmitPayload)); ok {
		t.Error("language form parsed as time off")
	}
	if _, _, err := ParseLanguageSubmit(parse(t, strings.Replace(languageSubmitPayload, `"pirate_de"`, `""`, 1))); err == nil {
		t.Error("empty selection accepted")
	}
}

func TestLanguageModal(t *testing.T) {
	m := LanguageModal(i18n.MustNew("de"), "nerd_fr", i18n.MustNew("en"), false)
	text := RenderPlain(m.Blocks.BlockSet)
	for _, want := range []string{
		"Sprache: Geek (Français)", // the current choice is preselected
		"🌐 Sprachen: Teamstandard (English); Čeština;",
		"Italiano; 日本語;", "Türkçe; Українська;", // ordered by language code
		"🤓 Nerd: Nerd (Čeština);", "🏴‍☠️ Pirat: Pirát (Čeština);",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("form lacks %q:\n%s", want, text)
		}
	}
	if m.CallbackID != CallbackLanguage || m.Title.Text != "Sprache" {
		t.Errorf("callback=%q title=%q", m.CallbackID, m.Title.Text)
	}
	if n, want := strings.Count(text, ";"), len(i18n.Languages())+1; n != want || n > 100 {
		t.Errorf("%d options, want %d (Slack allows 100)", n, want)
	}

	// Without a choice the default is preselected and names the Slack language.
	text = RenderPlain(LanguageModal(i18n.MustNew("en"), "", i18n.MustNew("ja"), true).Blocks.BlockSet)
	if !strings.Contains(text, "Language: Slack language (日本語)") {
		t.Errorf("default option:\n%s", text)
	}
}

func TestLanguageButtons(t *testing.T) {
	tr := i18n.MustNew("en")
	if text := RenderPlain([]goslack.Block{RequestDMBlock(tr, 7)}); !strings.Contains(text, "[🌐 Language]") {
		t.Errorf("summary buttons:\n%s", text)
	}
	if text := RenderPlain(TimeOffModal(tr, nil, domain.NewDate(2026, 10, 19)).Blocks.BlockSet); !strings.Contains(text, "[🌐 Language]") {
		t.Errorf("time-off form:\n%s", text)
	}
}
