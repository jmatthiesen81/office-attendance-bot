package web

import (
	"encoding/csv"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"officebot/internal/domain"
	"officebot/internal/stats"
	"officebot/internal/storage"
)

// statsQuery are the filters of the statistics page.
type statsQuery struct {
	From, To string // "2026-01"
	Checked  bool
	Persons  bool
}

func (s *Server) parseStatsQuery(r *http.Request) statsQuery {
	q := r.URL.Query()
	today := s.today().In(time.UTC)
	cur := today.Format("2006-01")
	def := today.AddDate(0, -11, 0).Format("2006-01")
	valid := func(m string) bool { _, err := time.Parse("2006-01", m); return err == nil }
	sq := statsQuery{From: q.Get("from"), To: q.Get("to"), Checked: q.Get("checked") == "1", Persons: q.Get("persons") == "1"}
	if !valid(sq.From) {
		sq.From = def
	}
	if !valid(sq.To) || sq.To > cur {
		sq.To = cur
	}
	if sq.From > sq.To {
		sq.From = sq.To
	}
	return sq
}

func (q statsQuery) query() string {
	v := "from=" + q.From + "&to=" + q.To
	if q.Checked {
		v += "&checked=1"
	}
	return v
}

type monthRow struct {
	*stats.Month
	Name         string
	Average      float64
	Max          int
	ResponseRate float64
	CheckedShare float64
}

type statsData struct {
	Q            statsQuery
	Query        string
	R            *stats.Report
	Months       []monthRow
	PerPerson    bool // allowed for this person
	Desks95      int
	Desks90      int
	DesksMax     int
	LowResponse  bool
	Unchecked    float64
	DaysChart    template.HTML
	WeekdayChart template.HTML
	DesksChart   template.HTML
	OfficeChart  template.HTML
	RunChart     template.HTML
	OfficeLabels []string
	RunLabels    []string
	Weekdays     [domain.NumDays]string
	DeskRows     []deskRow
}

type deskRow struct {
	Desks int
	Share float64
}

func (s *Server) perPersonAllowed(r *http.Request) bool {
	v := viewerFrom(r)
	return s.Cfg.WebStatsPerPerson && v != nil && v.User.Role == storage.RoleAdmin
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	lang := s.lang(r)
	q := s.parseStatsQuery(r)
	allowed := s.perPersonAllowed(r)
	if q.Persons && !allowed {
		q.Persons = false
	}
	rep, err := s.Stats.Report(ctx, q.From, q.To, q.Checked, q.Persons)
	if errors.Is(err, stats.ErrNoRange) {
		redirect(w, r, "/stats", "", "invalid")
		return
	}
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, "err.internal", err)
		return
	}
	if q.Persons {
		s.Svc.Audit(ctx, s.actor(r), "stats.view_persons", map[string]any{"from": q.From, "to": q.To, "checked": q.Checked})
	}
	d := statsData{Q: q, Query: q.query(), R: rep, PerPerson: allowed}
	sum := &rep.Summary
	if len(sum.Desks) > 0 {
		d.Desks95, d.Desks90, d.DesksMax = sum.DesksFor(0.95), sum.DesksFor(0.9), sum.Max
		for n, share := range sum.Desks {
			d.DeskRows = append(d.DeskRows, deskRow{Desks: n, Share: share})
		}
	}
	d.LowResponse = sum.WorkDays > 0 && sum.ResponseRate < 0.9
	d.Unchecked = 1 - sum.VerifiedShare
	for _, m := range rep.Months {
		row := monthRow{Month: m, Name: monthName(lang, m.Month), Max: len(m.Occupancy) - 1}
		if m.WorkDays > 0 {
			row.Average = float64(m.OfficePersonDays) / float64(m.WorkDays)
			row.CheckedShare = float64(m.VerifiedDays) / float64(m.WorkDays)
		}
		if m.Entries > 0 {
			row.ResponseRate = float64(m.Entries-m.NoAnswer) / float64(m.Entries)
		}
		d.Months = append(d.Months, row)
	}
	for i := range d.Weekdays {
		d.Weekdays[i] = weekdayShort(lang, domain.DayWeekdays[i])
	}
	d.OfficeLabels = []string{"0", "1–2", "3–5", "6–8", "9–12", "> 12"}
	d.RunLabels = []string{tr(lang, "stats.run_1w"), tr(lang, "stats.run_2w"), tr(lang, "stats.run_4w"), tr(lang, "stats.run_8w"), tr(lang, "stats.run_more")}
	s.charts(&d, lang)
	s.render(w, r, http.StatusOK, "stats", &Page{Title: tr(lang, "stats.title"), Nav: "stats", Data: d, PerPerson: allowed})
}

