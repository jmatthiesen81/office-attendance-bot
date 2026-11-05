package stats

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"officebot/internal/config"
	"officebot/internal/domain"
	"officebot/internal/storage"
)

// Service keeps the daily status up to date and builds reports.
type Service struct {
	store *storage.Store
	cfg   *config.Config
	ws    string
	log   *slog.Logger
	Now   func() time.Time
}

// New creates the statistics service for a workspace.
func New(store *storage.Store, cfg *config.Config, workspaceID string, log *slog.Logger) *Service {
	return &Service{store: store, cfg: cfg, ws: workspaceID, log: log, Now: time.Now}
}

func (s *Service) today() domain.Date { return domain.DateOf(s.Now().In(s.cfg.Location)) }

// WeekStatuses derives the daily status of every participant of a poll:
// office if the day is checked in the response, else away if time off
// covers it, else remote if the person answered, else no answer.
func WeekStatuses(snap *storage.Snapshot, loc *time.Location) []storage.AttendanceDay {
	dates := snap.Poll.Dates(loc)
	var out []storage.AttendanceDay
	for _, part := range snap.Participants {
		resp, answered := snap.Responses[part.SlackUserID]
		for i, day := range dates {
			st := storage.StatusNoAnswer
			switch {
			case answered && resp.Days[i]:
				st = storage.StatusOffice
			case domain.AwayOn(snap.Absences, part.SlackUserID, day):
				st = storage.StatusAway
			case answered:
				st = storage.StatusRemote
			}
			out = append(out, storage.AttendanceDay{SlackUserID: part.SlackUserID, Name: part.DisplayName, Day: day, Status: st, Planned: st})
		}
	}
	return out
}

// Sync is the scheduler step: it recalculates changed weeks, updates the
// monthly figures and applies STATS_RETENTION_MONTHS.
func (s *Service) Sync(ctx context.Context, now time.Time) error {
	if err := s.SyncPolls(ctx); err != nil {
		return fmt.Errorf("daily status: %w", err)
	}
	if err := s.RefreshMonths(ctx); err != nil {
		return fmt.Errorf("monthly figures: %w", err)
	}
	return s.ApplyRetention(ctx)
}

// SyncPolls recalculates the daily status of every closed poll that changed.
func (s *Service) SyncPolls(ctx context.Context) error {
	dirty, err := s.store.DirtyClosedPolls(ctx, s.ws)
	if err != nil {
		return err
	}
	for _, d := range dirty {
		snap, err := s.store.PollSnapshot(ctx, d.Poll.ID)
		if err != nil {
			return err
		}
		week := fmt.Sprintf("%04d-W%02d", d.Poll.ISOYear, d.Poll.ISOWeek)
		if err := s.store.ReplaceAttendanceWeek(ctx, s.ws, d.Poll.ID, week, WeekStatuses(snap, s.cfg.Location), d.Dirty, s.Now()); err != nil {
			return err
		}
		s.log.Debug("daily status calculated", "week", week)
	}
	return nil
}

// input loads the daily data from from to to.
func (s *Service) input(ctx context.Context, from, to domain.Date, userID string, onlyVerified bool) (*Input, error) {
	rows, err := s.store.AttendanceDays(ctx, s.ws, userID, from, to)
	if err != nil {
		return nil, err
	}
	closedList, err := s.store.ListClosedDays(ctx, s.ws, from, to)
	if err != nil {
		return nil, err
	}
	closed := make(map[string]string, len(closedList))
	for _, c := range closedList {
		closed[c.Day.String()] = c.Reason
	}
	verified, err := s.store.VerifiedDays(ctx, s.ws, from, to)
	if err != nil {
		return nil, err
	}
	return &Input{Rows: rows, Closed: closed, Verified: verified, OnlyVerified: onlyVerified}, nil
}

func firstOfMonth(d domain.Date) domain.Date { return d.AddDays(1 - d.In(time.UTC).Day()) }

func lastOfMonth(d domain.Date) domain.Date {
	t := d.In(time.UTC)
	return domain.NewDate(t.Year(), t.Month()+1, 0)
}

func parseMonth(m string) (domain.Date, error) { return domain.ParseDate(m + "-01") }

// saveMonths stores the monthly figures of all months in from..to.
func (s *Service) saveMonths(ctx context.Context, from, to domain.Date, freeze bool) error {
	in, err := s.input(ctx, firstOfMonth(from), lastOfMonth(to), "", false)
	if err != nil {
		return err
	}
	for key, m := range Months(in) {
		data, err := json.Marshal(m)
		if err != nil {
			return err
		}
		if err := s.store.SaveMonthlyStats(ctx, s.ws, key, data, freeze, s.Now()); err != nil {
			return err
		}
	}
	return nil
}

// RefreshMonths stores the figures of every complete month with daily data.
// Frozen months are left alone (see storage.SaveMonthlyStats).
func (s *Service) RefreshMonths(ctx context.Context) error {
	first, _, ok, err := s.store.AttendanceRange(ctx, s.ws)
	if err != nil || !ok {
		return err
	}
	end := firstOfMonth(s.today()).AddDays(-1)
	if end.Before(first) {
		return nil
	}
	return s.saveMonths(ctx, first, end, false)
}

