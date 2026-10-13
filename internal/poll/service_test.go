package poll

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	goslack "github.com/slack-go/slack"

	"officebot/internal/config"
	"officebot/internal/domain"
	"officebot/internal/isoweek"
	"officebot/internal/participants"
	"officebot/internal/slack"
	"officebot/internal/slack/slacktest"
	"officebot/internal/storage"
	"officebot/internal/worker"
)

type env struct {
	t      *testing.T
	ctx    context.Context
	cfg    *config.Config
	store  *storage.Store
	api    *slacktest.Fake
	svc    *Service
	outbox *worker.Outbox
	now    time.Time
	nowMu  sync.Mutex
	loc    *time.Location
	trig   atomic.Int32
}

func (e *env) Trigger() { e.trig.Add(1) }
func (e *env) Notify()  {}

func (e *env) setNow(t time.Time) { e.nowMu.Lock(); e.now = t; e.nowMu.Unlock() }
func (e *env) clock() time.Time   { e.nowMu.Lock(); defer e.nowMu.Unlock(); return e.now }

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func newEnv(t *testing.T, users ...string) *env {
	t.Helper()
	get := map[string]string{
		"DRY_RUN": "true", "SLACK_SUMMARY_CHANNEL_ID": "CSUM", "SLACK_USER_IDS": strings.Join(users, ","),
		"REMINDER_MAX_COUNT": "1", "BOT_LANGUAGE": "de",
	}
	cfg, err := config.LoadFrom(func(k string) string { return get[k] })
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	api := slacktest.New()
	for _, u := range users {
		api.AddUser(u, "Name "+u)
	}
	e := &env{t: t, ctx: context.Background(), cfg: cfg, store: store, api: api, loc: cfg.Location}
	e.setNow(time.Date(2026, 10, 19, 9, 0, 0, 0, cfg.Location)) // Monday KW 43, 09:00
	resolver := participants.New(api, participants.Source{UserIDs: cfg.UserIDs}, "T1", "UBOT", quiet)
	e.svc = NewService(store, api, resolver, cfg, "T1", e, e, quiet)
	e.svc.Now = e.clock
	e.outbox = worker.NewOutbox(store, e.svc, quiet)
	e.outbox.Now = e.clock
	return e
}

var kw43 = isoweek.Week{Year: 2026, Week: 43}

func (e *env) start() *domain.Poll {
	e.t.Helper()
	p, _, err := e.svc.StartWeek(e.ctx, kw43)
	if err != nil {
		e.t.Fatal(err)
	}
	e.drain()
	return p
}

// drain runs all due outbox jobs.
func (e *env) drain() {
	e.t.Helper()
	if _, err := e.outbox.ProcessDue(e.ctx); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) saveReq(p *domain.Poll, user string, days domain.Days) slack.Interaction {
	part, err := e.store.GetParticipant(e.ctx, p.ID, user)
	if err != nil {
		e.t.Fatal(err)
	}
	return slack.Interaction{TeamID: "T1", UserID: user, PollID: p.ID, ChannelID: part.DMChannelID, MessageTS: part.DMMessageTS, Days: days}
}

func TestStartWeekIdempotent(t *testing.T) {
	e := newEnv(t, "U1", "U2", "U3")
	p1, created, err := e.svc.StartWeek(e.ctx, kw43)
	if err != nil || !created {
		t.Fatal(created, err)
	}
	p2, created, err := e.svc.StartWeek(e.ctx, kw43)
	if err != nil || created || p2.ID != p1.ID {
		t.Fatalf("second start: %v %v", created, err)
	}
	e.drain()
	if n := len(e.api.Posts); n != 3 {
		t.Errorf("posts = %d, want 3 DMs", n)
	}
	// restart: EnsureCurrentPoll must not create anything new
	if err := e.svc.EnsureCurrentPoll(e.ctx, e.clock()); err != nil {
		t.Fatal(err)
	}
	e.drain()
	if n := len(e.api.Posts); n != 3 {
		t.Errorf("posts after restart = %d", n)
	}
	if p1.Deadline.In(e.loc).Format("Mon 15:04") != "Fri 08:00" {
		t.Errorf("deadline = %v", p1.Deadline.In(e.loc))
	}
}

