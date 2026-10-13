package participants

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"testing"

	"officebot/internal/slack/slacktest"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func ids(t *testing.T, s *Service) []string {
	t.Helper()
	parts, err := s.Resolve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, p := range parts {
		out = append(out, p.SlackUserID)
	}
	return out
}

func fake() *slacktest.Fake {
	f := slacktest.New()
	f.AddUser("U1", "Anna")
	f.AddUser("U2", "Peter")
	f.AddUser("U3", "Lisa")
	f.AddUser("UBOT", "officebot").IsBot = true
	f.AddUser("UAPP", "other app").IsAppUser = true
	f.AddUser("UOLD", "left company").Deleted = true
	f.AddUser("UEXT", "guest").TeamID = "T9"
	return f
}

func TestChannelMode(t *testing.T) {
	f := fake()
	f.Members["COFFICE"] = []string{"U1", "UBOT", "U2", "UAPP", "UOLD", "U1", "U3", "UEXT", "USLACKBOT"}
	s := New(f, Source{ChannelID: "COFFICE", Exclude: []string{"U3"}}, "T1", "UBOT", quiet)
	if got := ids(t, s); !reflect.DeepEqual(got, []string{"U1", "U2"}) {
		t.Errorf("participants = %v", got)
	}
}

func TestUserListMode(t *testing.T) {
	f := fake()
	s := New(f, Source{UserIDs: []string{"U3", "U1", "UGONE", "UOLD", "U3"}}, "T1", "UBOT", quiet)
	// unknown user is skipped, deactivated excluded, duplicates removed
	if got := ids(t, s); !reflect.DeepEqual(got, []string{"U3", "U1"}) {
		t.Errorf("participants = %v", got)
	}
}

func TestDirectory(t *testing.T) {
	f := fake()
	s := New(f, Source{Exclude: []string{"U2"}}, "T1", "UBOT", quiet)
	entries, err := s.Directory(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, e := range entries {
		got[e.User.ID] = e.Exclusion
	}
	want := map[string]string{
		"U1": "", "U3": "", "U2": ReasonExcluded, "UBOT": ReasonBot, "UAPP": ReasonBot,
		"UOLD": ReasonDeactivated, "UEXT": ReasonForeignTeam,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("directory = %v", got)
	}
	// sorted by name: Anna (U1) first
	if entries[0].User.ID != "U1" {
		t.Errorf("not sorted by name: %v", entries[0].User)
	}

	f.Members["CTEAM"] = []string{"U3", "UBOT"}
	entries, _ = s.Directory(context.Background(), "CTEAM")
	if len(entries) != 2 {
		t.Errorf("channel filter: %v", entries)
	}
}

func TestChannelError(t *testing.T) {
	s := New(fake(), Source{ChannelID: "CNOPE"}, "T1", "UBOT", quiet)
	if _, err := s.Resolve(context.Background()); err == nil {
		t.Error("expected error for unknown channel")
	}
}
