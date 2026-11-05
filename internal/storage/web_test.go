package storage

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestWebUsersLastAdmin(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)

	if err := s.AddWebUser(ctx, "T1", WebUser{SlackUserID: "U1", Role: RoleAdmin, DisplayName: "Anna"}, "cli", now); err != nil {
		t.Fatal(err)
	}
	if err := s.AddWebUser(ctx, "T1", WebUser{SlackUserID: "U1", Role: RoleViewer}, "cli", now); !errors.Is(err, ErrExists) {
		t.Errorf("duplicate: got %v", err)
	}
	if _, err := s.SetWebUserRole(ctx, "T1", "U1", RoleViewer, "cli", now); !errors.Is(err, ErrLastAdmin) {
		t.Errorf("downgrading the last admin: got %v", err)
	}
	if err := s.RemoveWebUser(ctx, "T1", "U1", "cli", now); !errors.Is(err, ErrLastAdmin) {
		t.Errorf("removing the last admin: got %v", err)
	}

	if err := s.AddWebUser(ctx, "T1", WebUser{SlackUserID: "U2", Role: RoleAdmin}, "U1", now); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateWebSession(ctx, "T1", WebSession{IDHash: "h1", SlackUserID: "U1", CSRFToken: "c",
		CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if changed, err := s.SetWebUserRole(ctx, "T1", "U1", RoleViewer, "U2", now); err != nil || !changed {
		t.Fatalf("downgrade with a second admin: changed=%v err=%v", changed, err)
	}
	if err := s.RemoveWebUser(ctx, "T1", "U1", "U2", now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetWebSession(ctx, "T1", "h1", time.Hour, now); !errors.Is(err, ErrNotFound) {
		t.Errorf("session survived removal: %v", err)
	}
	users, err := s.ListWebUsers(ctx, "T1")
	if err != nil || len(users) != 1 || users[0].SlackUserID != "U2" {
		t.Errorf("users: %+v %v", users, err)
	}
	entries, err := s.ListAudit(ctx, "T1", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 4 {
		t.Errorf("audit entries: %d, want 4: %+v", len(entries), entries)
	}
}

func TestWebSessions(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	sess := WebSession{IDHash: "h", SlackUserID: "U1", CSRFToken: "c", CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(12 * time.Hour)}
	if err := s.CreateWebSession(ctx, "T1", sess); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetWebSession(ctx, "T2", "h", 2*time.Hour, now); !errors.Is(err, ErrNotFound) {
		t.Errorf("session of another workspace: %v", err)
	}
	// Activity within the idle time keeps the session alive.
	for _, d := range []time.Duration{time.Hour, 2*time.Hour + 59*time.Minute} {
		if _, err := s.GetWebSession(ctx, "T1", "h", 2*time.Hour, now.Add(d)); err != nil {
			t.Fatalf("after %s: %v", d, err)
		}
	}
	if _, err := s.GetWebSession(ctx, "T1", "h", 2*time.Hour, now.Add(5*time.Hour)); !errors.Is(err, ErrNotFound) {
		t.Errorf("idle session still valid: %v", err)
	}

	if err := s.CreateLoginState(ctx, "st", "n", "/stats", now.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if nonce, ret, err := s.ConsumeLoginState(ctx, "st", now); err != nil || nonce != "n" || ret != "/stats" {
		t.Errorf("consume: %q %q %v", nonce, ret, err)
	}
	if _, _, err := s.ConsumeLoginState(ctx, "st", now); !errors.Is(err, ErrNotFound) {
		t.Errorf("state used twice: %v", err)
	}
	if err := s.CreateLoginState(ctx, "old", "n", "", now.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ConsumeLoginState(ctx, "old", now.Add(11*time.Minute)); !errors.Is(err, ErrNotFound) {
		t.Errorf("expired state accepted: %v", err)
	}
}

func TestRoleAtLeast(t *testing.T) {
	cases := []struct {
		have, need string
		want       bool
	}{
		{RoleAdmin, RoleAdmin, true}, {RoleAdmin, RoleAnalyst, true}, {RoleAdmin, RoleViewer, true},
		{RoleAnalyst, RoleAdmin, false}, {RoleAnalyst, RoleAnalyst, true}, {RoleAnalyst, RoleViewer, true},
		{RoleViewer, RoleAnalyst, false}, {RoleViewer, RoleViewer, true},
		{"", RoleViewer, false}, {"root", RoleViewer, false},
	}
	for _, c := range cases {
		if got := RoleAtLeast(c.have, c.need); got != c.want {
			t.Errorf("RoleAtLeast(%q, %q) = %v", c.have, c.need, got)
		}
	}
}

func TestAnalystRoleStored(t *testing.T) {
	s, ctx := openTest(t), context.Background()
	now := time.Now()
	for _, u := range []WebUser{{SlackUserID: "U3", Role: RoleViewer}, {SlackUserID: "U2", Role: RoleAnalyst}, {SlackUserID: "U1", Role: RoleAdmin}} {
		if err := s.AddWebUser(ctx, "T1", u, "cli", now); err != nil {
			t.Fatal(err)
		}
	}
	users, err := s.ListWebUsers(ctx, "T1")
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, u := range users {
		order = append(order, u.Role)
	}
	if strings.Join(order, ",") != "admin,analyst,viewer" {
		t.Errorf("order: %v", order)
	}
	if _, err := s.SetWebUserRole(ctx, "T1", "U3", RoleAnalyst, "U1", now); err != nil {
		t.Error(err)
	}
	if _, err := s.SetWebUserRole(ctx, "T1", "U3", "boss", "U1", now); err == nil {
		t.Error("unknown role accepted")
	}
}
