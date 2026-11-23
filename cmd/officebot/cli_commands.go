package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"officebot/internal/config"
	"officebot/internal/domain"
	"officebot/internal/i18n"
	"officebot/internal/poll"
	"officebot/internal/storage"
	"officebot/internal/summary"
)

// attendance corrects a participant's response, also after the deadline.
// Changes are only written to the database; the running bot updates the DM,
// the summary and posts the change notice within 30 s.
func attendance(ctx context.Context, a *app, p *domain.Poll, sub, user, dayList string, notice bool) error {
	ops := map[string]poll.AttendanceChange{"set": poll.AttendanceSet, "add": poll.AttendanceAdd, "remove": poll.AttendanceRemove}
	op, ok := ops[sub]
	if !ok {
		return fmt.Errorf("unknown attendance command %q (use set, add or remove)", sub)
	}
	if user == "" || dayList == "" {
		return fmt.Errorf("usage: officebot attendance %s --user U --days mon,tue [--week W] [--no-notice]", sub)
	}
	var days domain.Days
	if !(op == poll.AttendanceSet && strings.EqualFold(strings.TrimSpace(dayList), "none")) {
		var err error
		if days, err = parseDays(dayList); err != nil {
			return err
		}
		if days.Count() == 0 {
			return errors.New("--days is empty; use --days none with 'set' for no office day")
		}
	}
	before, after, changed, err := a.svc.SetAttendance(ctx, p.ID, user, op, days, notice, "cli")
	if err != nil {
		return err
	}
	answer := func(r *domain.Response) string {
		if r == nil {
			return "no answer"
		}
		if r.Days.Count() == 0 {
			return "no office day"
		}
		return strings.Join(r.Days.Names(func(wd time.Weekday) string { return wd.String()[:3] }), ", ")
	}
	week := fmt.Sprintf("%d-W%02d", p.ISOYear, p.ISOWeek)
	if !changed {
		fmt.Printf("%s in %s: unchanged (%s)\n", user, week, answer(after))
		return nil
	}
	fmt.Printf("%s in %s: %s -> %s\n", user, week, answer(before), answer(after))
	fmt.Println("the running bot updates the DM and the summary within 30 s")
	return nil
}

