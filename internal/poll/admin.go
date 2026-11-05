package poll

import (
	"context"
	"errors"
	"fmt"

	"officebot/internal/domain"
	"officebot/internal/i18n"
	"officebot/internal/storage"
)

// Errors of the admin operations shared by the CLI and the web interface.
var (
	ErrPastDate  = errors.New("the date must not be in the past")
	ErrDateRange = errors.New("the end date must not be before the start date")
)

// audit records an admin change. The change itself is already committed,
// so a failure is only logged.
func (s *Service) audit(ctx context.Context, actor, action string, details map[string]any) {
	if err := s.store.Audit(ctx, s.workspaceID, actor, action, details, s.Now()); err != nil {
		s.log.Error("write audit log", "action", action, "actor", actor, "err", err)
	}
}

// recordPlan stores the daily status of closed polls before a correction.
func (s *Service) recordPlan(ctx context.Context) error {
	if s.SyncStats == nil {
		return nil
	}
	if err := s.SyncStats(ctx); err != nil {
		return fmt.Errorf("record daily status before the correction: %w", err)
	}
	return nil
}

// Audit records a change made outside the service, e.g. a purge.
func (s *Service) Audit(ctx context.Context, actor, action string, details map[string]any) {
	s.audit(ctx, actor, action, details)
}

func (s *Service) checkFutureRange(from, to domain.Date) error {
	if from.Before(s.today()) {
		return fmt.Errorf("%w (today is %s)", ErrPastDate, s.today())
	}
	if to.Before(from) {
		return ErrDateRange
	}
	return nil
}

// AddTimeOff enters time off of userID from from to to (both included) on
// behalf of actor. Past days are a record and cannot be added; overlapping
// and adjacent entries are merged.
func (s *Service) AddTimeOff(ctx context.Context, userID string, from, to domain.Date, actor string) (domain.Absence, error) {
	if err := s.checkFutureRange(from, to); err != nil {
		return domain.Absence{}, err
	}
	ab, err := s.store.AddAbsence(ctx, s.workspaceID, userID, from, to, actor, s.Now())
	if err != nil {
		return ab, err
	}
	s.audit(ctx, actor, "time_off.add", map[string]any{"user": userID, "from": from.String(), "to": to.String()})
	return ab, s.AbsencesChanged(ctx, userID)
}

// RemoveTimeOff makes userID present again from from to to. changed is
// false if there was no time off in that range.
func (s *Service) RemoveTimeOff(ctx context.Context, userID string, from, to domain.Date, actor string) (changed bool, err error) {
	if err := s.checkFutureRange(from, to); err != nil {
		return false, err
	}
	changed, err = s.store.RemoveAbsenceDays(ctx, s.workspaceID, userID, from, to, s.Now())
	if err != nil || !changed {
		return changed, err
	}
	s.audit(ctx, actor, "time_off.remove", map[string]any{"user": userID, "from": from.String(), "to": to.String()})
	return true, s.AbsencesChanged(ctx, userID)
}

// DeleteTimeOff deletes one time-off entry, also a past one (correction by
// an admin). It returns storage.ErrNotFound for an unknown ID.
func (s *Service) DeleteTimeOff(ctx context.Context, id int64, actor string) (*domain.Absence, error) {
	if err := s.recordPlan(ctx); err != nil {
		return nil, err
	}
	ab, err := s.store.DeleteAbsence(ctx, s.workspaceID, "", id)
	if err != nil {
		return nil, err
	}
	s.audit(ctx, actor, "time_off.delete", map[string]any{"user": ab.SlackUserID, "from": ab.Start.String(), "to": ab.End.String()})
	return ab, s.AbsencesChanged(ctx, ab.SlackUserID)
}

// SetLanguage sets the language of a person's DMs (ADR 0002) on behalf of actor.
func (s *Service) SetLanguage(ctx context.Context, userID, lang, actor string) (*i18n.Translator, error) {
	tr, err := i18n.New(lang)
	if err != nil {
		return nil, err
	}
	if err := s.store.SetUserLanguage(ctx, s.workspaceID, userID, tr.Lang(), s.Now()); err != nil {
		return nil, err
	}
	s.audit(ctx, actor, "language.set", map[string]any{"user": userID, "language": tr.Lang()})
	return tr, s.LanguageChanged(ctx, userID)
}

