package web

import (
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"officebot/internal/domain"
	"officebot/internal/isoweek"
	"officebot/internal/poll"
	"officebot/internal/stats"
	"officebot/internal/storage"
)

// ---------------------------------------------------------------- day check

type dayRow struct {
	UserID, Name string
	Status       string
	Planned      string
	Corrected    bool
}

type dayData struct {
	Day        domain.Date
	Label      string
	Prev, Next string
	State      string // "ok", "no_poll", "open", "future"
	Closed     string // reason if the office is closed
	IsClosed   bool
	Verified   *storage.VerifiedDay
	VerifiedBy string
	Rows       []dayRow
	Counts     map[string]int
}

// workday returns d moved by step days, skipping weekends.
func workday(d domain.Date, step int) domain.Date {
	d = d.AddDays(step)
	for d.Weekday() == time.Saturday || d.Weekday() == time.Sunday {
		d = d.AddDays(step)
	}
	return d
}

func (s *Server) handleDays(w http.ResponseWriter, r *http.Request) {
	d := s.today()
	if d.Weekday() == time.Saturday || d.Weekday() == time.Sunday {
		d = workday(d, -1)
	}
	http.Redirect(w, r, "/days/"+d.String(), http.StatusSeeOther)
}

// dayPoll returns the poll of the week containing day.
func (s *Server) dayPoll(r *http.Request, day domain.Date) (*domain.Poll, error) {
	w := isoweek.Of(day.In(s.Cfg.Location))
	p, err := s.Store.GetPollByWeek(r.Context(), s.TeamID, w.Year, w.Week)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, nil
	}
	return p, err
}

func (s *Server) handleDay(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	lang := s.lang(r)
	day, err := domain.ParseDate(r.PathValue("date"))
	if err != nil {
		s.fail(w, r, http.StatusNotFound, "err.not_found", nil)
		return
	}
	d := dayData{Day: day, Prev: workday(day, -1).String(), Next: workday(day, 1).String(), State: "ok", Counts: map[string]int{},
		Label: weekdayShort(lang, day.Weekday()) + " " + fmtDate(lang, day.In(time.UTC))}
	if closed, err := s.Store.ListClosedDays(ctx, s.TeamID, day, day); err == nil && len(closed) > 0 {
		d.IsClosed, d.Closed = true, closed[0].Reason
	}
	if v, err := s.Store.VerifiedDays(ctx, s.TeamID, day, day); err == nil {
		if vd, ok := v[day.String()]; ok {
			vd.At = vd.At.In(s.Cfg.Location)
			d.Verified = &vd
		}
	}
	p, err := s.dayPoll(r, day)
	switch {
	case err != nil:
		s.fail(w, r, http.StatusInternalServerError, "err.internal", err)
		return
	case day.After(s.today()):
		d.State = "future"
	case p == nil:
		d.State = "no_poll"
	case p.IsAcceptingResponses(s.Now()):
		d.State = "open"
	}
	if d.State == "ok" {
		snap, err := s.Store.PollSnapshot(ctx, p.ID)
		if err != nil {
			s.fail(w, r, http.StatusInternalServerError, "err.internal", err)
			return
		}
		planned := map[string]string{}
		if rows, err := s.Store.AttendanceDays(ctx, s.TeamID, "", day, day); err == nil {
			for _, a := range rows {
				planned[a.SlackUserID] = a.Planned
			}
		}
		for _, a := range stats.WeekStatuses(snap, s.Cfg.Location) {
			if a.Day.String() != day.String() {
				continue
			}
			row := dayRow{UserID: a.SlackUserID, Name: a.Name, Status: a.Status, Planned: planned[a.SlackUserID]}
			row.Corrected = row.Planned != "" && row.Planned != row.Status
			d.Counts[a.Status]++
			d.Rows = append(d.Rows, row)
		}
		sort.SliceStable(d.Rows, func(i, j int) bool { return strings.ToLower(d.Rows[i].Name) < strings.ToLower(d.Rows[j].Name) })
		if d.Verified != nil {
			_, names, _ := s.people(ctx)
			d.VerifiedBy = nameOf(names, d.Verified.By)
		}
	}
	s.render(w, r, http.StatusOK, "day", &Page{Title: tr(lang, "nav.days"), Nav: "days", Data: d})
}