func (s *Server) charts(d *statsData, lang string) {
	rep := d.R
	people := tr(lang, "stats.people")
	var days []bar
	for _, p := range rep.Days {
		t := p.Day.In(time.UTC)
		days = append(days, bar{Label: shortDate(lang, t), Value: float64(p.Office),
			Title: fmt.Sprintf("%s %s: %d/%d %s", weekdayShort(lang, t.Weekday()), fmtDate(lang, t), p.Office, p.Total, people)})
	}
	ref := ""
	if rep.Desks > 0 {
		ref = tr(lang, "stats.desks_now", rep.Desks)
	}
	d.DaysChart = barChart(days, chartOpts{Ref: float64(rep.Desks), RefLabel: ref, Title: tr(lang, "stats.chart_days")})

	var wd []bar
	for i, v := range rep.Summary.WeekdayAverage {
		wd = append(wd, bar{Label: d.Weekdays[i], Value: v, Title: fmt.Sprintf("%s: Ø %s %s", d.Weekdays[i], trimFloat(v), people)})
	}
	d.WeekdayChart = barChart(wd, chartOpts{Width: 360, Ref: float64(rep.Desks), RefLabel: ref, Title: tr(lang, "stats.chart_weekdays")})

	var desks []bar
	for _, row := range d.DeskRows {
		desks = append(desks, bar{Label: itoa(row.Desks), Value: row.Share,
			Title: tr(lang, "stats.desks_tooltip", row.Desks, fmt.Sprintf("%.0f %%", row.Share*100))})
	}
	d.DesksChart = barChart(desks, chartOpts{Width: 360, Percent: true, Ref: 0.95, RefLabel: "95 %", MaxLabels: 15, Title: tr(lang, "stats.chart_desks")})

	var office []bar
	for i, n := range rep.Summary.OfficeBuckets {
		office = append(office, bar{Label: d.OfficeLabels[i], Value: float64(n), Title: tr(lang, "stats.office_tooltip", n, d.OfficeLabels[i])})
	}
	d.OfficeChart = barChart(office, chartOpts{Width: 360, Title: tr(lang, "stats.chart_office")})

	var run []bar
	for i, n := range rep.RunBuckets {
		run = append(run, bar{Label: d.RunLabels[i], Value: float64(n), Title: tr(lang, "stats.run_tooltip", n, d.RunLabels[i])})
	}
	d.RunChart = barChart(run, chartOpts{Width: 360, Title: tr(lang, "stats.chart_runs")})
}

// ----------------------------------------------------------------- exports

func csvResponse(w http.ResponseWriter, name string) *csv.Writer {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	return csv.NewWriter(w)
}

func f2(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) }

// handleStatsCSV exports the monthly figures without names.
func (s *Server) handleStatsCSV(w http.ResponseWriter, r *http.Request) {
	q := s.parseStatsQuery(r)
	rep, err := s.Stats.Report(r.Context(), q.From, q.To, q.Checked, false)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, "err.internal", err)
		return
	}
	cw := csvResponse(w, fmt.Sprintf("office-stats-%s-%s.csv", q.From, q.To))
	_ = cw.Write([]string{"month", "source", "work_days", "closed_days", "office_person_days", "average", "median", "p90", "p95", "max",
		"response_rate", "checked_days", "corrected_share", "people",
		"office_days_0", "office_days_1_2", "office_days_3_5", "office_days_6_8", "office_days_9_12", "office_days_13_plus",
		"longest_run_1w", "longest_run_2w", "longest_run_4w", "longest_run_8w", "longest_run_more"})
	for _, m := range rep.Months {
		sum := stats.Combine([]*stats.Month{m})
		source := "daily"
		if m.Stored {
			source = "monthly"
		}
		row := []string{m.Month, source, itoa(m.WorkDays), itoa(m.ClosedDays), itoa(m.OfficePersonDays), f2(sum.Average),
			itoa(sum.Median), itoa(sum.P90), itoa(sum.P95), itoa(sum.Max), f2(sum.ResponseRate), itoa(m.VerifiedDays), f2(sum.CorrectedShare), itoa(m.People)}
		for _, n := range m.OfficeBuckets {
			row = append(row, itoa(n))
		}
		for _, n := range m.RunBuckets {
			row = append(row, itoa(n))
		}
		_ = cw.Write(row)
	}
	cw.Flush()
}

// handlePersonsCSV exports the figures per person (admins, only with
// WEB_STATS_PER_PERSON); every export is audited.
func (s *Server) handlePersonsCSV(w http.ResponseWriter, r *http.Request) {
	if !s.perPersonAllowed(r) {
		s.fail(w, r, http.StatusForbidden, "err.per_person_off", nil)
		return
	}
	q := s.parseStatsQuery(r)
	rep, err := s.Stats.Report(r.Context(), q.From, q.To, q.Checked, true)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, "err.internal", err)
		return
	}
	s.Svc.Audit(r.Context(), s.actor(r), "stats.export_persons", map[string]any{"from": q.From, "to": q.To, "checked": q.Checked, "people": len(rep.People)})
	cw := csvResponse(w, fmt.Sprintf("office-stats-persons-%s-%s.csv", q.From, q.To))
	_ = cw.Write([]string{"user_id", "name", "month", "office", "remote", "away", "no_answer", "office_rate",
		"last_office_day", "current_run_start", "current_run_work_days", "longest_run_start", "longest_run_end", "longest_run_work_days",
		"longest_run_weeks", "longest_run_remote", "longest_run_away", "longest_run_no_answer"})
	date := func(d domain.Date) string {
		if d.IsZero() {
			return ""
		}
		return d.String()
	}
	for _, p := range rep.People {
		for _, m := range append(p.Months, stats.PersonMonth{Month: "total", Office: p.Total.Office, Remote: p.Total.Remote, Away: p.Total.Away, NoAnswer: p.Total.NoAnswer}) {
			_ = cw.Write([]string{p.UserID, csvSafe(p.Name), m.Month, itoa(m.Office), itoa(m.Remote), itoa(m.Away), itoa(m.NoAnswer), f2(m.Rate()),
				date(p.LastOffice), date(p.Current.Start), itoa(p.Current.WorkDays), date(p.Longest.Start), date(p.Longest.End),
				itoa(p.Longest.WorkDays), trimFloat(p.Longest.Weeks()), itoa(p.Longest.Remote), itoa(p.Longest.Away), itoa(p.Longest.NoAnswer)})
		}
	}
	cw.Flush()
}

// csvSafe keeps spreadsheet programs from running a name as a formula.
func csvSafe(v string) string {
	if v != "" && strings.ContainsRune("=+-@\t\r", rune(v[0])) {
		return "'" + v
	}
	return v
}
