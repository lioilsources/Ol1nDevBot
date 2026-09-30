// Command ol1nbot publishes deploys, store releases, metrics, the weekly
// GitHub changelog and milestones to the Ol1n Discord server.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // distroless has no zoneinfo

	"github.com/bwmarrin/discordgo"

	"github.com/lioilsources/Ol1nDevBot/internal/changelog"
	"github.com/lioilsources/Ol1nDevBot/internal/config"
	"github.com/lioilsources/Ol1nDevBot/internal/discord"
	"github.com/lioilsources/Ol1nDevBot/internal/events"
	"github.com/lioilsources/Ol1nDevBot/internal/health"
	"github.com/lioilsources/Ol1nDevBot/internal/llm"
	"github.com/lioilsources/Ol1nDevBot/internal/metrics"
	"github.com/lioilsources/Ol1nDevBot/internal/milestones"
	"github.com/lioilsources/Ol1nDevBot/internal/outbox"
	"github.com/lioilsources/Ol1nDevBot/internal/scheduler"
	"github.com/lioilsources/Ol1nDevBot/internal/sources/apple"
	"github.com/lioilsources/Ol1nDevBot/internal/sources/deployhook"
	"github.com/lioilsources/Ol1nDevBot/internal/sources/github"
	"github.com/lioilsources/Ol1nDevBot/internal/sources/gplay"
	"github.com/lioilsources/Ol1nDevBot/internal/sources/versions"
	"github.com/lioilsources/Ol1nDevBot/internal/store"
)

// version is set with -ldflags "-X main.version=…".
var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

