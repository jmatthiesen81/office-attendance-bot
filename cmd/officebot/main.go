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
//	officebot web-user add --user U [--role R]                   grant web access (R: admin, analyst, viewer; default admin)
//	officebot web-user set-role --user U --role R                change the role
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
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
	_ "time/tzdata" // time zones without OS tzdata (distroless)

	goslack "github.com/slack-go/slack"

	"officebot/internal/config"
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
