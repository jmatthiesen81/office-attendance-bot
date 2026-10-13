package isoweek

import (
	"testing"
	"time"
)

func berlin(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func TestMondayAndOf(t *testing.T) {
	loc := berlin(t)
	cases := []struct {
		week Week
		want string
	}{
		{Week{2026, 43}, "2026-10-19"},
		{Week{2026, 1}, "2025-12-29"}, // week 1 starts in previous year
		{Week{2020, 53}, "2020-12-28"},
		{Week{2027, 1}, "2027-01-04"},
		{Week{2026, 53}, "2026-12-28"},
		{Week{2021, 52}, "2021-12-27"},
	}
	for _, c := range cases {
		got := c.week.Monday(loc).Format("2006-01-02")
		if got != c.want {
			t.Errorf("%v.Monday() = %s, want %s", c.week, got, c.want)
		}
		if back := Of(c.week.Monday(loc)); back != c.week {
			t.Errorf("Of(%v.Monday()) = %v", c.week, back)
		}
	}
}

func TestYearBoundaries(t *testing.T) {
	loc := berlin(t)
	cases := []struct {
		date string
		want Week
	}{
		{"2020-12-31", Week{2020, 53}},
		{"2021-01-01", Week{2020, 53}},
		{"2021-01-03", Week{2020, 53}},
		{"2021-01-04", Week{2021, 1}},
		{"2024-12-30", Week{2025, 1}},
		{"2026-12-31", Week{2026, 53}},
		{"2027-01-01", Week{2026, 53}},
		{"2027-01-04", Week{2027, 1}},
	}
	for _, c := range cases {
		d, _ := time.ParseInLocation("2006-01-02", c.date, loc)
		if got := Of(d); got != c.want {
			t.Errorf("Of(%s) = %v, want %v", c.date, got, c.want)
		}
	}
}

func TestAddAcrossYears(t *testing.T) {
	if got := (Week{2026, 53}).Add(1); got != (Week{2027, 1}) {
		t.Errorf("2026-W53 + 1 = %v", got)
	}
	if got := (Week{2026, 52}).Add(1); got != (Week{2026, 53}) {
		t.Errorf("2026-W52 + 1 = %v", got)
	}
	if got := (Week{2025, 52}).Add(1); got != (Week{2026, 1}) {
		t.Errorf("2025-W52 + 1 = %v", got)
	}
	if got := (Week{2021, 1}).Add(-1); got != (Week{2020, 53}) {
		t.Errorf("2021-W01 - 1 = %v", got)
	}
}

func TestWeeksInYear(t *testing.T) {
	for year, want := range map[int]int{2020: 53, 2021: 52, 2025: 52, 2026: 53, 2027: 52} {
		if got := WeeksInYear(year); got != want {
			t.Errorf("WeeksInYear(%d) = %d, want %d", year, got, want)
		}
	}
}

func TestParse(t *testing.T) {
	w, err := Parse("2026-W43")
	if err != nil || w != (Week{2026, 43}) {
		t.Fatalf("Parse = %v, %v", w, err)
	}
	if w.String() != "2026-W43" {
		t.Errorf("String = %s", w)
	}
	for _, bad := range []string{"2026-43", "2025-W53", "2026-W00", "foo"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) should fail", bad)
		}
	}
}

func TestPrevNextAcrossDST(t *testing.T) {
	loc := berlin(t)
	// DST ends on Sunday 2026-10-25 in Berlin. Monday 09:00 must stay 09:00 local.
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, loc) // Saturday
	next := Next(now, time.Monday, 9, 0, loc)
	if next.Format("2006-01-02 15:04 MST") != "2026-10-26 09:00 CET" {
		t.Errorf("Next = %s", next.Format("2006-01-02 15:04 MST"))
	}
	prev := Prev(now, time.Monday, 9, 0, loc)
	if prev.Format("2006-01-02 15:04 MST") != "2026-10-19 09:00 CEST" {
		t.Errorf("Prev = %s", prev.Format("2006-01-02 15:04 MST"))
	}
	// Exactly at the occurrence: Prev includes it, Next excludes it.
	at := time.Date(2026, 10, 19, 9, 0, 0, 0, loc)
	if !Prev(at, time.Monday, 9, 0, loc).Equal(at) {
		t.Error("Prev should include now")
	}
	if Next(at, time.Monday, 9, 0, loc).Equal(at) {
		t.Error("Next should exclude now")
	}
	// DST starts 2027-03-28; Monday 2027-03-29 09:00 is CEST.
	n := Next(time.Date(2027, 3, 27, 0, 0, 0, 0, loc), time.Monday, 9, 0, loc)
	if n.Format("15:04 MST") != "09:00 CEST" {
		t.Errorf("spring Next = %s", n.Format("2006-01-02 15:04 MST"))
	}
}
