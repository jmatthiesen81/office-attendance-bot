package poll

import (
	"strings"
	"testing"
	"time"

	"officebot/internal/domain"
	"officebot/internal/slack"
)

func (e *env) setLanguage(user, lang string) {
	e.t.Helper()
	if errs := e.svc.SubmitLanguage(e.ctx, slack.Interaction{TeamID: "T1", UserID: user, Language: lang}); errs != nil {
		e.t.Fatalf("submit %s: %v", lang, errs)
	}
	e.drain()
}

func TestMixedLanguages(t *testing.T) {
	e := newEnv(t, "U1", "U2") // team default: de
	e.setLanguage("U2", "en")
	p := e.start()
	if dm := e.lastDM("U1"); !strings.Contains(dm, "[Speichern]") || !strings.Contains(dm, "[🌐 Sprache]") {
		t.Errorf("U1 DM not in the team language:\n%s", dm)
	}
	if dm := e.lastDM("U2"); !strings.Contains(dm, "[Save]") || !strings.Contains(dm, "week 43") {
		t.Errorf("U2 DM not in English:\n%s", dm)
	}

	e.setNow(time.Date(2026, 10, 20, 9, 0, 0, 0, e.loc)) // Tuesday reminder round
	if err := e.svc.RunDueReminders(e.ctx, e.clock()); err != nil {
		t.Fatal(err)
	}
	e.drain()
	reminder := func(user string) string {
		posts := e.api.PostsTo("D" + user)
		if len(posts) != 2 {
			t.Fatalf("%s: %d posts, want DM and reminder", user, len(posts))
		}
		return posts[1].Msg.Text
	}
	if r := reminder("U1"); !strings.Contains(r, "Erinnerung") {
		t.Errorf("U1 reminder: %s", r)
	}
	if r := reminder("U2"); !strings.Contains(r, "Reminder") {
		t.Errorf("U2 reminder: %s", r)
	}

	// Replies to clicks use the clicking person's language, too.
	e.setNow(p.Deadline.Add(time.Minute))
	e.svc.HandleSave(e.ctx, e.saveReq(p, "U2", domain.Days{true}))
	if got := e.api.Ephemerals[len(e.api.Ephemerals)-1]; !strings.HasPrefix(got, "U2: 🔒 The poll has been closed") {
		t.Errorf("reply = %q", got)
	}
}

func TestLanguageChangeWhileDMOpen(t *testing.T) {
	e := newEnv(t, "U1")
	p := e.start()
	e.svc.HandleSave(e.ctx, e.saveReq(p, "U1", domain.Days{true, false, true}))

	e.setLanguage("U1", "fr")
	dm := e.lastDM("U1")
	if !strings.Contains(dm, "[Enregistrer]") || !strings.Contains(dm, "[x] Lundi") {
		t.Errorf("DM not re-rendered in French with the saved answer:\n%s", dm)
	}
	if lang, _ := e.store.GetUserLanguage(e.ctx, "T1", "U1"); lang != "fr" {
		t.Errorf("stored language = %q", lang)
	}

	e.setLanguage("U1", slack.LanguageDefault)
	if dm := e.lastDM("U1"); !strings.Contains(dm, "[Speichern]") {
		t.Errorf("DM not back in the team language:\n%s", dm)
	}
	if lang, _ := e.store.GetUserLanguage(e.ctx, "T1", "U1"); lang != "" {
		t.Errorf("choice not cleared: %q", lang)
	}
}

func TestUnknownLanguage(t *testing.T) {
	e := newEnv(t, "U1")
	errs := e.svc.SubmitLanguage(e.ctx, slack.Interaction{TeamID: "T1", UserID: "U1", Language: "xx"})
	if errs[slack.LanguageBlock] == "" {
		t.Fatalf("unknown language accepted: %v", errs)
	}
	if lang, _ := e.store.GetUserLanguage(e.ctx, "T1", "U1"); lang != "" {
		t.Errorf("stored %q", lang)
	}

	// A stored language whose catalog is gone (rollback) falls back to the
	// team default, but stays stored.
	if err := e.store.SetUserLanguage(e.ctx, "T1", "U1", "xx", e.clock()); err != nil {
		t.Fatal(err)
	}
	e.start()
	if dm := e.lastDM("U1"); !strings.Contains(dm, "[Speichern]") {
		t.Errorf("DM:\n%s", dm)
	}
	if lang, _ := e.store.GetUserLanguage(e.ctx, "T1", "U1"); lang != "xx" {
		t.Errorf("stored language dropped: %q", lang)
	}
}