// healthcheck lets the distroless image probe itself (no curl available).
func healthcheck() int {
	addr := os.Getenv("HTTP_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	c := http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + addr + "/healthz")
	if err != nil || resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

func run() error {
	env, err := config.LoadEnv()
	if err != nil {
		return err
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: env.LogLevel}))
	slog.SetDefault(log)
	loc, err := time.LoadLocation(env.TZ)
	if err != nil {
		return fmt.Errorf("TZ: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, env.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	reg := config.NewRegistry(env.AppsYAML, func(ctx context.Context) ([]config.App, error) {
		rows, err := st.ListAppOverrides(ctx)
		if err != nil {
			return nil, err
		}
		var apps []config.App
		for _, r := range rows {
			a, err := config.UnmarshalAppYAML(r.Yaml)
			if err != nil {
				log.Warn("skipping invalid app override", "slug", r.Slug, "err", err)
				continue
			}
			apps = append(apps, a)
		}
		return apps, nil
	})
	if err := reg.Reload(ctx); err != nil {
		return err
	}
	guildID := env.DiscordGuildID
	if guildID == "" {
		guildID = reg.File().GuildID
	}
	if guildID == "" {
		return errors.New("DISCORD_GUILD_ID (or guild_id in apps.yaml) is required")
	}

	ob := outbox.New(st, log)
	tracker := health.NewTracker(st, ob, log)

	// Discord: Gateway with GUILDS intent only (slash commands); no message content.
	sess, err := discordgo.New("Bot " + env.DiscordBotToken)
	if err != nil {
		return err
	}
	sess.Identify.Intents = discordgo.IntentsGuilds
	if err := sess.Open(); err != nil {
		return fmt.Errorf("discord gateway: %w", err)
	}
	defer sess.Close()
	log.Info("connected to discord", "guild", guildID, "version", version)

	reconciler := discord.NewReconciler(sess, st, guildID, log)
	syncFn := func(ctx context.Context) ([]string, error) {
		if err := reg.Reload(ctx); err != nil {
			return nil, err
		}
		diff, err := reconciler.Reconcile(ctx, reg.Apps())
		if len(diff) > 0 {
			outbox.BotLog(ctx, ob, log, "info", "Reconcile", "• "+strings.Join(diff, "\n• "))
		}
		return diff, err
	}
	if err := tracker.Run(ctx, "reconcile", func(ctx context.Context) error { _, err := syncFn(ctx); return err }); err != nil {
		log.Error("initial reconcile failed; publishing will retry once channels exist", "err", err)
	}

	board := discord.NewStatusBoard(sess, st, reg, loc, log)
	updateBoard := func(ctx context.Context) error { return board.Update(ctx) }

	pub := discord.NewPublisher(sess, st, guildID, loc, ob.Notify(), func() string { return reg.File().Discord.OpsRole }, log)
	pub.OnSent = func(ctx context.Context, ev events.Event, target string) {
		if target != ev.Targets()[0] {
			return
		}
		switch ev.(type) {
		case events.StoreVersionEvent, events.DeployEvent:
			if err := board.Update(ctx); err != nil {
				log.Warn("status board update", "err", err)
			}
		}
	}

	engine := &milestones.Engine{St: st, Reg: reg, Emit: ob, Loc: loc, Log: log}
	ob.Subscribe(engine.OnEvent)
	recorder := &versions.Recorder{St: st, Emit: ob, Log: log}
	metricsSvc := &metrics.Service{St: st, Reg: reg, Emit: ob, Loc: loc}

	sched := scheduler.New(loc, tracker, log)
	add := func(j scheduler.Job) {
		if err := sched.Add(ctx, j); err != nil {
			log.Error("schedule job", "job", j.Name, "err", err)
		}
	}
	thenMilestones := func(fn func(context.Context) error) func(context.Context) error {
		return func(ctx context.Context) error {
			err := fn(ctx)
			if mErr := engine.EvaluateAll(ctx); mErr != nil {
				log.Error("milestones", "err", mErr)
			}
			return err
		}
	}

	add(scheduler.Job{Name: "reconcile", Spec: "@every 6h", Fn: func(ctx context.Context) error { _, err := syncFn(ctx); return err }})
	add(scheduler.Job{Name: "status-board", Spec: "5 * * * *", RunAtStart: true, Fn: updateBoard})

	lookup := &apple.LookupPoller{Client: apple.NewLookupClient(), Apps: reg, Recorder: recorder, Log: log}
	add(scheduler.Job{Name: "apple-lookup", Spec: "@every 30m", RunAtStart: true, Fn: lookup.Poll})

	if env.ASCEnabled() {
		asc, err := apple.NewASC(env.ASCKeyID, env.ASCIssuerID, env.ASCPrivateKeyPath)
		if err != nil {
			return err
		}
		vp := &apple.VersionsPoller{ASC: asc, Apps: reg, Recorder: recorder, Log: log}
		add(scheduler.Job{Name: "asc-versions", Spec: "@every 2h", RunAtStart: true, Fn: vp.Poll})
		if env.ASCVendorNumber != "" {
			sp := &apple.SalesPoller{ASC: asc, Vendor: env.ASCVendorNumber, Apps: reg, St: st, Loc: loc, Log: log}
			add(scheduler.Job{Name: "asc-sales", Spec: "0 8 * * *", RunAtStart: true, Fn: thenMilestones(sp.Poll)})
		} else {
			log.Info("ASC_VENDOR_NUMBER not set: App Store sales disabled")
		}
	} else {
		log.Info("ASC credentials not set: App Store Connect pollers disabled")
	}

	if env.GPlayEnabled() {
		gp, err := gplay.NewClient(ctx, env.GPlaySAJSONPath)
		if err != nil {
			return err
		}
		tp := &gplay.TracksPoller{Client: gp, Apps: reg, Recorder: recorder, Log: log}
		add(scheduler.Job{Name: "play-tracks", Spec: "@every 30m", RunAtStart: true, Fn: tp.Poll})
		if env.GPlayReportsBucket != "" {
			sp := &gplay.StatsPoller{Client: gp, Bucket: env.GPlayReportsBucket, Apps: reg, St: st, Loc: loc, Log: log}
			add(scheduler.Job{Name: "play-stats", Spec: "15 8 * * *", RunAtStart: true, Fn: thenMilestones(sp.Poll)})
		} else {
			log.Info("GPLAY_REPORTS_BUCKET not set: Play install stats disabled")
		}
	} else {
		log.Info("GPLAY_SA_JSON_PATH not set: Google Play pollers disabled")
	}

	var changelogNow func(context.Context, string) (events.ChangelogEvent, error)
	if env.GitHubToken != "" {
		var summarizer changelog.Summarizer
		if env.LLMEnabled() {
			summarizer = llm.New(env.LLMBaseURL, env.LLMAPIKey, env.LLMModel)
		} else {
			log.Info("LLM_MODEL not set: changelog uses the raw commit list")
		}
		cl := &changelog.Service{GH: github.New(env.GitHubToken), LLM: summarizer, St: st, Reg: reg, Emit: ob, Loc: loc, Log: log}
		add(scheduler.Job{Name: "github-changelog", Spec: "0 9 * * 1", Timeout: 20 * time.Minute, Fn: cl.Weekly})
		changelogNow = cl.Now
	} else {
		log.Info("GITHUB_TOKEN not set: changelog disabled")
	}

	add(scheduler.Job{Name: "metrics-daily", Spec: "45 8 * * *", Fn: func(ctx context.Context) error {
		if err := metricsSvc.Daily(ctx); err != nil {
			return err
		}
		return updateBoard(ctx)
	}})
	add(scheduler.Job{Name: "metrics-weekly", Spec: "30 9 * * 1", Fn: metricsSvc.Weekly})

	cmds := discord.NewCommands(ctx, sess, env.DiscordAppID, guildID, discord.CommandDeps{
		Reg: reg, St: st, Emit: ob, Loc: loc, Log: log,
		Sync:         syncFn,
		ChangelogNow: changelogNow,
		Metrics:      metricsSvc.Last,
		Status: func(ctx context.Context) (string, error) {
			s, err := tracker.Summary(ctx, loc)
			return fmt.Sprintf("Verze `%s`, %d appek\n\n%s", version, len(reg.Apps()), s), err
		},
	})
	sess.AddHandler(cmds.Handle)
	if err := cmds.Register(); err != nil {
		return fmt.Errorf("register slash commands: %w", err)
	}

	srv := &http.Server{Addr: env.HTTPAddr, Handler: routes(env, st, reg, ob, log), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		log.Info("http listening", "addr", env.HTTPAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server", "err", err)
			stop()
		}
	}()

	go pub.Run(ctx)
	sched.Start(ctx, 10*time.Second)
	outbox.BotLog(ctx, ob, log, "ok", "Ol1nBot nastartoval", fmt.Sprintf("Verze `%s`, %d appek.", version, len(reg.Apps())))

	<-ctx.Done()
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	sched.Stop()
	return nil
}

func routes(env config.Env, st *store.Store, reg *config.Registry, ob *outbox.Outbox, log *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := st.DB.PingContext(r.Context()); err != nil {
			http.Error(w, "db: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok"))
	})
	var hook http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "deploy hook disabled: DEPLOY_HOOK_SECRET not set", http.StatusServiceUnavailable)
	})
	if env.DeployHookSecret != "" {
		hook = &deployhook.Handler{Secret: []byte(env.DeployHookSecret), Store: st, Apps: reg, Emit: ob, Log: log}
	}
	// Caddy may or may not strip the /discordbot prefix; accept both.
	mux.Handle("POST /discordbot/deploy", hook)
	mux.Handle("POST /deploy", hook)
	return mux
}
