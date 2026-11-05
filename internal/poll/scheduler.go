package poll

import (
	"context"
	"log/slog"
	"time"

	"officebot/internal/isoweek"
)

// Scheduler triggers weekly starts, deadline closing, reminders and
// retention cleanup. It sleeps on a timer until the next relevant moment
// (no busy waiting). Every tick is idempotent and catches up anything that
// became due while the process was down.
type Scheduler struct {
	svc *Service
	log *slog.Logger
	// MaxSleep bounds sleeps (clock jumps, suspended hosts, cleanup).
	MaxSleep time.Duration
	// ErrorRetry is the delay after a failed tick (e.g. Slack unreachable).
	ErrorRetry time.Duration
	// Extra steps run after the built-in ones on every tick.
	Extra []Step
}

// Step is a unit of scheduled work. It must be idempotent.
type Step struct {
	Name string
	Fn   func(context.Context, time.Time) error
}

// NewScheduler creates the scheduler.
func NewScheduler(svc *Service, log *slog.Logger) *Scheduler {
	return &Scheduler{svc: svc, log: log, MaxSleep: time.Hour, ErrorRetry: time.Minute}
}

// Run executes ticks until ctx is cancelled.
func (s *Scheduler) Run(ctx context.Context) {
	for {
		ok := s.Tick(ctx)
		if ctx.Err() != nil {
			return
		}
		now := s.svc.Now()
		next := s.NextWake(ctx, now)
		if !ok {
			next = now.Add(s.ErrorRetry)
		}
		s.log.Debug("scheduler sleeping", "until", next.In(s.svc.cfg.Location))
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// Tick performs all due work. It returns false if something failed.
func (s *Scheduler) Tick(ctx context.Context) bool {
	now := s.svc.Now()
	ok := true
	steps := append([]Step{
		{"close due polls", s.svc.CloseDue},
		{"start weekly poll", s.svc.EnsureCurrentPoll},
		{"reminders", s.svc.RunDueReminders},
		{"cleanup", s.svc.Cleanup},
	}, s.Extra...)
	for _, st := range steps {
		if err := st.Fn(ctx, now); err != nil {
			s.log.Error("scheduler: "+st.Name, "err", err)
			ok = false
		}
	}
	return ok
}

// NextWake returns the next moment at which something may become due.
func (s *Scheduler) NextWake(ctx context.Context, now time.Time) time.Time {
	cfg := s.svc.cfg
	next := now.Add(s.MaxSleep)
	consider := func(t time.Time) {
		if t.After(now) && t.Before(next) {
			next = t
		}
	}
	consider(isoweek.Next(now, cfg.PollWeekday, cfg.PollTime.Hour, cfg.PollTime.Minute, cfg.Location))
	if polls, err := s.svc.store.OpenPolls(ctx); err == nil {
		for _, p := range polls {
			consider(p.Deadline)
			if cfg.ReminderEnabled {
				for _, t := range s.svc.ReminderTimes(&p) {
					consider(t)
				}
			}
		}
	}
	// Small delay so that the wake-up lands safely after the boundary.
	return next.Add(time.Second)
}
