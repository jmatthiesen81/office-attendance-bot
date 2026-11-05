package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"officebot/internal/domain"
	"officebot/internal/isoweek"
	"officebot/internal/storage"
)

// adminEnv is a test environment with a logged-in admin and the poll of
// week 2026-W45 (Monday 2 November), started on Monday morning.
func adminEnv(t *testing.T, extra map[string]string) (*testEnv, *http.Client, string, *domain.Poll) {
	t.Helper()
	e := newTestEnv(t, extra)
	now := time.Date(2026, 11, 2, 7, 0, 0, 0, e.cfg.Location)
	e.setNow(now)
	p, _, err := e.svc.StartWeek(e.ctx, isoweek.Week{Year: 2026, Week: 45})
	if err != nil {
		t.Fatal(err)
	}
	e.addWebUser("U1", storage.RoleAdmin)
	c := e.client()
	e.login(c, "U1", nil)
	return e, c, e.csrf(c, "/access"), p
}

func (e *testEnv) setNow(t time.Time) {
	e.svc.Now = func() time.Time { return t }
	e.srv.Now = func() time.Time { return t }
	e.stats.Now = func() time.Time { return t }
}

func TestAdminPagesRender(t *testing.T) {
	e, c, _, _ := adminEnv(t, nil)
	for _, path := range []string{"/", "/weeks/2026-W45", "/weeks/2026-W30", "/absences", "/languages", "/closed-days", "/access", "/audit"} {
		res := e.get(c, path)
		body := e.body(res)
		if res.StatusCode != http.StatusOK || !strings.Contains(body, "</main>") {
			t.Errorf("%s: %d", path, res.StatusCode)
		}
	}
	if res := e.get(c, "/weeks"); res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/weeks/2026-W45" {
		t.Errorf("/weeks: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
}

func TestWeekCorrection(t *testing.T) {
	e, c, csrf, p := adminEnv(t, nil)
	form := url.Values{"csrf": {csrf}, "user": {"U2"}, "day": {"mon", "wed"}}
	res := e.post(c, "/weeks/2026-W45/attendance", form, e.http.URL)
	if !strings.Contains(res.Header.Get("Location"), "msg=queued") {
		t.Fatalf("correction: %s", res.Header.Get("Location"))
	}
	resp, err := e.store.GetResponse(e.ctx, p.ID, "U2")
	if err != nil || resp == nil || resp.Days != (domain.Days{true, false, true}) {
		t.Fatalf("response: %+v %v", resp, err)
	}
	if !audited(e, "attendance.set", "U1") {
		t.Errorf("correction not audited")
	}
	res = e.post(c, "/weeks/2026-W45/attendance", form, e.http.URL)
	if !strings.Contains(res.Header.Get("Location"), "msg=unchanged") {
		t.Errorf("same correction again: %s", res.Header.Get("Location"))
	}
	form.Set("day", "sat")
	if res := e.post(c, "/weeks/2026-W45/attendance", form, e.http.URL); !strings.Contains(res.Header.Get("Location"), "err=invalid") {
		t.Errorf("invalid day: %s", res.Header.Get("Location"))
	}
	if res := e.post(c, "/weeks/2026-W45/refresh", url.Values{"csrf": {csrf}}, e.http.URL); !strings.Contains(res.Header.Get("Location"), "msg=queued") {
		t.Errorf("refresh: %s", res.Header.Get("Location"))
	}
}

func TestAbsencesLanguagesClosedDays(t *testing.T) {
	e, c, csrf, _ := adminEnv(t, nil)
	post := func(path string, v url.Values) string {
		v.Set("csrf", csrf)
		return e.post(c, path, v, e.http.URL).Header.Get("Location")
	}
	if loc := post("/absences/add", url.Values{"user": {"U2"}, "from": {"2026-10-30"}, "to": {"2026-11-03"}}); !strings.Contains(loc, "err=past") {
		t.Errorf("past time off: %s", loc)
	}
	if loc := post("/absences/add", url.Values{"user": {"U2"}, "from": {"2026-11-05"}, "to": {"2026-11-04"}}); !strings.Contains(loc, "err=range") {
		t.Errorf("inverted range: %s", loc)
	}
	if loc := post("/absences/add", url.Values{"user": {"U2"}, "from": {"2026-11-04"}, "to": {"2026-11-06"}}); !strings.Contains(loc, "msg=added") {
		t.Errorf("time off: %s", loc)
	}
	abs, _ := e.store.ListAbsences(e.ctx, "T1", "U2", domain.NewDate(2026, 11, 2))
	if len(abs) != 1 || abs[0].CreatedBy != "U1" {
		t.Fatalf("absences: %+v", abs)
	}
	if body := e.body(e.get(c, "/absences")); !strings.Contains(body, "2026-11-04") {
		t.Errorf("time off not listed")
	}
	if loc := post("/absences/delete", url.Values{"id": {itoa(int(abs[0].ID))}}); !strings.Contains(loc, "msg=removed") {
		t.Errorf("delete time off: %s", loc)
	}

	if loc := post("/languages/set", url.Values{"user": {"U2"}, "language": {"fr"}}); !strings.Contains(loc, "msg=queued") {
		t.Errorf("set language: %s", loc)
	}
	if l, _ := e.store.GetUserLanguage(e.ctx, "T1", "U2"); l != "fr" {
		t.Errorf("language: %q", l)
	}
	if loc := post("/languages/set", url.Values{"user": {"U2"}, "language": {"klingon"}}); !strings.Contains(loc, "err=invalid") {
		t.Errorf("unknown language: %s", loc)
	}
	if loc := post("/languages/reset", url.Values{"user": {"U2"}}); !strings.Contains(loc, "msg=queued") {
		t.Errorf("reset language: %s", loc)
	}

	// 2026-12-24 is a Thursday: Thursday, Friday, then the weekend is skipped.
	if loc := post("/closed-days/add", url.Values{"from": {"2026-12-24"}, "to": {"2026-12-27"}, "reason": {"Weihnachten"}}); !strings.Contains(loc, "msg=added") {
		t.Errorf("closed days: %s", loc)
	}
	days, _ := e.store.ListClosedDays(e.ctx, "T1", domain.NewDate(2026, 12, 1), domain.NewDate(2026, 12, 31))
	if len(days) != 2 || days[0].Reason != "Weihnachten" {
		t.Errorf("closed days: %+v", days)
	}
	if loc := post("/closed-days/remove", url.Values{"day": {"2026-12-24"}}); !strings.Contains(loc, "msg=removed") {
		t.Errorf("remove closed day: %s", loc)
	}
	if body := e.body(e.get(c, "/audit")); !strings.Contains(body, "closed_days.remove") || !strings.Contains(body, "time_off.add") {
		t.Errorf("audit log incomplete")
	}
}

func audited(e *testEnv, action, actor string) bool {
	entries, _ := e.store.ListAudit(e.ctx, "T1", 100, 0)
	for _, en := range entries {
		if en.Action == action && en.Actor == actor {
			return true
		}
	}
	return false
}

func TestDayCheckAndPurge(t *testing.T) {
	e, c, csrf, p := adminEnv(t, nil)
	post := func(path string, v url.Values) string {
		v.Set("csrf", csrf)
		return e.post(c, path, v, e.http.URL).Header.Get("Location")
	}
	if loc := post("/days/2026-11-02", url.Values{"s_U1": {"office"}}); !strings.Contains(loc, "err=poll_open") {
		t.Errorf("day check of an open poll: %s", loc)
	}
	// The poll is still open on Monday.
	if body := e.body(e.get(c, "/days/2026-11-02")); !strings.Contains(body, tr("de", "err.poll_open")) {
		t.Errorf("open poll not reported")
	}
	if _, _, err := e.store.SaveResponse(e.ctx, p.ID, "U1", domain.Days{true}, time.Now()); err != nil {
		t.Fatal(err)
	}
	e.setNow(time.Date(2026, 11, 6, 12, 0, 0, 0, e.cfg.Location))
	if err := e.svc.CloseDue(e.ctx, e.srv.Now()); err != nil {
		t.Fatal(err)
	}
	// Four days later the session has expired; log in again.
	e.login(c, "U1", nil)
	csrf = e.csrf(c, "/access")
	if body := e.body(e.get(c, "/days/2026-11-02")); !strings.Contains(body, `name="s_U1" value="office" checked`) {
		t.Errorf("day check does not show the plan: %s", body)
	}
	loc := post("/days/2026-11-02", url.Values{"s_U1": {"office"}, "s_U2": {"office"}, "s_U3": {"away"}, "verify": {"1"}})
	if !strings.Contains(loc, "msg=verified") {
		t.Fatalf("save day: %s", loc)
	}
	rows, _ := e.store.AttendanceDays(e.ctx, "T1", "", domain.NewDate(2026, 11, 2), domain.NewDate(2026, 11, 2))
	got := map[string]string{}
	for _, r := range rows {
		got[r.SlackUserID] = r.Status + "/" + r.Planned
	}
	if got["U1"] != "office/office" || got["U2"] != "office/no_answer" || got["U3"] != "away/no_answer" {
		t.Errorf("daily status after the day check: %v", got)
	}
	if v, _ := e.store.VerifiedDays(e.ctx, "T1", domain.NewDate(2026, 11, 2), domain.NewDate(2026, 11, 2)); v["2026-11-02"].By != "U1" {
		t.Errorf("day not verified: %v", v)
	}
	if body := e.body(e.get(c, "/days/2026-11-02")); !strings.Contains(body, "korrigiert") {
		t.Errorf("correction not shown: %s", body)
	}
	if loc := post("/days/2026-11-09", url.Values{"s_U1": {"office"}}); !strings.Contains(loc, "err=future") {
		t.Errorf("future day: %s", loc)
	}

	// Purge with preview and confirmation.
	if body := e.body(e.post(c, "/retention/preview", url.Values{"csrf": {csrf}, "kind": {"user"}, "value": {"U3"}}, e.http.URL)); !strings.Contains(body, "Endgültig löschen") {
		t.Fatalf("no confirmation page")
	}
	if loc := post("/retention/purge", url.Values{"kind": {"user"}, "value": {"U3"}}); !strings.Contains(loc, "err=invalid") {
		t.Errorf("purge without confirmation: %s", loc)
	}
	if loc := post("/retention/purge", url.Values{"kind": {"user"}, "value": {"U3"}, "confirm": {"1"}}); !strings.Contains(loc, "msg=purged") {
		t.Errorf("purge: %s", loc)
	}
	if rows, _ := e.store.AttendanceDays(e.ctx, "T1", "U3", domain.NewDate(2026, 11, 1), domain.NewDate(2026, 11, 30)); len(rows) != 0 {
		t.Errorf("daily data of U3 left: %d", len(rows))
	}
	for _, path := range []string{"/retention", "/days"} {
		if res := e.get(c, path); res.StatusCode != http.StatusOK && res.StatusCode != http.StatusSeeOther {
			t.Errorf("%s: %d", path, res.StatusCode)
		}
	}
	if !audited(e, "stats.purge", "U1") || !audited(e, "day.verify", "U1") || !audited(e, "day.correct", "U1") {
		t.Errorf("day check or purge not audited")
	}
}

func TestStatsPage(t *testing.T) {
	e, admin, _, p := adminEnv(t, map[string]string{"WEB_STATS_PER_PERSON": "true", "OFFICE_DESKS": "2"})
	for u, d := range map[string]domain.Days{"U1": {true, true, true}, "U2": {true}, "U3": {}} {
		if _, _, err := e.store.SaveResponse(e.ctx, p.ID, u, d, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	e.setNow(time.Date(2026, 11, 9, 9, 0, 0, 0, e.cfg.Location))
	if err := e.svc.CloseDue(e.ctx, e.srv.Now()); err != nil {
		t.Fatal(err)
	}
	e.login(admin, "U1", nil)
	e.addWebUser("U2", storage.RoleViewer)
	viewer := e.client()
	e.login(viewer, "U2", nil)

	body := e.body(e.get(viewer, "/stats?from=2026-11&to=2026-11&persons=1"))
	for _, want := range []string{"<svg class=\"chart\"", "Personen im Office pro Tag", "2 Plätze hätten an 95 % der Tage gereicht", "Heute gibt es 2 Plätze"} {
		if !strings.Contains(body, want) {
			t.Errorf("viewer page lacks %q", want)
		}
	}
	if strings.Contains(body, "Name U3") || strings.Contains(body, "Letzter Office-Tag") {
		t.Errorf("viewer sees per-person data")
	}
	if res := e.get(viewer, "/stats/persons.csv"); res.StatusCode != http.StatusForbidden {
		t.Errorf("viewer person export: %d", res.StatusCode)
	}
	csvBody := e.body(e.get(viewer, "/stats.csv?from=2026-11&to=2026-11"))
	if !strings.HasPrefix(csvBody, "month,source,work_days") || !strings.Contains(csvBody, "2026-11,daily,5,0,4,") {
		t.Errorf("aggregate CSV: %s", csvBody)
	}

	body = e.body(e.get(admin, "/stats?from=2026-11&to=2026-11&persons=1"))
	if !strings.Contains(body, "Name U3") || !strings.Contains(body, "Längster Zeitraum ohne Office") {
		t.Errorf("admin does not see per-person data")
	}
	csvBody = e.body(e.get(admin, "/stats/persons.csv?from=2026-11&to=2026-11"))
	if !strings.Contains(csvBody, "U1,Name U1,2026-11,3,2,0,0,0.60") {
		t.Errorf("person CSV: %s", csvBody)
	}
	if !audited(e, "stats.view_persons", "U1") || !audited(e, "stats.export_persons", "U1") {
		t.Errorf("per-person access not audited")
	}
}
