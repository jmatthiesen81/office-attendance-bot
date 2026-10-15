// Package poll contains the business logic: starting weekly polls, saving
// responses, closing polls at the deadline, reminders and DM delivery.
package poll

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"officebot/internal/config"
	"officebot/internal/domain"
	"officebot/internal/i18n"
	"officebot/internal/isoweek"
	"officebot/internal/slack"
	"officebot/internal/storage"
	"officebot/internal/worker"
)

// Resolver determines the participants of a new poll and checks late joiners.
type Resolver interface {
	Resolve(ctx context.Context) ([]domain.Participant, error)
	// Eligible returns the participant if userID belongs to the target group
	// now, otherwise a non-empty reason.
	Eligible(ctx context.Context, userID string) (*domain.Participant, string, error)
}

// RequestDMCooldown limits how often one person can request a new DM.
const RequestDMCooldown = 2 * time.Minute

// Notifier is implemented by components that should wake up after a change.
type Notifier interface{ Notify() }

// Trigger is implemented by the summary updater.
type Trigger interface{ Trigger() }

// ErrDeadlinePassed is returned when starting a poll whose deadline is over.
var ErrDeadlinePassed = errors.New("deadline of this week has already passed")

// Service implements the poll workflow.
type Service struct {
	store        *storage.Store
	api          slack.API
	participants Resolver
	cfg          *config.Config
	workspaceID  string
	outbox       Notifier
	summary      Trigger
	log          *slog.Logger
	Now          func() time.Time

	// dmLocks serialise DM updates per participant so that a slower, older
	// render can never overwrite a newer one.
	dmLocks [64]sync.Mutex
}

type nopNotifier struct{}

func (nopNotifier) Notify()  {}
func (nopNotifier) Trigger() {}

// NewService creates the poll service. outbox and summary may be nil (CLI usage).
func NewService(store *storage.Store, api slack.API, participants Resolver, cfg *config.Config, workspaceID string,
	outbox Notifier, summary Trigger, log *slog.Logger) *Service {
	if outbox == nil {
		outbox = nopNotifier{}
	}
	if summary == nil {
		summary = nopNotifier{}
	}
	return &Service{store: store, api: api, participants: participants, cfg: cfg, workspaceID: workspaceID,
		outbox: outbox, summary: summary, log: log, Now: time.Now}
}

// tr returns the translator of the team default language, used for the
// channel summary. Texts only one person sees use trFor.
func (s *Service) tr() *i18n.Translator { return s.cfg.Translator }

// SetNotifiers wires the outbox worker and summary updater after construction.
func (s *Service) SetNotifiers(outbox Notifier, summary Trigger) {
	s.outbox, s.summary = outbox, summary
}

// ---------------------------------------------------------------- weekly start

// CurrentTargetWeek returns the week the latest scheduled poll start refers
// to, and that start time.
func (s *Service) CurrentTargetWeek(now time.Time) (isoweek.Week, time.Time) {
	last := isoweek.Prev(now, s.cfg.PollWeekday, s.cfg.PollTime.Hour, s.cfg.PollTime.Minute, s.cfg.Location)
	return isoweek.Of(last).Add(s.cfg.TargetWeekOffset()), last
}

// EnsureCurrentPoll starts the poll of the latest scheduled run if it does
// not exist yet. This also catches up runs missed during downtime, as long as
// the response deadline has not passed.
func (s *Service) EnsureCurrentPoll(ctx context.Context, now time.Time) error {
	week, _ := s.CurrentTargetWeek(now)
	if !now.Before(s.cfg.Deadline(week)) {
		return nil
	}
	_, _, err := s.StartWeek(ctx, week)
	return err
}

