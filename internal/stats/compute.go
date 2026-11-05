// Package stats derives the statistics of ADR 0003 from the daily status of
// every person: office occupancy, office days per person and month, and
// periods without an office day. compute.go holds the pure calculations;
// service.go reads and writes the database.
package stats

import (
	"math"
	"sort"
	"time"

	"officebot/internal/domain"
	"officebot/internal/storage"
)

// Bucket limits. OfficeBuckets count office days of a person in a month:
// 0, 1–2, 3–5, 6–8, 9–12, more than 12. RunBuckets count the longest period
// without office in work days: up to 1, 2, 4, 8 and more than 8 weeks.
var (
	OfficeBucketMax = []int{0, 2, 5, 8, 12} // the last bucket is open
	RunBucketMax    = []int{5, 10, 20, 40}  // the last bucket is open
)

// NumOfficeBuckets and NumRunBuckets are the bucket counts.
const (
	NumOfficeBuckets = 6
	NumRunBuckets    = 5
)

func bucket(limits []int, v int) int {
	for i, max := range limits {
		if v <= max {
			return i
		}
	}
	return len(limits)
}

// Month is the aggregate of one month. It contains no personal data and is
// stored as JSON in monthly_stats, so it must stay backward compatible.
type Month struct {
	Month            string `json:"month"`
	WorkDays         int    `json:"work_days"` // days with data that are not closed
	ClosedDays       int    `json:"closed_days"`
	OfficePersonDays int    `json:"office_person_days"`
	// Occupancy[n] is the number of work days with n people in the office.
	Occupancy        []int                 `json:"occupancy"`
	WeekdayOffice    [domain.NumDays]int   `json:"weekday_office"`
	WeekdayDays      [domain.NumDays]int   `json:"weekday_days"`
	Entries          int                   `json:"entries"` // person-days
	NoAnswer         int                   `json:"no_answer"`
	Away             int                   `json:"away"`
	Remote           int                   `json:"remote"`
	VerifiedDays     int                   `json:"verified_days"`
	CheckedEntries   int                   `json:"checked_entries"` // entries on checked days
	CorrectedEntries int                   `json:"corrected_entries"`
	OfficeBuckets    [NumOfficeBuckets]int `json:"office_buckets"` // people per bucket
	RunBuckets       [NumRunBuckets]int    `json:"run_buckets"`    // people per bucket
	People           int                   `json:"people"`

	// Stored marks a month taken from monthly_stats (no daily data used).
	Stored bool `json:"-"`
}

// Input is the daily data a report is calculated from.
type Input struct {
	Rows     []storage.AttendanceDay // ordered by person and day
	Closed   map[string]string       // closed days with reason
	Verified map[string]storage.VerifiedDay
	// OnlyVerified limits everything to checked days.
	OnlyVerified bool
}

// usable reports whether a row counts: not on a closed day and, if
// requested, only on checked days.
func (in *Input) usable(r storage.AttendanceDay) bool {
	day := r.Day.String()
	if _, closed := in.Closed[day]; closed {
		return false
	}
	if in.OnlyVerified {
		_, ok := in.Verified[day]
		return ok
	}
	return true
}

// Months aggregates the input per calendar month.
func Months(in *Input) map[string]*Month {
	months := map[string]*Month{}
	get := func(d domain.Date) *Month {
		key := monthKey(d)
		m := months[key]
		if m == nil {
			m = &Month{Month: key}
			months[key] = m
		}
		return m
	}
	type dayAgg struct{ office int }
	days := map[string]*dayAgg{}
	closedSeen := map[string]bool{}
	for _, r := range in.Rows {
		day := r.Day.String()
		if _, closed := in.Closed[day]; closed {
			if !closedSeen[day] {
				closedSeen[day] = true
				get(r.Day).ClosedDays++
			}
			continue
		}
		if !in.usable(r) {
			continue
		}
		m := get(r.Day)
		d := days[day]
		if d == nil {
			d = &dayAgg{}
			days[day] = d
		}
		m.Entries++
		switch r.Status {
		case storage.StatusOffice:
			d.office++
			m.OfficePersonDays++
		case storage.StatusNoAnswer:
			m.NoAnswer++
		case storage.StatusAway:
			m.Away++
		case storage.StatusRemote:
			m.Remote++
		}
		if _, ok := in.Verified[day]; ok {
			m.CheckedEntries++
			if r.Corrected() {
				m.CorrectedEntries++
			}
		}
	}
	for day, d := range days {
		date, _ := domain.ParseDate(day)
		m := get(date)
		m.WorkDays++
		for len(m.Occupancy) <= d.office {
			m.Occupancy = append(m.Occupancy, 0)
		}
		m.Occupancy[d.office]++
		if i := int(date.Weekday()) - 1; i >= 0 && i < domain.NumDays {
			m.WeekdayDays[i]++
			m.WeekdayOffice[i] += d.office
		}
		if _, ok := in.Verified[day]; ok {
			m.VerifiedDays++
		}
	}
	// Per person and month: office days and the longest run without office.
	for _, person := range byPerson(in) {
		perMonth := map[string][]storage.AttendanceDay{}
		for _, r := range person {
			perMonth[monthKey(r.Day)] = append(perMonth[monthKey(r.Day)], r)
		}
		for key, rows := range perMonth {
			m := months[key]
			office := 0
			for _, r := range rows {
				if r.Status == storage.StatusOffice {
					office++
				}
			}
			m.People++
			m.OfficeBuckets[bucket(OfficeBucketMax, office)]++
			m.RunBuckets[bucket(RunBucketMax, LongestRun(rows).WorkDays)]++
		}
	}
	return months
}

