package poll

import (
	"strings"
	"testing"
	"time"

	"officebot/internal/domain"
	"officebot/internal/participants"
	"officebot/internal/slack"
)

func day(d int) domain.Date { return domain.NewDate(2026, 10, d) }

func (e *env) addAbsence(user string, from, to domain.Date) domain.Absence {
	e.t.Helper()
	a, err := e.store.AddAbsence(e.ctx, "T1", user, from, to, user, e.clock())
	if err != nil {
		e.t.Fatal(err)
	}
	if err := e.svc.AbsencesChanged(e.ctx, user); err != nil {
		e.t.Fatal(err)
	}
	return a
}

func (e *env) absences(user string) string {
	e.t.Helper()
	abs, err := e.store.ListAbsences(e.ctx, "T1", user, day(1))
	if err != nil {
		e.t.Fatal(err)
	}
	var out []string
	for _, a := range abs {
		out = append(out, a.Start.String()+".."+a.End.String())
	}
	return strings.Join(out, " ")
}

// lastDM returns the latest rendering of a user's poll DM (post or update).
func (e *env) lastDM(user string) string {
	e.t.Helper()
	if ups := e.api.UpdatesTo("D" + user); len(ups) > 0 {
		return slack.RenderPlain(ups[len(ups)-1].Msg.Blocks)
	}
	posts := e.api.PostsTo("D" + user)
	if len(posts) == 0 {
		e.t.Fatalf("no DM to %s", user)
	}
	return slack.RenderPlain(posts[0].Msg.Blocks)
}

func TestAwayAllWeekGetsNoDMOrReminder(t *testing.T) {
	e := newEnv(t, "U1", "U2")
	a := e.addAbsence("U2", day(17), day(25))
	p := e.start()
	if n := len(e.api.PostsTo("DU2")); n != 0 {
		t.Fatalf("away participant got %d DMs", n)
	}
	part, _ := e.store.GetParticipant(e.ctx, p.ID, "U2")
	if part.DeliveryStatus != domain.DeliveryAway {
		t.Errorf("delivery status = %s", part.DeliveryStatus)
	}

	e.setNow(time.Date(2026, 10, 20, 9, 0, 0, 0, e.loc)) // Tuesday reminder round
	e.svc.RunDueReminders(e.ctx, e.clock())
	e.drain()
	if len(e.api.PostsTo("DU1")) != 2 || len(e.api.PostsTo("DU2")) != 0 {
		t.Fatalf("reminders: U1 posts=%d, U2 posts=%d", len(e.api.PostsTo("DU1")), len(e.api.PostsTo("DU2")))
	}

	// U2 deletes the absence in the form: the held-back DM is sent now.
	e.svc.HandleDeleteTimeOff(e.ctx, slack.Interaction{TeamID: "T1", UserID: "U2", AbsenceID: a.ID, ViewID: "V1"})
	e.drain()
	if dm := e.lastDM("U2"); !strings.Contains(dm, "[Speichern]") {
		t.Errorf("DM after the absence was deleted:\n%s", dm)
	}
	v, ok := e.api.LastView()
	if !ok || v.ViewID != "V1" || !strings.Contains(slack.RenderPlain(v.View.Blocks.BlockSet), "Keine Abwesenheiten geplant") {
		t.Errorf("form not refreshed: %+v", v)
	}
}

func TestAwayButtonAndUndo(t *testing.T) {
	e := newEnv(t, "U1")
	p := e.start()
	before := e.trig.Load()

	req := e.saveReq(p, "U1", domain.Days{})
	req.Away = true
	e.svc.HandleAway(e.ctx, req)
	e.drain()
	if got := e.absences("U1"); got != "2026-10-19..2026-10-23" {
		t.Errorf("absences = %s", got)
	}
	dm := e.lastDM("U1")
	if !strings.Contains(dm, "als abwesend eingetragen") || !strings.Contains(dm, "[Doch nicht abwesend]") || strings.Contains(dm, "[Speichern]") {
		t.Errorf("away DM:\n%s", dm)
	}
	if e.trig.Load() == before {
		t.Error("summary not triggered")
	}
	e.setNow(time.Date(2026, 10, 20, 9, 0, 0, 0, e.loc))
	e.svc.RunDueReminders(e.ctx, e.clock())
	e.drain()
	if n := len(e.api.PostsTo("DU1")); n != 1 {
		t.Errorf("reminder sent while away (posts=%d)", n)
	}

	// Undo on Tuesday keeps Monday, which has already passed.
	req.Away = false
	e.svc.HandleAway(e.ctx, req)
	e.drain()
	if got := e.absences("U1"); got != "2026-10-19..2026-10-19" {
		t.Errorf("absences after undo = %s", got)
	}
	if dm := e.lastDM("U1"); !strings.Contains(dm, "[Speichern]") || !strings.Contains(dm, "[🌴 Ich bin diese Woche abwesend]") {
		t.Errorf("DM after undo:\n%s", dm)
	}

	// Undo within a longer vacation only gives back the polled week.
	e.addAbsence("U1", day(12), day(30))
	e.svc.HandleAway(e.ctx, req)
	if got := e.absences("U1"); got != "2026-10-12..2026-10-19 2026-10-24..2026-10-30" {
		t.Errorf("absences after undo inside vacation = %s", got)
	}
}

