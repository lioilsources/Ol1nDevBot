package discord

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/lioilsources/Ol1nDevBot/internal/config"
	"github.com/lioilsources/Ol1nDevBot/internal/store"
)

func testLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func testStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestReconcileCreatesIsIdempotentAndArchives(t *testing.T) {
	ctx := context.Background()
	api, st := newFakeAPI(), testStore(t)
	r := NewReconciler(api, st, "g", testLog())
	apps := []config.App{{Slug: "storyteller", Name: "Storyteller", Emoji: "📖"}, {Slug: "kiran", Name: "Kiran"}}

	diff, err := r.Reconcile(ctx, apps)
	if err != nil {
		t.Fatal(err)
	}
	// 4 categories + 5 infra/announcement channels + 2×3 app channels.
	if len(api.channels) != 4+5+6 {
		t.Fatalf("channels: %d (%v)", len(api.channels), diff)
	}
	rel := api.byName("storyteller-releases")
	cat := api.byName("📖 Storyteller")
	if rel == nil || cat == nil || rel.ParentID != cat.ID {
		t.Fatal("app channel not inside its category")
	}
	if api.byName("announcements").Type != discordgo.ChannelTypeGuildNews {
		t.Fatal("announcements should be a news channel")
	}

	diff, err = r.Reconcile(ctx, apps)
	if err != nil || len(diff) != 0 || len(api.channels) != 15 {
		t.Fatalf("second run not idempotent: %v %v %d", diff, err, len(api.channels))
	}

	// Remove kiran → archived, never deleted.
	diff, err = r.Reconcile(ctx, apps[:1])
	if err != nil {
		t.Fatal(err)
	}
	archive := api.byName("🗄 ARCHIVE")
	if archive == nil {
		t.Fatalf("archive category missing: %v", diff)
	}
	zz := api.byName("zz-kiran-releases")
	if zz == nil || zz.ParentID != archive.ID {
		t.Fatalf("kiran-releases not archived: %v", diff)
	}
	if api.byName("zz-📱 Kiran") == nil {
		t.Fatalf("kiran category not renamed: %v", diff)
	}
	if len(api.channels) != 16 {
		t.Fatalf("something was deleted or duplicated: %d", len(api.channels))
	}

	// Re-add kiran → restored from archive.
	if _, err := r.Reconcile(ctx, apps); err != nil {
		t.Fatal(err)
	}
	back := api.byName("kiran-releases")
	if back == nil || back.ID != zz.ID || back.ParentID != api.byName("📱 Kiran").ID {
		t.Fatal("kiran not restored in place")
	}
}

func TestReconcileAdoptsExistingAndFallsBackFromNews(t *testing.T) {
	ctx := context.Background()
	api, st := newFakeAPI(), testStore(t)
	api.noNews = true
	api.channels = []*discordgo.Channel{{ID: "42", Name: "deploys", Type: discordgo.ChannelTypeGuildText}}
	r := NewReconciler(api, st, "g", testLog())
	diff, err := r.Reconcile(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if id, _ := ChannelID(ctx, st, "infra:deploys"); id != "42" {
		t.Fatalf("existing #deploys not adopted: %v", diff)
	}
	if api.channel("42").ParentID != api.byName("🛠 INFRA").ID {
		t.Fatal("adopted channel not moved into INFRA")
	}
	if api.byName("announcements").Type != discordgo.ChannelTypeGuildText || !strings.Contains(strings.Join(diff, "\n"), "Community") {
		t.Fatalf("news fallback: %v", diff)
	}
}
