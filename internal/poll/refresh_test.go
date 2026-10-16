package poll

import (
	"testing"
	"time"

	"officebot/internal/storage"
)

func TestRefreshOnNewVersion(t *testing.T) {
	e := newEnv(t, "U1", "U2")
	p := e.start()
	updates := func() int { return len(e.api.UpdatesTo("DU1")) + len(e.api.UpdatesTo("DU2")) }
	summaryVersion := func() int64 {
		got, err := e.store.GetPoll(e.ctx, p.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got.SummaryVersion
	}
	refresh := func(version string) {
		t.Helper()
		if err := e.svc.RefreshOnNewVersion(e.ctx, version); err != nil {
			t.Fatal(err)
		}
		e.drain()
	}

	before, sv := updates(), summaryVersion()
	refresh("1.0.0")
	if got := updates() - before; got != 2 {
		t.Errorf("first start: %d DM updates, want 2", got)
	}
	if summaryVersion() <= sv {
		t.Error("summary not marked for re-sync")
	}
	if v, _ := e.store.GetState(e.ctx, storage.StateRenderedVersion); v != "1.0.0" {
		t.Errorf("stored version = %q", v)
	}

	// A plain restart of the same version leaves the messages alone.
	before, sv = updates(), summaryVersion()
	refresh("1.0.0")
	if updates() != before || summaryVersion() != sv {
		t.Error("same version re-rendered the messages")
	}

	// After the deadline the latest poll is closed, but still refreshed.
	e.setNow(p.Deadline.Add(time.Hour))
	if err := e.svc.CloseDue(e.ctx, e.clock()); err != nil {
		t.Fatal(err)
	}
	e.drain()
	before = updates()
	refresh("1.0.1")
	if got := updates() - before; got != 2 {
		t.Errorf("closed poll after update: %d DM updates, want 2", got)
	}
	if got, _ := e.store.GetPoll(e.ctx, p.ID); got.IsAcceptingResponses(e.clock()) {
		t.Error("refresh reopened the poll")
	}
}

func TestRefreshMessagesWithoutPolls(t *testing.T) {
	e := newEnv(t, "U1")
	if n, err := e.svc.RefreshMessages(e.ctx); err != nil || n != 0 {
		t.Fatalf("n = %d, err = %v", n, err)
	}
	if err := e.svc.RefreshOnNewVersion(e.ctx, "dev"); err != nil {
		t.Fatal(err)
	}
	if v, _ := e.store.GetState(e.ctx, storage.StateRenderedVersion); v != "dev" {
		t.Errorf("stored version = %q", v)
	}
}
