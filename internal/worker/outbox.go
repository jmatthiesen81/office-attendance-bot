// Package worker processes outbox jobs with retries.
package worker

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"officebot/internal/slack"
	"officebot/internal/storage"
)

// ErrPermanent wraps errors that must not be retried.
var ErrPermanent = errors.New("permanent failure")

// Executor runs a single job. Returning an error that wraps ErrPermanent or a
// permanent Slack error marks the job failed; other errors are retried.
type Executor interface {
	ExecuteJob(ctx context.Context, job *storage.Job) error
}

// Outbox processes due jobs sequentially. Sequential processing keeps the
// request rate low enough for Slack's limits; 429 responses are honoured.
type Outbox struct {
	store *storage.Store
	exec  Executor
	log   *slog.Logger

	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
	// IdleWake bounds the sleep when no job is due, so that jobs enqueued
	// by another process (CLI) are picked up.
	IdleWake time.Duration
	Now      func() time.Time

	wake chan struct{}
}

// NewOutbox creates the worker.
func NewOutbox(store *storage.Store, exec Executor, log *slog.Logger) *Outbox {
	return &Outbox{
		store: store, exec: exec, log: log,
		MaxAttempts: 8, BaseDelay: 5 * time.Second, MaxDelay: 30 * time.Minute, IdleWake: 30 * time.Second,
		Now:  time.Now,
		wake: make(chan struct{}, 1),
	}
}

// Notify wakes the worker after new jobs were enqueued. It never blocks.
func (o *Outbox) Notify() {
	select {
	case o.wake <- struct{}{}:
	default:
	}
}

// Run processes jobs until ctx is cancelled. Jobs left running by a crash
// are reset first.
func (o *Outbox) Run(ctx context.Context) {
	if n, err := o.store.ResetRunningJobs(ctx); err != nil {
		o.log.Error("reset running jobs", "err", err)
	} else if n > 0 {
		o.log.Info("resumed interrupted jobs", "count", n)
	}
	for {
		processed, err := o.ProcessDue(ctx)
		if err != nil && ctx.Err() == nil {
			o.log.Error("outbox processing", "err", err)
		}
		if ctx.Err() != nil {
			return
		}
		if processed > 0 {
			continue
		}
		sleep := o.IdleWake
		if next, ok, err := o.store.NextJobDue(ctx); err == nil && ok {
			sleep = min(max(next.Sub(o.Now()), 100*time.Millisecond), o.IdleWake)
		}
		timer := time.NewTimer(sleep)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-o.wake:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// ProcessDue runs all jobs that are due now and returns how many ran.
func (o *Outbox) ProcessDue(ctx context.Context) (int, error) {
	n := 0
	for ctx.Err() == nil {
		job, err := o.store.ClaimDueJob(ctx, o.Now())
		if err != nil {
			return n, err
		}
		if job == nil {
			return n, nil
		}
		n++
		o.finish(ctx, job, o.exec.ExecuteJob(ctx, job))
	}
	return n, ctx.Err()
}

func (o *Outbox) finish(ctx context.Context, job *storage.Job, err error) {
	// Persist the outcome even if shutdown was requested meanwhile.
	ctx = context.WithoutCancel(ctx)
	log := o.log.With("job", job.ID, "type", job.Type, "poll", job.PollID, "participant", job.ParticipantID, "attempt", job.Attempts)
	var serr error
	switch {
	case err == nil:
		serr = o.store.CompleteJob(ctx, job.ID)
	case errors.Is(err, ErrPermanent) || slack.IsPermanent(err):
		log.Error("job failed permanently", "err", err)
		serr = o.store.FailJob(ctx, job.ID, err.Error())
	case job.Attempts >= o.MaxAttempts:
		log.Error("job failed, giving up", "err", err)
		serr = o.store.FailJob(ctx, job.ID, err.Error())
	default:
		delay := o.backoff(job.Attempts)
		if ra, ok := slack.RetryAfter(err); ok {
			delay = ra + time.Second
		}
		log.Warn("job failed, retrying", "in", delay, "err", err)
		serr = o.store.RetryJob(ctx, job.ID, o.Now().Add(delay), err.Error())
	}
	if serr != nil {
		log.Error("store job result", "err", serr)
	}
}

func (o *Outbox) backoff(attempt int) time.Duration {
	d := o.BaseDelay << min(attempt-1, 16)
	return min(d, o.MaxDelay)
}
