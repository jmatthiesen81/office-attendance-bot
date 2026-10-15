package poll

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"officebot/internal/domain"
	"officebot/internal/i18n"
	"officebot/internal/slack"
	"officebot/internal/storage"
)

// errSummaryMissing delays a change notice until the summary it replies to exists.
var errSummaryMissing = errors.New("summary not posted yet")

// postChangeNotice announces a changed response as a thread reply under the
// channel summary. All changes since the notice was scheduled are combined:
// the reply shows the difference between the selection before the first of
// them and the current one, and nothing at all if they cancel out.
//
// The reply is posted before the notice is finished, so a crash in between
// may repeat it once; losing a notice would be worse.
func (s *Service) postChangeNotice(ctx context.Context, job *storage.Job) error {
	p, part, resp, err := s.load(ctx, job.ParticipantID)
	if errors.Is(err, storage.ErrNotFound) {
		return nil // poll deleted meanwhile
	}
	if err != nil {
		return err
	}
	baseline, found, err := s.store.GetChangeNotice(ctx, p.ID, part.SlackUserID)
	if err != nil || !found {
		return err
	}
	now := s.Now()
	due := now.Add(s.cfg.SummaryChangeNoticeDelay)
	text := ""
	if resp != nil {
		text = changeNoticeText(s.tr(), displayName(part), baseline, resp.Days)
	}
	if text == "" {
		s.log.Info("changes cancelled out, no notice", "user", part.SlackUserID, "poll", p.ID)
		return s.store.FinishChangeNotice(ctx, p.ID, part.SlackUserID, resp, due, now)
	}
	if p.SummaryMessageTS == "" {
		return errSummaryMissing
	}
	if _, err := s.api.PostMessage(ctx, p.SummaryChannelID, slack.Message{Text: text, ThreadTS: p.SummaryMessageTS}); err != nil {
		return fmt.Errorf("post change notice: %w", err)
	}
	s.log.Info("change notice posted", "user", part.SlackUserID, "poll", p.ID, "revision", resp.Revision)
	return s.store.FinishChangeNotice(ctx, p.ID, part.SlackUserID, resp, due, now)
}

// changeNoticeText describes the change from before to after, or returns ""
// if the selection is the same.
func changeNoticeText(tr *i18n.Translator, name string, before, after domain.Days) string {
	var added, removed domain.Days
	for i := range before {
		added[i] = after[i] && !before[i]
		removed[i] = before[i] && !after[i]
	}
	days := func(d domain.Days) string { return tr.List(d.Names(tr.Weekday)) }
	switch {
	case added.Count() == 0 && removed.Count() == 0:
		return ""
	case after.Count() == 0:
		return tr.T("change.no_office", "name", name)
	case before.Count() == 0:
		return tr.T("change.back", "name", name, "added", days(added))
	case removed.Count() == 0:
		return tr.T("change.added", "name", name, "added", days(added))
	case added.Count() == 0:
		return tr.T("change.removed", "name", name, "removed", days(removed))
	default:
		return tr.T("change.swapped", "name", name, "removed", days(removed), "added", days(added))
	}
}

// displayName returns the name shown in a change notice. A plain name
// instead of a mention keeps the reply from notifying the person.
func displayName(part *domain.Participant) string {
	if part.DisplayName == "" {
		return "<@" + part.SlackUserID + ">"
	}
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "*", "").Replace(part.DisplayName)
}
