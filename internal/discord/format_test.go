package discord

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/lioilsources/Ol1nDevBot/internal/events"
)

var update = flag.Bool("update", false, "rewrite golden files")

func prague(t *testing.T) *time.Location {
	loc, err := time.LoadLocation("Europe/Prague")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func TestRenderGolden(t *testing.T) {
	loc := prague(t)
	at := time.Date(2026, 9, 29, 10, 15, 0, 0, loc)
	cases := []struct {
		name   string
		ev     events.Event
		target string
	}{
		{"deploy_success", events.DeployEvent{Service: "storyteller-api", Env: "prod", Version: "v1.4.2", Commit: "abc123def456", CommitURL: "https://github.com/lioilsources/storyteller-api/commit/abc123def456", URL: "https://storyteller.ol1n.com", Status: "success", Notes: "migrace DB", DurationSec: 95, At: at}, events.TargetDeploys},
		{"deploy_failed", events.DeployEvent{Service: "storyteller-api", Env: "prod", Version: "v1.4.3", Status: "failed", At: at}, events.TargetDeploys},
		{"store_version", events.StoreVersionEvent{AppSlug: "storyteller", AppName: "Storyteller", Store: events.StoreAppStore, Version: "1.5.0", Build: "42", ReleaseNotes: "Nové pohádky.", StoreURL: "https://apps.apple.com/app/id1", IconURL: "https://is1.mzstatic.com/icon.png", ReleaseDate: at}, events.AppReleases("storyteller")},
		{"version_state", events.VersionStateEvent{AppSlug: "storyteller", AppName: "Storyteller", Store: events.StoreAppStore, Version: "1.5.0", State: "IN_REVIEW", PrevState: "WAITING_FOR_REVIEW"}, events.AppReleases("storyteller")},
		{"new_app", events.NewAppEvent{AppSlug: "storyteller", AppName: "Storyteller", Emoji: "📖", Description: "Pohádky na dobrou noc.", AppStoreURL: "https://apps.apple.com/app/id1", PlayStoreURL: "https://play.google.com/store/apps/details?id=com.ol1n.storyteller", Everyone: true}, events.TargetAnnouncements},
		{"milestone", events.MilestoneEvent{AppSlug: "storyteller", AppName: "Storyteller", MilestoneKey: "downloads_1000", Title: "1\u00a0000 stažení!", Detail: "Celkem 1\u00a0012 stažení napříč story."}, events.TargetAnnouncements},
		{"metrics_weekly", weekly(), events.TargetMetrics},
		{"metrics_weekly_app", weekly(), events.AppMetricsTarget("storyteller")},
		{"metrics_daily", events.MetricsEvent{Period: "daily", From: "2026-09-28", To: "2026-09-28", Apps: weekly().Apps}, events.TargetMetrics},
		{"changelog", events.ChangelogEvent{From: time.Date(2026, 9, 21, 0, 0, 0, 0, loc), To: time.Date(2026, 9, 27, 23, 59, 59, 0, loc), Repos: []events.RepoChangelog{{Repo: "lioilsources/storyteller", Markdown: "**✨ Features**\n- Nové pohádky [#12](<https://github.com/lioilsources/storyteller/pull/12>)"}}}, events.TargetChangelog},
		{"bot_log", events.BotLogEvent{Level: "error", Title: "apple-lookup selhává", Message: "3× po sobě", At: at}, events.TargetBotLog},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, err := Render(c.ev, c.target, RenderContext{Loc: loc, OpsRoleID: "777"})
			if err != nil {
				t.Fatal(err)
			}
			got, _ := json.MarshalIndent(r, "", "  ")
			path := filepath.Join("testdata", c.name+".golden.json")
			if *update {
				os.MkdirAll("testdata", 0o755)
				os.WriteFile(path, append(got, '\n'), 0o644)
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (run go test ./internal/discord -update)", err)
			}
			if strings.TrimSpace(string(want)) != strings.TrimSpace(string(got)) {
				t.Errorf("golden mismatch for %s:\n%s", c.name, got)
			}
		})
	}
}

func weekly() events.MetricsEvent {
	return events.MetricsEvent{Period: "weekly", From: "2026-09-21", To: "2026-09-27", Apps: []events.AppFigures{{
		AppSlug: "storyteller", AppName: "Storyteller", Emoji: "📖",
		Current:  events.Figures{Downloads: 1250, Impressions: 20000, PageViews: 5000, Uninstalls: 12},
		Previous: events.Figures{Downloads: 1000, Impressions: 21000, PageViews: 4000},
		ByStore:  map[string]events.Figures{events.StoreAppStore: {Downloads: 800}, events.StorePlayStore: {Downloads: 450}},
	}}}
}

func TestSplitMessage(t *testing.T) {
	long := strings.Repeat("ř", 1500) + "\n\n" + strings.Repeat("ž", 1500)
	parts := SplitMessage(long, 2000)
	if len(parts) != 2 {
		t.Fatalf("parts: %d", len(parts))
	}
	for _, p := range parts {
		if utf8.RuneCountInString(p) > 2000 || !utf8.ValidString(p) {
			t.Fatal("bad chunk")
		}
	}
	if got := SplitMessage(strings.Repeat("a", 4500), 2000); len(got) != 3 {
		t.Fatalf("hard split: %d", len(got))
	}
}

func TestFormatting(t *testing.T) {
	loc := prague(t)
	d := func(y int, m time.Month, day int) time.Time { return time.Date(y, m, day, 0, 0, 0, 0, loc) }
	for _, c := range []struct{ got, want string }{
		{FormatDateRange(d(2026, 9, 22), d(2026, 9, 28)), "22.–28. 9. 2026"},
		{FormatDateRange(d(2026, 9, 29), d(2026, 10, 5)), "29. 9.–5. 10. 2026"},
		{FormatDateRange(d(2025, 12, 29), d(2026, 1, 4)), "29. 12. 2025–4. 1. 2026"},
		{FormatInt(1000), "1\u00a0000"},
		{FormatInt(1234567), "1\u00a0234\u00a0567"},
		{FormatInt(999), "999"},
		{Delta(150, 100), "(▲ 50 %)"},
		{Delta(50, 100), "(▼ 50 %)"},
		{Delta(5, 0), "(nové)"},
		{Delta(0, 0), ""},
		{CVR(25, 1000), "2,5 %"},
		{CVR(5, 0), "–"},
	} {
		if c.got != c.want {
			t.Errorf("got %q want %q", c.got, c.want)
		}
	}
}
