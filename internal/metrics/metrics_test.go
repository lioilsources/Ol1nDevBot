package metrics

import (
	"context"
	"testing"
	"time"

	"github.com/lioilsources/Ol1nDevBot/internal/config"
	"github.com/lioilsources/Ol1nDevBot/internal/events"
	"github.com/lioilsources/Ol1nDevBot/internal/store"
	"github.com/lioilsources/Ol1nDevBot/internal/store/db"
)

type rec struct{ evs []events.Event }

func (r *rec) Emit(_ context.Context, ev events.Event) (bool, error) {
	r.evs = append(r.evs, ev)
	return true, nil
}

func TestWeeklyDigestAndQuietWithoutData(t *testing.T) {
	ctx := context.Background()
	loc, _ := time.LoadLocation("Europe/Prague")
	store.Now = func() time.Time { return time.Date(2026, 9, 28, 9, 30, 0, 0, loc) } // Monday
	t.Cleanup(func() { store.Now = time.Now })

	st, _ := store.Open(ctx, ":memory:")
	reg := config.NewStaticRegistry(&config.AppsFile{Apps: []config.App{
		{Slug: "s", Name: "S", IOS: &config.IOS{BundleID: "b"}, Android: &config.Android{Package: "p"}},
		{Slug: "backend", Name: "Backend only"},
	}})
	r := &rec{}
	svc := &Service{St: st, Reg: reg, Emit: r, Loc: loc}

	if err := svc.Weekly(ctx); err != nil || len(r.evs) != 0 {
		t.Fatalf("no data should mean no digest: %v %d", err, len(r.evs))
	}

	put := func(storeID, date string, dl, pv int64) {
		st.UpsertMetricsDaily(ctx, db.UpsertMetricsDailyParams{AppSlug: "s", Store: storeID, Date: date, Downloads: dl, UpdatedAt: 1})
		if pv > 0 {
			st.UpsertStorePerformance(ctx, db.UpsertStorePerformanceParams{AppSlug: "s", Store: storeID, Date: date, PageViews: pv, UpdatedAt: 1})
		}
	}
	put(events.StoreAppStore, "2026-09-21", 10, 0)   // Monday of last week
	put(events.StorePlayStore, "2026-09-27", 5, 100) // Sunday of last week
	put(events.StoreAppStore, "2026-09-14", 3, 0)    // week before
	put(events.StoreAppStore, "2026-09-28", 1000, 0) // today: excluded
	put(events.StoreAppStore, "2026-09-13", 1000, 0) // two weeks ago: excluded

	if err := svc.Weekly(ctx); err != nil {
		t.Fatal(err)
	}
	ev := r.evs[0].(events.MetricsEvent)
	if ev.From != "2026-09-21" || ev.To != "2026-09-27" || len(ev.Apps) != 1 {
		t.Fatalf("window/apps: %+v", ev)
	}
	a := ev.Apps[0]
	if a.Current.Downloads != 15 || a.Previous.Downloads != 3 || a.Current.PageViews != 100 || a.ByStore[events.StorePlayStore].Downloads != 5 {
		t.Fatalf("figures: %+v", a)
	}
	if len(ev.Targets()) != 2 {
		t.Fatalf("weekly should also go to per-app channel: %v", ev.Targets())
	}

	last, _ := svc.Last(ctx, reg.Apps()[0], 7) // 21.–27. 9.
	if last.Current.Downloads != 15 {
		t.Fatalf("last 7d: %+v", last.Current)
	}
}
