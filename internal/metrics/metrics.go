// Package metrics aggregates metrics_daily into daily lines and weekly digests.
package metrics

import (
	"context"
	"time"

	"github.com/lioilsources/Ol1nDevBot/internal/config"
	"github.com/lioilsources/Ol1nDevBot/internal/events"
	"github.com/lioilsources/Ol1nDevBot/internal/store"
	"github.com/lioilsources/Ol1nDevBot/internal/store/db"
)

// Service builds metrics events.
type Service struct {
	St   *store.Store
	Reg  *config.Registry
	Emit events.Emitter
	Loc  *time.Location
}

// Figures returns an app's metrics for [from, to] and the equally long
// period right before it.
func (s *Service) Figures(ctx context.Context, app config.App, from, to time.Time) (events.AppFigures, error) {
	days := int(to.Sub(from).Hours()/24) + 1
	prevTo, prevFrom := from.AddDate(0, 0, -1), from.AddDate(0, 0, -days)
	af := events.AppFigures{AppSlug: app.Slug, AppName: app.Name, Emoji: app.DisplayEmoji(), ByStore: map[string]events.Figures{}}
	cur, err := s.St.SumMetrics(ctx, db.SumMetricsParams{AppSlug: app.Slug, Date: date(from), Date_2: date(to)})
	if err != nil {
		return af, err
	}
	prev, err := s.St.SumMetrics(ctx, db.SumMetricsParams{AppSlug: app.Slug, Date: date(prevFrom), Date_2: date(prevTo)})
	if err != nil {
		return af, err
	}
	af.Current = events.Figures(cur)
	af.Previous = events.Figures(prev)
	byStore, err := s.St.SumMetricsByStore(ctx, db.SumMetricsByStoreParams{AppSlug: app.Slug, Date: date(from), Date_2: date(to)})
	if err != nil {
		return af, err
	}
	for _, r := range byStore {
		af.ByStore[r.Store] = events.Figures{Downloads: r.Downloads, Impressions: r.Impressions, PageViews: r.PageViews, Uninstalls: r.Uninstalls}
	}
	return af, nil
}

// Last returns figures for the last n full days (ending yesterday).
func (s *Service) Last(ctx context.Context, app config.App, days int) (events.AppFigures, error) {
	yesterday := s.today().AddDate(0, 0, -1)
	return s.Figures(ctx, app, yesterday.AddDate(0, 0, -(days-1)), yesterday)
}

// Daily emits the short line for yesterday.
func (s *Service) Daily(ctx context.Context) error {
	y := s.today().AddDate(0, 0, -1)
	return s.emit(ctx, "daily", y, y)
}

// Weekly emits the digest for the previous Monday–Sunday.
func (s *Service) Weekly(ctx context.Context) error {
	today := s.today()
	sinceMonday := (int(today.Weekday()) + 6) % 7
	monday := today.AddDate(0, 0, -sinceMonday-7)
	return s.emit(ctx, "weekly", monday, monday.AddDate(0, 0, 6))
}

func (s *Service) emit(ctx context.Context, period string, from, to time.Time) error {
	ev := events.MetricsEvent{Period: period, From: date(from), To: date(to)}
	hasData := false
	for _, app := range s.Reg.Apps() {
		if app.IOS == nil && app.Android == nil {
			continue // backend-only apps have no store metrics
		}
		af, err := s.Figures(ctx, app, from, to)
		if err != nil {
			return err
		}
		hasData = hasData || len(af.ByStore) > 0
		ev.Apps = append(ev.Apps, af)
	}
	if !hasData {
		return nil // no store integrations configured / no data yet: stay quiet
	}
	_, err := s.Emit.Emit(ctx, ev)
	return err
}

func (s *Service) today() time.Time {
	n := store.Now().In(s.Loc)
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, s.Loc)
}

func date(t time.Time) string { return t.Format(time.DateOnly) }
