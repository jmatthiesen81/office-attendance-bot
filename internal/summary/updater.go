package summary

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"officebot/internal/domain"
	"officebot/internal/slack"
	"officebot/internal/storage"
)

// Updater is the single writer of channel summaries. Change notifications
// are debounced; every sync renders the current database state, and the
// version counters ensure that nothing older than the persisted state ever
// remains in Slack: whenever summary_version > summary_synced_version the
// poll is synced again (also periodically and after restarts).
type Updater struct {
	store    *storage.Store
	api      slack.API
	renderer Renderer
	log      *slog.Logger

	Debounce  time.Duration
	Reconcile time.Duration
	Now       func() time.Time

	trigger chan struct{}
	backoff map[int64]retryState
}

type retryState struct {
	failures  int
	notBefore time.Time
}

// NewUpdater creates the summary updater.
func NewUpdater(store *storage.Store, api slack.API, renderer Renderer, log *slog.Logger) *Updater {
	return &Updater{
		store: store, api: api, renderer: renderer, log: log,
		Debounce: 1500 * time.Millisecond, Reconcile: 30 * time.Second, Now: time.Now,
		trigger: make(chan struct{}, 1),
		backoff: map[int64]retryState{},
	}
}

// Trigger requests a sync soon. It never blocks.
func (u *Updater) Trigger() {
	select {
	case u.trigger <- struct{}{}:
	default:
	}
}

// Run syncs until ctx is cancelled.
func (u *Updater) Run(ctx context.Context) {
	u.SyncAll(ctx)
	ticker := time.NewTicker(u.Reconcile)
	defer ticker.Stop()
	var debounce <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-u.trigger:
			if debounce == nil {
				debounce = time.After(u.Debounce)
			}
		case <-debounce:
			debounce = nil
			u.SyncAll(ctx)
		case <-ticker.C:
			u.SyncAll(ctx)
		}
	}
}

// SyncAll syncs every poll whose summary is missing or outdated.
func (u *Updater) SyncAll(ctx context.Context) {
	polls, err := u.store.PollsNeedingSummarySync(ctx)
	if err != nil {
		u.log.Error("load polls for summary sync", "err", err)
		return
	}
	now := u.Now()
	for _, p := range polls {
		if b, ok := u.backoff[p.ID]; ok && now.Before(b.notBefore) {
			continue
		}
		if err := u.Sync(ctx, p.ID); err != nil {
			b := u.backoff[p.ID]
			b.failures++
			delay := backoffDelay(b.failures)
			if ra, ok := slack.RetryAfter(err); ok && ra > delay {
				delay = ra
			}
			b.notBefore = now.Add(delay)
			u.backoff[p.ID] = b
			level := slog.LevelWarn
			if slack.IsPermanent(err) {
				level = slog.LevelError
			}
			u.log.Log(ctx, level, "summary sync failed, will retry", "poll", p.ID, "retry_in", delay, "err", err)
			continue
		}
		delete(u.backoff, p.ID)
	}
}

func backoffDelay(failures int) time.Duration {
	d := 5 * time.Second << min(failures-1, 9)
	return min(d, 30*time.Minute)
}

func metadataKey(p *domain.Poll) string {
	return fmt.Sprintf("%s:%d-W%02d#%d", p.WorkspaceID, p.ISOYear, p.ISOWeek, p.ID)
}

// Sync writes the current state of one poll to its summary message.
func (u *Updater) Sync(ctx context.Context, pollID int64) error {
	snap, err := u.store.PollSnapshot(ctx, pollID)
	if err != nil {
		return err
	}
	p := &snap.Poll
	version := p.SummaryVersion
	meta := slack.NewMetadata(slack.MetaSummary, metadataKey(p))

	ts := p.SummaryMessageTS
	if ts == "" {
		// A crash between posting and storing the ts would otherwise lead to
		// a second summary; look for our own message first.
		found := false
		if ts, found, err = u.api.FindMessageByMetadata(ctx, p.SummaryChannelID, slack.MetaSummary, metadataKey(p)); err != nil {
			u.log.Debug("summary lookup by metadata failed", "poll", p.ID, "err", err)
		}
		if !found {
			msg := u.renderer.Placeholder(p)
			msg.Metadata = meta
			if ts, err = u.api.PostMessage(ctx, p.SummaryChannelID, msg); err != nil {
				return fmt.Errorf("post summary: %w", err)
			}
			u.log.Info("summary posted", "poll", p.ID, "channel", p.SummaryChannelID, "ts", ts)
		}
		if err := u.store.SetSummaryMessage(ctx, p.ID, ts); err != nil {
			return err
		}
	}

	msg := u.renderer.Render(snap, u.Now())
	msg.Metadata = meta
	if err := u.api.UpdateMessage(ctx, p.SummaryChannelID, ts, msg); err != nil {
		if slack.ErrorCode(err) == "message_not_found" {
			// Someone deleted the summary: forget it so the next sync re-posts it.
			u.log.Warn("summary message was deleted, re-posting", "poll", p.ID)
			if err := u.store.SetSummaryMessage(ctx, p.ID, ""); err != nil {
				return err
			}
			return u.Sync(ctx, pollID)
		}
		return fmt.Errorf("update summary: %w", err)
	}
	return u.store.MarkSummarySynced(ctx, p.ID, version)
}
