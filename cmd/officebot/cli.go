package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"officebot/internal/config"
	"officebot/internal/domain"
	"officebot/internal/isoweek"
	"officebot/internal/slack"
	"officebot/internal/storage"
)

// --------------------------------------------------------------------- CLI

func cli(ctx context.Context, cmd string, args []string, cfg *config.Config, log *slog.Logger) error {
	var sub string
	if cmd == "away" || cmd == "lang" || cmd == "attendance" || cmd == "web-user" || cmd == "stats" {
		if len(args) == 0 || strings.HasPrefix(args[0], "-") {
			switch cmd {
			case "stats":
				return errors.New("usage: officebot stats purge --year Y | --before YYYY-MM-DD | --user U [--yes]")
			case "web-user":
				return errors.New("usage: officebot web-user list|add|set-role|remove [--user U] [--role admin|analyst|viewer]")
			case "lang":
				return errors.New("usage: officebot lang list|set|reset [flags]")
			case "attendance":
				return errors.New("usage: officebot attendance set|add|remove --user U --days mon,tue [--week W] [--no-notice] | verify --date D [--undo]")
			}
			return errors.New("usage: officebot away list|add|remove [flags]")
		}
		sub, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	weekFlag := fs.String("week", "", "ISO week, e.g. 2026-W43 (default: current target week)")
	yes := fs.Bool("yes", false, "confirm destructive operation")
	user := fs.String("user", "", "Slack user ID (simulate-save, simulate-request, away, lang)")
	from := fs.String("from", "", "first day of time off, YYYY-MM-DD (away)")
	to := fs.String("to", "", "last day of time off, YYYY-MM-DD (away)")
	id := fs.Int64("id", 0, "ID of a time-off entry as shown by 'away list' (away remove)")
	days := fs.String("days", "", "comma separated days mon,tue,wed,thu,fri; empty = no office day (simulate-save), 'none' = no office day (attendance set)")
	noNotice := fs.Bool("no-notice", false, "do not announce the change in the summary thread (attendance)")
	channel := fs.String("channel", "", "only members of this channel ID (list-users)")
	all := fs.Bool("all", false, "also show bots, deactivated and excluded users (list-users)")
	role := fs.String("role", "", "role in the web interface: admin, analyst or viewer (web-user)")
	date := fs.String("date", "", "day, YYYY-MM-DD (attendance verify)")
	undo := fs.Bool("undo", false, "remove the checked mark (attendance verify)")
	year := fs.Int("year", 0, "calendar year (stats purge)")
	before := fs.String("before", "", "delete everything before this day, YYYY-MM-DD (stats purge)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if cmd == "migrate" || cmd == "backup" {
		store, err := storage.Open(ctx, cfg.DatabasePath)
		if err != nil {
			return err
		}
		defer store.Close()
		if cmd == "migrate" {
			log.Info("database is up to date", "path", cfg.DatabasePath)
			return nil
		}
		if fs.NArg() != 1 {
			return errors.New("usage: officebot backup <target file>")
		}
		if err := store.Backup(ctx, fs.Arg(0)); err != nil {
			return err
		}
		fmt.Println("backup written to", fs.Arg(0))
		return nil
	}

	if cmd == "list-users" {
		a, err := connect(ctx, cfg, log)
		if err != nil {
			return err
		}
		return listUsers(ctx, a, *channel, *all)
	}

	a, err := setup(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer a.store.Close()

	week, _ := a.svc.CurrentTargetWeek(time.Now())
	if *weekFlag != "" {
		if week, err = isoweek.Parse(*weekFlag); err != nil {
			return err
		}
	}
	findPoll := func() (*domain.Poll, error) {
		p, err := a.store.GetPollByWeek(ctx, a.teamID, week.Year, week.Week)
		if errors.Is(err, storage.ErrNotFound) {
			return nil, fmt.Errorf("no poll for %s", week)
		}
		return p, err
	}

	switch cmd {
	case "start-week":
		p, created, err := a.svc.StartWeek(ctx, week)
		if err != nil {
			return err
		}
		if created {
			fmt.Printf("poll %d for %s created; the running bot sends the DMs within 30 s\n", p.ID, week)
		} else {
			fmt.Printf("poll %d for %s already exists; nothing sent\n", p.ID, week)
		}
	case "resync":
		ids := []int64{}
		if *weekFlag != "" {
			p, err := findPoll()
			if err != nil {
				return err
			}
			ids = append(ids, p.ID)
		}
		n, err := a.store.RequestSummaryResync(ctx, ids...)
		if err != nil {
			return err
		}
		fmt.Printf("summary re-sync requested for %d poll(s); the running bot updates within 30 s\n", n)
	case "refresh":
		var polls []domain.Poll
		if *weekFlag != "" {
			p, err := findPoll()
			if err != nil {
				return err
			}
			polls = append(polls, *p)
		}
		n, err := a.svc.RefreshMessages(ctx, polls...)
		if err != nil {
			return err
		}
		fmt.Printf("DMs and summary of %d poll(s) queued for re-rendering; the running bot updates within 30 s\n", n)
	case "status":
		return status(ctx, a, cfg.Location)
	case "away":
		return away(ctx, a, sub, *user, *from, *to, *id, cfg.Location)
	case "lang":
		return lang(ctx, a, sub, *user, fs.Arg(0), cfg.Location)
	case "web-user":
		return webUser(ctx, a, sub, *user, *role, cfg.Location)
	case "stats":
		return statsPurge(ctx, a, cfg, sub, *year, *before, *user, *yes, log)
	case "attendance":
		if sub == "verify" {
			return verifyDay(ctx, a, *date, *undo)
		}
		if *weekFlag == "" {
			week = isoweek.Of(time.Now().In(cfg.Location)) // corrections are about the current week
		}
		p, err := findPoll()
		if err != nil {
			return err
		}
		return attendance(ctx, a, p, sub, *user, *days, !*noNotice)
	case "delete-week":
		p, err := findPoll()
		if err != nil {
			return err
		}
		if !*yes {
			return fmt.Errorf("this deletes poll %d (%s) incl. all responses; repeat with --yes", p.ID, week)
		}
		if err := a.store.DeletePoll(ctx, p.ID); err != nil {
			return err
		}
		fmt.Printf("poll %d for %s deleted (Slack messages are left untouched)\n", p.ID, week)
	case "simulate-save":
		if !cfg.DryRun {
			return errors.New("simulate-save is only available with DRY_RUN=true")
		}
		p, err := findPoll()
		if err != nil {
			return err
		}
		part, err := a.store.GetParticipant(ctx, p.ID, *user)
		if err != nil {
			return fmt.Errorf("participant %q: %w", *user, err)
		}
		req := slack.Interaction{TeamID: a.teamID, UserID: *user, PollID: p.ID, ChannelID: part.DMChannelID, MessageTS: part.DMMessageTS}
		if req.Days, err = parseDays(*days); err != nil {
			return err
		}
		a.svc.HandleSave(ctx, req)
		fmt.Println("saved; the running bot updates the summary within 30 s")
	case "simulate-request":
		if !cfg.DryRun {
			return errors.New("simulate-request is only available with DRY_RUN=true")
		}
		p, err := findPoll()
		if err != nil {
			return err
		}
		a.svc.HandleRequestDM(ctx, slack.Interaction{TeamID: a.teamID, UserID: *user, PollID: p.ID,
			ChannelID: p.SummaryChannelID, MessageTS: p.SummaryMessageTS})
		fmt.Println("requested; the reply is logged above, the running bot sends the DM within 30 s")
	}
	return nil
}

// parseDays parses a comma separated list like "mon,wed".
func parseDays(list string) (domain.Days, error) {
	var days domain.Days
	for _, d := range strings.Split(list, ",") {
		if d = strings.ToLower(strings.TrimSpace(d)); d == "" {
			continue
		}
		i, ok := domain.DayIndex(d)
		if !ok {
			return days, fmt.Errorf("unknown day %q (use mon,tue,wed,thu,fri)", d)
		}
		days[i] = true
	}
	return days, nil
}
