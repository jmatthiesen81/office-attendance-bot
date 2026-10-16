package storage

import (
	"context"
	"testing"
)

func TestUserLanguage(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	if lang, err := s.GetUserLanguage(ctx, "T1", "U1"); err != nil || lang != "" {
		t.Fatalf("no choice yet: %q %v", lang, err)
	}
	for _, lang := range []string{"fr", "pirate_de"} { // second call overwrites
		if err := s.SetUserLanguage(ctx, "T1", "U1", lang, t0); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetUserLanguage(ctx, "T2", "U1", "ja", t0); err != nil {
		t.Fatal(err)
	}
	if lang, _ := s.GetUserLanguage(ctx, "T1", "U1"); lang != "pirate_de" {
		t.Errorf("language = %q", lang)
	}
	if list, err := s.ListUserLanguages(ctx, "T1"); err != nil || len(list) != 1 || list[0].Language != "pirate_de" || !list[0].UpdatedAt.Equal(t0) {
		t.Errorf("list = %+v %v", list, err)
	}
	if changed, err := s.ResetUserLanguage(ctx, "T1", "U1"); err != nil || !changed {
		t.Fatalf("reset: %v %v", changed, err)
	}
	if changed, _ := s.ResetUserLanguage(ctx, "T1", "U1"); changed {
		t.Error("second reset reported a change")
	}
	// Other workspaces are separate.
	if lang, _ := s.GetUserLanguage(ctx, "T2", "U1"); lang != "ja" {
		t.Errorf("T2 language = %q", lang)
	}
}

func TestSlackLocaleOfLatestPoll(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	if locale, err := s.SlackLocale(ctx, "T1", "U1"); err != nil || locale != "" {
		t.Fatalf("no participation: %q %v", locale, err)
	}
	for week, locale := range map[int]string{43: "de-DE", 44: "en-GB"} {
		parts := users("U1")
		parts[0].SlackLocale = locale
		if _, err := s.CreatePoll(ctx, newPoll(week), parts, t0); err != nil {
			t.Fatal(err)
		}
	}
	if locale, _ := s.SlackLocale(ctx, "T1", "U1"); locale != "en-GB" {
		t.Errorf("locale = %q, want the one of the latest poll", locale)
	}
}

func TestDeleteUserSettingsBefore(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	if _, err := s.CreatePoll(ctx, newPoll(43), users("U1"), t0); err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{"U1", "U2"} {
		if err := s.SetUserLanguage(ctx, "T1", u, "fr", t0); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetUserLanguage(ctx, "T1", "U3", "fr", t0.AddDate(0, 0, 7)); err != nil {
		t.Fatal(err)
	}
	// U1 still takes part in a poll, U3 changed it recently: only U2 goes.
	if n, err := s.DeleteUserSettingsBefore(ctx, t0.AddDate(0, 0, 1)); err != nil || n != 1 {
		t.Fatalf("deleted %d, %v", n, err)
	}
	if lang, _ := s.GetUserLanguage(ctx, "T1", "U2"); lang != "" {
		t.Error("setting of U2 kept")
	}
}

func TestAppState(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	if v, err := s.GetState(ctx, StateRenderedVersion); err != nil || v != "" {
		t.Fatalf("unset: %q %v", v, err)
	}
	for _, v := range []string{"1.0.0", "1.0.1"} { // second call overwrites
		if err := s.SetState(ctx, StateRenderedVersion, v, t0); err != nil {
			t.Fatal(err)
		}
	}
	if v, _ := s.GetState(ctx, StateRenderedVersion); v != "1.0.1" {
		t.Errorf("value = %q", v)
	}
}
