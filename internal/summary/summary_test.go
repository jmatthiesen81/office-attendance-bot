package summary

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	goslack "github.com/slack-go/slack"

	"officebot/internal/domain"
	"officebot/internal/i18n"
	"officebot/internal/slack"
	"officebot/internal/slack/slacktest"
	"officebot/internal/storage"
)

var (
	quiet  = slog.New(slog.NewTextHandler(io.Discard, nil))
	loc, _ = time.LoadLocation("Europe/Berlin")
	t0     = time.Date(2026, 10, 19, 9, 42, 0, 0, loc)
)

func snapshot(n int, responses map[string]domain.Days) *storage.Snapshot {
	snap := &storage.Snapshot{
		Poll: domain.Poll{ID: 1, WorkspaceID: "T1", ISOYear: 2026, ISOWeek: 43, Status: domain.PollOpen,
			WeekStart: time.Date(2026, 10, 19, 0, 0, 0, 0, loc), WeekEnd: time.Date(2026, 10, 23, 0, 0, 0, 0, loc),
			Deadline: time.Date(2026, 10, 23, 8, 0, 0, 0, loc)},
		Responses: map[string]domain.Response{},
	}
	for i := 1; i <= n; i++ {
		id := fmt.Sprintf("U%d", i)
		snap.Participants = append(snap.Participants, domain.Participant{SlackUserID: id, DisplayName: fmt.Sprintf("Name %03d", i)})
	}
	for id, d := range responses {
		snap.Responses[id] = domain.Response{SlackUserID: id, Days: d}
	}
	return snap
}

func TestAggregate(t *testing.T) {
	st := Aggregate(snapshot(6, map[string]domain.Days{
		"U1": {true, false, true, false, true},
		"U2": {true, true, true},
		"U3": {},
		"U4": {false, true, true, true},
	}), loc)
	if st.Answered != 4 || st.Total != 6 {
		t.Errorf("answered %d/%d", st.Answered, st.Total)
	}
	counts := []int{len(st.Days[0]), len(st.Days[1]), len(st.Days[2]), len(st.Days[3]), len(st.Days[4])}
	if fmt.Sprint(counts) != "[2 2 3 1 1]" {
		t.Errorf("counts = %v", counts)
	}
	// "no office day" (U3) is answered, not open
	if len(st.Open) != 2 || st.Open[0].SlackUserID != "U5" || len(st.NoOffice) != 1 || st.NoOffice[0].SlackUserID != "U3" {
		t.Errorf("open=%v noOffice=%v", st.Open, st.NoOffice)
	}
}

func TestRender(t *testing.T) {
	r := Renderer{Loc: loc, MaxNames: 40, T: i18n.MustNew("de")}
	msg := r.Render(snapshot(6, map[string]domain.Days{
		"U1": {true, false, true},
		"U2": {true},
		"U3": {},
		"U4": {false, true},
	}), t0)
	text := slack.RenderPlain(msg.Blocks)
	for _, want := range []string{
		"# 🏢 Büroanwesenheit – KW 43 (19.–23.10.2026)",
		"*Montag (2)*\n<@U1>, <@U2>",
		"*Dienstag (1)*\n<@U4>",
		"*Donnerstag (0)*\n–",
		"*Rückmeldungen:* 4 von 6 Mitarbeitern",
		"*Noch offen:* <@U5>, <@U6>",
		"*Nicht im Büro:* <@U3>",
		"*Letzte Aktualisierung:* 09:42 Uhr",
		"Abgabe bis Fr, 23.10. 08:00 Uhr",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("summary lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(msg.Text, "<@") {
		t.Error("notification fallback text must not mention users")
	}

	if !strings.Contains(text, "[📩 Abfrage an mich senden]") {
		t.Error("request button missing while the poll is open")
	}

	closed := snapshot(1, nil)
	closed.Poll.Status = domain.PollClosed
	closedText := slack.RenderPlain(r.Render(closed, t0).Blocks)
	if !strings.Contains(closedText, "geschlossen") || strings.Contains(closedText, "Abfrage an mich senden") {
		t.Errorf("closed summary:\n%s", closedText)
	}
	// After the deadline the button disappears even before the poll is closed.
	if strings.Contains(slack.RenderPlain(r.Render(snapshot(1, nil), closed.Poll.Deadline).Blocks), "Abfrage an mich senden") {
		t.Error("request button shown after deadline")
	}
}

func TestRenderAway(t *testing.T) {
	snap := snapshot(4, map[string]domain.Days{"U1": {true}, "U2": {true}})
	snap.Absences = []domain.Absence{
		{SlackUserID: "U2", Start: domain.NewDate(2026, 10, 17), End: domain.NewDate(2026, 10, 25)}, // all week
		{SlackUserID: "U3", Start: domain.NewDate(2026, 10, 19), End: domain.NewDate(2026, 10, 20)}, // Mon–Tue only
	}
	st := Aggregate(snap, loc)
	if st.Total != 3 || st.Answered != 1 || len(st.Away) != 1 || len(st.Days[0]) != 1 || len(st.Open) != 2 {
		t.Errorf("total=%d answered=%d away=%v monday=%v open=%v", st.Total, st.Answered, st.Away, st.Days[0], st.Open)
	}
	text := slack.RenderPlain(Renderer{Loc: loc, MaxNames: 40, T: i18n.MustNew("de")}.Render(snap, t0).Blocks)
	for _, want := range []string{
		"*Montag (1)*\n<@U1>",
		"*Rückmeldungen:* 1 von 3 Mitarbeitern · 1 abwesend",
		"*Noch offen:* <@U3>, <@U4>",
		"*Abwesend:* <@U2>",
		"[🌴 Abwesenheit planen]",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("summary lacks %q:\n%s", want, text)
		}
	}
}

