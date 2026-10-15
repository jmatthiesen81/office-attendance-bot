package poll

import (
	"strings"
	"testing"
	"time"

	"officebot/internal/domain"
	"officebot/internal/i18n"
	"officebot/internal/slack/slacktest"
)

// startWithSummary starts the poll and pretends the summary was posted.
func (e *env) startWithSummary() *domain.Poll {
	e.t.Helper()
	p := e.start()
	if err := e.store.SetSummaryMessage(e.ctx, p.ID, "1.1"); err != nil {
		e.t.Fatal(err)
	}
	return p
}

// after advances the clock and runs the jobs due then.
func (e *env) after(d time.Duration) {
	e.t.Helper()
	e.setNow(e.clock().Add(d))
	e.drain()
}

func (e *env) notices() []slacktest.Post {
	var out []slacktest.Post
	for _, post := range e.api.PostsTo("CSUM") {
		if post.Msg.ThreadTS != "" {
			out = append(out, post)
		}
	}
	return out
}

func TestChangeNoticeCombinesChanges(t *testing.T) {
	e := newEnv(t, "U1", "U2")
	p := e.startWithSummary()

	// first answers are never announced
	e.svc.HandleSave(e.ctx, e.saveReq(p, "U1", domain.Days{true, true, true}))
	e.svc.HandleSave(e.ctx, e.saveReq(p, "U2", domain.Days{true}))
	e.after(time.Hour)
	if n := len(e.notices()); n != 0 {
		t.Fatalf("notices after first answers = %d", n)
	}

	// two saves within the delay become one reply with the net change
	e.svc.HandleSave(e.ctx, e.saveReq(p, "U1", domain.Days{true, true, false, true}))
	e.after(time.Minute)
	e.svc.HandleSave(e.ctx, e.saveReq(p, "U1", domain.Days{true, false, false, true}))
	e.after(59 * time.Second)
	if n := len(e.notices()); n != 0 {
		t.Fatalf("notice posted before the delay: %d", n)
	}
	e.after(time.Second)
	got := e.notices()
	if len(got) != 1 {
		t.Fatalf("notices = %d, want 1", len(got))
	}
	want := "🔄 *Name U1* hat die Anwesenheit geändert und kommt nun nicht am Dienstag, Mittwoch, dafür aber am Donnerstag."
	if got[0].Msg.Text != want || got[0].Msg.ThreadTS != "1.1" {
		t.Errorf("notice = %q in thread %q", got[0].Msg.Text, got[0].Msg.ThreadTS)
	}

	// a later change gets its own reply, compared to the announced state
	e.svc.HandleSave(e.ctx, e.saveReq(p, "U1", domain.Days{}))
	e.after(2 * time.Minute)
	if got = e.notices(); len(got) != 2 || !strings.Contains(got[1].Msg.Text, "diese Woche nun doch nicht ins Büro") {
		t.Fatalf("second notice = %+v", got)
	}
	e.svc.HandleSave(e.ctx, e.saveReq(p, "U1", domain.Days{false, false, false, false, true}))
	e.after(2 * time.Minute)
	if got = e.notices(); len(got) != 3 || got[2].Msg.Text != "🔄 *Name U1* kommt nun doch ins Büro: Freitag." {
		t.Fatalf("third notice = %+v", got)
	}
}

func TestChangeNoticeCancelledOut(t *testing.T) {
	e := newEnv(t, "U1")
	p := e.startWithSummary()
	e.svc.HandleSave(e.ctx, e.saveReq(p, "U1", domain.Days{true}))
	e.after(time.Hour)
	e.svc.HandleSave(e.ctx, e.saveReq(p, "U1", domain.Days{true, true}))
	e.svc.HandleSave(e.ctx, e.saveReq(p, "U1", domain.Days{true}))
	e.after(time.Hour)
	if n := len(e.notices()); n != 0 {
		t.Fatalf("notices = %d, want none", n)
	}
	if _, found, _ := e.store.GetChangeNotice(e.ctx, p.ID, "U1"); found {
		t.Error("notice still pending")
	}
	// the next change is announced again
	e.svc.HandleSave(e.ctx, e.saveReq(p, "U1", domain.Days{false, true}))
	e.after(time.Hour)
	if got := e.notices(); len(got) != 1 || got[0].Msg.Text != "🔄 *Name U1* hat die Anwesenheit geändert und kommt nun nicht am Montag, dafür aber am Dienstag." {
		t.Fatalf("notices = %+v", got)
	}
}