func TestLanguageFromSlack(t *testing.T) {
	e := newEnv(t, "U1", "U2", "U3")
	e.cfg.LanguageFromSlack = true
	e.api.Users["U1"].Locale = "en-US"
	e.api.Users["U2"].Locale = "ko-KR" // no catalog: team default
	e.api.Users["U3"].Locale = "en-US"
	e.setLanguage("U3", "ja") // own choice beats the Slack language
	e.start()
	for user, want := range map[string]string{"U1": "[Save]", "U2": "[Speichern]", "U3": "[保存]"} {
		if dm := e.lastDM(user); !strings.Contains(dm, want) {
			t.Errorf("%s DM lacks %s:\n%s", user, want, dm)
		}
	}

	e.svc.HandleOpenLanguage(e.ctx, slack.Interaction{TeamID: "T1", UserID: "U1", TriggerID: "TR1"})
	v, _ := e.api.LastView()
	if form := slack.RenderPlain(v.View.Blocks.BlockSet); !strings.Contains(form, "Language: Slack language (English)") {
		t.Errorf("form of U1:\n%s", form)
	}

	// Without the setting, the Slack language is ignored.
	e.cfg.LanguageFromSlack = false
	e.svc.HandleOpenLanguage(e.ctx, slack.Interaction{TeamID: "T1", UserID: "U1", TriggerID: "TR2"})
	v, _ = e.api.LastView()
	if form := slack.RenderPlain(v.View.Blocks.BlockSet); !strings.Contains(form, "Sprache: Teamstandard (Deutsch)") {
		t.Errorf("form of U1:\n%s", form)
	}
}

func TestOpenLanguageForm(t *testing.T) {
	e := newEnv(t, "U1")
	e.setLanguage("U1", "pirate_en")
	e.svc.HandleOpenLanguage(e.ctx, slack.Interaction{TeamID: "T1", UserID: "U1", TriggerID: "TR1"})
	v, ok := e.api.LastView()
	if !ok || v.TriggerID != "TR1" || v.Pushed || v.View.CallbackID != slack.CallbackLanguage {
		t.Fatalf("view = %+v", v)
	}
	if form := slack.RenderPlain(v.View.Blocks.BlockSet); !strings.Contains(form, "Language: Pirate (English)") || !strings.Contains(form, "matey") {
		t.Errorf("form not in the chosen language with the choice preselected:\n%s", form)
	}

	// From the time-off form, the language form is pushed on top of it.
	e.svc.HandleOpenLanguage(e.ctx, slack.Interaction{TeamID: "T1", UserID: "U1", TriggerID: "TR2", ViewID: "V1"})
	if v, _ := e.api.LastView(); !v.Pushed || v.TriggerID != "TR2" {
		t.Errorf("view = %+v", v)
	}

	// Foreign workspaces are ignored.
	e.svc.HandleOpenLanguage(e.ctx, slack.Interaction{TeamID: "T9", UserID: "U1", TriggerID: "TR3"})
	if errs := e.svc.SubmitLanguage(e.ctx, slack.Interaction{TeamID: "T9", UserID: "U1", Language: "fr"}); errs != nil {
		t.Errorf("errs = %v", errs)
	}
	if len(e.api.Views) != 2 {
		t.Errorf("views = %d", len(e.api.Views))
	}
	if lang, _ := e.store.GetUserLanguage(e.ctx, "T1", "U1"); lang != "pirate_en" {
		t.Errorf("foreign workspace changed the language: %q", lang)
	}
}
