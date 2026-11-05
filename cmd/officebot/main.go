// Command officebot runs the Slack office attendance bot.
//
//	officebot [run]                                              start the bot (default)
//	officebot start-week [--week 2026-W43]                       start a week's poll now (idempotent)
//	officebot resync [--week 2026-W43]                           force a re-sync of the channel summary
//	officebot refresh [--week 2026-W43]                          re-render the DMs and the summary (e.g. after an update)
//	officebot status                                             show polls and outbox state
//	officebot list-users [--channel C] [--all]                   list user IDs, e.g. to fill SLACK_USER_IDS
//	officebot away list [--user U]                               list upcoming time off
//	officebot away add --user U --from D --to D                  add time off (YYYY-MM-DD, days included)
//	officebot away remove --user U --from D --to D | --id N      remove days or one entry of time off
//	officebot lang list                                          list the language choices of people
//	officebot lang set --user U <language>                       set a person's language, e.g. de or pirate_en
//	officebot lang reset --user U                                back to the team default
//	officebot attendance verify --date D [--undo]                mark a day as checked: statuses match actual attendance
//	officebot stats purge --year Y | --before D | --user U [--yes] delete daily status (monthly figures are kept)
//	officebot web-user list                                      list who may log in to the web interface
//	officebot web-user add --user U [--role admin|viewer]        grant web access (default role: admin)
//	officebot web-user set-role --user U --role admin|viewer     change the role
//	officebot web-user remove --user U                           revoke web access and end all sessions
//	officebot delete-week --week W --yes                         delete a week's poll from the database (testing)
//	officebot simulate-save --user U --days mon,wed [--week W]   dry-run only: answer as a user
//	officebot simulate-request --user U [--week W]               dry-run only: click "Abfrage an mich senden"
//	officebot migrate                                            apply database migrations and exit
//	officebot backup /data/backup.db                             write a consistent database copy (safe while running)
//	officebot healthcheck                                        container health probe
//	officebot version                                            print the version
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"
	_ "time/tzdata" // time zones without OS tzdata (distroless)

	goslack "github.com/slack-go/slack"

	"officebot/internal/config"
	"officebot/internal/domain"
	"officebot/internal/i18n"
	"officebot/internal/isoweek"
	"officebot/internal/participants"
	"officebot/internal/poll"
	"officebot/internal/slack"
	"officebot/internal/stats"
	"officebot/internal/storage"
	"officebot/internal/summary"
	"officebot/internal/web"
	"officebot/internal/worker"
)

// version is set at build time (-ldflags "-X main.version=1.2.3").
var version = "dev"

func main() {
	cmd := "run"
	args := os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	if cmd == "healthcheck" {
		os.Exit(healthcheck())
	}
	if cmd == "version" {
		fmt.Println(version)
		return
	}

	load := config.Load
	if cmd == "list-users" {
		load = config.LoadForSetup // helps to find the channel and user IDs
	}
	cfg, err := load()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch cmd {
	case "run":
		err = run(ctx, cfg, log)
	case "start-week", "resync", "refresh", "status", "delete-week", "simulate-save", "simulate-request", "migrate", "backup", "list-users", "away", "lang", "attendance", "web-user", "stats":
		err = cli(ctx, cmd, args, cfg, log)
	default:
		err = fmt.Errorf("unknown command %q", cmd)
	}
	if err != nil {
		log.Error("exiting", "err", err)
		os.Exit(1)
	}
}

// app bundles the wired components.
type app struct {
	store   *storage.Store
	api     slack.API
	raw     *goslack.Client
	svc     *poll.Service
	stats   *stats.Service
	people  *participants.Service
	teamID  string
	botUser string
}

