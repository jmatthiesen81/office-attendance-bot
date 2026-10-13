// Package summary renders and synchronises the weekly channel overview.
package summary

import (
	"fmt"
	"sort"
	"strings"
	"time"

	goslack "github.com/slack-go/slack"

	"officebot/internal/domain"
	"officebot/internal/i18n"
	"officebot/internal/slack"
	"officebot/internal/storage"
)

// maxSectionChars stays below Slack's 3000 character limit for section text.
const maxSectionChars = 2800

// Stats is the aggregated poll state.
type Stats struct {
	Days        [domain.NumDays][]domain.Participant
	Answered    int
	Total       int
	Open        []domain.Participant // no response yet
	NoOffice    []domain.Participant // answered with an empty selection
	Unreachable []domain.Participant // DM could not be delivered
	Away        []domain.Participant // away all week; not part of Total
}

// Aggregate groups participants by day. Not answering is kept strictly
// separate from answering with "no office day". People who are away for the
// whole week are only listed as away, whatever they answered before.
func Aggregate(snap *storage.Snapshot, loc *time.Location) Stats {
	parts := append([]domain.Participant(nil), snap.Participants...)
	sort.SliceStable(parts, func(i, j int) bool {
		return strings.ToLower(parts[i].DisplayName) < strings.ToLower(parts[j].DisplayName)
	})
	week := snap.Poll.Dates(loc)
	st := Stats{}
	for _, p := range parts {
		if domain.AwayAllWeek(snap.Absences, p.SlackUserID, week) {
			st.Away = append(st.Away, p)
			continue
		}
		st.Total++
		r, ok := snap.Responses[p.SlackUserID]
		if !ok {
			st.Open = append(st.Open, p)
			if p.DeliveryStatus == domain.DeliveryUnreachable || p.DeliveryStatus == domain.DeliveryFailed {
				st.Unreachable = append(st.Unreachable, p)
			}
			continue
		}
		st.Answered++
		if r.Days.Count() == 0 {
			st.NoOffice = append(st.NoOffice, p)
		}
		for i, on := range r.Days {
			if on {
				st.Days[i] = append(st.Days[i], p)
			}
		}
	}
	return st
}

// Renderer builds the summary message.
type Renderer struct {
	Loc      *time.Location
	MaxNames int
	T        *i18n.Translator
}

func (r Renderer) title(p *domain.Poll) string {
	return r.T.T("summary.title", "week", p.ISOWeek, "range", r.T.DateRange(p.WeekStart.In(r.Loc), p.WeekEnd.In(r.Loc)))
}

// Placeholder is posted first. It deliberately contains no user mentions so
// that publishing the summary does not notify everybody listed as open;
// mentions only appear through chat.update, which does not notify.
func (r Renderer) Placeholder(p *domain.Poll) slack.Message {
	t := r.title(p)
	return slack.Message{
		Text: t,
		Blocks: []goslack.Block{
			goslack.NewHeaderBlock(goslack.NewTextBlockObject(goslack.PlainTextType, t, true, false)),
			goslack.NewContextBlock("", goslack.NewTextBlockObject(goslack.MarkdownType, r.T.T("summary.placeholder"), false, false)),
		},
	}
}

// Render renders the full summary. The block count is constant (< 15), well
// below Slack's limit of 50; long name lists are truncated per section.
func (r Renderer) Render(snap *storage.Snapshot, now time.Time) slack.Message {
	p := &snap.Poll
	st := Aggregate(snap, r.Loc)
	md := func(s string) *goslack.TextBlockObject {
		return goslack.NewTextBlockObject(goslack.MarkdownType, s, false, false)
	}

	blocks := []goslack.Block{
		goslack.NewHeaderBlock(goslack.NewTextBlockObject(goslack.PlainTextType, r.title(p), true, false)),
	}
	for i, people := range st.Days {
		text := fmt.Sprintf("*%s (%d)*\n%s", r.T.Weekday(domain.DayWeekdays[i]), len(people), r.names(people, "–"))
		blocks = append(blocks, goslack.NewSectionBlock(md(text), nil, nil))
	}
	blocks = append(blocks, goslack.NewDividerBlock())

	lines := []string{r.T.T("summary.responses", "answered", st.Answered, "total", st.Total)}
	if len(st.Away) > 0 {
		lines[0] = r.T.T("summary.responses_away", "answered", st.Answered, "total", st.Total, "away", len(st.Away))
	}
	if len(st.Open) > 0 {
		lines = append(lines, r.T.T("summary.open", "names", r.names(st.Open, "")))
	}
	if len(st.NoOffice) > 0 {
		lines = append(lines, r.T.T("summary.no_office", "names", r.names(st.NoOffice, "")))
	}
	if len(st.Away) > 0 {
		lines = append(lines, r.T.T("summary.away", "names", r.names(st.Away, "")))
	}
	blocks = append(blocks, goslack.NewSectionBlock(md(clip(strings.Join(lines, "\n"))), nil, nil))
	if len(st.Unreachable) > 0 {
		blocks = append(blocks, goslack.NewContextBlock("", md(r.T.T("summary.unreachable", "names", r.names(st.Unreachable, "")))))
	}

	footer := r.T.T("summary.updated", "time", r.T.Time(now.In(r.Loc)))
	open := p.IsAcceptingResponses(now)
	if open {
		footer += " · " + r.T.T("summary.deadline", "deadline", r.T.DateTime(p.Deadline.In(r.Loc)))
	} else {
		footer += " · " + r.T.T("summary.closed")
	}
	blocks = append(blocks, goslack.NewContextBlock("", md(footer)))
	if open {
		// For anyone who did not get (or cannot find) their DM, and for
		// people who joined the target group after the weekly start.
		blocks = append(blocks, slack.RequestDMBlock(r.T, p.ID))
	}

	return slack.Message{
		Text:   r.T.T("summary.fallback", "title", r.title(p), "answered", st.Answered, "total", st.Total),
		Blocks: blocks,
	}
}

// names renders user mentions, truncated to MaxNames and the section limit.
func (r Renderer) names(people []domain.Participant, empty string) string {
	if len(people) == 0 {
		return empty
	}
	limit := r.MaxNames
	if limit <= 0 {
		limit = len(people)
	}
	var sb strings.Builder
	shown := 0
	for _, p := range people {
		m := "<@" + p.SlackUserID + ">"
		if shown >= limit || sb.Len()+len(m)+40 > maxSectionChars/2 {
			break
		}
		if shown > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString(m)
		shown++
	}
	if rest := len(people) - shown; rest > 0 {
		sb.WriteString(r.T.T("summary.more", "count", rest))
	}
	return sb.String()
}

func clip(s string) string {
	if len(s) <= maxSectionChars {
		return s
	}
	return s[:maxSectionChars] + " …"
}