func TestDMContent(t *testing.T) {
	e := newEnv(t, "U1")
	e.start()
	posts := e.api.PostsTo("DU1")
	if len(posts) != 1 {
		t.Fatalf("posts = %d", len(posts))
	}
	text := slack.RenderPlain(posts[0].Msg.Blocks)
	for _, want := range []string{"Büroplanung – KW 43 (19.–23.10.2026)", "diese Woche im Büro", "[ ] Montag", "[ ] Freitag", "[Speichern]"} {
		if !strings.Contains(text, want) {
			t.Errorf("DM lacks %q:\n%s", want, text)
		}
	}
	if posts[0].Msg.Metadata == nil || posts[0].Msg.Metadata.EventType != slack.MetaPollDM {
		t.Error("DM without metadata")
	}
}

func TestSaveFlow(t *testing.T) {
	e := newEnv(t, "U1", "U2")
	p := e.start()

	e.svc.HandleSave(e.ctx, e.saveReq(p, "U1", domain.Days{true, false, true}))
	r, _ := e.store.GetResponse(e.ctx, p.ID, "U1")
	if r == nil || r.Days != (domain.Days{true, false, true}) {
		t.Fatalf("response = %+v", r)
	}
	ups := e.api.UpdatesTo("DU1")
	if len(ups) != 1 {
		t.Fatalf("DM updates = %d", len(ups))
	}
	text := slack.RenderPlain(ups[0].Msg.Blocks)
	if !strings.Contains(text, "[x] Montag") || !strings.Contains(text, "[ ] Dienstag") || !strings.Contains(text, "Gespeichert") || !strings.Contains(text, "Mo, Mi") {
		t.Errorf("updated DM:\n%s", text)
	}
	if e.trig.Load() == 0 {
		t.Error("summary not triggered")
	}

	// duplicate interaction: no new revision, no new summary trigger
	before := e.trig.Load()
	e.svc.HandleSave(e.ctx, e.saveReq(p, "U1", domain.Days{true, false, true}))
	r, _ = e.store.GetResponse(e.ctx, p.ID, "U1")
	if r.Revision != 1 || e.trig.Load() != before {
		t.Errorf("duplicate changed state: rev %d", r.Revision)
	}

	// empty selection is a valid answer
	e.svc.HandleSave(e.ctx, e.saveReq(p, "U2", domain.Days{}))
	r, _ = e.store.GetResponse(e.ctx, p.ID, "U2")
	if r == nil || r.Days.Count() != 0 {
		t.Errorf("empty answer not stored: %+v", r)
	}
	text = slack.RenderPlain(e.api.UpdatesTo("DU2")[0].Msg.Blocks)
	if !strings.Contains(text, "kein Bürotag") {
		t.Errorf("empty answer confirmation:\n%s", text)
	}
}

func TestSaveValidation(t *testing.T) {
	e := newEnv(t, "U1", "U2")
	p := e.start()
	good := e.saveReq(p, "U1", domain.Days{true})

	cases := map[string]func(r *slack.Interaction){
		"foreign workspace": func(r *slack.Interaction) { r.TeamID = "T2" },
		"non participant":   func(r *slack.Interaction) { r.UserID = "UX" },
		"unknown poll":      func(r *slack.Interaction) { r.PollID = 999 },
		"other user's DM":   func(r *slack.Interaction) { r.UserID = "U2" },
		"stale message":     func(r *slack.Interaction) { r.MessageTS = "1.0" },
	}
	for name, mutate := range cases {
		req := good
		mutate(&req)
		e.svc.HandleSave(e.ctx, req)
		snap, _ := e.store.PollSnapshot(e.ctx, p.ID)
		if len(snap.Responses) != 0 {
			t.Fatalf("%s: response accepted", name)
		}
	}
	if e.api.EphemeralCount() != 4 { // foreign workspace gets no reply
		t.Errorf("ephemerals = %d", e.api.EphemeralCount())
	}
}