// StartWeek creates the poll for a week idempotently: if it already exists,
// the existing poll is returned and nothing is sent.
func (s *Service) StartWeek(ctx context.Context, week isoweek.Week) (*domain.Poll, bool, error) {
	if p, err := s.store.GetPollByWeek(ctx, s.workspaceID, week.Year, week.Week); err == nil {
		return p, false, nil
	} else if !errors.Is(err, storage.ErrNotFound) {
		return nil, false, err
	}
	now := s.Now()
	loc := s.cfg.Location
	p := &domain.Poll{
		WorkspaceID:      s.workspaceID,
		ISOYear:          week.Year,
		ISOWeek:          week.Week,
		WeekStart:        week.Monday(loc),
		WeekEnd:          week.Day(time.Friday, loc),
		Deadline:         s.cfg.Deadline(week),
		SummaryChannelID: s.cfg.SummaryChannelID,
	}
	if !now.Before(p.Deadline) {
		return nil, false, fmt.Errorf("start %s: %w", week, ErrDeadlinePassed)
	}
	parts, err := s.participants.Resolve(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("resolve participants: %w", err)
	}
	if len(parts) == 0 {
		return nil, false, fmt.Errorf("start %s: no participants found (check SLACK_PARTICIPANTS_CHANNEL_ID / SLACK_USER_IDS)", week)
	}
	created, err := s.store.CreatePoll(ctx, p, parts, now)
	if err != nil {
		return nil, false, fmt.Errorf("create poll %s: %w", week, err)
	}
	if !created {
		p, err = s.store.GetPollByWeek(ctx, s.workspaceID, week.Year, week.Week)
		return p, false, err
	}
	s.log.Info("poll started", "week", week.String(), "poll", p.ID, "participants", len(parts), "deadline", p.Deadline.In(loc))
	s.outbox.Notify()
	s.summary.Trigger()
	return p, true, nil
}

// CloseDue closes polls whose deadline has passed.
func (s *Service) CloseDue(ctx context.Context, now time.Time) error {
	ids, err := s.store.CloseDuePolls(ctx, now)
	if err != nil {
		return err
	}
	if len(ids) > 0 {
		s.log.Info("polls closed at deadline", "polls", ids)
		s.outbox.Notify()
		s.summary.Trigger()
	}
	return nil
}

// Cleanup deletes polls older than the retention period.
func (s *Service) Cleanup(ctx context.Context, now time.Time) error {
	n, err := s.store.DeletePollsBefore(ctx, now.AddDate(0, 0, -7*s.cfg.RetentionWeeks))
	if err == nil && n > 0 {
		s.log.Info("deleted old polls", "count", n)
	}
	if err != nil {
		return err
	}
	n, err = s.store.DeleteAbsencesBefore(ctx, domain.DateOf(now.In(s.cfg.Location)).AddDays(-7*s.cfg.RetentionWeeks))
	if err == nil && n > 0 {
		s.log.Info("deleted old absences", "count", n)
	}
	if err != nil {
		return err
	}
	n, err = s.store.DeleteUserSettingsBefore(ctx, now.AddDate(0, 0, -7*s.cfg.RetentionWeeks))
	if err == nil && n > 0 {
		s.log.Info("deleted settings of people no longer polled", "count", n)
	}
	return err
}

// ------------------------------------------------------------------ reminders

// ReminderTimes returns the configured reminder occurrences for a poll that
// lie after its creation and before its deadline.
func (s *Service) ReminderTimes(p *domain.Poll) []time.Time {
	week := isoweek.Week{Year: p.ISOYear, Week: p.ISOWeek}
	var out []time.Time
	for _, w := range []isoweek.Week{week.Add(-1), week} {
		for _, wd := range s.cfg.ReminderWeekdays {
			t := w.At(wd, s.cfg.ReminderTime.Hour, s.cfg.ReminderTime.Minute, s.cfg.Location)
			if t.After(p.CreatedAt) && t.Before(p.Deadline) {
				out = append(out, t)
			}
		}
	}
	return out
}

// RunDueReminders enqueues reminders for all due reminder rounds. Each
// round runs once; rounds missed during downtime collapse into one.
func (s *Service) RunDueReminders(ctx context.Context, now time.Time) error {
	if !s.cfg.ReminderEnabled || s.cfg.ReminderMaxCount == 0 {
		return nil
	}
	polls, err := s.store.OpenPolls(ctx)
	if err != nil {
		return err
	}
	for _, p := range polls {
		if !p.IsAcceptingResponses(now) {
			continue
		}
		var due []time.Time
		for _, t := range s.ReminderTimes(&p) {
			if !t.After(now) {
				due = append(due, t)
			}
		}
		if len(due) == 0 {
			continue
		}
		n, err := s.store.CreateReminderRound(ctx, p.ID, due, s.cfg.ReminderMaxCount, now)
		if err != nil {
			return err
		}
		if n > 0 {
			s.log.Info("reminders enqueued", "poll", p.ID, "count", n)
			s.outbox.Notify()
		}
	}
	return nil
}