// Corrections shortly after the first answer belong to it; the grace
// period counts from the first answer, not from the last correction.
func TestChangeNoticeGracePeriod(t *testing.T) {
	e := newEnv(t, "U1")
	p := e.startWithSummary()
	e.svc.HandleSave(e.ctx, e.saveReq(p, "U1", domain.Days{true, true, true}))
	e.after(5 * time.Minute)
	e.svc.HandleSave(e.ctx, e.saveReq(p, "U1", domain.Days{true, true}))
	e.after(4*time.Minute + 59*time.Second)
	e.svc.HandleSave(e.ctx, e.saveReq(p, "U1", domain.Days{true}))
	e.after(time.Second)
	if n := len(e.notices()); n != 0 {
		t.Fatalf("notices within grace period = %d", n)
	}
	// 10 minutes after the first answer, changes are announced again
	e.svc.HandleSave(e.ctx, e.saveReq(p, "U1", domain.Days{true, true}))
	e.after(2 * time.Minute)
	if got := e.notices(); len(got) != 1 || got[0].Msg.Text != "🔄 *Name U1* kommt jetzt zusätzlich am Dienstag." {
		t.Fatalf("notices = %+v", got)
	}
}

func TestChangeNoticeDisabled(t *testing.T) {
	e := newEnv(t, "U1")
	e.cfg.SummaryChangeNotices = false
	p := e.startWithSummary()
	e.svc.HandleSave(e.ctx, e.saveReq(p, "U1", domain.Days{true}))
	e.svc.HandleSave(e.ctx, e.saveReq(p, "U1", domain.Days{false, true}))
	e.after(time.Hour)
	if n := len(e.notices()); n != 0 {
		t.Fatalf("notices = %d, want none", n)
	}
}

func TestChangeNoticeWaitsForSummary(t *testing.T) {
	e := newEnv(t, "U1")
	p := e.start()
	e.svc.HandleSave(e.ctx, e.saveReq(p, "U1", domain.Days{true}))
	e.after(time.Hour)
	e.svc.HandleSave(e.ctx, e.saveReq(p, "U1", domain.Days{true, true}))
	e.after(2 * time.Minute)
	if n := len(e.notices()); n != 0 {
		t.Fatalf("notice without summary: %d", n)
	}
	if err := e.store.SetSummaryMessage(e.ctx, p.ID, "1.1"); err != nil {
		t.Fatal(err)
	}
	e.after(time.Hour) // retry with backoff
	if got := e.notices(); len(got) != 1 || got[0].Msg.Text != "🔄 *Name U1* kommt jetzt zusätzlich am Dienstag." {
		t.Fatalf("notices = %+v", got)
	}
}

// A change saved while the reply is being posted is announced afterwards.
func TestChangeNoticeChangeWhilePosting(t *testing.T) {
	e := newEnv(t, "U1")
	p := e.startWithSummary()
	e.svc.HandleSave(e.ctx, e.saveReq(p, "U1", domain.Days{true}))
	e.after(time.Hour)
	e.svc.HandleSave(e.ctx, e.saveReq(p, "U1", domain.Days{true, true}))
	e.api.Fail = func(method, channel string) error {
		if method == "chat.postMessage" && channel == "CSUM" {
			e.api.Fail = nil
			if _, _, err := e.store.SaveResponseWithNotice(e.ctx, p.ID, "U1", domain.Days{true, true, true}, e.clock(), e.clock().Add(time.Minute), 0); err != nil {
				t.Error(err)
			}
		}
		return nil
	}
	e.after(2 * time.Minute)
	e.after(2 * time.Minute)
	got := e.notices()
	if len(got) != 2 || got[0].Msg.Text != "🔄 *Name U1* kommt jetzt zusätzlich am Dienstag." ||
		got[1].Msg.Text != "🔄 *Name U1* kommt jetzt zusätzlich am Mittwoch." {
		t.Fatalf("notices = %+v", got)
	}
}

func TestChangeNoticeText(t *testing.T) {
	tr := i18n.MustNew("en")
	cases := []struct {
		before, after domain.Days
		want          string
	}{
		{domain.Days{true}, domain.Days{true}, ""},
		{domain.Days{true, true, true}, domain.Days{true, true, false, true},
			"🔄 *X* changed their attendance: no longer coming in on Wednesday, but on Thursday instead."},
		{domain.Days{true}, domain.Days{true, false, true, false, true}, "🔄 *X* is now also coming in on Wednesday, Friday."},
		{domain.Days{true, true}, domain.Days{true}, "🔄 *X* is no longer coming in on Tuesday."},
		{domain.Days{true, true}, domain.Days{}, "🔄 *X* is no longer coming to the office this week."},
		{domain.Days{}, domain.Days{false, true}, "🔄 *X* is coming to the office after all: Tuesday."},
	}
	for _, c := range cases {
		if got := changeNoticeText(tr, "X", c.before, c.after); got != c.want {
			t.Errorf("%v -> %v: %q, want %q", c.before, c.after, got, c.want)
		}
	}
	if got := displayName(&domain.Participant{SlackUserID: "U1", DisplayName: "*A<b>*"}); got != "A&lt;b&gt;" {
		t.Errorf("escaped name = %q", got)
	}
	if got := displayName(&domain.Participant{SlackUserID: "U1"}); got != "<@U1>" {
		t.Errorf("fallback name = %q", got)
	}
}
