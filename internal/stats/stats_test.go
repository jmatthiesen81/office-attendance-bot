package stats

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"officebot/internal/config"
	"officebot/internal/domain"
	"officebot/internal/isoweek"
	"officebot/internal/participants"
	"officebot/internal/poll"
	"officebot/internal/slack/slacktest"
	"officebot/internal/storage"
)

func day(s string) domain.Date {
	d, err := domain.ParseDate(s)
	if err != nil {
		panic(err)
	}
	return d
}

func row(user, d, status string) storage.AttendanceDay {
	return storage.AttendanceDay{SlackUserID: user, Name: "Name " + user, Day: day(d), Status: status, Planned: status}
}

const (
	O = storage.StatusOffice
	R = storage.StatusRemote
	A = storage.StatusAway
	N = storage.StatusNoAnswer
)

// week returns the rows of one person for Monday to Friday starting at monday.
func week(user, monday string, statuses ...string) []storage.AttendanceDay {
	var out []storage.AttendanceDay
	for i, st := range statuses {
		out = append(out, row(user, day(monday).AddDays(i).String(), st))
	}
	return out
}

func TestRunsSkipClosedAndUnaskedDays(t *testing.T) {
	// U1: office on Monday, then remote, away and no answer across a month
	// boundary; 2026-10-30 is closed, the week of 2026-11-09 was not asked.
	var rows []storage.AttendanceDay
	rows = append(rows, week("U1", "2026-10-26", O, R, R, A, R)...)
	rows = append(rows, week("U1", "2026-11-02", A, A, N, R, R)...)
	rows = append(rows, week("U1", "2026-11-16", R, O, R, R, R)...)
	in := &Input{Rows: rows, Closed: map[string]string{"2026-10-30": "closed"}}
	people := byPerson(in)
	if len(people) != 1 || len(people[0]) != 14 {
		t.Fatalf("usable rows: %d", len(people[0]))
	}
	got := LongestRun(people[0])
	if got.Start != day("2026-10-27") || got.End != day("2026-11-16") || got.WorkDays != 9 ||
		got.Remote != 5 || got.Away != 3 || got.NoAnswer != 1 {
		t.Errorf("longest run: %+v", got)
	}
	if got.Weeks() != 3 {
		t.Errorf("weeks: %v", got.Weeks())
	}

	p := People(in, in)
	if len(p) != 1 || p[0].LastOffice != day("2026-11-17") || p[0].Current.WorkDays != 3 || p[0].Current.Start != day("2026-11-18") {
		t.Errorf("person: %+v", p)
	}
	if len(p[0].Months) != 2 || p[0].Months[0].Office != 1 || p[0].Months[0].Remote != 2 || p[0].Months[1].Office != 1 {
		t.Errorf("months of person: %+v", p[0].Months)
	}
}

func TestMonthsAndCombine(t *testing.T) {
	// Four people over one week in November; Wednesday is closed.
	var rows []storage.AttendanceDay
	rows = append(rows, week("U1", "2026-11-02", O, O, O, O, O)...)
	rows = append(rows, week("U2", "2026-11-02", O, O, R, R, R)...)
	rows = append(rows, week("U3", "2026-11-02", O, R, R, A, N)...)
	rows = append(rows, week("U4", "2026-11-02", R, R, O, R, R)...)
	rows[1].Planned = R // U1 planned remote on Tuesday, an admin corrected it
	in := &Input{
		Rows:     rows,
		Closed:   map[string]string{"2026-11-04": "Holiday"},
		Verified: map[string]storage.VerifiedDay{"2026-11-02": {}, "2026-11-03": {}},
	}
	months := Months(in)
	m := months["2026-11"]
	if m == nil {
		t.Fatal("no month")
	}
	// Mon 3, Tue 2, Thu 1, Fri 1 people in the office.
	if m.WorkDays != 4 || m.ClosedDays != 1 || m.OfficePersonDays != 7 {
		t.Errorf("month: %+v", m)
	}
	if want := []int{0, 2, 1, 1}; len(m.Occupancy) != 4 || m.Occupancy[1] != 2 || m.Occupancy[3] != 1 {
		t.Errorf("occupancy %v, want %v", m.Occupancy, want)
	}
	if m.Entries != 16 || m.NoAnswer != 1 || m.Away != 1 || m.VerifiedDays != 2 || m.CheckedEntries != 8 || m.CorrectedEntries != 1 {
		t.Errorf("entries: %+v", m)
	}
	// Office days per person: 4, 2, 1, 0 (U4's office day is the closed Wednesday).
	if m.OfficeBuckets != [NumOfficeBuckets]int{1, 2, 1, 0, 0, 0} || m.People != 4 {
		t.Errorf("office buckets: %v", m.OfficeBuckets)
	}

	s := Combine([]*Month{m})
	if s.Average != 7.0/4 || s.Median != 1 || s.P90 != 3 || s.Max != 3 {
		t.Errorf("summary: avg %v median %d p90 %d max %d", s.Average, s.Median, s.P90, s.Max)
	}
	if s.DesksFor(0.5) != 1 || s.DesksFor(0.75) != 2 || s.DesksFor(1) != 3 {
		t.Errorf("desks: %v", s.Desks)
	}
	if s.WeekdayAverage[0] != 3 || s.WeekdayAverage[2] != 0 {
		t.Errorf("weekday averages: %v", s.WeekdayAverage)
	}
	if s.ResponseRate != 15.0/16 || s.VerifiedShare != 0.5 || s.CorrectedShare != 1.0/8 {
		t.Errorf("quality: response %v verified %v corrected %v", s.ResponseRate, s.VerifiedShare, s.CorrectedShare)
	}

	// Only checked days: Monday and Tuesday.
	in.OnlyVerified = true
	m = Months(in)["2026-11"]
	if m.WorkDays != 2 || m.OfficePersonDays != 5 {
		t.Errorf("checked days only: %+v", m)
	}

	// Stored months round-trip through JSON and combine exactly.
	b, _ := json.Marshal(months["2026-11"])
	var back Month
	if err := json.Unmarshal(b, &back); err != nil || back.OfficePersonDays != 7 || back.Occupancy[3] != 1 {
		t.Errorf("JSON round trip: %+v %v", back, err)
	}
}