// --------------------------------------------------------------- interactions

// HandleSave validates and persists a "Speichern" click. Identity comes
// exclusively from the Slack-authenticated payload; the poll reference is
// checked against the participant snapshot and the stored DM message.
func (s *Service) HandleSave(ctx context.Context, req slack.Interaction) {
	log := s.log.With("user", req.UserID, "poll", req.PollID)
	p, part, ok := s.dmInteraction(ctx, req, log)
	if !ok {
		return
	}
	now := s.Now()
	var noticeAt time.Time
	if s.cfg.SummaryChangeNotices {
		noticeAt = now.Add(s.cfg.SummaryChangeNoticeDelay)
	}
	resp, changed, err := s.store.SaveResponseWithNotice(ctx, p.ID, req.UserID, req.Days, now, noticeAt, s.cfg.SummaryChangeNoticeGrace)
	if err != nil {
		log.Error("save response", "err", err)
		s.RejectInteraction(ctx, req, s.trFor(ctx, req.UserID).T("reply.save_failed"))
		return
	}
	log.Info("response saved", "days", resp.Days.Count(), "revision", resp.Revision, "changed", changed)
	// The response is committed; Slack failures below never undo it.
	if changed {
		s.summary.Trigger()
		s.outbox.Notify() // a change notice may have been scheduled
	}
	s.refreshOrEnqueue(ctx, part)
}

// dmInteraction validates a click in a poll DM. It answers the user and
// returns ok = false if the click must not be processed.
func (s *Service) dmInteraction(ctx context.Context, req slack.Interaction, log *slog.Logger) (*domain.Poll, *domain.Participant, bool) {
	if req.TeamID != s.workspaceID {
		log.Warn("interaction from foreign workspace rejected", "team", req.TeamID)
		return nil, nil, false
	}
	tr := s.trFor(ctx, req.UserID)
	p, err := s.store.GetPoll(ctx, req.PollID)
	if errors.Is(err, storage.ErrNotFound) || (err == nil && p.WorkspaceID != req.TeamID) {
		log.Warn("interaction for unknown poll rejected")
		s.RejectInteraction(ctx, req, tr.T("reply.poll_gone"))
		return nil, nil, false
	}
	if err != nil {
		log.Error("load poll", "err", err)
		s.RejectInteraction(ctx, req, tr.T("reply.save_unavailable"))
		return nil, nil, false
	}
	part, err := s.store.GetParticipant(ctx, p.ID, req.UserID)
	if errors.Is(err, storage.ErrNotFound) {
		log.Warn("interaction from non-participant rejected")
		s.RejectInteraction(ctx, req, tr.T("reply.not_participant"))
		return nil, nil, false
	}
	if err != nil {
		log.Error("load participant", "err", err)
		s.RejectInteraction(ctx, req, tr.T("reply.save_unavailable"))
		return nil, nil, false
	}
	if (part.DMChannelID != "" && req.ChannelID != part.DMChannelID) || (part.DMMessageTS != "" && req.MessageTS != part.DMMessageTS) {
		log.Warn("interaction from unexpected message rejected", "channel", req.ChannelID, "ts", req.MessageTS)
		s.RejectInteraction(ctx, req, tr.T("reply.stale_dm"))
		return nil, nil, false
	}
	if !p.IsAcceptingResponses(s.Now()) {
		log.Info("interaction after deadline rejected")
		s.RejectInteraction(ctx, req, tr.T("reply.closed_no_changes", "deadline", tr.DateTime(p.Deadline.In(s.cfg.Location))))
		s.refreshOrEnqueue(ctx, part)
		return nil, nil, false
	}
	return p, part, true
}

func (s *Service) refreshOrEnqueue(ctx context.Context, part *domain.Participant) {
	if err := s.RefreshDM(ctx, part.ID); err != nil {
		s.log.Warn("DM update failed, queued for retry", "participant", part.ID, "err", err)
		if err := s.store.EnqueueJob(ctx, storage.JobUpdateDM, part.PollID, part.ID, "", s.Now()); err != nil {
			s.log.Error("enqueue DM update", "err", err)
		}
		s.outbox.Notify()
	}
}