func TestDeadline(t *testing.T) {
	e := newEnv(t, "U1", "U2")
	p := e.start()
	e.svc.HandleSave(e.ctx, e.saveReq(p, "U1", domain.Days{false, true}))

	// Friday 08:00: closed, even before the scheduler ran
	e.setNow(time.Date(2026, 10, 23, 8, 0, 0, 0, e.loc))
	e.svc.HandleSave(e.ctx, e.saveReq(p, "U1", domain.Days{true}))
	r, _ := e.store.GetResponse(e.ctx, p.ID, "U1")
	if r.Days != (domain.Days{false, true}) {
		t.Errorf("save after deadline accepted: %+v", r.Days)
	}
	if !strings.Contains(e.api.Ephemerals[len(e.api.Ephemerals)-1], "geschlossen") {
		t.Error("user not informed about closed poll")
	}

	if err := e.svc.CloseDue(e.ctx, e.clock()); err != nil {
		t.Fatal(err)
	}
	e.drain()
	got, _ := e.store.GetPoll(e.ctx, p.ID)
	if got.Status != domain.PollClosed {
		t.Fatal("poll not closed")
	}
	ups := e.api.UpdatesTo("DU2")
	last := slack.RenderPlain(ups[len(ups)-1].Msg.Blocks)
	if !strings.Contains(last, "geschlossen") || strings.Contains(last, "[Speichern]") {
		t.Errorf("closed DM still interactive:\n%s", last)
	}
}

func TestDeadlineConfigurable(t *testing.T) {
	e := newEnv(t, "U1")
	e.cfg.DeadlineWeekday, e.cfg.DeadlineTime = time.Wednesday, config.TimeOfDay{Hour: 12}
	p := e.start()
	if got := p.Deadline.In(e.loc).Format("2006-01-02 15:04"); got != "2026-10-21 12:00" {
		t.Errorf("deadline = %s", got)
	}
}