// ------------------------------------------------------------- integration

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

type env struct {
	ctx   context.Context
	cfg   *config.Config
	store *storage.Store
	svc   *poll.Service
	stats *Service
	now   time.Time
}

func newEnv(t *testing.T, extra map[string]string) *env {
	t.Helper()
	get := map[string]string{"DRY_RUN": "true", "SLACK_SUMMARY_CHANNEL_ID": "CSUM", "SLACK_USER_IDS": "U1,U2,U3"}
	for k, v := range extra {
		get[k] = v
	}
	cfg, err := config.LoadFrom(func(k string) string { return get[k] })
	if err != nil {
		t.Fatal(err)
	}
	e := &env{ctx: context.Background(), cfg: cfg}
	e.store, err = storage.Open(e.ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.store.Close() })
	api := slacktest.New()
	for _, u := range []string{"U1", "U2", "U3"} {
		api.AddUser(u, "Name "+u)
	}
	people := participants.New(api, participants.Source{UserIDs: cfg.UserIDs}, "T1", "UBOT", quiet)
	e.svc = poll.NewService(e.store, api, people, cfg, "T1", nil, nil, quiet)
	e.stats = New(e.store, cfg, "T1", quiet)
	e.svc.SyncStats = e.stats.SyncPolls
	clock := func() time.Time { return e.now }
	e.svc.Now, e.stats.Now = clock, clock
	return e
}