// RejectInteraction shows an ephemeral message to the user.
func (s *Service) RejectInteraction(ctx context.Context, req slack.Interaction, reason string) {
	if req.TeamID != s.workspaceID || req.ChannelID == "" || req.UserID == "" {
		return
	}
	if err := s.api.PostEphemeral(ctx, req.ChannelID, req.UserID, reason); err != nil {
		s.log.Warn("ephemeral message failed", "user", req.UserID, "err", err)
	}
}

// HandleRequestDM handles "Abfrage an mich senden" in the channel summary:
// participants get a fresh DM (the old one is marked as superseded), late
// joiners of the target group are added to the running poll. Everybody else
// gets an explanation. All answers are ephemeral, i.e. only visible to the
// person who clicked.
func (s *Service) HandleRequestDM(ctx context.Context, req slack.Interaction) {
	log := s.log.With("user", req.UserID, "poll", req.PollID)
	if req.TeamID != s.workspaceID {
		log.Warn("interaction from foreign workspace rejected", "team", req.TeamID)
		return
	}
	reply := func(text string) { s.RejectInteraction(ctx, req, text) }
	tr := s.trFor(ctx, req.UserID)
	tryAgain := tr.T("reply.try_again")

	p, err := s.store.GetPoll(ctx, req.PollID)
	if errors.Is(err, storage.ErrNotFound) || (err == nil && p.WorkspaceID != req.TeamID) {
		log.Warn("request for unknown poll rejected")
		reply(tr.T("reply.poll_gone"))
		return
	}
	if err != nil {
		log.Error("load poll", "err", err)
		reply(tryAgain)
		return
	}
	if req.ChannelID != p.SummaryChannelID || req.MessageTS != p.SummaryMessageTS {
		log.Warn("request from unexpected message rejected", "channel", req.ChannelID, "ts", req.MessageTS)
		reply(tr.T("reply.stale_summary"))
		return
	}
	now := s.Now()
	if !p.IsAcceptingResponses(now) {
		reply(tr.T("reply.closed", "deadline", tr.DateTime(p.Deadline.In(s.cfg.Location))))
		return
	}

	part, err := s.store.GetParticipant(ctx, p.ID, req.UserID)
	if errors.Is(err, storage.ErrNotFound) {
		s.addLateJoiner(ctx, p, req, tr, reply)
		return
	}
	if err != nil {
		log.Error("load participant", "err", err)
		reply(tryAgain)
		return
	}

	mu := &s.dmLocks[part.ID%int64(len(s.dmLocks))]
	mu.Lock()
	defer mu.Unlock()
	switch {
	case part.DMMessageTS == "" && part.DeliveryStatus == domain.DeliveryPending:
		reply(tr.T("reply.dm_pending"))
		return
	case part.DeliveryStatus != domain.DeliveryAway && now.Sub(part.LastDeliveryAttempt) < RequestDMCooldown:
		reply(tr.T("reply.dm_cooldown"))
		return
	}
	if part.DMMessageTS != "" {
		resp, err := s.store.GetResponse(ctx, p.ID, part.SlackUserID)
		if err == nil {
			err = s.api.UpdateMessage(ctx, part.DMChannelID, part.DMMessageTS, slack.SupersededDM(s.view(p, resp, false, now, tr)))
		}
		if err != nil && !slack.IsMessageGone(err) {
			// Not critical: saves on the old message are rejected anyway.
			log.Warn("could not mark old DM as superseded", "err", err)
		}
	}
	if _, err := s.store.RequestNewDM(ctx, part.ID, now); err != nil {
		log.Error("request new DM", "err", err)
		reply(tryAgain)
		return
	}
	log.Info("poll DM re-requested")
	s.outbox.Notify()
	reply(tr.T("reply.dm_resent"))
}