func (s *Server) handleDaySave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	day, err := domain.ParseDate(r.PathValue("date"))
	back := "/days/" + r.PathValue("date")
	if err != nil {
		redirect(w, r, "/days", "", "invalid")
		return
	}
	if day.After(s.today()) {
		redirect(w, r, back, "", "future")
		return
	}
	p, err := s.dayPoll(r, day)
	if err != nil || p == nil {
		redirect(w, r, back, "", "no_poll")
		return
	}
	parts, err := s.Store.ListParticipants(ctx, p.ID)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, "err.internal", err)
		return
	}
	changed := 0
	for _, part := range parts {
		status := r.PostFormValue("s_" + part.SlackUserID)
		if status == "" || status == storage.StatusNoAnswer {
			continue
		}
		ok, err := s.Svc.CorrectDay(ctx, p.ID, part.SlackUserID, day, status, s.actor(r))
		if errors.Is(err, poll.ErrPollOpen) {
			redirect(w, r, back, "", "poll_open")
			return
		}
		if err != nil {
			s.Log.Warn("day check", "user", part.SlackUserID, "err", err)
			redirect(w, r, back, "", "invalid")
			return
		}
		if ok {
			changed++
		}
	}
	verified := false
	if r.PostFormValue("verify") == "1" {
		if err := s.Store.VerifyDay(ctx, s.TeamID, day, s.actor(r), s.Now()); err != nil {
			s.fail(w, r, http.StatusInternalServerError, "err.internal", err)
			return
		}
		verified = true
	}
	if err := s.Stats.SyncPolls(ctx); err != nil {
		s.Log.Warn("recalculate daily status", "err", err)
	}
	switch {
	case verified:
		redirect(w, r, back, "verified", "")
	case changed > 0:
		redirect(w, r, back, "saved", "")
	default:
		redirect(w, r, back, "unchanged", "")
	}
}

func (s *Server) handleDayUnverify(w http.ResponseWriter, r *http.Request) {
	day, err := domain.ParseDate(r.PathValue("date"))
	back := "/days/" + r.PathValue("date")
	if err != nil {
		redirect(w, r, "/days", "", "invalid")
		return
	}
	if err := s.Store.UnverifyDay(r.Context(), s.TeamID, day, s.actor(r), s.Now()); err != nil && !errors.Is(err, storage.ErrNotFound) {
		s.fail(w, r, http.StatusInternalServerError, "err.internal", err)
		return
	}
	redirect(w, r, back, "unverified", "")
}

// ---------------------------------------------------------------- retention

type retentionData struct {
	RetentionMonths int
	RetentionWeeks  int
	First, Last     domain.Date
	HasDaily        bool
	FirstMonthly    string
	Years           []int
	People          []storage.Person
}

func (s *Server) handleRetention(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	d := retentionData{RetentionMonths: s.Cfg.StatsRetentionMonths, RetentionWeeks: s.Cfg.RetentionWeeks}
	var err error
	if d.First, d.Last, d.HasDaily, err = s.Store.AttendanceRange(ctx, s.TeamID); err != nil {
		s.fail(w, r, http.StatusInternalServerError, "err.internal", err)
		return
	}
	d.FirstMonthly, _ = s.Store.FirstMonthlyStats(ctx, s.TeamID)
	if d.HasDaily {
		for y := d.First.In(time.UTC).Year(); y <= d.Last.In(time.UTC).Year(); y++ {
			d.Years = append(d.Years, y)
		}
	}
	if d.People, _, err = s.people(ctx); err != nil {
		s.fail(w, r, http.StatusInternalServerError, "err.internal", err)
		return
	}
	s.render(w, r, http.StatusOK, "retention", &Page{Title: tr(s.lang(r), "nav.retention"), Nav: "retention", Data: d})
}

// purgeScope reads the scope of a purge from the form: kind is "year",
// "before" or "user", value the year, day or Slack user ID.
func purgeScope(r *http.Request) (storage.PurgeScope, bool) {
	var scope storage.PurgeScope
	value := strings.TrimSpace(r.PostFormValue("value"))
	switch r.PostFormValue("kind") {
	case "year":
		y, err := strconv.Atoi(value)
		if err != nil || y < 2000 || y > 2100 {
			return scope, false
		}
		scope.From, scope.To = domain.NewDate(y, 1, 1), domain.NewDate(y, 12, 31)
	case "before":
		d, err := domain.ParseDate(value)
		if err != nil {
			return scope, false
		}
		scope.To = d.AddDays(-1)
	case "user":
		if value == "" {
			return scope, false
		}
		scope.UserID = value
	default:
		return scope, false
	}
	return scope, true
}

type purgeData struct {
	Kind, Value string
	Name        string
	Preview     storage.PurgePreview
}

func (s *Server) handlePurgePreview(w http.ResponseWriter, r *http.Request) {
	scope, ok := purgeScope(r)
	if !ok {
		redirect(w, r, "/retention", "", "invalid")
		return
	}
	p, err := s.Stats.PreviewPurge(r.Context(), scope)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, "err.internal", err)
		return
	}
	d := purgeData{Kind: r.PostFormValue("kind"), Value: strings.TrimSpace(r.PostFormValue("value")), Preview: p}
	if scope.UserID != "" {
		_, names, _ := s.people(r.Context())
		d.Name = nameOf(names, scope.UserID)
	}
	s.render(w, r, http.StatusOK, "purge", &Page{Title: tr(s.lang(r), "purge.title"), Nav: "retention", Data: d})
}

func (s *Server) handlePurge(w http.ResponseWriter, r *http.Request) {
	scope, ok := purgeScope(r)
	if !ok || r.PostFormValue("confirm") != "1" {
		redirect(w, r, "/retention", "", "invalid")
		return
	}
	if _, err := s.Stats.Purge(r.Context(), scope, s.actor(r)); err != nil {
		s.fail(w, r, http.StatusInternalServerError, "err.internal", err)
		return
	}
	redirect(w, r, "/retention", "purged", "")
}
