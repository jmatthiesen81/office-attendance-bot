package poll

import (
	"context"
	"errors"
	"fmt"
	"time"

	"officebot/internal/domain"
	"officebot/internal/storage"
)

// AttendanceChange changes a response on behalf of a participant.
type AttendanceChange int

const (
	AttendanceSet    AttendanceChange = iota // replace the selection
	AttendanceAdd                            // select these days in addition
	AttendanceRemove                         // deselect these days
)

// SetAttendance changes a participant's response as an admin (CLI). Unlike
// HandleSave it does not check the deadline, so past days and closed polls
// can be corrected too. The change goes through the normal save path: the
// summary is marked outdated and a change notice is scheduled like for a
// save in Slack, unless notice is false. Nothing is sent from here; the
// running bot updates the DM, the summary and posts the notice.
func (s *Service) SetAttendance(ctx context.Context, pollID int64, userID string, op AttendanceChange, days domain.Days, notice bool) (before *domain.Response, after *domain.Response, changed bool, err error) {
	part, err := s.store.GetParticipant(ctx, pollID, userID)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, nil, false, fmt.Errorf("%s is not a participant of this poll", userID)
	}
	if err != nil {
		return nil, nil, false, err
	}
	before, err = s.store.GetResponse(ctx, pollID, userID)
	if err != nil {
		return nil, nil, false, err
	}
	var next domain.Days
	switch op {
	case AttendanceSet:
		next = days
	case AttendanceAdd, AttendanceRemove:
		if before == nil {
			if op == AttendanceRemove {
				return nil, nil, false, fmt.Errorf("%s has not answered yet; nothing to remove", userID)
			}
		} else {
			next = before.Days
		}
		for i, d := range days {
			if d {
				next[i] = op == AttendanceAdd
			}
		}
	default:
		return nil, nil, false, fmt.Errorf("unknown attendance change %d", op)
	}

	now := s.Now()
	var noticeAt time.Time // zero: no change notice
	if notice && s.cfg.SummaryChangeNotices {
		noticeAt = now.Add(s.cfg.SummaryChangeNoticeDelay)
	}
	after, changed, err = s.store.SaveResponseWithNotice(ctx, pollID, userID, next, now, noticeAt, s.cfg.SummaryChangeNoticeGrace)
	if err != nil || !changed {
		return before, after, changed, err
	}
	if part.DMMessageTS != "" {
		if err := s.store.EnqueueJob(ctx, storage.JobUpdateDM, pollID, part.ID, "", now); err != nil {
			return before, after, changed, err
		}
	}
	s.summary.Trigger()
	s.outbox.Notify()
	return before, after, changed, nil
}