func (s *Service) addLateJoiner(ctx context.Context, p *domain.Poll, req slack.Interaction, tr *i18n.Translator, reply func(string)) {
	log := s.log.With("user", req.UserID, "poll", p.ID)
	part, reason, err := s.participants.Eligible(ctx, req.UserID)
	if err != nil {
		log.Error("check late joiner", "err", err)
		reply(tr.T("reply.try_again"))
		return
	}
	if reason != "" {
		log.Info("request from user outside the target group", "reason", reason)
		reply(tr.T("reply.not_eligible"))
		return
	}
	created, err := s.store.AddParticipant(ctx, p.ID, *part, s.Now())
	if err != nil {
		log.Error("add late joiner", "err", err)
		reply(tr.T("reply.try_again"))
		return
	}
	if created {
		log.Info("late joiner added to poll")
		s.outbox.Notify()
		s.summary.Trigger()
	}
	reply(tr.T("reply.late_joiner_added"))
}

// ------------------------------------------------------------------- DM jobs

func (s *Service) view(p *domain.Poll, resp *domain.Response, away bool, now time.Time, tr *i18n.Translator) slack.DMView {
	return slack.DMView{
		PollID:    p.ID,
		Week:      isoweek.Week{Year: p.ISOYear, Week: p.ISOWeek},
		WeekStart: p.WeekStart,
		WeekEnd:   p.WeekEnd,
		Deadline:  p.Deadline,
		NextWeek:  s.cfg.TargetWeekOffset() == 1,
		Response:  resp,
		Closed:    !p.IsAcceptingResponses(now),
		Away:      away,
		Loc:       s.cfg.Location,
		T:         tr,
	}
}

// dmMetadataKey identifies one poll DM. Re-requested DMs carry their
// generation so that crash recovery never adopts an older DM.
func dmMetadataKey(p *domain.Poll, part *domain.Participant) string {
	key := fmt.Sprintf("%s:%d:%s", p.WorkspaceID, p.ID, part.SlackUserID)
	if part.DMGeneration > 0 {
		key += fmt.Sprintf("#%d", part.DMGeneration)
	}
	return key
}

// ExecuteJob implements worker.Executor.
func (s *Service) ExecuteJob(ctx context.Context, job *storage.Job) error {
	switch job.Type {
	case storage.JobSendDM:
		return s.sendDM(ctx, job)
	case storage.JobUpdateDM:
		return s.RefreshDM(ctx, job.ParticipantID)
	case storage.JobReminderDM:
		return s.sendReminder(ctx, job)
	case storage.JobChangeNotice:
		return s.postChangeNotice(ctx, job)
	default:
		return fmt.Errorf("%w: unknown job type %q", worker.ErrPermanent, job.Type)
	}
}

func (s *Service) load(ctx context.Context, participantID int64) (*domain.Poll, *domain.Participant, *domain.Response, error) {
	part, err := s.store.GetParticipantByID(ctx, participantID)
	if err != nil {
		return nil, nil, nil, err
	}
	p, err := s.store.GetPoll(ctx, part.PollID)
	if err != nil {
		return nil, nil, nil, err
	}
	resp, err := s.store.GetResponse(ctx, p.ID, part.SlackUserID)
	return p, part, resp, err
}

func (s *Service) markUndeliverable(ctx context.Context, p *domain.Poll, part *domain.Participant, err error) error {
	status := domain.DeliveryFailed
	if slack.IsUnreachableUser(err) {
		status = domain.DeliveryUnreachable
	}
	s.log.Error("poll DM cannot be delivered", "user", part.SlackUserID, "poll", p.ID, "status", status, "err", err)
	if serr := s.store.SetDeliveryStatus(ctx, part.ID, status, s.Now()); serr != nil {
		return serr
	}
	if _, serr := s.store.RequestSummaryResync(ctx, p.ID); serr != nil {
		return serr
	}
	s.summary.Trigger()
	return fmt.Errorf("%w: %v", worker.ErrPermanent, err)
}

