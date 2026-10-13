package poll

import (
	"context"
	"errors"
	"strconv"
	"time"

	"officebot/internal/domain"
	"officebot/internal/slack"
	"officebot/internal/storage"
)

// today returns the current calendar day in the poll time zone.
func (s *Service) today() domain.Date { return domain.DateOf(s.Now().In(s.cfg.Location)) }

// awayAllWeek reports whether userID is away on every office day of p's week.
func (s *Service) awayAllWeek(ctx context.Context, p *domain.Poll, userID string) (bool, error) {
	week := p.Dates(s.cfg.Location)
	abs, err := s.store.AbsencesBetween(ctx, s.workspaceID, userID, week[0], week[domain.NumDays-1])
	if err != nil {
		return false, err
	}
	return domain.AwayAllWeek(abs, userID, week), nil
}

// awayForReminder reports whether a reminder must be skipped because the
// person is away all week or on the day of the reminder round. The round is
// the reminder job's dedupe key (Unix seconds).
func (s *Service) awayForReminder(ctx context.Context, p *domain.Poll, userID, roundKey string) (bool, error) {
	if away, err := s.awayAllWeek(ctx, p, userID); err != nil || away {
		return away, err
	}
	round := s.Now()
	if sec, err := strconv.ParseInt(roundKey, 10, 64); err == nil {
		round = time.Unix(sec, 0)
	}
	day := domain.DateOf(round.In(s.cfg.Location))
	abs, err := s.store.AbsencesBetween(ctx, s.workspaceID, userID, day, day)
	return len(abs) > 0, err
}