// ResetLanguage puts a person back on the team default. changed is false if
// they had not chosen a language.
func (s *Service) ResetLanguage(ctx context.Context, userID, actor string) (changed bool, err error) {
	changed, err = s.store.ResetUserLanguage(ctx, s.workspaceID, userID)
	if err != nil || !changed {
		return changed, err
	}
	s.audit(ctx, actor, "language.reset", map[string]any{"user": userID})
	return true, s.LanguageChanged(ctx, userID)
}

// Errors of the day check.
var (
	ErrPollOpen  = errors.New("the poll of this day is still running")
	ErrNotInPoll = errors.New("the day is not an office day of this poll")
)

// Day statuses an admin can set in the day check (see storage.Status*).
const (
	dayOffice = "office"
	dayRemote = "remote"
	dayAway   = "away"
)

// CorrectDay sets the actual status of userID on one day of a closed poll
// (day check, ADR 0003): office checks the day in the response, remote
// unchecks it and removes time off of that day, away unchecks it and enters
// time off for that day, also in the past. A person without an answer gets
// one; their other days then count as remote. changed is false if the
// status was already set.
func (s *Service) CorrectDay(ctx context.Context, pollID int64, userID string, day domain.Date, status, actor string) (changed bool, err error) {
	if status != dayOffice && status != dayRemote && status != dayAway {
		return false, fmt.Errorf("unknown status %q", status)
	}
	p, err := s.store.GetPoll(ctx, pollID)
	if err != nil {
		return false, err
	}
	if p.IsAcceptingResponses(s.Now()) {
		return false, ErrPollOpen
	}
	if err := s.recordPlan(ctx); err != nil {
		return false, err
	}
	idx := -1
	for i, d := range p.Dates(s.cfg.Location) {
		if d.String() == day.String() {
			idx = i
		}
	}
	if idx < 0 {
		return false, ErrNotInPoll
	}
	part, err := s.store.GetParticipant(ctx, pollID, userID)
	if err != nil {
		return false, err
	}
	resp, err := s.store.GetResponse(ctx, pollID, userID)
	if err != nil {
		return false, err
	}
	abs, err := s.store.AbsencesBetween(ctx, s.workspaceID, userID, day, day)
	if err != nil {
		return false, err
	}
	away := len(abs) > 0
	office := resp != nil && resp.Days[idx]
	before := dayRemote
	switch {
	case office:
		before = dayOffice
	case away:
		before = dayAway
	case resp == nil:
		before = "no_answer"
	}
	if before == status {
		return false, nil
	}

	now := s.Now()
	var days domain.Days
	if resp != nil {
		days = resp.Days
	}
	saveDay := func(on bool) error {
		if resp != nil && days[idx] == on {
			return nil
		}
		days[idx] = on
		_, _, err := s.store.SaveResponse(ctx, pollID, userID, days, now)
		return err
	}
	switch status {
	case dayOffice:
		err = saveDay(true)
	case dayRemote:
		if away {
			if _, err = s.store.RemoveAbsenceDays(ctx, s.workspaceID, userID, day, day, now); err != nil {
				return false, err
			}
		}
		err = saveDay(false)
	case dayAway:
		if office {
			if err = saveDay(false); err != nil {
				return false, err
			}
		}
		if !away {
			_, err = s.store.AddAbsence(ctx, s.workspaceID, userID, day, day, actor, now)
		}
	}
	if err != nil {
		return false, err
	}
	// Time off does not change the summary version by itself.
	if _, err := s.store.RequestSummaryResync(ctx, pollID); err != nil {
		return true, err
	}
	if part.DMMessageTS != "" {
		if err := s.store.EnqueueJob(ctx, storage.JobUpdateDM, pollID, part.ID, "", now); err != nil {
			return true, err
		}
	}
	s.audit(ctx, actor, "day.correct", map[string]any{"user": userID, "day": day.String(), "from": before, "to": status})
	s.summary.Trigger()
	s.outbox.Notify()
	return true, nil
}
