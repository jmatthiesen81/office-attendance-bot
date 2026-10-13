package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	goslack "github.com/slack-go/slack"

	"officebot/internal/domain"
	"officebot/internal/storage"
)

type execFunc func(*storage.Job) error

func (f execFunc) ExecuteJob(_ context.Context, j *storage.Job) error { return f(j) }

func setup(t *testing.T, fn execFunc) (*storage.Store, *Outbox, *time.Time) {
	t.Helper()
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	now := time.Date(2026, 10, 19, 7, 0, 0, 0, time.UTC)
	p := &domain.Poll{WorkspaceID: "T1", ISOYear: 2026, ISOWeek: 43, WeekStart: now, WeekEnd: now.AddDate(0, 0, 4),
		Deadline: now.AddDate(0, 0, 4), SummaryChannelID: "C"}
	if _, err := store.CreatePoll(ctx, p, []domain.Participant{{SlackUserID: "U1"}}, now); err != nil {
		t.Fatal(err)
	}
	o := NewOutbox(store, fn, slog.New(slog.NewTextHandler(io.Discard, nil)))
	o.Now = func() time.Time { return now }
	return store, o, &now
}

func TestBackoffAndSuccess(t *testing.T) {
	calls := 0
	store, o, now := setup(t, func(*storage.Job) error {
		calls++
		if calls < 3 {
			return errors.New("temporary")
		}
		return nil
	})
	ctx := context.Background()
	o.ProcessDue(ctx)
	*now = now.Add(4 * time.Second)
	o.ProcessDue(ctx)
	if calls != 1 {
		t.Fatalf("retried before backoff: %d", calls)
	}
	*now = now.Add(2 * time.Second) // 5 s after first failure
	o.ProcessDue(ctx)
	*now = now.Add(10 * time.Second) // second backoff is 10 s
	o.ProcessDue(ctx)
	stats, _ := store.JobStats(ctx)
	if calls != 3 || stats["done"] != 1 {
		t.Errorf("calls=%d stats=%v", calls, stats)
	}
}

func TestRetryAfter(t *testing.T) {
	calls := 0
	_, o, now := setup(t, func(*storage.Job) error {
		calls++
		if calls == 1 {
			return &goslack.RateLimitedError{RetryAfter: 30 * time.Second}
		}
		return nil
	})
	ctx := context.Background()
	o.ProcessDue(ctx)
	*now = now.Add(20 * time.Second)
	o.ProcessDue(ctx)
	if calls != 1 {
		t.Fatal("Retry-After not honoured")
	}
	*now = now.Add(15 * time.Second)
	o.ProcessDue(ctx)
	if calls != 2 {
		t.Fatal("not retried after Retry-After")
	}
}

func TestPermanentAndMaxAttempts(t *testing.T) {
	store, o, _ := setup(t, func(*storage.Job) error { return goslack.SlackErrorResponse{Err: "missing_scope"} })
	ctx := context.Background()
	o.ProcessDue(ctx)
	if stats, _ := store.JobStats(ctx); stats["failed"] != 1 {
		t.Errorf("permanent error not failed: %v", stats)
	}

	calls := 0
	store, o, now := setup(t, func(*storage.Job) error { calls++; return errors.New("down") })
	o.MaxAttempts = 3
	for range 10 {
		o.ProcessDue(ctx)
		*now = now.Add(time.Hour)
	}
	if stats, _ := store.JobStats(ctx); calls != 3 || stats["failed"] != 1 {
		t.Errorf("calls=%d stats=%v", calls, stats)
	}
}