// AbsencesChanged applies changed time off of userID to the running polls:
// the summary is re-rendered, a delivered DM switches between the away view
// and the normal one, and a DM held back because of an absence is sent now
// if the person is no longer away. It only writes to the database, so the
// CLI can use it as well; the running bot does the sending.
func (s *Service) AbsencesChanged(ctx context.Context, userID string) error {
	polls, err := s.store.OpenPolls(ctx)
	if err != nil {
		return err
	}
	now := s.Now()
	for _, p := range polls {
		if p.WorkspaceID != s.workspaceID || !p.IsAcceptingResponses(now) {
			continue
		}
		part, err := s.store.GetParticipant(ctx, p.ID, userID)
		if errors.Is(err, storage.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if _, err := s.store.RequestSummaryResync(ctx, p.ID); err != nil {
			return err
		}
		away, err := s.awayAllWeek(ctx, &p, userID)
		if err != nil {
			return err
		}
		switch {
		case part.DMMessageTS != "":
			err = s.store.EnqueueJob(ctx, storage.JobUpdateDM, p.ID, part.ID, "", now)
		case part.DeliveryStatus == domain.DeliveryAway && !away:
			s.log.Info("participant no longer away, sending poll DM", "user", userID, "poll", p.ID)
			_, err = s.store.RequestNewDM(ctx, part.ID, now)
		}
		if err != nil {
			return err
		}
	}
	s.outbox.Notify()
	s.summary.Trigger()
	return nil
}

// HandleAway handles "I'm away this week" (req.Away) and its undo in the
// poll DM: the office days of the polled week are added to or removed from
// the person's time off.
func (s *Service) HandleAway(ctx context.Context, req slack.Interaction) {
	log := s.log.With("user", req.UserID, "poll", req.PollID, "away", req.Away)
	p, part, ok := s.dmInteraction(ctx, req, log)
	if !ok {
		return
	}
	week := p.Dates(s.cfg.Location)
	var err error
	if req.Away {
		_, err = s.store.AddAbsence(ctx, s.workspaceID, req.UserID, week[0], week[domain.NumDays-1], req.UserID, s.Now())
	} else {
		_, err = s.store.RemoveAbsenceDays(ctx, s.workspaceID, req.UserID, week[0], week[domain.NumDays-1], s.Now())
	}
	if err != nil {
		log.Error("change absence", "err", err)
		s.RejectInteraction(ctx, req, s.tr().T("reply.save_failed"))
		return
	}
	log.Info("away state changed from poll DM")
	if err := s.AbsencesChanged(ctx, req.UserID); err != nil {
		log.Error("apply absence change", "err", err)
	}
	s.refreshOrEnqueue(ctx, part)
}

// timeOffModal renders the time-off form with the user's upcoming time off.
func (s *Service) timeOffModal(ctx context.Context, userID string) (*slack.ModalView, error) {
	today := s.today()
	upcoming, err := s.store.ListAbsences(ctx, s.workspaceID, userID, today)
	if err != nil {
		return nil, err
	}
	v := slack.TimeOffModal(s.tr(), upcoming, today)
	return &v, nil
}

// HandleOpenTimeOff opens the time-off form for the clicking user.
func (s *Service) HandleOpenTimeOff(ctx context.Context, req slack.Interaction) {
	log := s.log.With("user", req.UserID)
	if req.TeamID != s.workspaceID {
		log.Warn("interaction from foreign workspace rejected", "team", req.TeamID)
		return
	}
	v, err := s.timeOffModal(ctx, req.UserID)
	if err == nil {
		err = s.api.OpenView(ctx, req.TriggerID, *v)
	}
	if err != nil {
		log.Error("open time-off form", "err", err)
		s.RejectInteraction(ctx, req, s.tr().T("reply.try_again"))
	}
}

// HandleDeleteTimeOff deletes one of the user's own entries from the open
// time-off form and refreshes the form.
func (s *Service) HandleDeleteTimeOff(ctx context.Context, req slack.Interaction) {
	log := s.log.With("user", req.UserID, "absence", req.AbsenceID)
	if req.TeamID != s.workspaceID {
		log.Warn("interaction from foreign workspace rejected", "team", req.TeamID)
		return
	}
	_, err := s.store.DeleteAbsence(ctx, s.workspaceID, req.UserID, req.AbsenceID)
	switch {
	case errors.Is(err, storage.ErrNotFound):
		log.Warn("delete of unknown or foreign absence ignored")
	case err != nil:
		log.Error("delete absence", "err", err)
		return
	default:
		log.Info("absence deleted")
		if err := s.AbsencesChanged(ctx, req.UserID); err != nil {
			log.Error("apply absence change", "err", err)
		}
	}
	v, err := s.timeOffModal(ctx, req.UserID)
	if err == nil {
		err = s.api.UpdateView(ctx, req.ViewID, *v)
	}
	if err != nil {
		log.Warn("refresh time-off form", "err", err)
	}
}

// SubmitTimeOff validates and stores a submitted time-off form. It returns
// error messages per input block, or nil if the form may close.
func (s *Service) SubmitTimeOff(ctx context.Context, req slack.Interaction) map[string]string {
	log := s.log.With("user", req.UserID)
	if req.TeamID != s.workspaceID {
		log.Warn("interaction from foreign workspace rejected", "team", req.TeamID)
		return nil
	}
	tr := s.tr()
	errs := map[string]string{}
	today := s.today()
	switch {
	case req.Start.IsZero():
		errs[slack.TimeOffStartBlock] = tr.T("timeoff.error.missing")
	case req.End.IsZero():
		errs[slack.TimeOffEndBlock] = tr.T("timeoff.error.missing")
	case req.End.Before(req.Start):
		errs[slack.TimeOffEndBlock] = tr.T("timeoff.error.order")
	case req.End.Before(today):
		errs[slack.TimeOffEndBlock] = tr.T("timeoff.error.past")
	case req.End.After(domain.DateOf(today.In(time.UTC).AddDate(1, 0, 0))):
		errs[slack.TimeOffEndBlock] = tr.T("timeoff.error.too_far")
	}
	if len(errs) > 0 {
		return errs
	}
	a, err := s.store.AddAbsence(ctx, s.workspaceID, req.UserID, req.Start, req.End, req.UserID, s.Now())
	if err != nil {
		log.Error("add absence", "err", err)
		return map[string]string{slack.TimeOffEndBlock: tr.T("timeoff.error.failed")}
	}
	log.Info("absence planned", "start", a.Start, "end", a.End)
	if err := s.AbsencesChanged(ctx, req.UserID); err != nil {
		log.Error("apply absence change", "err", err)
	}
	return nil
}