func TestRenderLargeTeam(t *testing.T) {
	resp := map[string]domain.Days{}
	for i := 1; i <= 400; i++ {
		resp[fmt.Sprintf("U%d", i)] = domain.Days{true, true, true, true, true}
	}
	msg := Renderer{Loc: loc, MaxNames: 40, T: i18n.MustNew("de")}.Render(snapshot(500, resp), t0)
	if len(msg.Blocks) > 50 {
		t.Fatalf("too many blocks: %d", len(msg.Blocks))
	}
	for _, b := range msg.Blocks {
		if s, ok := b.(*goslack.SectionBlock); ok && len(s.Text.Text) > 3000 {
			t.Errorf("section too long: %d chars", len(s.Text.Text))
		}
	}
	text := slack.RenderPlain(msg.Blocks)
	if !strings.Contains(text, "*Montag (400)*") || !strings.Contains(text, "… und 360 weitere") || !strings.Contains(text, "… und 60 weitere") {
		t.Errorf("truncation missing:\n%s", text[:300])
	}
}

func setup(t *testing.T) (*storage.Store, *slacktest.Fake, *Updater, *domain.Poll) {
	t.Helper()
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	p := snapshot(0, nil).Poll
	p.SummaryChannelID = "CSUM"
	store.CreatePoll(ctx, &p, []domain.Participant{{SlackUserID: "U1"}, {SlackUserID: "U2"}}, t0)
	api := slacktest.New()
	u := NewUpdater(store, api, Renderer{Loc: loc, MaxNames: 40, T: i18n.MustNew("de")}, quiet)
	u.Now = func() time.Time { return t0 }
	return store, api, u, &p
}

func TestUpdaterLifecycle(t *testing.T) {
	ctx := context.Background()
	store, api, u, p := setup(t)

	u.SyncAll(ctx)
	posts := api.PostsTo("CSUM")
	if len(posts) != 1 || strings.Contains(slack.RenderPlain(posts[0].Msg.Blocks), "<@") {
		t.Fatalf("placeholder must be posted without mentions: %+v", posts)
	}
	if ups := api.UpdatesTo("CSUM"); len(ups) != 1 || !strings.Contains(slack.RenderPlain(ups[0].Msg.Blocks), "*Noch offen:* <@U1>, <@U2>") {
		t.Fatalf("full summary not applied via update")
	}

	// nothing changed → no API call
	u.SyncAll(ctx)
	if len(api.UpdatesTo("CSUM")) != 1 {
		t.Error("unchanged summary was updated")
	}

	// a change → exactly one chat.update of the same message, no new post
	store.SaveResponse(ctx, p.ID, "U1", domain.Days{true}, t0)
	u.SyncAll(ctx)
	ups := api.UpdatesTo("CSUM")
	if len(ups) != 2 || ups[1].TS != posts[0].TS || len(api.PostsTo("CSUM")) != 1 {
		t.Fatalf("updates=%d posts=%d", len(ups), len(api.PostsTo("CSUM")))
	}
	if !strings.Contains(slack.RenderPlain(ups[1].Msg.Blocks), "*Montag (1)*\n<@U1>") {
		t.Error("update does not contain new answer")
	}
}

func TestUpdaterRecoversFromErrors(t *testing.T) {
	ctx := context.Background()
	store, api, u, p := setup(t)
	u.SyncAll(ctx)

	api.Fail = func(method, _ string) error {
		if method == "chat.update" {
			return errors.New("connection reset")
		}
		return nil
	}
	store.SaveResponse(ctx, p.ID, "U2", domain.Days{false, true}, t0)
	u.SyncAll(ctx)
	got, _ := store.GetPoll(ctx, p.ID)
	if got.SummarySyncedVersion >= got.SummaryVersion {
		t.Fatal("failed sync marked as synced")
	}

	api.Fail = nil
	u.SyncAll(ctx) // still in backoff
	if n := len(api.UpdatesTo("CSUM")); n != 1 {
		t.Fatalf("retried during backoff: %d", n)
	}
	u.Now = func() time.Time { return t0.Add(time.Minute) }
	u.SyncAll(ctx)
	got, _ = store.GetPoll(ctx, p.ID)
	if got.SummarySyncedVersion != got.SummaryVersion {
		t.Error("summary not consistent after recovery")
	}
}

func TestUpdaterRepostsDeletedSummary(t *testing.T) {
	ctx := context.Background()
	store, api, u, p := setup(t)
	u.SyncAll(ctx)
	first := api.PostsTo("CSUM")[0].TS
	api.Deleted[first] = true

	store.SaveResponse(ctx, p.ID, "U1", domain.Days{true}, t0)
	u.SyncAll(ctx)
	posts := api.PostsTo("CSUM")
	if len(posts) != 2 {
		t.Fatalf("posts = %d", len(posts))
	}
	got, _ := store.GetPoll(ctx, p.ID)
	if got.SummaryMessageTS != posts[1].TS || got.SummarySyncedVersion != got.SummaryVersion {
		t.Errorf("poll after repost: %+v", got)
	}
}

func TestUpdaterFindsOrphanedSummary(t *testing.T) {
	ctx := context.Background()
	store, api, u, p := setup(t)
	// crash after posting: message exists, ts was never stored
	api.LoseNextPostResponse = true
	u.SyncAll(ctx)
	u.Now = func() time.Time { return t0.Add(time.Hour) }
	u.SyncAll(ctx)
	if n := len(api.PostsTo("CSUM")); n != 1 {
		t.Fatalf("summary posted %d times", n)
	}
	got, _ := store.GetPoll(ctx, p.ID)
	if got.SummaryMessageTS == "" {
		t.Error("orphaned summary not adopted")
	}
}
