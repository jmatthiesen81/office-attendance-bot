package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"officebot/internal/domain"
	"officebot/internal/i18n"
	"officebot/internal/isoweek"
	"officebot/internal/poll"
	"officebot/internal/storage"
)

func (s *Server) actor(r *http.Request) string { return viewerFrom(r).User.SlackUserID }

func (s *Server) today() domain.Date { return domain.DateOf(s.Now().In(s.Cfg.Location)) }

// people returns the names of everybody the bot knows, by Slack user ID.
func (s *Server) people(ctx context.Context) ([]storage.Person, map[string]string, error) {
	list, err := s.Store.KnownPeople(ctx, s.TeamID)
	if err != nil {
		return nil, nil, err
	}
	names := make(map[string]string, len(list))
	for _, p := range list {
		names[p.SlackUserID] = p.Name
	}
	return list, names, nil
}

func nameOf(names map[string]string, id string) string {
	if n := names[id]; n != "" {
		return n
	}
	return id
}

// ------------------------------------------------------------------- weeks

type weekRow struct {
	UserID   string
	Name     string
	Days     [domain.NumDays]bool
	Away     [domain.NumDays]bool
	Answered bool
}

type weekData struct {
	Week       string
	Prev, Next string
	Poll       *domain.Poll
	Open       bool
	DayLabels  [domain.NumDays]string
	DayKeys    [domain.NumDays]string
	Rows       []weekRow
	Recent     []string
}

func (s *Server) handleWeeks(w http.ResponseWriter, r *http.Request) {
	week, _ := s.Svc.CurrentTargetWeek(s.Now())
	http.Redirect(w, r, "/weeks/"+week.String(), http.StatusSeeOther)
}

func (s *Server) loadWeek(r *http.Request) (isoweek.Week, *domain.Poll, error) {
	week, err := isoweek.Parse(r.PathValue("week"))
	if err != nil {
		return week, nil, err
	}
	p, err := s.Store.GetPollByWeek(r.Context(), s.TeamID, week.Year, week.Week)
	if errors.Is(err, storage.ErrNotFound) {
		return week, nil, nil
	}
	return week, p, err
}

func (s *Server) handleWeek(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	lang := s.lang(r)
	week, p, err := s.loadWeek(r)
	if err != nil {
		s.fail(w, r, http.StatusNotFound, "err.not_found", nil)
		return
	}
	d := weekData{Week: week.String(), Prev: week.Add(-1).String(), Next: week.Add(1).String(), Poll: p, DayKeys: domain.DayKeys}
	loc := s.Cfg.Location
	for i := range d.DayLabels {
		day := week.Monday(loc).AddDate(0, 0, i)
		d.DayLabels[i] = weekdayShort(lang, day.Weekday()) + " " + shortDate(lang, day)
	}
	if recent, err := s.Store.ListPolls(ctx, s.TeamID, 26); err == nil {
		for _, rp := range recent {
			d.Recent = append(d.Recent, weekKey(rp.ISOYear, rp.ISOWeek))
		}
	}
	if p != nil {
		d.Open = p.IsAcceptingResponses(s.Now())
		snap, err := s.Store.PollSnapshot(ctx, p.ID)
		if err != nil {
			s.fail(w, r, http.StatusInternalServerError, "err.internal", err)
			return
		}
		dates := snap.Poll.Dates(loc)
		for _, part := range snap.Participants {
			row := weekRow{UserID: part.SlackUserID, Name: part.DisplayName}
			if resp, ok := snap.Responses[part.SlackUserID]; ok {
				row.Days, row.Answered = resp.Days, true
			}
			for i, day := range dates {
				row.Away[i] = domain.AwayOn(snap.Absences, part.SlackUserID, day)
			}
			d.Rows = append(d.Rows, row)
		}
		sort.SliceStable(d.Rows, func(i, j int) bool { return strings.ToLower(d.Rows[i].Name) < strings.ToLower(d.Rows[j].Name) })
	}
	s.render(w, r, http.StatusOK, "week", &Page{Title: tr(lang, "week.title", week.Week, week.Year), Nav: "weeks", Data: d})
}

func (s *Server) handleWeekAttendance(w http.ResponseWriter, r *http.Request) {
	week, p, err := s.loadWeek(r)
	back := "/weeks/" + week.String()
	if err != nil || p == nil {
		redirect(w, r, back, "", "not_found")
		return
	}
	var days domain.Days
	for _, k := range r.PostForm["day"] {
		i, ok := domain.DayIndex(k)
		if !ok {
			redirect(w, r, back, "", "invalid")
			return
		}
		days[i] = true
	}
	_, _, changed, err := s.Svc.SetAttendance(r.Context(), p.ID, r.PostFormValue("user"), poll.AttendanceSet, days, r.PostFormValue("notice") == "1", s.actor(r))
	switch {
	case err != nil:
		s.Log.Warn("set attendance", "err", err)
		redirect(w, r, back, "", "invalid")
	case !changed:
		redirect(w, r, back, "unchanged", "")
	default:
		redirect(w, r, back, "queued", "")
	}
}