// byPerson returns the usable rows grouped per person, each ordered by day.
func byPerson(in *Input) [][]storage.AttendanceDay {
	var out [][]storage.AttendanceDay
	var cur []storage.AttendanceDay
	for _, r := range in.Rows {
		if !in.usable(r) {
			continue
		}
		if len(cur) > 0 && cur[0].SlackUserID != r.SlackUserID {
			out = append(out, cur)
			cur = nil
		}
		cur = append(cur, r)
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

func monthKey(d domain.Date) string { return d.String()[:7] }

// Run is a period of consecutive work days without an office day. Closed
// days and days a person was not asked neither break nor extend it.
type Run struct {
	Start, End domain.Date
	WorkDays   int
	Remote     int
	Away       int
	NoAnswer   int
}

// Weeks is the calendar span of the run in weeks.
func (r Run) Weeks() float64 {
	if r.WorkDays == 0 {
		return 0
	}
	days := r.End.In(time.UTC).Sub(r.Start.In(time.UTC)).Hours()/24 + 1
	return math.Round(days/7*10) / 10
}

// runs splits the rows of one person (ordered by day) into runs without office.
func runs(rows []storage.AttendanceDay) []Run {
	var out []Run
	var cur *Run
	for _, r := range rows {
		if r.Status == storage.StatusOffice {
			if cur != nil {
				out = append(out, *cur)
				cur = nil
			}
			continue
		}
		if cur == nil {
			cur = &Run{Start: r.Day}
		}
		cur.End = r.Day
		cur.WorkDays++
		switch r.Status {
		case storage.StatusRemote:
			cur.Remote++
		case storage.StatusAway:
			cur.Away++
		case storage.StatusNoAnswer:
			cur.NoAnswer++
		}
	}
	if cur != nil {
		out = append(out, *cur)
	}
	return out
}

// LongestRun returns the longest run without office (the earliest one on a tie).
func LongestRun(rows []storage.AttendanceDay) Run {
	var best Run
	for _, r := range runs(rows) {
		if r.WorkDays > best.WorkDays {
			best = r
		}
	}
	return best
}

// ------------------------------------------------------------------ report

// Summary combines months into figures for a whole period.
type Summary struct {
	WorkDays         int
	OfficePersonDays int
	Average          float64
	Median           int
	P90, P95, Max    int
	WeekdayAverage   [domain.NumDays]float64
	// Desks[n] is the share of work days on which n desks would have been enough.
	Desks          []float64
	ResponseRate   float64
	VerifiedShare  float64
	CorrectedShare float64 // corrected entries among entries on checked days
	CheckedEntries int
	OfficeBuckets  [NumOfficeBuckets]int
	PersonMonths   int
	Entries        int
	Remote, Away   int
	NoAnswer       int
}

// DesksFor returns the smallest number of desks that would have been enough
// on at least share of the work days.
func (s *Summary) DesksFor(share float64) int {
	for n, v := range s.Desks {
		if v >= share-1e-9 {
			return n
		}
	}
	return len(s.Desks) - 1
}

// Combine sums up months; percentiles come from the merged occupancy
// histograms, so they are exact for the whole period.
func Combine(months []*Month) Summary {
	var s Summary
	var occ []int
	var verified, wdOffice, wdDays = 0, [domain.NumDays]int{}, [domain.NumDays]int{}
	for _, m := range months {
		s.WorkDays += m.WorkDays
		s.OfficePersonDays += m.OfficePersonDays
		s.Entries += m.Entries
		s.NoAnswer += m.NoAnswer
		s.Remote += m.Remote
		s.Away += m.Away
		s.CheckedEntries += m.CheckedEntries
		s.CorrectedShare += float64(m.CorrectedEntries)
		s.PersonMonths += m.People
		verified += m.VerifiedDays
		for n, days := range m.Occupancy {
			for len(occ) <= n {
				occ = append(occ, 0)
			}
			occ[n] += days
		}
		for i := range wdOffice {
			wdOffice[i] += m.WeekdayOffice[i]
			wdDays[i] += m.WeekdayDays[i]
		}
		for i := range s.OfficeBuckets {
			s.OfficeBuckets[i] += m.OfficeBuckets[i]
		}
	}
	if s.CheckedEntries > 0 {
		s.CorrectedShare /= float64(s.CheckedEntries)
	} else {
		s.CorrectedShare = 0
	}
	if s.WorkDays == 0 {
		return s
	}
	s.Average = float64(s.OfficePersonDays) / float64(s.WorkDays)
	s.Median = percentile(occ, s.WorkDays, 0.5)
	s.P90 = percentile(occ, s.WorkDays, 0.9)
	s.P95 = percentile(occ, s.WorkDays, 0.95)
	s.Max = len(occ) - 1
	for i := range wdOffice {
		if wdDays[i] > 0 {
			s.WeekdayAverage[i] = float64(wdOffice[i]) / float64(wdDays[i])
		}
	}
	cum := 0
	s.Desks = make([]float64, len(occ))
	for n, days := range occ {
		cum += days
		s.Desks[n] = float64(cum) / float64(s.WorkDays)
	}
	if s.Entries > 0 {
		s.ResponseRate = float64(s.Entries-s.NoAnswer) / float64(s.Entries)
	}
	s.VerifiedShare = float64(verified) / float64(s.WorkDays)
	return s
}

// percentile returns the nearest-rank percentile of a histogram.
func percentile(hist []int, total int, p float64) int {
	rank := int(math.Ceil(p * float64(total)))
	rank = max(rank, 1)
	cum := 0
	for n, c := range hist {
		cum += c
		if cum >= rank {
			return n
		}
	}
	return len(hist) - 1
}

// DayPoint is the occupancy of one work day.
type DayPoint struct {
	Day      domain.Date
	Office   int
	Total    int // people asked
	Verified bool
}

// Days returns the occupancy of every usable work day, ordered by day.
func Days(in *Input) []DayPoint {
	m := map[string]*DayPoint{}
	for _, r := range in.Rows {
		if !in.usable(r) {
			continue
		}
		day := r.Day.String()
		p := m[day]
		if p == nil {
			_, verified := in.Verified[day]
			p = &DayPoint{Day: r.Day, Verified: verified}
			m[day] = p
		}
		p.Total++
		if r.Status == storage.StatusOffice {
			p.Office++
		}
	}
	out := make([]DayPoint, 0, len(m))
	for _, p := range m {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Day.Before(out[j].Day) })
	return out
}

// RunBucketsOf returns the distribution of every person's longest run
// without office over the whole input.
func RunBucketsOf(in *Input) (buckets [NumRunBuckets]int, people int) {
	for _, rows := range byPerson(in) {
		buckets[bucket(RunBucketMax, LongestRun(rows).WorkDays)]++
		people++
	}
	return buckets, people
}

// --------------------------------------------------------------- per person

// PersonMonth are the figures of one person in one month.
type PersonMonth struct {
	Month                          string
	Office, Remote, Away, NoAnswer int
}

// Rate is the share of office days among the days the person was asked and
// not away.
func (m PersonMonth) Rate() float64 {
	if d := m.Office + m.Remote + m.NoAnswer; d > 0 {
		return float64(m.Office) / float64(d)
	}
	return 0
}

// Person are the figures of one person.
type Person struct {
	UserID, Name string
	Months       []PersonMonth
	Total        PersonMonth
	LastOffice   domain.Date // within all daily data up to the end of the period
	Current      Run         // current run without office at the end of the data
	Longest      Run         // longest run within the period
}

// People calculates the per-person figures from the rows of the period (in).
// history contains all daily data up to the end of the period, for the last
// office day and the current run.
func People(in *Input, history *Input) []Person {
	hist := map[string][]storage.AttendanceDay{}
	for _, rows := range byPerson(history) {
		hist[rows[0].SlackUserID] = rows
	}
	var out []Person
	for _, rows := range byPerson(in) {
		p := Person{UserID: rows[0].SlackUserID, Name: rows[len(rows)-1].Name}
		months := map[string]*PersonMonth{}
		var order []string
		for _, r := range rows {
			key := monthKey(r.Day)
			pm := months[key]
			if pm == nil {
				pm = &PersonMonth{Month: key}
				months[key] = pm
				order = append(order, key)
			}
			for _, m := range []*PersonMonth{pm, &p.Total} {
				switch r.Status {
				case storage.StatusOffice:
					m.Office++
				case storage.StatusRemote:
					m.Remote++
				case storage.StatusAway:
					m.Away++
				case storage.StatusNoAnswer:
					m.NoAnswer++
				}
			}
		}
		for _, key := range order {
			p.Months = append(p.Months, *months[key])
		}
		p.Longest = LongestRun(rows)
		all := hist[p.UserID]
		for i := len(all) - 1; i >= 0; i-- {
			if all[i].Status == storage.StatusOffice {
				p.LastOffice = all[i].Day
				break
			}
		}
		if rs := runs(all); len(rs) > 0 && rs[len(rs)-1].End.String() == all[len(all)-1].Day.String() {
			p.Current = rs[len(rs)-1]
		}
		out = append(out, p)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