// ApplyRetention deletes daily data of months older than
// STATS_RETENTION_MONTHS; their monthly figures are frozen first.
func (s *Service) ApplyRetention(ctx context.Context) error {
	if s.cfg.StatsRetentionMonths == 0 {
		return nil
	}
	first, _, ok, err := s.store.AttendanceRange(ctx, s.ws)
	if err != nil || !ok {
		return err
	}
	t := s.today().In(time.UTC)
	cutoff := domain.NewDate(t.Year(), t.Month()-time.Month(s.cfg.StatsRetentionMonths), 1).AddDays(-1)
	if cutoff.Before(first) {
		return nil
	}
	n, err := s.Purge(ctx, storage.PurgeScope{To: cutoff}, "retention")
	if err == nil && n > 0 {
		s.log.Info("deleted daily status older than the retention period", "until", cutoff.String(), "rows", n)
	}
	return err
}

// PreviewPurge shows what Purge would delete.
func (s *Service) PreviewPurge(ctx context.Context, scope storage.PurgeScope) (storage.PurgePreview, error) {
	return s.store.PreviewPurge(ctx, s.ws, scope)
}

// Purge deletes daily data after freezing the monthly figures of all
// affected months, so statistics without names stay complete.
func (s *Service) Purge(ctx context.Context, scope storage.PurgeScope, actor string) (int64, error) {
	p, err := s.store.PreviewPurge(ctx, s.ws, scope)
	if err != nil {
		return 0, err
	}
	if p.Rows > 0 {
		if err := s.SyncPolls(ctx); err != nil {
			return 0, err
		}
		if err := s.saveMonths(ctx, p.First, p.Last, true); err != nil {
			return 0, err
		}
	} else if scope.UserID == "" {
		return 0, nil
	}
	return s.store.Purge(ctx, s.ws, scope, actor, s.Now())
}

// ------------------------------------------------------------------ report

// Report are the statistics of a period.
type Report struct {
	From, To     domain.Date
	OnlyVerified bool
	Months       []*Month
	Summary      Summary
	Days         []DayPoint
	RunBuckets   [NumRunBuckets]int
	RunPeople    int
	// StoredMonths are months taken from monthly_stats because their daily
	// data was deleted; SkippedMonths had no daily data and were left out
	// because only checked days were requested.
	StoredMonths  int
	SkippedMonths int
	People        []Person
	Desks         int
	FirstData     string // first month with any data
}

// ErrNoRange is returned for an empty or inverted period.
var ErrNoRange = errors.New("invalid period")

// Report builds the statistics from the first day of fromMonth to the last
// day of toMonth (at most today). perPerson adds the figures per person.
func (s *Service) Report(ctx context.Context, fromMonth, toMonth string, onlyVerified, perPerson bool) (*Report, error) {
	from, err := parseMonth(fromMonth)
	if err != nil {
		return nil, ErrNoRange
	}
	toStart, err := parseMonth(toMonth)
	if err != nil || toStart.Before(from) {
		return nil, ErrNoRange
	}
	if err := s.SyncPolls(ctx); err != nil {
		return nil, err
	}
	to := lastOfMonth(toStart)
	if today := s.today(); to.After(today) {
		to = today
	}
	r := &Report{From: from, To: to, OnlyVerified: onlyVerified, Desks: s.cfg.OfficeDesks}
	in, err := s.input(ctx, from, to, "", onlyVerified)
	if err != nil {
		return nil, err
	}
	stored, err := s.store.ListMonthlyStats(ctx, s.ws, monthKey(from), monthKey(to))
	if err != nil {
		return nil, err
	}
	// Frozen months come from monthly_stats; their remaining daily data is
	// not used, so all figures of a month have one source.
	frozen := map[string]bool{}
	for _, m := range stored {
		if m.Frozen {
			frozen[m.Month] = true
		}
	}
	if len(frozen) > 0 {
		rows := in.Rows[:0:0]
		for _, row := range in.Rows {
			if !frozen[monthKey(row.Day)] {
				rows = append(rows, row)
			}
		}
		in.Rows = rows
	}
	months := Months(in)
	for _, sm := range stored {
		if _, live := months[sm.Month]; live && !sm.Frozen {
			continue
		}
		if onlyVerified {
			r.SkippedMonths++
			continue
		}
		var m Month
		if err := json.Unmarshal(sm.Data, &m); err != nil {
			s.log.Warn("unreadable monthly figures", "month", sm.Month, "err", err)
			continue
		}
		m.Stored = true
		months[sm.Month] = &m
		r.StoredMonths++
	}
	for _, m := range months {
		r.Months = append(r.Months, m)
	}
	sort.Slice(r.Months, func(i, j int) bool { return r.Months[i].Month < r.Months[j].Month })
	r.Summary = Combine(r.Months)
	r.Days = Days(in)
	r.RunBuckets, r.RunPeople = RunBucketsOf(in)
	if first, _, ok, err := s.store.AttendanceRange(ctx, s.ws); err == nil && ok {
		r.FirstData = monthKey(first)
	}
	if fm, err := s.store.FirstMonthlyStats(ctx, s.ws); err == nil && fm != "" && (r.FirstData == "" || fm < r.FirstData) {
		r.FirstData = fm
	}
	if perPerson {
		first := from
		if f, _, ok, err := s.store.AttendanceRange(ctx, s.ws); err == nil && ok && f.Before(first) {
			first = f
		}
		history, err := s.input(ctx, first, to, "", onlyVerified)
		if err != nil {
			return nil, err
		}
		r.People = People(in, history)
	}
	return r, nil
}
