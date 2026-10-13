package storage

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"officebot/internal/domain"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

var t0 = time.Date(2026, 10, 19, 7, 0, 0, 0, time.UTC)

func newPoll(week int) *domain.Poll {
	start := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC).AddDate(0, 0, 7*(week-43))
	return &domain.Poll{WorkspaceID: "T1", ISOYear: 2026, ISOWeek: week, WeekStart: start,
		WeekEnd: start.AddDate(0, 0, 4), Deadline: start.AddDate(0, 0, 4).Add(6 * time.Hour), SummaryChannelID: "C1"}
}

func users(ids ...string) []domain.Participant {
	var out []domain.Participant
	for _, id := range ids {
		out = append(out, domain.Participant{SlackUserID: id, DisplayName: "name-" + id})
	}
	return out
}

func TestMigrationsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.db")
	for range 2 {
		s, err := Open(context.Background(), path)
		if err != nil {
			t.Fatal(err)
		}
		s.Close()
	}
}

func TestCreatePollIdempotent(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	p := newPoll(43)
	created, err := s.CreatePoll(ctx, p, users("U1", "U2", "U3"), t0)
	if err != nil || !created {
		t.Fatalf("first create: %v %v", created, err)
	}
	again := newPoll(43)
	created, err = s.CreatePoll(ctx, again, users("U1", "U2", "U3", "U4"), t0)
	if err != nil || created {
		t.Fatalf("second create must be a no-op: %v %v", created, err)
	}
	parts, _ := s.ListParticipants(ctx, p.ID)
	if len(parts) != 3 {
		t.Errorf("participants = %d, want 3 (snapshot must not change)", len(parts))
	}
	stats, _ := s.JobStats(ctx)
	if stats["pending"] != 3 {
		t.Errorf("jobs = %v, want 3 pending", stats)
	}
}