// runWeek starts a week, saves responses (user -> days) and closes it.
func (e *env) runWeek(t *testing.T, w isoweek.Week, answers map[string]domain.Days) *domain.Poll {
	t.Helper()
	e.now = w.Monday(e.cfg.Location).Add(7 * time.Hour)
	p, _, err := e.svc.StartWeek(e.ctx, w)
	if err != nil {
		t.Fatal(err)
	}
	for u, d := range answers {
		if _, _, err := e.store.SaveResponse(e.ctx, p.ID, u, d, e.now); err != nil {
			t.Fatal(err)
		}
	}
	e.now = e.cfg.Deadline(w).Add(time.Minute)
	if err := e.svc.CloseDue(e.ctx, e.now); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSyncCorrectAndPurge(t *testing.T) {
	e := newEnv(t, nil)
	w45 := isoweek.Week{Year: 2026, Week: 45}
	p := e.runWeek(t, w45, map[string]domain.Days{"U1": {true, true, false, false, false}, "U2": {}})
	if _, err := e.store.AddAbsence(e.ctx, "T1", "U3", day("2026-11-05"), day("2026-11-06"), "U3", e.now); err != nil {
		t.Fatal(err)
	}
	if err := e.stats.SyncPolls(e.ctx); err != nil {
		t.Fatal(err)
	}
	rows, _ := e.store.AttendanceDays(e.ctx, "T1", "", day("2026-11-02"), day("2026-11-06"))
	got := map[string]string{}
	for _, r := range rows {
		got[r.SlackUserID+" "+r.Day.String()] = r.Status
	}
	for k, want := range map[string]string{
		"U1 2026-11-02": O, "U1 2026-11-04": R, "U2 2026-11-02": R, "U3 2026-11-02": N, "U3 2026-11-05": A,
	} {
		if got[k] != want {
			t.Errorf("%s: %q, want %q", k, got[k], want)
		}
	}
	if len(rows) != 15 {
		t.Errorf("rows: %d", len(rows))
	}

	// The day check corrects Wednesday: U2 was in the office after all.
	e.now = time.Date(2026, 11, 6, 12, 0, 0, 0, e.cfg.Location)
	if changed, err := e.svc.CorrectDay(e.ctx, p.ID, "U2", day("2026-11-04"), O, "U1"); err != nil || !changed {
		t.Fatalf("correct: %v %v", changed, err)
	}
	if changed, err := e.svc.CorrectDay(e.ctx, p.ID, "U3", day("2026-11-02"), A, "U1"); err != nil || !changed {
		t.Fatalf("correct to away: %v %v", changed, err)
	}
	if err := e.stats.SyncPolls(e.ctx); err != nil {
		t.Fatal(err)
	}
	rows, _ = e.store.AttendanceDays(e.ctx, "T1", "U2", day("2026-11-04"), day("2026-11-04"))
	if len(rows) != 1 || rows[0].Status != O || rows[0].Planned != R || !rows[0].Corrected() {
		t.Errorf("corrected row: %+v", rows)
	}
	rows, _ = e.store.AttendanceDays(e.ctx, "T1", "U3", day("2026-11-02"), day("2026-11-02"))
	if len(rows) != 1 || rows[0].Status != A || rows[0].Planned != N {
		t.Errorf("past time off from the day check: %+v", rows)
	}

	// Report for November (today is 6 November).
	r, err := e.stats.Report(e.ctx, "2026-11", "2026-11", false, true)
	if err != nil {
		t.Fatal(err)
	}
	if r.Summary.WorkDays != 5 || r.Summary.OfficePersonDays != 3 || len(r.People) != 3 {
		t.Errorf("report: %+v", r.Summary)
	}

	// Purging a person freezes the month first; the anonymous figures stay.
	e.now = time.Date(2026, 12, 2, 12, 0, 0, 0, e.cfg.Location)
	prev, err := e.stats.PreviewPurge(e.ctx, storage.PurgeScope{UserID: "U2"})
	if err != nil || prev.Rows != 5 || prev.Responses != 1 {
		t.Fatalf("preview: %+v %v", prev, err)
	}
	if _, err := e.stats.Purge(e.ctx, storage.PurgeScope{UserID: "U2"}, "cli"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.GetParticipant(e.ctx, p.ID, "U2"); err == nil {
		t.Error("participation of the purged person survived")
	}
	// A recalculation of the week must not bring U2 back.
	if _, err := e.store.RequestSummaryResync(e.ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.stats.SyncPolls(e.ctx); err != nil {
		t.Fatal(err)
	}
	if rows, _ := e.store.AttendanceDays(e.ctx, "T1", "U2", day("2026-11-01"), day("2026-11-30")); len(rows) != 0 {
		t.Errorf("purged person recalculated: %+v", rows)
	}
	r, err = e.stats.Report(e.ctx, "2026-11", "2026-11", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if r.StoredMonths != 1 || r.Summary.OfficePersonDays != 3 || r.Summary.PersonMonths != 3 {
		t.Errorf("frozen month not used: stored %d, %+v", r.StoredMonths, r.Summary)
	}
	r, _ = e.stats.Report(e.ctx, "2026-11", "2026-11", true, false)
	if r.SkippedMonths != 1 || r.Summary.WorkDays != 0 {
		t.Errorf("checked days only with a frozen month: skipped %d, %+v", r.SkippedMonths, r.Summary)
	}
}

func TestRetention(t *testing.T) {
	e := newEnv(t, map[string]string{"STATS_RETENTION_MONTHS": "2"})
	e.runWeek(t, isoweek.Week{Year: 2026, Week: 45}, map[string]domain.Days{"U1": {true}})
	e.runWeek(t, isoweek.Week{Year: 2027, Week: 2}, map[string]domain.Days{"U1": {true, true}})
	// 1 February 2027: November 2026 is older than two months, January not.
	e.now = time.Date(2027, 2, 1, 9, 0, 0, 0, e.cfg.Location)
	if err := e.stats.Sync(e.ctx, e.now); err != nil {
		t.Fatal(err)
	}
	first, _, ok, err := e.store.AttendanceRange(e.ctx, "T1")
	if err != nil || !ok || first != day("2027-01-11") {
		t.Errorf("daily data after retention starts %s (ok %v, %v)", first, ok, err)
	}
	stored, _ := e.store.ListMonthlyStats(e.ctx, "T1", "2026-11", "2027-01")
	if len(stored) != 2 || !stored[0].Frozen || stored[1].Frozen {
		t.Errorf("monthly figures: %+v", stored)
	}
	r, err := e.stats.Report(e.ctx, "2026-11", "2027-01", false, false)
	if err != nil || r.Summary.OfficePersonDays != 3 || r.StoredMonths != 1 {
		t.Errorf("report across retention: %+v %v", r, err)
	}
}