// connect creates the Slack client and identifies the workspace. It needs no
// database, so read-only setup tools (list-users) can use it on their own.
func connect(ctx context.Context, cfg *config.Config, log *slog.Logger) (*app, error) {
	a := &app{}
	if cfg.DryRun {
		log.Warn("DRY RUN: no messages are sent to Slack")
		a.api = &slack.DryRun{Log: log}
	} else {
		a.raw = goslack.New(cfg.SlackBotToken, goslack.OptionAppLevelToken(cfg.SlackAppToken))
		a.api = slack.NewClient(a.raw)
	}

	// Slack may be unreachable at boot; retry auth.test with backoff.
	for attempt := 1; ; attempt++ {
		var err error
		a.teamID, a.botUser, err = a.api.AuthTest(ctx)
		if err == nil {
			break
		}
		if slack.IsPermanent(err) || ctx.Err() != nil {
			return nil, fmt.Errorf("slack auth.test (check SLACK_BOT_TOKEN): %w", err)
		}
		delay := min(time.Duration(attempt)*5*time.Second, time.Minute)
		log.Warn("slack not reachable, retrying", "in", delay, "err", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
	}
	log.Info("connected to slack workspace", "team", a.teamID)

	a.people = participants.New(a.api, participants.Source{
		ChannelID: cfg.ParticipantsChannelID, UserIDs: cfg.UserIDs, Exclude: cfg.ExcludeUserIDs,
	}, a.teamID, a.botUser, log)
	return a, nil
}

// setup connects to Slack and opens the database.
func setup(ctx context.Context, cfg *config.Config, log *slog.Logger) (*app, error) {
	a, err := connect(ctx, cfg, log)
	if err != nil {
		return nil, err
	}
	if a.store, err = storage.Open(ctx, cfg.DatabasePath); err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	a.svc = poll.NewService(a.store, a.api, a.people, cfg, a.teamID, nil, nil, log)
	a.stats = stats.New(a.store, cfg, a.teamID, log.With("component", "stats"))
	a.svc.SyncStats = a.stats.SyncPolls
	return a, nil
}

func run(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	log.Info("starting officebot", "version", version, "config", cfg)
	a, err := setup(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer a.store.Close()

	outbox := worker.NewOutbox(a.store, a.svc, log)
	updater := summary.NewUpdater(a.store, a.api, summary.Renderer{Loc: cfg.Location, MaxNames: cfg.SummaryMaxNames, T: cfg.Translator, ProjectLink: cfg.SummaryProjectLink}, log)
	a.svc.SetNotifiers(outbox, updater)
	// After an update, existing messages show the new texts and layout at
	// once, not only with the next poll. Not fatal: the next start retries.
	if cfg.RefreshOnUpdate {
		if err := a.svc.RefreshOnNewVersion(ctx, version); err != nil {
			log.Error("re-render messages for the new version", "err", err)
		}
	}
	sched := poll.NewScheduler(a.svc, log)
	sched.Extra = append(sched.Extra, poll.Step{Name: "statistics", Fn: a.stats.Sync})

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	var wg sync.WaitGroup
	goRun := func(fn func()) { wg.Add(1); go func() { defer wg.Done(); fn() }() }

	connected := func() bool { return true }
	if !cfg.DryRun {
		socket := slack.NewSocket(a.raw, a.svc, log)
		connected = socket.Connected
		goRun(func() {
			if err := socket.Run(ctx); err != nil {
				cancel(err)
			}
		})
	}
	goRun(func() { outbox.Run(ctx) })
	goRun(func() { updater.Run(ctx) })
	goRun(func() { sched.Run(ctx) })
	goRun(func() { healthWriter(ctx, cfg.HealthFile, a.store, connected, cancel, log) })
	if cfg.WebEnabled() {
		srv, err := web.New(web.Deps{Store: a.store, Svc: a.svc, Stats: a.stats, People: a.people, Slack: a.api, Cfg: cfg, TeamID: a.teamID, Log: log.With("component", "web")})
		if err != nil {
			cancel(err)
		} else {
			goRun(func() {
				if err := srv.Run(ctx); err != nil {
					cancel(err)
				}
			})
		}
	}

	<-ctx.Done()
	log.Info("shutting down")
	wg.Wait()
	_ = os.Remove(cfg.HealthFile)
	if err := context.Cause(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// ------------------------------------------------------------------ health

type health struct {
	Time       int64 `json:"time"`
	Socket     bool  `json:"socket"`
	DownSince  int64 `json:"down_since,omitempty"`
	DatabaseOK bool  `json:"database_ok"`
}

const (
	healthInterval = 15 * time.Second
	socketGrace    = 2 * time.Minute  // healthcheck tolerates short reconnects
	socketGiveUp   = 15 * time.Minute // then exit so the restart policy kicks in
)

func healthWriter(ctx context.Context, path string, store *storage.Store, connected func() bool, cancel context.CancelCauseFunc, log *slog.Logger) {
	var downSince time.Time
	t := time.NewTicker(healthInterval)
	defer t.Stop()
	for {
		now := time.Now()
		h := health{Time: now.Unix(), Socket: connected(), DatabaseOK: store.Ping(ctx) == nil}
		if h.Socket {
			downSince = time.Time{}
		} else {
			if downSince.IsZero() {
				downSince = now
			}
			h.DownSince = downSince.Unix()
			if now.Sub(downSince) > socketGiveUp {
				cancel(fmt.Errorf("socket mode disconnected for more than %s", socketGiveUp))
				return
			}
		}
		if b, err := json.Marshal(h); err == nil {
			if err := os.WriteFile(path, b, 0o600); err != nil {
				log.Warn("write health file", "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func healthcheck() int {
	path := os.Getenv("HEALTH_FILE")
	if path == "" {
		path = "/tmp/officebot.health"
	}
	b, err := os.ReadFile(path)
	if err != nil {
		fmt.Println("unhealthy: no health file:", err)
		return 1
	}
	var h health
	if err := json.Unmarshal(b, &h); err != nil {
		fmt.Println("unhealthy: bad health file")
		return 1
	}
	now := time.Now()
	switch {
	case now.Sub(time.Unix(h.Time, 0)) > 4*healthInterval:
		fmt.Println("unhealthy: process not responding")
		return 1
	case !h.DatabaseOK:
		fmt.Println("unhealthy: database not available")
		return 1
	case !h.Socket && now.Sub(time.Unix(h.DownSince, 0)) > socketGrace:
		fmt.Println("unhealthy: slack socket mode disconnected since", time.Unix(h.DownSince, 0).Format(time.RFC3339))
		return 1
	}
	fmt.Println("healthy")
	return 0
}

// --------------------------------------------------------------------- CLI

func cli(ctx context.Context, cmd string, args []string, cfg *config.Config, log *slog.Logger) error {
	var sub string
	if cmd == "away" || cmd == "lang" || cmd == "attendance" || cmd == "web-user" || cmd == "stats" {
		if len(args) == 0 || strings.HasPrefix(args[0], "-") {
			switch cmd {
			case "stats":
				return errors.New("usage: officebot stats purge --year Y | --before YYYY-MM-DD | --user U [--yes]")
			case "web-user":
				return errors.New("usage: officebot web-user list|add|set-role|remove [--user U] [--role admin|viewer]")
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
	role := fs.String("role", "", "role in the web interface: admin or viewer (web-user)")
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
			return fmt.Errorf("unknown role %q (use admin or viewer)", role)
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
			return errors.New("usage: officebot web-user set-role --user U --role admin|viewer")
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