func (s *Server) handleWeekRefresh(w http.ResponseWriter, r *http.Request) {
	week, p, err := s.loadWeek(r)
	back := "/weeks/" + week.String()
	if err != nil || p == nil {
		redirect(w, r, back, "", "not_found")
		return
	}
	if r.PathValue("action") == "resync" {
		_, err = s.Store.RequestSummaryResync(r.Context(), p.ID)
	} else {
		_, err = s.Svc.RefreshMessages(r.Context(), *p)
	}
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, "err.internal", err)
		return
	}
	s.Svc.Audit(r.Context(), s.actor(r), "week."+r.PathValue("action"), map[string]any{"week": week.String()})
	redirect(w, r, back, "queued", "")
}

// ---------------------------------------------------------------- time off

type absenceRow struct {
	domain.Absence
	Name string
}

type absenceData struct {
	Rows   []absenceRow
	People []storage.Person
	Today  string
}

func (s *Server) handleAbsences(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	list, names, err := s.people(ctx)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, "err.internal", err)
		return
	}
	abs, err := s.Store.ListAbsences(ctx, s.TeamID, "", s.today())
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, "err.internal", err)
		return
	}
	d := absenceData{People: list, Today: s.today().String()}
	for _, a := range abs {
		d.Rows = append(d.Rows, absenceRow{Absence: a, Name: nameOf(names, a.SlackUserID)})
	}
	sort.SliceStable(d.Rows, func(i, j int) bool { return d.Rows[i].Start.Before(d.Rows[j].Start) })
	s.render(w, r, http.StatusOK, "absences", &Page{Title: tr(s.lang(r), "nav.absences"), Nav: "absences", Data: d})
}

func parseRange(r *http.Request) (domain.Date, domain.Date, error) {
	from, err := domain.ParseDate(r.PostFormValue("from"))
	if err != nil {
		return from, from, err
	}
	toValue := r.PostFormValue("to")
	if toValue == "" {
		return from, from, nil
	}
	to, err := domain.ParseDate(toValue)
	return from, to, err
}

func dateErrKey(err error) string {
	switch {
	case errors.Is(err, poll.ErrPastDate):
		return "past"
	case errors.Is(err, poll.ErrDateRange):
		return "range"
	}
	return "invalid"
}

func (s *Server) handleAbsenceAdd(w http.ResponseWriter, r *http.Request) {
	user := r.PostFormValue("user")
	from, to, err := parseRange(r)
	if err != nil || user == "" {
		redirect(w, r, "/absences", "", "invalid")
		return
	}
	if _, err := s.Svc.AddTimeOff(r.Context(), user, from, to, s.actor(r)); err != nil {
		redirect(w, r, "/absences", "", dateErrKey(err))
		return
	}
	redirect(w, r, "/absences", "added", "")
}

func (s *Server) handleAbsenceDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PostFormValue("id"), 10, 64)
	if err != nil {
		redirect(w, r, "/absences", "", "invalid")
		return
	}
	if _, err := s.Svc.DeleteTimeOff(r.Context(), id, s.actor(r)); errors.Is(err, storage.ErrNotFound) {
		redirect(w, r, "/absences", "", "not_found")
		return
	} else if err != nil {
		s.fail(w, r, http.StatusInternalServerError, "err.internal", err)
		return
	}
	redirect(w, r, "/absences", "removed", "")
}

// --------------------------------------------------------------- languages

type languageRow struct {
	UserID, Name, Language, LanguageName string
	UpdatedAt                            time.Time
}

type languageOption struct{ Code, Name string }

type languageData struct {
	Rows      []languageRow
	People    []storage.Person
	Languages []languageOption
	Default   string
}

func (s *Server) handleLanguages(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	list, names, err := s.people(ctx)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, "err.internal", err)
		return
	}
	langs, err := s.Store.ListUserLanguages(ctx, s.TeamID)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, "err.internal", err)
		return
	}
	d := languageData{People: list, Default: s.Cfg.Translator.T("language.name")}
	for _, l := range langs {
		row := languageRow{UserID: l.SlackUserID, Name: nameOf(names, l.SlackUserID), Language: l.Language, UpdatedAt: l.UpdatedAt.In(s.Cfg.Location)}
		if t, err := i18n.New(l.Language); err == nil {
			row.LanguageName = t.T("language.name")
		}
		d.Rows = append(d.Rows, row)
	}
	for _, code := range i18n.Languages() {
		d.Languages = append(d.Languages, languageOption{Code: code, Name: i18n.MustNew(code).T("language.name")})
	}
	s.render(w, r, http.StatusOK, "languages", &Page{Title: tr(s.lang(r), "nav.languages"), Nav: "languages", Data: d})
}