func TestCatchUpAfterDowntime(t *testing.T) {
	e := newEnv(t, "U1")
	// Bot was down on Monday 09:00; starts Wednesday → poll is created.
	e.setNow(time.Date(2026, 10, 21, 14, 0, 0, 0, e.loc))
	if err := e.svc.EnsureCurrentPoll(e.ctx, e.clock()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.GetPollByWeek(e.ctx, "T1", 2026, 43); err != nil {
		t.Fatal("missed poll not caught up")
	}

	// Started after the deadline of week 44 → nothing is created.
	e2 := newEnv(t, "U1")
	e2.setNow(time.Date(2026, 10, 30, 9, 0, 0, 0, e2.loc))
	e2.svc.EnsureCurrentPoll(e2.ctx, e2.clock())
	if _, err := e2.store.GetPollByWeek(e2.ctx, "T1", 2026, 44); !errors.Is(err, storage.ErrNotFound) {
		t.Error("poll created after deadline")
	}

	// Before Monday 07:00 the previous week's poll is the target, not the new one.
	e3 := newEnv(t, "U1")
	e3.setNow(time.Date(2026, 10, 26, 6, 59, 0, 0, e3.loc))
	e3.svc.EnsureCurrentPoll(e3.ctx, e3.clock())
	if _, err := e3.store.GetPollByWeek(e3.ctx, "T1", 2026, 44); !errors.Is(err, storage.ErrNotFound) {
		t.Error("poll created before schedule")
	}
}

func TestYearBoundaryPoll(t *testing.T) {
	e := newEnv(t, "U1")
	e.setNow(time.Date(2026, 12, 28, 10, 0, 0, 0, e.loc)) // Monday of 2026-W53
	if err := e.svc.EnsureCurrentPoll(e.ctx, e.clock()); err != nil {
		t.Fatal(err)
	}
	p, err := e.store.GetPollByWeek(e.ctx, "T1", 2026, 53)
	if err != nil {
		t.Fatal(err)
	}
	if p.Deadline.In(e.loc).Format("2006-01-02") != "2027-01-01" {
		t.Errorf("deadline = %v", p.Deadline.In(e.loc))
	}
	// A Friday start in 2026-W53 targets 2027-W01.
	e.cfg.PollWeekday = time.Friday
	e.setNow(time.Date(2027, 1, 1, 10, 0, 0, 0, e.loc))
	if w, _ := e.svc.CurrentTargetWeek(e.clock()); w != (isoweek.Week{Year: 2027, Week: 1}) {
		t.Errorf("target = %v", w)
	}
}

func TestPollAfterMondayTargetsNextWeek(t *testing.T) {
	e := newEnv(t, "U1")
	e.cfg.PollWeekday = time.Friday
	e.cfg.DeadlineWeekday = time.Monday
	e.setNow(time.Date(2026, 10, 23, 7, 30, 0, 0, e.loc)) // Friday KW 43, after the start
	if err := e.svc.EnsureCurrentPoll(e.ctx, e.clock()); err != nil {
		t.Fatal(err)
	}
	p, err := e.store.GetPollByWeek(e.ctx, "T1", 2026, 44)
	if err != nil {
		t.Fatal("next week's poll not started")
	}
	if got := p.Deadline.In(e.loc).Format("2006-01-02 15:04"); got != "2026-10-26 08:00" {
		t.Errorf("deadline = %s", got)
	}
	e.drain()
	posts := e.api.PostsTo("DU1")
	if len(posts) != 1 {
		t.Fatalf("DM posts = %d", len(posts))
	}
	if dm := slack.RenderPlain(posts[0].Msg.Blocks); !strings.Contains(dm, "nächste Woche") || !strings.Contains(dm, "KW 44") {
		t.Errorf("DM does not ask about next week:\n%s", dm)
	}
}

func TestDMRetryAndRateLimit(t *testing.T) {
	e := newEnv(t, "U1")
	var calls atomic.Int32
	e.api.Fail = func(method, _ string) error {
		if method == "chat.postMessage" && calls.Add(1) <= 2 {
			if calls.Load() == 1 {
				return &goslack.RateLimitedError{RetryAfter: 3 * time.Second}
			}
			return errors.New("dial tcp: i/o timeout")
		}
		return nil
	}
	p, _, _ := e.svc.StartWeek(e.ctx, kw43)
	e.drain()
	if len(e.api.Posts) != 0 {
		t.Fatal("posted despite errors")
	}
	// retry after Retry-After (+1s)
	e.setNow(e.clock().Add(5 * time.Second))
	e.drain()
	e.setNow(e.clock().Add(time.Hour))
	e.drain()
	part, _ := e.store.GetParticipant(e.ctx, p.ID, "U1")
	if part.DeliveryStatus != domain.DeliverySent || len(e.api.Posts) != 1 {
		t.Errorf("status %s, posts %d", part.DeliveryStatus, len(e.api.Posts))
	}
}

func TestNoDuplicateDMAfterLostResponse(t *testing.T) {
	e := newEnv(t, "U1")
	e.api.LoseNextPostResponse = true // Slack posted, but we never got the ts
	p, _, _ := e.svc.StartWeek(e.ctx, kw43)
	e.drain()
	e.setNow(e.clock().Add(time.Hour))
	e.drain()
	if n := len(e.api.PostsTo("DU1")); n != 1 {
		t.Fatalf("DM posted %d times", n)
	}
	part, _ := e.store.GetParticipant(e.ctx, p.ID, "U1")
	if part.DMMessageTS != e.api.Posts[0].TS {
		t.Error("recovered ts not stored")
	}
}

func TestUnreachableUser(t *testing.T) {
	e := newEnv(t, "U1", "U2")
	e.api.Fail = func(method, ch string) error {
		if method == "chat.postMessage" && ch == "DU2" {
			return slacktest.SlackError("user_disabled")
		}
		return nil
	}
	p := e.start()
	part, _ := e.store.GetParticipant(e.ctx, p.ID, "U2")
	if part.DeliveryStatus != domain.DeliveryUnreachable {
		t.Errorf("status = %s", part.DeliveryStatus)
	}
	stats, _ := e.store.JobStats(e.ctx)
	if stats["failed"] != 1 || stats["pending"] != 0 {
		t.Errorf("jobs = %v", stats)
	}
}

func TestSlackFailureKeepsResponse(t *testing.T) {
	e := newEnv(t, "U1")
	p := e.start()
	e.api.Fail = func(method, _ string) error {
		if method == "chat.update" {
			return errors.New("connection refused")
		}
		return nil
	}
	e.svc.HandleSave(e.ctx, e.saveReq(p, "U1", domain.Days{true}))
	if r, _ := e.store.GetResponse(e.ctx, p.ID, "U1"); r == nil {
		t.Fatal("response lost because Slack failed")
	}
	// DM update is retried via outbox once Slack is back
	e.api.Fail = nil
	e.setNow(e.clock().Add(time.Minute))
	e.drain()
	if len(e.api.UpdatesTo("DU1")) != 1 {
		t.Error("DM update not retried")
	}
}

func TestReminders(t *testing.T) {
	e := newEnv(t, "U1", "U2")
	p := e.start()
	e.svc.HandleSave(e.ctx, e.saveReq(p, "U1", domain.Days{true}))

	// Monday: not due yet
	e.svc.RunDueReminders(e.ctx, e.clock())
	e.drain()
	if len(e.api.Posts) != 2 {
		t.Fatal("reminder sent too early")
	}
	// Tuesday 09:00 → reminder for U2 only
	e.setNow(time.Date(2026, 10, 20, 9, 0, 0, 0, e.loc))
	e.svc.RunDueReminders(e.ctx, e.clock())
	e.drain()
	e.svc.RunDueReminders(e.ctx, e.clock()) // idempotent
	e.drain()
	reminders := e.api.PostsTo("DU2")[1:]
	if len(reminders) != 1 || len(e.api.PostsTo("DU1")) != 1 {
		t.Fatalf("reminders U2=%d U1=%d", len(reminders), len(e.api.PostsTo("DU1"))-1)
	}
	if !strings.Contains(reminders[0].Msg.Text, "Zur Abstimmung") {
		t.Errorf("reminder without link: %s", reminders[0].Msg.Text)
	}
}

func TestConcurrentSavesSameUser(t *testing.T) {
	e := newEnv(t, "U1")
	p := e.start()
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d := domain.Days{}
			d[i%5] = true
			e.svc.HandleSave(e.ctx, e.saveReq(p, "U1", d))
		}()
	}
	wg.Wait()
	// Final save of the last writer, rendered after the last commit:
	e.svc.HandleSave(e.ctx, e.saveReq(p, "U1", domain.Days{false, false, false, false, true}))
	ups := e.api.UpdatesTo("DU1")
	last := slack.RenderPlain(ups[len(ups)-1].Msg.Blocks)
	if !strings.Contains(last, "[x] Freitag") || strings.Contains(last, "[x] Montag") {
		t.Errorf("DM does not show latest state:\n%s", last)
	}
}

