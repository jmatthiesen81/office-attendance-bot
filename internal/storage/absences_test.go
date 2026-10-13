package storage

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"officebot/internal/domain"
)

func d(day int) domain.Date { return domain.NewDate(2026, 10, day) }

func ranges(abs []domain.Absence) string {
	out := ""
	for _, a := range abs {
		out += fmt.Sprintf("%s:%d-%d ", a.SlackUserID, a.Start.In(time.UTC).Day(), a.End.In(time.UTC).Day())
	}
	return out
}

func TestAddAbsenceMerges(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	mustAdd := func(user string, from, to int) domain.Absence {
		t.Helper()
		a, err := s.AddAbsence(ctx, "T1", user, d(from), d(to), user, t0)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	mustAdd("U1", 5, 7)
	mustAdd("U1", 12, 14)
	mustAdd("U2", 8, 11) // other user: never merged
	if a := mustAdd("U1", 8, 11); a.Start != d(5) || a.End != d(14) {
		t.Errorf("adjacent ranges not merged: %v", a)
	}
	mustAdd("U1", 20, 22)
	if a := mustAdd("U1", 21, 25); a.Start != d(20) || a.End != d(25) {
		t.Errorf("overlapping ranges not merged: %v", a)
	}
	all, err := s.ListAbsences(ctx, "T1", "", d(1))
	if err != nil {
		t.Fatal(err)
	}
	if got := ranges(all); got != "U1:5-14 U1:20-25 U2:8-11 " {
		t.Errorf("absences = %s", got)
	}
	// ListAbsences only returns entries ending on or after the given day.
	if own, _ := s.ListAbsences(ctx, "T1", "U1", d(15)); ranges(own) != "U1:20-25 " {
		t.Errorf("upcoming = %s", ranges(own))
	}
	// Other workspaces are separate.
	if other, _ := s.ListAbsences(ctx, "T2", "", d(1)); len(other) != 0 {
		t.Errorf("foreign workspace sees %s", ranges(other))
	}
}

func TestRemoveAbsenceDays(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	s.AddAbsence(ctx, "T1", "U1", d(5), d(25), "U1", t0)
	s.AddAbsence(ctx, "T1", "U2", d(5), d(25), "U2", t0)

	// Removing a week from the middle splits the absence.
	changed, err := s.RemoveAbsenceDays(ctx, "T1", "U1", d(12), d(16), t0)
	if err != nil || !changed {
		t.Fatal(changed, err)
	}
	// Removing the start shortens it.
	s.RemoveAbsenceDays(ctx, "T1", "U1", d(1), d(6), t0)
	got, _ := s.ListAbsences(ctx, "T1", "", d(1))
	if ranges(got) != "U1:7-11 U1:17-25 U2:5-25 " {
		t.Errorf("absences = %s", ranges(got))
	}
	if changed, _ := s.RemoveAbsenceDays(ctx, "T1", "U1", d(26), d(30), t0); changed {
		t.Error("nothing to remove, but changed")
	}
}

func TestDeleteAbsenceOnlyOwn(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	a, _ := s.AddAbsence(ctx, "T1", "U1", d(5), d(9), "U1", t0)
	if _, err := s.DeleteAbsence(ctx, "T1", "U2", a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("U2 deleted U1's absence: %v", err)
	}
	if _, err := s.DeleteAbsence(ctx, "T2", "", a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted from foreign workspace: %v", err)
	}
	if del, err := s.DeleteAbsence(ctx, "T1", "U1", a.ID); err != nil || del.SlackUserID != "U1" {
		t.Fatalf("own delete: %v %v", del, err)
	}
	// An admin (empty user) may delete anybody's absence.
	b, _ := s.AddAbsence(ctx, "T1", "U1", d(5), d(9), "cli", t0)
	if _, err := s.DeleteAbsence(ctx, "T1", "", b.ID); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteAbsencesBefore(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	s.AddAbsence(ctx, "T1", "U1", d(1), d(9), "U1", t0)
	s.AddAbsence(ctx, "T1", "U1", d(12), d(20), "U1", t0)
	if n, err := s.DeleteAbsencesBefore(ctx, d(10)); err != nil || n != 1 {
		t.Fatalf("deleted %d, %v", n, err)
	}
}

func TestSnapshotAbsences(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	p := newPoll(43) // 19.–23.10.
	if _, err := s.CreatePoll(ctx, p, users("U1", "U2"), t0); err != nil {
		t.Fatal(err)
	}
	s.AddAbsence(ctx, "T1", "U1", d(19), d(23), "U1", t0)
	s.AddAbsence(ctx, "T1", "U2", d(1), d(5), "U2", t0)   // long before
	s.AddAbsence(ctx, "T1", "U9", d(19), d(23), "U9", t0) // not a participant
	snap, err := s.PollSnapshot(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ranges(snap.Absences) != "U1:19-23 " {
		t.Errorf("snapshot absences = %s", ranges(snap.Absences))
	}
}