func TestNewPollClosesOlderOnes(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	old := newPoll(42)
	if _, err := s.CreatePoll(ctx, old, users("U1"), t0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreatePoll(ctx, newPoll(43), users("U1"), t0); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetPoll(ctx, old.ID)
	if got.Status != domain.PollClosed {
		t.Errorf("old poll status = %s", got.Status)
	}
}

func TestSaveResponse(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	p := newPoll(43)
	s.CreatePoll(ctx, p, users("U1", "U2"), t0)

	// multi-selection
	r, changed, err := s.SaveResponse(ctx, p.ID, "U1", domain.Days{true, false, true, false, true}, t0)
	if err != nil || !changed || r.Revision != 1 || r.Days.Count() != 3 {
		t.Fatalf("first save: %+v %v %v", r, changed, err)
	}
	// identical save (duplicate interaction) is a no-op
	r, changed, _ = s.SaveResponse(ctx, p.ID, "U1", domain.Days{true, false, true, false, true}, t0.Add(time.Minute))
	if changed || r.Revision != 1 {
		t.Errorf("duplicate save changed state: %+v", r)
	}
	// re-answer
	r, changed, _ = s.SaveResponse(ctx, p.ID, "U1", domain.Days{false, true}, t0.Add(2*time.Minute))
	if !changed || r.Revision != 2 || r.Days != (domain.Days{false, true}) || !r.SubmittedAt.Equal(t0) {
		t.Errorf("re-answer: %+v", r)
	}
	// empty selection is a valid answer
	r, changed, _ = s.SaveResponse(ctx, p.ID, "U2", domain.Days{}, t0)
	if !changed || r == nil || r.Days.Count() != 0 {
		t.Errorf("empty selection: %+v", r)
	}
	snap, _ := s.PollSnapshot(ctx, p.ID)
	if len(snap.Responses) != 2 {
		t.Errorf("responses = %d", len(snap.Responses))
	}
	// initial version 1 + three changing saves
	if snap.Poll.SummaryVersion != 4 {
		t.Errorf("summary version = %d, want 4", snap.Poll.SummaryVersion)
	}
	// non-participants cannot have responses (FK)
	if _, _, err := s.SaveResponse(ctx, p.ID, "UX", domain.Days{true}, t0); err == nil {
		t.Error("response of non-participant accepted")
	}
}

func TestConcurrentSaves(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	p := newPoll(43)
	var ids []string
	for i := range 10 {
		ids = append(ids, fmt.Sprintf("U%d", i))
	}
	s.CreatePoll(ctx, p, users(ids...), t0)

	var wg sync.WaitGroup
	for i := range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			user := ids[i%10]
			days := domain.Days{}
			days[i%5] = true
			if _, _, err := s.SaveResponse(ctx, p.ID, user, days, t0.Add(time.Duration(i)*time.Second)); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	snap, _ := s.PollSnapshot(ctx, p.ID)
	if len(snap.Responses) != 10 {
		t.Fatalf("responses = %d", len(snap.Responses))
	}
	total := 0
	for _, r := range snap.Responses {
		if r.Days.Count() != 1 {
			t.Errorf("corrupt response %+v", r)
		}
		total += r.Revision
	}
	if want := int64(1 + total); snap.Poll.SummaryVersion != want {
		t.Errorf("summary version %d, want %d (one bump per changing save)", snap.Poll.SummaryVersion, want)
	}
}

func TestOutboxLifecycle(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	p := newPoll(43)
	s.CreatePoll(ctx, p, users("U1"), t0)

	j, err := s.ClaimDueJob(ctx, t0)
	if err != nil || j == nil || j.Type != JobSendDM || j.Attempts != 1 {
		t.Fatalf("claim: %+v %v", j, err)
	}
	if again, _ := s.ClaimDueJob(ctx, t0); again != nil {
		t.Fatal("running job claimed twice")
	}
	s.RetryJob(ctx, j.ID, t0.Add(time.Minute), "boom")
	if early, _ := s.ClaimDueJob(ctx, t0); early != nil {
		t.Fatal("job claimed before next_attempt_at")
	}
	next, ok, _ := s.NextJobDue(ctx)
	if !ok || !next.Equal(t0.Add(time.Minute)) {
		t.Errorf("next due = %v %v", next, ok)
	}
	j, _ = s.ClaimDueJob(ctx, t0.Add(time.Minute))
	if j == nil || j.Attempts != 2 {
		t.Fatalf("second claim: %+v", j)
	}
	// crash: running job is reset on start
	if n, _ := s.ResetRunningJobs(ctx); n != 1 {
		t.Errorf("reset = %d", n)
	}
	j, _ = s.ClaimDueJob(ctx, t0.Add(time.Minute))
	s.CompleteJob(ctx, j.ID)
	// send_dm is not repeatable
	s.EnqueueJob(ctx, JobSendDM, p.ID, j.ParticipantID, "", t0)
	if j2, _ := s.ClaimDueJob(ctx, t0.Add(time.Hour)); j2 != nil {
		t.Error("completed send_dm re-enqueued")
	}
	// update_dm is repeatable, and re-enqueue while running keeps it pending
	s.EnqueueJob(ctx, JobUpdateDM, p.ID, j.ParticipantID, "", t0)
	u, _ := s.ClaimDueJob(ctx, t0)
	s.EnqueueJob(ctx, JobUpdateDM, p.ID, j.ParticipantID, "", t0)
	s.CompleteJob(ctx, u.ID)
	if u2, _ := s.ClaimDueJob(ctx, t0); u2 == nil || u2.ID != u.ID {
		t.Error("update requested during run was lost")
	}
}

func TestCloseDuePolls(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	p := newPoll(43)
	s.CreatePoll(ctx, p, users("U1", "U2"), t0)
	parts, _ := s.ListParticipants(ctx, p.ID)
	s.MarkDelivered(ctx, parts[0].ID, "DU1", "1.1", t0)

	if ids, _ := s.CloseDuePolls(ctx, p.Deadline.Add(-time.Second)); len(ids) != 0 {
		t.Fatal("closed before deadline")
	}
	ids, err := s.CloseDuePolls(ctx, p.Deadline)
	if err != nil || len(ids) != 1 {
		t.Fatalf("close: %v %v", ids, err)
	}
	stats, _ := s.JobStats(ctx)
	// pending send_dm jobs are cancelled, U1 (delivered) gets one update_dm
	if stats["pending"] != 1 || stats["done"] != 2 {
		t.Errorf("jobs after close: %v", stats)
	}
}

func TestReminderRounds(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	p := newPoll(43)
	s.CreatePoll(ctx, p, users("U1", "U2", "U3"), t0)
	parts, _ := s.ListParticipants(ctx, p.ID)
	for _, part := range parts[:2] {
		s.MarkDelivered(ctx, part.ID, "D"+part.SlackUserID, "1.1", t0)
	}
	s.SaveResponse(ctx, p.ID, "U1", domain.Days{}, t0) // answered "no office day"

	r1, r2 := t0.Add(24*time.Hour), t0.Add(48*time.Hour)
	// two missed rounds collapse into one reminder for U2 only (U3 has no DM)
	n, err := s.CreateReminderRound(ctx, p.ID, []time.Time{r1, r2}, 2, t0)
	if err != nil || n != 1 {
		t.Fatalf("enqueued = %d, %v", n, err)
	}
	if n, _ := s.CreateReminderRound(ctx, p.ID, []time.Time{r1, r2}, 2, t0); n != 0 {
		t.Errorf("round repeated: %d", n)
	}
	s.IncrementReminderCount(ctx, parts[1].ID)
	s.IncrementReminderCount(ctx, parts[1].ID)
	if n, _ := s.CreateReminderRound(ctx, p.ID, []time.Time{r1, r2, t0.Add(72 * time.Hour)}, 2, t0); n != 0 {
		t.Errorf("max reminder count ignored: %d", n)
	}
}

func TestRetention(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	p := newPoll(43)
	s.CreatePoll(ctx, p, users("U1"), t0)
	s.SaveResponse(ctx, p.ID, "U1", domain.Days{true}, t0)
	if n, _ := s.DeletePollsBefore(ctx, p.WeekEnd); n != 0 {
		t.Fatal("deleted too early")
	}
	if n, _ := s.DeletePollsBefore(ctx, p.WeekEnd.Add(time.Second)); n != 1 {
		t.Fatal("not deleted")
	}
	if r, _ := s.GetResponse(ctx, p.ID, "U1"); r != nil {
		t.Error("responses not cascaded")
	}
}

func TestBackup(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	p := newPoll(43)
	s.CreatePoll(ctx, p, users("U1"), t0)
	target := filepath.Join(t.TempDir(), "backup.db")
	for range 2 { // the second run overwrites the first backup
		if err := s.Backup(ctx, target); err != nil {
			t.Fatal(err)
		}
	}
	b, err := Open(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if _, err := b.GetPoll(ctx, p.ID); err != nil {
		t.Errorf("backup incomplete: %v", err)
	}
}
