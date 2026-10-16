package poll

import (
	"context"
	"errors"
	"slices"

	"officebot/internal/domain"
	"officebot/internal/storage"
)

// visiblePolls returns the polls of this workspace whose messages people
// still work with: all running polls and, after the deadline, the latest
// poll, whose closed DM and summary stay in view until the next poll.
func (s *Service) visiblePolls(ctx context.Context) ([]domain.Poll, error) {
	polls, err := s.store.OpenPolls(ctx)
	if err != nil {
		return nil, err
	}
	week, _ := s.CurrentTargetWeek(s.Now())
	latest, err := s.store.GetPollByWeek(ctx, s.workspaceID, week.Year, week.Week)
	switch {
	case err == nil:
		if !slices.ContainsFunc(polls, func(p domain.Poll) bool { return p.ID == latest.ID }) {
			polls = append(polls, *latest)
		}
	case !errors.Is(err, storage.ErrNotFound):
		return nil, err
	}
	return slices.DeleteFunc(polls, func(p domain.Poll) bool { return p.WorkspaceID != s.workspaceID }), nil
}

// RefreshMessages re-renders the delivered DMs and the channel summary of
// the given polls, or of the visible polls if none are given, e.g. after an
// update changed texts or layout. Clicks stay subject to the usual checks:
// a closed poll stays closed. It only writes to the database, so the CLI can
// use it as well; the running bot does the sending. It returns the number of
// polls refreshed.
func (s *Service) RefreshMessages(ctx context.Context, polls ...domain.Poll) (int, error) {
	if len(polls) == 0 {
		var err error
		if polls, err = s.visiblePolls(ctx); err != nil {
			return 0, err
		}
	}
	now := s.Now()
	for _, p := range polls {
		if err := s.store.RequestDMRefresh(ctx, p.ID, now); err != nil {
			return 0, err
		}
		if _, err := s.store.RequestSummaryResync(ctx, p.ID); err != nil {
			return 0, err
		}
	}
	if len(polls) > 0 {
		s.outbox.Notify()
		s.summary.Trigger()
	}
	return len(polls), nil
}

// RefreshOnNewVersion calls RefreshMessages once per bot version, so that
// an update becomes visible at once and not only with the next poll. Plain
// restarts of the same version leave the messages alone.
func (s *Service) RefreshOnNewVersion(ctx context.Context, version string) error {
	last, err := s.store.GetState(ctx, storage.StateRenderedVersion)
	if err != nil {
		return err
	}
	if last == version {
		return nil
	}
	n, err := s.RefreshMessages(ctx)
	if err != nil {
		return err
	}
	if err := s.store.SetState(ctx, storage.StateRenderedVersion, version, s.Now()); err != nil {
		return err
	}
	s.log.Info("new version, messages re-rendered", "version", version, "previous", last, "polls", n)
	return nil
}
