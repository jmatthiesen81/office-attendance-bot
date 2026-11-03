package poll

import (
	"strings"
	"testing"
	"time"

	"officebot/internal/domain"
	"officebot/internal/slack"
)

func TestSetAttendance(t *testing.T) {
	e := newEnv(t, "U1", "U2")
	p := e.startWithSummary()
	e.svc.HandleSave(e.ctx, e.saveReq(p, "U1", domain.Days{true, true, true}))
	e.after(time.Hour)

	// remove a past day after the deadline: allowed for admins
	e.setNow(time.Date(2026, 10, 23, 9, 0, 0, 0, e.loc))
	if err := e.svc.CloseDue(e.ctx, e.clock()); err != nil {
		t.Fatal(err)
	}
	e.drain()
	before := len(e.api.UpdatesTo("DU1"))
	_, after, changed, err := e.svc.SetAttendance(e.ctx, p.ID, "U1", AttendanceRemove, domain.Days{false, true}, true)
	if err != nil || !changed || after.Days != (domain.Days{true, false, true}) {
		t.Fatalf("remove: %+v changed=%v err=%v", after, changed, err)
	}
	e.after(2 * time.Minute)
	ups := e.api.UpdatesTo("DU1")
	if len(ups) <= before {
		t.Fatal("DM not re-rendered")
	}
	if text := slack.RenderPlain(ups[len(ups)-1].Msg.Blocks); !strings.Contains(text, "geschlossen") || strings.Contains(text, "[Speichern]") {
		t.Errorf("DM after correction:\n%s", text)
	}
	if got, _ := e.store.GetPoll(e.ctx, p.ID); got.Status != domain.PollClosed {
		t.Error("correction reopened the poll")
	}
	if n := e.notices(); len(n) != 1 || !strings.Contains(n[0].Msg.Text, "Dienstag") {
		t.Errorf("notices = %+v", n)
	}

	// same change again: nothing happens
	if _, _, changed, err = e.svc.SetAttendance(e.ctx, p.ID, "U1", AttendanceRemove, domain.Days{false, true}, true); err != nil || changed {
		t.Errorf("repeat: changed=%v err=%v", changed, err)
	}

	// add and set; --no-notice posts nothing
	if _, after, _, err = e.svc.SetAttendance(e.ctx, p.ID, "U1", AttendanceAdd, domain.Days{4: true}, false); err != nil || after.Days != (domain.Days{true, false, true, false, true}) {
		t.Fatalf("add: %+v err=%v", after, err)
	}
	if _, after, _, err = e.svc.SetAttendance(e.ctx, p.ID, "U1", AttendanceSet, domain.Days{}, false); err != nil || after.Days.Count() != 0 {
		t.Fatalf("set none: %+v err=%v", after, err)
	}
	e.after(2 * time.Minute)
	if n := e.notices(); len(n) != 1 {
		t.Errorf("notices with notice=false: %d", len(n))
	}

	// errors
	if _, _, _, err = e.svc.SetAttendance(e.ctx, p.ID, "U2", AttendanceRemove, domain.Days{true}, true); err == nil {
		t.Error("remove without answer accepted")
	}
	if _, _, _, err = e.svc.SetAttendance(e.ctx, p.ID, "UX", AttendanceSet, domain.Days{true}, true); err == nil {
		t.Error("non participant accepted")
	}
	// add for someone who has not answered creates the answer
	if _, after, _, err = e.svc.SetAttendance(e.ctx, p.ID, "U2", AttendanceAdd, domain.Days{true}, true); err != nil || after.Days != (domain.Days{true}) {
		t.Errorf("add without answer: %+v err=%v", after, err)
	}
}