func TestReminderSkippedOnAwayDay(t *testing.T) {
	e := newEnv(t, "U1")
	p := e.start()
	e.addAbsence("U1", day(20), day(20)) // only Tuesday, the reminder day
	e.drain()
	if dm := e.lastDM("U1"); !strings.Contains(dm, "[Speichern]") {
		t.Errorf("partly away participant must keep the normal DM:\n%s", dm)
	}
	e.setNow(time.Date(2026, 10, 20, 9, 0, 0, 0, e.loc))
	e.svc.RunDueReminders(e.ctx, e.clock())
	e.drain()
	part, _ := e.store.GetParticipant(e.ctx, p.ID, "U1")
	if n := len(e.api.PostsTo("DU1")); n != 1 || part.ReminderCount != 0 {
		t.Errorf("reminder on away day: posts=%d count=%d", n, part.ReminderCount)
	}
}

func TestRequestDMWhileAway(t *testing.T) {
	e := newEnv(t, "U1")
	e.addAbsence("U1", day(19), day(23))
	p := e.start()
	e.store.SetSummaryMessage(e.ctx, p.ID, "S1")
	e.svc.HandleRequestDM(e.ctx, e.requestReq(p, "U1"))
	e.drain()
	if dm := e.lastDM("U1"); !strings.Contains(dm, "[Doch nicht abwesend]") {
		t.Errorf("requested DM must show the away view:\n%s", dm)
	}
}

func TestLateJoinerWhileAway(t *testing.T) {
	e := newEnv(t, "U1", "U2")
	e.api.Members["CTEAM"] = []string{"U1"}
	e.svc.participants = participants.New(e.api, participants.Source{ChannelID: "CTEAM"}, "T1", "UBOT", quiet)
	p := e.start()
	e.store.SetSummaryMessage(e.ctx, p.ID, "S1")
	e.store.AddAbsence(e.ctx, "T1", "U2", day(19), day(23), "U2", e.clock())

	e.api.Members["CTEAM"] = append(e.api.Members["CTEAM"], "U2")
	e.svc.HandleRequestDM(e.ctx, e.requestReq(p, "U2"))
	e.drain()
	if dm := e.lastDM("U2"); !strings.Contains(dm, "[Doch nicht abwesend]") {
		t.Errorf("late joiner who is away must get the away view:\n%s", dm)
	}
}

