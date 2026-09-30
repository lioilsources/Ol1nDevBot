package milestones

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/lioilsources/Ol1nDevBot/internal/config"
	"github.com/lioilsources/Ol1nDevBot/internal/events"
	"github.com/lioilsources/Ol1nDevBot/internal/outbox"
	"github.com/lioilsources/Ol1nDevBot/internal/store"
	"github.com/lioilsources/Ol1nDevBot/internal/store/db"
)

func v(store, version string, announced int64) db.AppVersion {
	return db.AppVersion{Store: store, Version: version, Announced: announced}
}

const ios, play = events.StoreAppStore, events.StorePlayStore

func TestRules(t *testing.T) {
	cases := []struct {
		name  string
		key   string
		facts Facts
		want  int
	}{
		{"first release", "first_release", Facts{Versions: []db.AppVersion{v(ios, "0.9", 1)}}, 1},
		{"no versions", "first_release", Facts{}, 0},
		{"launched app history is silent", "first_release", Facts{Versions: []db.AppVersion{v(ios, "2.0", 0), v(ios, "2.1", 1)}}, 0},
		{"v1_0 reached", "v1_0", Facts{Versions: []db.AppVersion{v(ios, "0.9", 1), v(ios, "1.0.0", 1)}}, 1},
		{"v1_0 with v prefix", "v1_0", Facts{Versions: []db.AppVersion{v(play, "v1.2", 1)}}, 1},
		{"v1_0 not yet", "v1_0", Facts{Versions: []db.AppVersion{v(ios, "0.12.3", 1)}}, 0},
		{"v1_0 pre-existing", "v1_0", Facts{Versions: []db.AppVersion{v(ios, "1.3", 0), v(ios, "1.4", 1)}}, 0},
		{"both stores", "both_stores", Facts{Versions: []db.AppVersion{v(ios, "1.0", 0), v(play, "1.0", 1)}}, 1},
		{"both stores pre-existing", "both_stores", Facts{Versions: []db.AppVersion{v(ios, "1.0", 0), v(play, "1.0", 0)}}, 0},
		{"one store", "both_stores", Facts{Versions: []db.AppVersion{v(ios, "1.0", 1)}}, 0},
		{"downloads reached", "downloads_1000", Facts{TotalDownloads: 1000}, 1},
		{"downloads not yet", "downloads_1000", Facts{TotalDownloads: 999}, 0},
		{"growth 2x", "weekly_growth_2x", Facts{WeekDownloads: 100, PrevWeekDownloads: 50}, 1},
		{"growth below minimum", "weekly_growth_2x", Facts{WeekDownloads: 40, PrevWeekDownloads: 10}, 0},
		{"growth from zero ignored", "weekly_growth_2x", Facts{WeekDownloads: 400, PrevWeekDownloads: 0}, 0},
		{"growth too small", "weekly_growth_2x", Facts{WeekDownloads: 99, PrevWeekDownloads: 50}, 0},
		{"deploys per service", "deploys_100", Facts{SuccessfulDeploys: map[string]int64{"a": 100, "b": 99}}, 1},
	}
	for _, c := range cases {
		if got := Evaluate(c.key, c.facts); len(got) != c.want {
			t.Errorf("%s: got %d hits (%+v), want %d", c.name, len(got), got, c.want)
		}
	}
	if h := Evaluate("deploys_100", Facts{SuccessfulDeploys: map[string]int64{"api": 150}}); h[0].Key != "deploys_100:api" {
		t.Errorf("deploys key: %q", h[0].Key)
	}
	if h := Evaluate("downloads_10000", Facts{TotalDownloads: 12345}); h[0].Title != "10 000 stažení!" {
		t.Errorf("title: %q", h[0].Title)
	}
}

type recorder struct{ evs []events.Event }

func (r *recorder) Emit(_ context.Context, ev events.Event) (bool, error) {
	r.evs = append(r.evs, ev)
	return true, nil
}

func TestEngineAnnouncesOnceWithNewApp(t *testing.T) {
	ctx := context.Background()
	st, _ := store.Open(ctx, ":memory:")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	app := config.App{Slug: "puff", Name: "Puff", IOS: &config.IOS{BundleID: "x", ASCAppID: "123"}, Android: &config.Android{Package: "com.x"}, DownloadsBaseline: 90}
	reg := config.NewStaticRegistry(&config.AppsFile{Announce: config.Announce{EveryoneOnNewApp: true}, Apps: []config.App{app}})
	rec := &recorder{}
	e := &Engine{St: st, Reg: reg, Emit: rec, Loc: time.UTC, Log: log}

	st.InsertAppVersion(ctx, db.InsertAppVersionParams{Store: ios, AppSlug: "puff", Version: "1.0.0", SeenAt: 1, Announced: 1})
	today := time.Now().UTC()
	st.UpsertMetricsDaily(ctx, db.UpsertMetricsDailyParams{AppSlug: "puff", Store: ios, Date: today.AddDate(0, 0, -2).Format(time.DateOnly), Downloads: 15, UpdatedAt: 1})

	trigger := &events.StoreVersionEvent{AppSlug: "puff", Store: ios, StoreURL: "https://apps.apple.com/app/puff/id123", IconURL: "icon"}
	if err := e.Evaluate(ctx, app, trigger); err != nil {
		t.Fatal(err)
	}
	keys := map[string]bool{}
	var newApp *events.NewAppEvent
	for _, ev := range rec.evs {
		switch x := ev.(type) {
		case events.MilestoneEvent:
			keys[x.MilestoneKey] = true
		case events.NewAppEvent:
			newApp = &x
		}
	}
	// first_release + v1_0 + downloads_100 (90 baseline + 15).
	if !keys["first_release"] || !keys["v1_0"] || !keys["downloads_100"] || len(keys) != 3 {
		t.Fatalf("milestones: %v", keys)
	}
	if newApp == nil || !newApp.Everyone || newApp.AppStoreURL != trigger.StoreURL || newApp.PlayStoreURL != "" || newApp.IconURL != "icon" {
		t.Fatalf("new app event: %+v", newApp)
	}

	n := len(rec.evs)
	e.Evaluate(ctx, app, nil)
	if len(rec.evs) != n {
		t.Fatal("milestones announced twice")
	}
}

func TestEngineSubscribedViaOutbox(t *testing.T) {
	ctx := context.Background()
	st, _ := store.Open(ctx, ":memory:")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := config.NewStaticRegistry(&config.AppsFile{Apps: []config.App{{Slug: "a", Name: "A", Milestones: []string{"first_release"}}}})
	ob := outbox.New(st, log)
	e := &Engine{St: st, Reg: reg, Emit: ob, Loc: time.UTC, Log: log}
	ob.Subscribe(e.OnEvent)

	st.InsertAppVersion(ctx, db.InsertAppVersionParams{Store: play, AppSlug: "a", Version: "0.1", SeenAt: 1, Announced: 1})
	ob.Emit(ctx, events.StoreVersionEvent{AppSlug: "a", AppName: "A", Store: play, Version: "0.1"})
	ms, _ := st.ListMilestones(ctx, "a")
	if len(ms) != 1 || ms[0].Key != "first_release" {
		t.Fatalf("milestones: %+v", ms)
	}
	stats, _ := st.OutboxStats(ctx)
	// version→releases, milestone→announcements+releases, new app→announcements
	if stats.Pending != 4 {
		t.Fatalf("outbox rows: %+v", stats)
	}
}
