package gplay

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/lioilsources/Ol1nDevBot/internal/config"
	"github.com/lioilsources/Ol1nDevBot/internal/events"
	"github.com/lioilsources/Ol1nDevBot/internal/outbox"
	"github.com/lioilsources/Ol1nDevBot/internal/sources/versions"
	"github.com/lioilsources/Ol1nDevBot/internal/store"
	"github.com/lioilsources/Ol1nDevBot/internal/store/db"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// utf16le encodes like Play Console exports (BOM + UTF-16LE).
func utf16le(s string) []byte {
	out := []byte{0xFF, 0xFE}
	for _, u := range utf16.Encode([]rune(s)) {
		out = append(out, byte(u), byte(u>>8))
	}
	return out
}

func fixture(t *testing.T, name string) string {
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestParseInstallsUTF16(t *testing.T) {
	rows, err := ParseInstalls(utf16le(fixture(t, "installs_overview.csv")))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0] != (InstallsRow{Date: "2026-09-27", Installs: 31, Uninstalls: 4}) || rows[1].Installs != 1204 {
		t.Fatalf("rows: %+v", rows)
	}
}

func TestParseStorePerformanceSumsCountries(t *testing.T) {
	perf, err := ParseStorePerformance([]byte(fixture(t, "store_performance_country.csv")))
	if err != nil {
		t.Fatal(err)
	}
	if perf["2026-09-27"].PageViews != 150 || perf["2026-09-28"].PageViews != 40 {
		t.Fatalf("perf: %+v", perf)
	}
}

func TestReleaseName(t *testing.T) {
	for _, c := range []struct {
		r    Release
		want string
	}{
		{Release{Name: "42 (1.5.0)", VersionCodes: []string{"42"}}, "1.5.0"},
		{Release{Name: "1.5.0"}, "1.5.0"},
		{Release{VersionCodes: []string{"41", "42"}}, "42"},
	} {
		if got := ReleaseName(c.r); got != c.want {
			t.Errorf("got %q want %q", got, c.want)
		}
	}
}

type fakePlay struct {
	mu        sync.Mutex
	track     string
	deleted   int
	committed bool
	objects   map[string][]byte
}

func (f *fakePlay) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := r.URL.Path
	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(p, "/edits"):
		w.Write([]byte(`{"id":"edit1"}`))
	case r.Method == http.MethodGet && strings.HasSuffix(p, "/edits/edit1/tracks/production"):
		w.Write([]byte(f.track))
	case r.Method == http.MethodDelete && strings.HasSuffix(p, "/edits/edit1"):
		f.deleted++
		w.WriteHeader(http.StatusNoContent)
	case strings.Contains(p, ":commit"):
		f.committed = true
	case r.Method == http.MethodGet && strings.HasPrefix(p, "/storage/v1/b/bucket/o/") && r.URL.Query().Get("alt") == "media":
		name := strings.TrimPrefix(p, "/storage/v1/b/bucket/o/")
		if b, ok := f.objects[name]; ok {
			w.Write(b)
			return
		}
		http.NotFound(w, r)
	case r.Method == http.MethodGet && p == "/storage/v1/b/bucket/o":
		prefix := r.URL.Query().Get("prefix")
		var items []string
		for name := range f.objects {
			if strings.HasPrefix(name, prefix) {
				items = append(items, `{"name":"`+name+`"}`)
			}
		}
		w.Write([]byte(`{"items":[` + strings.Join(items, ",") + `]}`))
	default:
		http.Error(w, "unexpected "+r.Method+" "+p, http.StatusTeapot)
	}
}

func TestTracksPoller(t *testing.T) {
	fp := &fakePlay{track: `{"track":"production","releases":[{"name":"41 (1.4.0)","versionCodes":["41"],"status":"completed"}]}`}
	srv := httptest.NewServer(fp)
	defer srv.Close()

	ctx := context.Background()
	st, _ := store.Open(ctx, ":memory:")
	ob := outbox.New(st, quiet())
	var evs []events.Event
	ob.Subscribe(func(_ context.Context, ev events.Event) { evs = append(evs, ev) })
	app := config.App{Slug: "storyteller", Name: "Storyteller", Launched: true, Android: &config.Android{Package: "com.ol1n.storyteller"}}
	reg := config.NewStaticRegistry(&config.AppsFile{Apps: []config.App{app}})
	p := &TracksPoller{Client: &Client{PublisherURL: srv.URL, HTTP: srv.Client()}, Apps: reg, Recorder: &versions.Recorder{St: st, Emit: ob, Log: quiet()}, Log: quiet()}

	if err := p.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if len(evs) != 0 {
		t.Fatalf("first poll of launched app must be silent: %+v", evs)
	}

	fp.track = `{"releases":[{"name":"42 (1.5.0)","versionCodes":["42"],"status":"inProgress","userFraction":0.2,"releaseNotes":[{"language":"en-US","text":"en"},{"language":"cs-CZ","text":"Nové pohádky"}]},{"name":"41 (1.4.0)","versionCodes":["41"],"status":"completed"}]}`
	if err := p.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	var state *events.VersionStateEvent
	var live *events.StoreVersionEvent
	for _, ev := range evs {
		switch x := ev.(type) {
		case events.VersionStateEvent:
			state = &x
		case events.StoreVersionEvent:
			live = &x
		}
	}
	if state == nil || state.Version != "1.5.0" || state.State != "inProgress" || state.Detail != "20 %" {
		t.Fatalf("state: %+v", state)
	}
	if live == nil || live.Version != "1.5.0" || live.Build != "42" || live.ReleaseNotes != "Nové pohádky" || live.StoreURL != "https://play.google.com/store/apps/details?id=com.ol1n.storyteller" {
		t.Fatalf("live: %+v", live)
	}
	if fp.deleted != 2 || fp.committed {
		t.Fatalf("edits must be deleted, never committed (deleted=%d committed=%v)", fp.deleted, fp.committed)
	}
}

func TestStatsPoller(t *testing.T) {
	month := time.Now().Format("200601")
	fp := &fakePlay{objects: map[string][]byte{
		"stats/installs/installs_com.x_" + month + "_overview.csv":                             utf16le(fixture(t, "installs_overview.csv")),
		"stats/store_performance/store_performance_com.x_" + month + "_country.csv":            []byte(fixture(t, "store_performance_country.csv")),
		"stats/store_performance/store_performance_com.x_" + month + "_traffic_source.csv.bak": []byte("ignored"),
	}}
	srv := httptest.NewServer(fp)
	defer srv.Close()

	ctx := context.Background()
	st, _ := store.Open(ctx, ":memory:")
	reg := config.NewStaticRegistry(&config.AppsFile{Apps: []config.App{{Slug: "x", Name: "X", Android: &config.Android{Package: "com.x"}}}})
	p := &StatsPoller{Client: &Client{StorageURL: srv.URL, HTTP: srv.Client()}, Bucket: "bucket", Apps: reg, St: st, Loc: time.Local, Log: quiet()}
	if err := p.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	p.Poll(ctx) // idempotent
	sum, _ := st.SumMetrics(ctx, db.SumMetricsParams{AppSlug: "x", Date: "2026-09-01", Date_2: "2026-09-30"})
	if sum.Downloads != 1235 || sum.Uninstalls != 11 || sum.PageViews != 190 {
		t.Fatalf("sum: %+v", sum)
	}
}