func TestTimeOffForm(t *testing.T) {
	e := newEnv(t, "U1", "U2")
	e.start()
	e.addAbsence("U1", day(26), day(28))

	e.svc.HandleOpenTimeOff(e.ctx, slack.Interaction{TeamID: "T1", UserID: "U1", TriggerID: "TR1"})
	v, ok := e.api.LastView()
	form := slack.RenderPlain(v.View.Blocks.BlockSet)
	if !ok || v.TriggerID != "TR1" || !strings.Contains(form, "Mo, 26.10.2026 – Mi, 28.10.2026 [Löschen]") ||
		!strings.Contains(form, "Erster Tag: 2026-10-19") {
		t.Fatalf("form:\n%s", form)
	}

	submit := func(from, to domain.Date) map[string]string {
		return e.svc.SubmitTimeOff(e.ctx, slack.Interaction{TeamID: "T1", UserID: "U1", Start: from, End: to})
	}
	cases := []struct {
		from, to domain.Date
		block    string
		want     string
	}{
		{domain.Date{}, day(23), slack.TimeOffStartBlock, "Bitte wähle"},
		{day(23), day(21), slack.TimeOffEndBlock, "nicht vor dem ersten"},
		{day(12), day(18), slack.TimeOffStartBlock, "Vergangenheit"},
		{day(16), day(21), slack.TimeOffStartBlock, "Vergangenheit"}, // ongoing, but started in the past
		{day(20), domain.NewDate(2027, 10, 20), slack.TimeOffEndBlock, "ein Jahr"},
	}
	for _, c := range cases {
		if errs := submit(c.from, c.to); !strings.Contains(errs[c.block], c.want) {
			t.Errorf("%v–%v: errors = %v", c.from, c.to, errs)
		}
	}
	if got := e.absences("U1"); got != "2026-10-26..2026-10-28" {
		t.Fatalf("invalid input stored: %s", got)
	}

	// Valid: the polled week, adjacent to the existing entry → merged, and
	// the DM switches to the away view.
	if errs := submit(day(19), day(25)); errs != nil {
		t.Fatalf("valid form rejected: %v", errs)
	}
	e.drain()
	if got := e.absences("U1"); got != "2026-10-19..2026-10-28" {
		t.Errorf("absences = %s", got)
	}
	if dm := e.lastDM("U1"); !strings.Contains(dm, "als abwesend eingetragen") {
		t.Errorf("DM not switched to away:\n%s", dm)
	}

	// Nobody can delete other people's time off.
	abs, _ := e.store.ListAbsences(e.ctx, "T1", "U1", day(1))
	e.svc.HandleDeleteTimeOff(e.ctx, slack.Interaction{TeamID: "T1", UserID: "U2", AbsenceID: abs[0].ID, ViewID: "V2"})
	if got := e.absences("U1"); got != "2026-10-19..2026-10-28" {
		t.Errorf("U2 deleted U1's time off: %s", got)
	}
}

func TestAwayButtonAfterWeekStarted(t *testing.T) {
	e := newEnv(t, "U1", "U2")
	p := e.start()
	e.svc.HandleSave(e.ctx, e.saveReq(p, "U2", domain.Days{true, true}))
	e.setNow(time.Date(2026, 10, 21, 9, 0, 0, 0, e.loc)) // Wednesday

	// Seen late and not answered yet: the whole week may still be reported.
	req := e.saveReq(p, "U1", domain.Days{})
	req.Away = true
	e.svc.HandleAway(e.ctx, req)
	e.drain()
	if got := e.absences("U1"); got != "2026-10-19..2026-10-23" {
		t.Errorf("late away for the whole week = %s", got)
	}
	if dm := e.lastDM("U1"); !strings.Contains(dm, "[Doch nicht abwesend]") {
		t.Errorf("DM not switched to away:\n%s", dm)
	}

	// Already answered: the answer for Monday and Tuesday must stand.
	req = e.saveReq(p, "U2", domain.Days{})
	req.Away = true
	e.svc.HandleAway(e.ctx, req)
	e.drain()
	if got := e.absences("U2"); got != "" {
		t.Errorf("answered days overwritten: %s", got)
	}
	if n := len(e.api.Ephemerals); n == 0 || !strings.HasPrefix(e.api.Ephemerals[n-1], "U2: ") || !strings.Contains(e.api.Ephemerals[n-1], "schon abgestimmt") {
		t.Errorf("replies = %q", e.api.Ephemerals)
	}
}

func TestDeleteOngoingTimeOffKeepsPast(t *testing.T) {
	e := newEnv(t, "U1")
	e.start()
	a := e.addAbsence("U1", day(19), day(23))
	e.setNow(time.Date(2026, 10, 21, 9, 0, 0, 0, e.loc)) // Wednesday

	e.svc.HandleDeleteTimeOff(e.ctx, slack.Interaction{TeamID: "T1", UserID: "U1", AbsenceID: a.ID, ViewID: "V1"})
	e.drain()
	if got := e.absences("U1"); got != "2026-10-19..2026-10-20" {
		t.Errorf("absences = %s", got)
	}
	if v, ok := e.api.LastView(); !ok || !strings.Contains(slack.RenderPlain(v.View.Blocks.BlockSet), "Keine Abwesenheiten geplant") {
		t.Errorf("form not refreshed: %+v", v)
	}
}