// requestReq simulates a click on "Abfrage an mich senden" in the summary.
func (e *env) requestReq(p *domain.Poll, user string) slack.Interaction {
	e.t.Helper()
	got, err := e.store.GetPoll(e.ctx, p.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	return slack.Interaction{TeamID: "T1", UserID: user, PollID: p.ID, ChannelID: got.SummaryChannelID, MessageTS: got.SummaryMessageTS}
}

func (e *env) lastEphemeral() string {
	e.t.Helper()
	if len(e.api.Ephemerals) == 0 {
		e.t.Fatal("no ephemeral message")
	}
	return e.api.Ephemerals[len(e.api.Ephemerals)-1]
}

func TestRequestDMResend(t *testing.T) {
	e := newEnv(t, "U1")
	p := e.start()
	e.store.SetSummaryMessage(e.ctx, p.ID, "S1")
	e.svc.HandleSave(e.ctx, e.saveReq(p, "U1", domain.Days{true}))
	oldReq := e.saveReq(p, "U1", domain.Days{false, true})

	// Directly after delivery: cooldown, nothing is sent.
	e.svc.HandleRequestDM(e.ctx, e.requestReq(p, "U1"))
	e.drain()
	if len(e.api.PostsTo("DU1")) != 1 || !strings.Contains(e.lastEphemeral(), "gerade erst") {
		t.Fatalf("cooldown ignored: posts=%d, reply=%q", len(e.api.PostsTo("DU1")), e.lastEphemeral())
	}

	e.setNow(e.clock().Add(RequestDMCooldown + time.Second))
	e.svc.HandleRequestDM(e.ctx, e.requestReq(p, "U1"))
	ups := e.api.UpdatesTo("DU1")
	if old := slack.RenderPlain(ups[len(ups)-1].Msg.Blocks); !strings.Contains(old, "ersetzt") || strings.Contains(old, "[Speichern]") {
		t.Errorf("old DM not superseded:\n%s", old)
	}
	// A second click before the new DM went out does not queue another one.
	e.svc.HandleRequestDM(e.ctx, e.requestReq(p, "U1"))
	if !strings.Contains(e.lastEphemeral(), "unterwegs") {
		t.Errorf("reply = %q", e.lastEphemeral())
	}
	e.drain()

	posts := e.api.PostsTo("DU1")
	if len(posts) != 2 {
		t.Fatalf("posts = %d, want 2", len(posts))
	}
	newDM := slack.RenderPlain(posts[1].Msg.Blocks)
	if !strings.Contains(newDM, "[x] Montag") || !strings.Contains(newDM, "[Speichern]") {
		t.Errorf("new DM must show the saved selection:\n%s", newDM)
	}
	if k := posts[1].Msg.Metadata.EventPayload["key"]; !strings.HasSuffix(k.(string), "#1") {
		t.Errorf("metadata key = %v", k)
	}

	// The old message is stale now, the new one works.
	e.svc.HandleSave(e.ctx, oldReq)
	if r, _ := e.store.GetResponse(e.ctx, p.ID, "U1"); r.Days != (domain.Days{true}) {
		t.Error("save on superseded DM accepted")
	}
	e.svc.HandleSave(e.ctx, e.saveReq(p, "U1", domain.Days{false, true}))
	if r, _ := e.store.GetResponse(e.ctx, p.ID, "U1"); r.Days != (domain.Days{false, true}) {
		t.Error("save on new DM rejected")
	}
}

func TestRequestDMNoDuplicateAfterLostResponse(t *testing.T) {
	e := newEnv(t, "U1")
	p := e.start()
	e.store.SetSummaryMessage(e.ctx, p.ID, "S1")
	e.setNow(e.clock().Add(RequestDMCooldown + time.Second))
	e.svc.HandleRequestDM(e.ctx, e.requestReq(p, "U1"))
	e.api.LoseNextPostResponse = true
	e.drain()
	e.setNow(e.clock().Add(time.Hour))
	e.drain()
	// Initial DM + exactly one re-requested DM: the retry adopted the lost
	// post instead of the old DM (different generation in the metadata key).
	posts := e.api.PostsTo("DU1")
	part, _ := e.store.GetParticipant(e.ctx, p.ID, "U1")
	if len(posts) != 2 || part.DMMessageTS != posts[1].TS {
		t.Errorf("posts=%d ts=%s", len(posts), part.DMMessageTS)
	}
}

func TestRequestDMRetryDoesNotAdoptOldDM(t *testing.T) {
	e := newEnv(t, "U1")
	p := e.start()
	e.store.SetSummaryMessage(e.ctx, p.ID, "S1")
	oldTS := e.api.PostsTo("DU1")[0].TS
	e.setNow(e.clock().Add(RequestDMCooldown + time.Second))
	e.svc.HandleRequestDM(e.ctx, e.requestReq(p, "U1"))

	// The first attempt fails before Slack posted anything.
	e.api.Fail = func(method, ch string) error {
		if method == "chat.postMessage" && ch == "DU1" {
			e.api.Fail = nil
			return errors.New("dial tcp: i/o timeout")
		}
		return nil
	}
	e.drain()
	e.setNow(e.clock().Add(time.Hour))
	e.drain()
	// The retry must post a new DM, not adopt the superseded old one.
	part, _ := e.store.GetParticipant(e.ctx, p.ID, "U1")
	if part.DMMessageTS == oldTS || len(e.api.PostsTo("DU1")) != 2 {
		t.Errorf("old DM adopted: ts=%s posts=%d", part.DMMessageTS, len(e.api.PostsTo("DU1")))
	}
}

func TestRequestDMLateJoiner(t *testing.T) {
	e := newEnv(t, "U1", "U2", "U3")
	e.api.Members["CTEAM"] = []string{"U1", "U2"}
	e.svc.participants = participants.New(e.api, participants.Source{ChannelID: "CTEAM"}, "T1", "UBOT", quiet)
	p := e.start()
	e.store.SetSummaryMessage(e.ctx, p.ID, "S1")
	before, _ := e.store.GetPoll(e.ctx, p.ID)

	// U3 is not in the target group (yet).
	e.svc.HandleRequestDM(e.ctx, e.requestReq(p, "U3"))
	if _, err := e.store.GetParticipant(e.ctx, p.ID, "U3"); err == nil || !strings.Contains(e.lastEphemeral(), "nicht zum Personenkreis") {
		t.Fatalf("outsider added: reply=%q", e.lastEphemeral())
	}

	// U3 joins the channel after the weekly start and asks for the poll.
	e.api.Members["CTEAM"] = append(e.api.Members["CTEAM"], "U3")
	e.svc.HandleRequestDM(e.ctx, e.requestReq(p, "U3"))
	e.drain()
	if len(e.api.PostsTo("DU3")) != 1 || !strings.Contains(e.lastEphemeral(), "hinzugefügt") {
		t.Fatalf("late joiner not added: reply=%q", e.lastEphemeral())
	}
	after, _ := e.store.GetPoll(e.ctx, p.ID)
	if after.SummaryVersion <= before.SummaryVersion {
		t.Error("summary not marked outdated")
	}
	// Clicking again does not add twice.
	e.svc.HandleRequestDM(e.ctx, e.requestReq(p, "U3"))
	parts, _ := e.store.ListParticipants(e.ctx, p.ID)
	if len(parts) != 3 {
		t.Errorf("participants = %d", len(parts))
	}
}

func TestRequestDMValidation(t *testing.T) {
	e := newEnv(t, "U1")
	p := e.start()
	e.store.SetSummaryMessage(e.ctx, p.ID, "S1")
	e.setNow(e.clock().Add(RequestDMCooldown + time.Second))

	stale := e.requestReq(p, "U1")
	stale.MessageTS = "S0"
	e.svc.HandleRequestDM(e.ctx, stale)
	foreign := e.requestReq(p, "U1")
	foreign.TeamID = "T2"
	e.svc.HandleRequestDM(e.ctx, foreign)
	e.setNow(p.Deadline)
	e.svc.HandleRequestDM(e.ctx, e.requestReq(p, "U1"))
	if !strings.Contains(e.lastEphemeral(), "geschlossen") {
		t.Errorf("reply after deadline = %q", e.lastEphemeral())
	}
	e.drain()
	if n := len(e.api.PostsTo("DU1")); n != 1 {
		t.Errorf("DM re-sent despite invalid requests: %d posts", n)
	}
}