func (s *Service) sendDM(ctx context.Context, job *storage.Job) error {
	p, part, resp, err := s.load(ctx, job.ParticipantID)
	if errors.Is(err, storage.ErrNotFound) {
		return nil // poll deleted meanwhile
	}
	if err != nil {
		return err
	}
	if part.DMMessageTS != "" {
		return nil
	}
	now := s.Now()
	if !p.IsAcceptingResponses(now) {
		return s.store.SetDeliveryStatus(ctx, part.ID, domain.DeliverySkipped, now)
	}
	away, err := s.awayAllWeek(ctx, p, part.SlackUserID)
	if err != nil {
		return err
	}
	if away && job.DedupeKey == "" {
		// Only the automatic weekly DM is held back; a requested one (late
		// joiner, re-request) shows the away state with an undo button.
		s.log.Info("poll DM not sent, participant is away all week", "user", part.SlackUserID, "poll", p.ID)
		return s.store.SetDeliveryStatus(ctx, part.ID, domain.DeliveryAway, now)
	}

	channel := part.DMChannelID
	if channel == "" {
		if channel, err = s.api.OpenDM(ctx, part.SlackUserID); err != nil {
			if slack.IsPermanent(err) {
				return s.markUndeliverable(ctx, p, part, err)
			}
			return fmt.Errorf("open DM: %w", err)
		}
		if err := s.store.SetDMChannel(ctx, part.ID, channel); err != nil {
			return err
		}
	}

	key := dmMetadataKey(p, part)
	if job.Attempts > 1 {
		// A previous attempt may have posted the DM without storing its ts.
		ts, found, err := s.api.FindMessageByMetadata(ctx, channel, slack.MetaPollDM, key)
		if err != nil {
			s.log.Debug("DM lookup by metadata failed", "user", part.SlackUserID, "err", err)
		} else if found {
			return s.store.MarkDelivered(ctx, part.ID, channel, ts, now)
		}
	}

	msg := slack.PollDM(s.view(p, resp, away, now, s.trFor(ctx, part.SlackUserID)))
	msg.Metadata = slack.NewMetadata(slack.MetaPollDM, key)
	ts, err := s.api.PostMessage(ctx, channel, msg)
	if err != nil {
		if slack.IsPermanent(err) {
			return s.markUndeliverable(ctx, p, part, err)
		}
		return fmt.Errorf("post DM: %w", err)
	}
	if err := s.store.MarkDelivered(ctx, part.ID, channel, ts, now); err != nil {
		return err
	}
	s.log.Info("poll DM sent", "user", part.SlackUserID, "poll", p.ID)
	return nil
}

// RefreshDM re-renders a participant's DM from the current database state.
func (s *Service) RefreshDM(ctx context.Context, participantID int64) error {
	mu := &s.dmLocks[participantID%int64(len(s.dmLocks))]
	mu.Lock()
	defer mu.Unlock()

	p, part, resp, err := s.load(ctx, participantID)
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if part.DMMessageTS == "" {
		return nil // not delivered yet; send_dm renders the current state
	}
	away, err := s.awayAllWeek(ctx, p, part.SlackUserID)
	if err != nil {
		return err
	}
	msg := slack.PollDM(s.view(p, resp, away, s.Now(), s.trFor(ctx, part.SlackUserID)))
	msg.Metadata = slack.NewMetadata(slack.MetaPollDM, dmMetadataKey(p, part))
	if err := s.api.UpdateMessage(ctx, part.DMChannelID, part.DMMessageTS, msg); err != nil {
		if slack.IsMessageGone(err) {
			s.log.Warn("poll DM no longer exists", "user", part.SlackUserID, "poll", p.ID)
			return nil
		}
		return err
	}
	return nil
}

func (s *Service) sendReminder(ctx context.Context, job *storage.Job) error {
	p, part, resp, err := s.load(ctx, job.ParticipantID)
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	now := s.Now()
	if resp != nil || !p.IsAcceptingResponses(now) || part.DMMessageTS == "" || part.ReminderCount >= s.cfg.ReminderMaxCount {
		return nil // answered meanwhile, closed or limit reached
	}
	if away, err := s.awayForReminder(ctx, p, part.SlackUserID, job.DedupeKey); err != nil || away {
		return err // no reminders while away
	}
	link, err := s.api.Permalink(ctx, part.DMChannelID, part.DMMessageTS)
	if err != nil {
		s.log.Debug("permalink failed", "err", err)
	}
	if _, err := s.api.PostMessage(ctx, part.DMChannelID, slack.Reminder(s.view(p, nil, false, now, s.trFor(ctx, part.SlackUserID)), link)); err != nil {
		return fmt.Errorf("post reminder: %w", err)
	}
	s.log.Info("reminder sent", "user", part.SlackUserID, "poll", p.ID)
	return s.store.IncrementReminderCount(ctx, part.ID)
}