// listUsers prints the workspace users with their IDs and a ready-to-paste
// SLACK_USER_IDS line containing everyone who would be asked.
func listUsers(ctx context.Context, a *app, channelID string, all bool) error {
	entries, err := a.people.Directory(ctx, channelID)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tREAL NAME\tNOTE")
	var ids []string
	hidden := 0
	for _, e := range entries {
		if e.Exclusion == "" {
			ids = append(ids, e.User.ID)
		} else if !all {
			hidden++
			continue
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", e.User.ID, e.User.Name, e.User.RealName, e.Exclusion)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Printf("\n%d user(s) would be asked", len(ids))
	if hidden > 0 {
		fmt.Printf(", %d bot/deactivated/excluded user(s) hidden (show with --all)", hidden)
	}
	fmt.Printf("\n\nSLACK_USER_IDS=%s\n", strings.Join(ids, ","))
	return nil
}

func status(ctx context.Context, a *app, loc *time.Location) error {
	polls, err := a.store.OpenPolls(ctx)
	if err != nil {
		return err
	}
	if len(polls) == 0 {
		fmt.Println("no open polls")
	}
	for _, p := range polls {
		snap, err := a.store.PollSnapshot(ctx, p.ID)
		if err != nil {
			return err
		}
		st := summary.Aggregate(snap, loc)
		week := snap.Poll.Dates(loc)
		fmt.Printf("poll %d  %d-W%02d  deadline %s  answered %d/%d  summary v%d (synced v%d)\n",
			p.ID, p.ISOYear, p.ISOWeek, p.Deadline.In(loc).Format(time.DateTime), st.Answered, st.Total, p.SummaryVersion, p.SummarySyncedVersion)
		for _, part := range snap.Participants {
			answer := "open"
			if domain.AwayAllWeek(snap.Absences, part.SlackUserID, week) {
				answer = "away"
			} else if r, ok := snap.Responses[part.SlackUserID]; ok {
				answer = strings.Join(r.Days.Names(func(wd time.Weekday) string { return wd.String()[:3] }), ", ")
				if answer == "" {
					answer = "no office day"
				}
			}
			fmt.Printf("  %-12s %-20s delivery=%-11s reminders=%d  %s\n", part.SlackUserID, part.DisplayName, part.DeliveryStatus, part.ReminderCount, answer)
		}
	}
	stats, err := a.store.JobStats(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("outbox jobs: %v\n", stats)
	return nil
}

// away manages time off. Changes are only written to the database; the
// running bot updates DMs and the summary within 30 s.
func away(ctx context.Context, a *app, sub, user, fromFlag, toFlag string, id int64, loc *time.Location) error {
	today := domain.DateOf(time.Now().In(loc))
	dates := func() (domain.Date, domain.Date, error) {
		if user == "" || fromFlag == "" || toFlag == "" {
			return domain.Date{}, domain.Date{}, fmt.Errorf("usage: officebot away %s --user U --from YYYY-MM-DD --to YYYY-MM-DD", sub)
		}
		from, err := domain.ParseDate(fromFlag)
		if err != nil {
			return from, from, err
		}
		to, err := domain.ParseDate(toFlag)
		return from, to, err
	}
	switch sub {
	case "list":
		abs, err := a.store.ListAbsences(ctx, a.teamID, user, today)
		if err != nil {
			return err
		}
		if len(abs) == 0 {
			fmt.Println("no upcoming time off")
			return nil
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tUSER\tFROM\tTO\tENTERED BY")
		for _, ab := range abs {
			fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\n", ab.ID, ab.SlackUserID, ab.Start, ab.End, ab.CreatedBy)
		}
		return w.Flush()
	case "add":
		from, to, err := dates()
		if err != nil {
			return err
		}
		ab, err := a.svc.AddTimeOff(ctx, user, from, to, "cli")
		if err != nil {
			return err
		}
		fmt.Printf("time off of %s: %s to %s (entry %d, merged with overlapping or adjacent entries)\n", user, ab.Start, ab.End, ab.ID)
	case "remove":
		if id > 0 {
			ab, err := a.svc.DeleteTimeOff(ctx, id, "cli")
			if errors.Is(err, storage.ErrNotFound) {
				return fmt.Errorf("no time-off entry %d", id)
			}
			if err != nil {
				return err
			}
			fmt.Printf("time off of %s from %s to %s deleted\n", ab.SlackUserID, ab.Start, ab.End)
			break
		}
		from, to, err := dates()
		if err != nil {
			return err
		}
		changed, err := a.svc.RemoveTimeOff(ctx, user, from, to, "cli")
		if err != nil {
			return err
		}
		if !changed {
			fmt.Printf("%s has no time off between %s and %s\n", user, from, to)
			return nil
		}
		fmt.Printf("%s is no longer away from %s to %s\n", user, from, to)
	default:
		return fmt.Errorf("unknown away command %q (use list, add or remove)", sub)
	}
	fmt.Println("the running bot updates DMs and the summary within 30 s")
	return nil
}

// lang manages the language people chose for their DMs (ADR 0002). Changes
// are only written to the database; the running bot re-renders open poll DMs
// within 30 s.
func lang(ctx context.Context, a *app, sub, user, language string, loc *time.Location) error {
	switch sub {
	case "list":
		langs, err := a.store.ListUserLanguages(ctx, a.teamID)
		if err != nil {
			return err
		}
		if len(langs) == 0 {
			fmt.Println("nobody has chosen a language; everyone gets the team default")
			return nil
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "USER\tLANGUAGE\tNAME\tCHANGED")
		for _, l := range langs {
			name := "(not available, team default applies)"
			if tr, err := i18n.New(l.Language); err == nil {
				name = tr.T("language.name")
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", l.SlackUserID, l.Language, name, l.UpdatedAt.In(loc).Format(time.DateTime))
		}
		return w.Flush()
	case "set":
		if user == "" || language == "" {
			return errors.New("usage: officebot lang set --user U <language>")
		}
		tr, err := a.svc.SetLanguage(ctx, user, language, "cli")
		if err != nil {
			return err
		}
		fmt.Printf("language of %s set to %s (%s)\n", user, tr.Lang(), tr.T("language.name"))
	case "reset":
		if user == "" {
			return errors.New("usage: officebot lang reset --user U")
		}
		changed, err := a.svc.ResetLanguage(ctx, user, "cli")
		if err != nil {
			return err
		}
		if !changed {
			fmt.Printf("%s has not chosen a language\n", user)
			return nil
		}
		fmt.Printf("language of %s reset to the team default\n", user)
	default:
		return fmt.Errorf("unknown lang command %q (use list, set or reset)", sub)
	}
	fmt.Println("the running bot updates open poll DMs within 30 s")
	return nil
}

// webUser manages who may log in to the web interface (ADR 0003). The first
// admin can only be added here; afterwards admins can also use the web
// interface.
func webUser(ctx context.Context, a *app, sub, user, role string, loc *time.Location) error {
	now := time.Now()
	needUser := func() error {
		if user == "" {
			return fmt.Errorf("usage: officebot web-user %s --user U", sub)
		}
		return nil
	}
	switch sub {
	case "list":
		users, err := a.store.ListWebUsers(ctx, a.teamID)
		if err != nil {
			return err
		}
		if len(users) == 0 {
			fmt.Println("nobody has access to the web interface; add the first admin with: officebot web-user add --user U")
			return nil
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "USER\tNAME\tROLE\tADDED BY\tADDED\tLAST LOGIN")
		for _, u := range users {
			last := "never"
			if !u.LastLoginAt.IsZero() {
				last = u.LastLoginAt.In(loc).Format(time.DateTime)
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", u.SlackUserID, u.DisplayName, u.Role, u.AddedBy, u.AddedAt.In(loc).Format(time.DateTime), last)
		}
		return w.Flush()
	case "add":
		if err := needUser(); err != nil {
			return err
		}
		if role == "" {
			role = storage.RoleAdmin
		}
		if !storage.ValidRole(role) {
			return fmt.Errorf("unknown role %q (use admin, analyst or viewer)", role)
		}
		name := ""
		info, err := a.api.UserInfo(ctx, user)
		if err != nil {
			return fmt.Errorf("slack user %s: %w", user, err)
		}
		if info.TeamID != "" && info.TeamID != a.teamID {
			return fmt.Errorf("%s belongs to another workspace", user)
		}
		name = info.Name
		err = a.store.AddWebUser(ctx, a.teamID, storage.WebUser{SlackUserID: user, Role: role, DisplayName: name}, "cli", now)
		if errors.Is(err, storage.ErrExists) {
			return fmt.Errorf("%s already has web access; change the role with: officebot web-user set-role --user %s --role %s", user, user, role)
		}
		if err != nil {
			return err
		}
		fmt.Printf("%s (%s) may now log in to the web interface as %s\n", user, name, role)
	case "set-role":
		if err := needUser(); err != nil {
			return err
		}
		if !storage.ValidRole(role) {
			return errors.New("usage: officebot web-user set-role --user U --role admin|analyst|viewer")
		}
		changed, err := a.store.SetWebUserRole(ctx, a.teamID, user, role, "cli", now)
		if errors.Is(err, storage.ErrNotFound) {
			return fmt.Errorf("%s has no web access", user)
		}
		if err != nil {
			return err
		}
		if !changed {
			fmt.Printf("%s already has the role %s\n", user, role)
			return nil
		}
		fmt.Printf("role of %s set to %s; it applies to running sessions at once\n", user, role)
	case "remove":
		if err := needUser(); err != nil {
			return err
		}
		err := a.store.RemoveWebUser(ctx, a.teamID, user, "cli", now)
		if errors.Is(err, storage.ErrNotFound) {
			return fmt.Errorf("%s has no web access", user)
		}
		if err != nil {
			return err
		}
		fmt.Printf("%s can no longer log in to the web interface; open sessions were ended\n", user)
	default:
		return fmt.Errorf("unknown web-user command %q (use list, add, set-role or remove)", sub)
	}
	return nil
}

// verifyDay marks a day as checked (ADR 0003): the status of everybody asked
// on that day matches actual attendance.
func verifyDay(ctx context.Context, a *app, dateFlag string, undo bool) error {
	day, err := domain.ParseDate(dateFlag)
	if err != nil {
		return errors.New("usage: officebot attendance verify --date YYYY-MM-DD [--undo]")
	}
	if undo {
		err := a.store.UnverifyDay(ctx, a.teamID, day, "cli", time.Now())
		if errors.Is(err, storage.ErrNotFound) {
			fmt.Printf("%s is not marked as checked\n", day)
			return nil
		}
		if err != nil {
			return err
		}
		fmt.Printf("%s is no longer marked as checked\n", day)
		return nil
	}
	if err := a.store.VerifyDay(ctx, a.teamID, day, "cli", time.Now()); err != nil {
		return err
	}
	fmt.Printf("%s marked as checked\n", day)
	return nil
}

// statsPurge deletes daily status per person (ADR 0003). The monthly figures
// without names are frozen first and kept. Without --yes it only shows what
// would be deleted.
func statsPurge(ctx context.Context, a *app, cfg *config.Config, sub string, year int, beforeFlag, user string, yes bool, log *slog.Logger) error {
	usage := errors.New("usage: officebot stats purge --year Y | --before YYYY-MM-DD | --user U [--yes]")
	if sub != "purge" {
		return usage
	}
	var scope storage.PurgeScope
	set := 0
	if year > 0 {
		scope.From, scope.To = domain.NewDate(year, 1, 1), domain.NewDate(year, 12, 31)
		set++
	}
	if beforeFlag != "" {
		d, err := domain.ParseDate(beforeFlag)
		if err != nil {
			return err
		}
		scope.To = d.AddDays(-1)
		set++
	}
	if user != "" {
		scope.UserID = user
		set++
	}
	if set != 1 {
		return usage
	}
	svc := a.stats
	p, err := svc.PreviewPurge(ctx, scope)
	if err != nil {
		return err
	}
	what := fmt.Sprintf("%d daily entries of %d person(s)", p.Rows, p.People)
	if p.Rows > 0 {
		what += fmt.Sprintf(" from %s to %s", p.First, p.Last)
	}
	if user != "" {
		what += fmt.Sprintf(", %d response(s) and %d time-off entry(ies) of %s, their participation in stored polls and their language choice", p.Responses, p.Absences, user)
	}
	if !yes {
		fmt.Printf("this would delete %s; monthly figures without names are kept. Repeat with --yes\n", what)
		return nil
	}
	if p.Rows == 0 && user == "" {
		fmt.Println("nothing to delete")
		return nil
	}
	if _, err := svc.Purge(ctx, scope, "cli"); err != nil {
		return err
	}
	fmt.Printf("deleted %s; monthly figures without names are kept\n", what)
	return nil
}
