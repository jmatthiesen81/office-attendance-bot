package web

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"officebot/internal/domain"
	"officebot/internal/summary"
)

type overviewPoll struct {
	Poll     domain.Poll
	Week     string
	Deadline time.Time
	Answered int
	Total    int
	Synced   bool
	Rows     []overviewRow
}

type overviewRow struct {
	UserID    string
	Name      string
	Delivery  string
	Reminders int
	Answer    string // "", "away", "none" or a list of day names
}

type overviewData struct {
	Polls  []overviewPoll
	Outbox []string
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	lang := s.lang(r)
	loc := s.Cfg.Location
	polls, err := s.Store.OpenPolls(ctx)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, "err.internal", err)
		return
	}
	var d overviewData
	for _, p := range polls {
		snap, err := s.Store.PollSnapshot(ctx, p.ID)
		if err != nil {
			s.fail(w, r, http.StatusInternalServerError, "err.internal", err)
			return
		}
		st := summary.Aggregate(snap, loc)
		op := overviewPoll{Poll: p, Week: weekKey(p.ISOYear, p.ISOWeek), Deadline: p.Deadline.In(loc), Answered: st.Answered, Total: st.Total,
			Synced: p.SummaryMessageTS != "" && p.SummarySyncedVersion >= p.SummaryVersion}
		dates := snap.Poll.Dates(loc)
		for _, part := range snap.Participants {
			row := overviewRow{UserID: part.SlackUserID, Name: part.DisplayName, Delivery: part.DeliveryStatus, Reminders: part.ReminderCount}
			if domain.AwayAllWeek(snap.Absences, part.SlackUserID, dates) {
				row.Answer = "away"
			} else if resp, ok := snap.Responses[part.SlackUserID]; ok {
				row.Answer = "none"
				if resp.Days.Count() > 0 {
					row.Answer = strings.Join(resp.Days.Names(func(wd time.Weekday) string { return weekdayShort(lang, wd) }), ", ")
				}
			}
			op.Rows = append(op.Rows, row)
		}
		sort.SliceStable(op.Rows, func(i, j int) bool { return strings.ToLower(op.Rows[i].Name) < strings.ToLower(op.Rows[j].Name) })
		d.Polls = append(d.Polls, op)
	}
	stats, err := s.Store.JobStats(ctx)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, "err.internal", err)
		return
	}
	for k, n := range stats {
		if n > 0 && k != "done" {
			d.Outbox = append(d.Outbox, k+": "+itoa(n))
		}
	}
	sort.Strings(d.Outbox)
	s.render(w, r, http.StatusOK, "overview", &Page{Title: tr(lang, "overview.title"), Nav: "overview", Data: d})
}