func (s *Server) handleLanguageSet(w http.ResponseWriter, r *http.Request) {
	user := r.PostFormValue("user")
	if user == "" {
		redirect(w, r, "/languages", "", "invalid")
		return
	}
	if _, err := s.Svc.SetLanguage(r.Context(), user, r.PostFormValue("language"), s.actor(r)); err != nil {
		s.Log.Warn("set language", "err", err)
		redirect(w, r, "/languages", "", "invalid")
		return
	}
	redirect(w, r, "/languages", "queued", "")
}

func (s *Server) handleLanguageReset(w http.ResponseWriter, r *http.Request) {
	changed, err := s.Svc.ResetLanguage(r.Context(), r.PostFormValue("user"), s.actor(r))
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, "err.internal", err)
		return
	}
	if !changed {
		redirect(w, r, "/languages", "unchanged", "")
		return
	}
	redirect(w, r, "/languages", "queued", "")
}

// ------------------------------------------------------------- closed days

type closedData struct {
	Days  []storage.ClosedDay
	Today string
}

func (s *Server) handleClosedDays(w http.ResponseWriter, r *http.Request) {
	today := s.today()
	days, err := s.Store.ListClosedDays(r.Context(), s.TeamID, today.AddDays(-400), today.AddDays(400))
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, "err.internal", err)
		return
	}
	s.render(w, r, http.StatusOK, "closed_days", &Page{Title: tr(s.lang(r), "nav.closed_days"), Nav: "closed_days",
		Data: closedData{Days: days, Today: today.String()}})
}

func (s *Server) handleClosedDayAdd(w http.ResponseWriter, r *http.Request) {
	from, to, err := parseRange(r)
	if err != nil || to.Before(from) || to.After(from.AddDays(366)) {
		redirect(w, r, "/closed-days", "", "invalid")
		return
	}
	reason := strings.TrimSpace(r.PostFormValue("reason"))
	if len(reason) > 200 {
		reason = reason[:200]
	}
	if _, err := s.Store.AddClosedDays(r.Context(), s.TeamID, from, to, reason, s.actor(r), s.Now()); err != nil {
		s.fail(w, r, http.StatusInternalServerError, "err.internal", err)
		return
	}
	redirect(w, r, "/closed-days", "added", "")
}

func (s *Server) handleClosedDayRemove(w http.ResponseWriter, r *http.Request) {
	day, err := domain.ParseDate(r.PostFormValue("day"))
	if err != nil {
		redirect(w, r, "/closed-days", "", "invalid")
		return
	}
	if err := s.Store.RemoveClosedDay(r.Context(), s.TeamID, day, s.actor(r), s.Now()); errors.Is(err, storage.ErrNotFound) {
		redirect(w, r, "/closed-days", "", "not_found")
		return
	} else if err != nil {
		s.fail(w, r, http.StatusInternalServerError, "err.internal", err)
		return
	}
	redirect(w, r, "/closed-days", "removed", "")
}

// ------------------------------------------------------------------- audit

type auditRow struct {
	storage.AuditEntry
	ActorName string
	Text      string
}

type auditData struct {
	Rows     []auditRow
	Page     int
	PrevPage int
	NextPage int
}

const auditPageSize = 100

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	page = max(page, 1)
	entries, err := s.Store.ListAudit(ctx, s.TeamID, auditPageSize+1, (page-1)*auditPageSize)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, "err.internal", err)
		return
	}
	_, names, err := s.people(ctx)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, "err.internal", err)
		return
	}
	if users, err := s.Store.ListWebUsers(ctx, s.TeamID); err == nil {
		for _, u := range users {
			if names[u.SlackUserID] == "" {
				names[u.SlackUserID] = u.DisplayName
			}
		}
	}
	d := auditData{Page: page, PrevPage: page - 1}
	if len(entries) > auditPageSize {
		entries, d.NextPage = entries[:auditPageSize], page+1
	}
	for _, e := range entries {
		e.At = e.At.In(s.Cfg.Location)
		d.Rows = append(d.Rows, auditRow{AuditEntry: e, ActorName: nameOf(names, e.Actor), Text: formatDetails(e.Details, names)})
	}
	s.render(w, r, http.StatusOK, "audit", &Page{Title: tr(s.lang(r), "nav.audit"), Nav: "audit", Data: d})
}

// formatDetails renders audit details as "key=value" pairs; Slack user IDs
// get the person's name.
func formatDetails(details map[string]any, names map[string]string) string {
	keys := make([]string, 0, len(details))
	for k := range details {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		v := fmt.Sprint(details[k])
		if k == "user" {
			if n := names[v]; n != "" {
				v = n + " (" + v + ")"
			}
		}
		parts = append(parts, k+"="+v)
	}
	return strings.Join(parts, ", ")
}
