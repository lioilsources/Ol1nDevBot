package milestones

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/lioilsources/Ol1nDevBot/internal/config"
	"github.com/lioilsources/Ol1nDevBot/internal/events"
	"github.com/lioilsources/Ol1nDevBot/internal/sources/versions"
	"github.com/lioilsources/Ol1nDevBot/internal/store"
	"github.com/lioilsources/Ol1nDevBot/internal/store/db"
)

// Engine evaluates milestones after version, deploy and metrics writes.
type Engine struct {
	St   *store.Store
	Reg  *config.Registry
	Emit events.Emitter
	Loc  *time.Location
	Log  *slog.Logger
}

// OnEvent is an outbox subscriber: re-evaluate the affected app.
func (e *Engine) OnEvent(ctx context.Context, ev events.Event) {
	var slug string
	var trigger *events.StoreVersionEvent
	switch x := ev.(type) {
	case events.StoreVersionEvent:
		slug, trigger = x.AppSlug, &x
	case events.DeployEvent:
		slug = x.AppSlug
	default:
		return
	}
	app, ok := e.Reg.App(slug)
	if !ok {
		return
	}
	if err := e.Evaluate(ctx, app, trigger); err != nil {
		e.Log.Error("milestone evaluation", "app", slug, "err", err)
	}
}

// EvaluateAll re-evaluates every app (after metrics imports).
func (e *Engine) EvaluateAll(ctx context.Context) error {
	for _, app := range e.Reg.Apps() {
		if err := e.Evaluate(ctx, app, nil); err != nil {
			return fmt.Errorf("%s: %w", app.Slug, err)
		}
	}
	return nil
}

// Facts gathers rule inputs for an app.
func (e *Engine) Facts(ctx context.Context, app config.App) (Facts, error) {
	var f Facts
	var err error
	if f.Versions, err = e.St.ListAppVersions(ctx, app.Slug); err != nil {
		return f, err
	}
	total, err := e.St.TotalDownloads(ctx, app.Slug)
	if err != nil {
		return f, err
	}
	f.TotalDownloads = total + app.DownloadsBaseline
	now := store.Now().In(e.Loc)
	day := func(d int) string { return now.AddDate(0, 0, -d).Format(time.DateOnly) }
	week, err := e.St.SumMetrics(ctx, db.SumMetricsParams{AppSlug: app.Slug, Date: day(7), Date_2: day(1)})
	if err != nil {
		return f, err
	}
	prev, err := e.St.SumMetrics(ctx, db.SumMetricsParams{AppSlug: app.Slug, Date: day(14), Date_2: day(8)})
	if err != nil {
		return f, err
	}
	f.WeekDownloads, f.PrevWeekDownloads = week.Downloads, prev.Downloads
	f.SuccessfulDeploys = map[string]int64{}
	for _, svc := range app.Services {
		n, err := e.St.CountSuccessfulDeploys(ctx, svc)
		if err != nil {
			return f, err
		}
		f.SuccessfulDeploys[svc] = n
	}
	return f, nil
}

// Evaluate checks all configured milestones of one app. trigger (optional) is
// the store version that caused the evaluation and supplies store links.
func (e *Engine) Evaluate(ctx context.Context, app config.App, trigger *events.StoreVersionEvent) error {
	facts, err := e.Facts(ctx, app)
	if err != nil {
		return err
	}
	file := e.Reg.File()
	for _, key := range app.MilestoneKeys() {
		for _, hit := range Evaluate(key, facts) {
			n, err := e.St.InsertMilestone(ctx, db.InsertMilestoneParams{AppSlug: app.Slug, Key: hit.Key, ReachedAt: store.Unix(), Announced: 1})
			if err != nil {
				return err
			}
			if n == 0 {
				continue // already reached earlier
			}
			e.Log.Info("milestone reached", "app", app.Slug, "key", hit.Key)
			if _, err := e.Emit.Emit(ctx, events.MilestoneEvent{
				AppSlug: app.Slug, AppName: app.Name, Emoji: app.DisplayEmoji(), MilestoneKey: hit.Key,
				Title: hit.Title, Detail: hit.Detail, Everyone: file.Announce.EveryoneOnMilestone,
			}); err != nil {
				return err
			}
			if hit.Key == "first_release" {
				if err := e.announceNewApp(ctx, app, trigger, file); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (e *Engine) announceNewApp(ctx context.Context, app config.App, trigger *events.StoreVersionEvent, file config.AppsFile) error {
	ev := events.NewAppEvent{
		AppSlug: app.Slug, AppName: app.Name, Emoji: app.DisplayEmoji(),
		AppStoreURL: versions.AppStoreURL(app), Everyone: file.Announce.EveryoneOnNewApp,
	}
	if trigger != nil {
		ev.IconURL = trigger.IconURL
		ev.Description = trigger.Description
		if trigger.Store == events.StoreAppStore && trigger.StoreURL != "" {
			ev.AppStoreURL = trigger.StoreURL
		}
	}
	// Only link stores the app is actually live in.
	live := map[string]bool{}
	vs, err := e.St.ListAppVersions(ctx, app.Slug)
	if err != nil {
		return err
	}
	for _, v := range vs {
		live[v.Store] = true
	}
	if !live[events.StoreAppStore] {
		ev.AppStoreURL = ""
	}
	if live[events.StorePlayStore] {
		ev.PlayStoreURL = versions.PlayStoreURL(app)
	}
	_, err = e.Emit.Emit(ctx, ev)
	return err
}
