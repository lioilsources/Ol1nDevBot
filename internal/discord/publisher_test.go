package discord

import (
	"context"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/lioilsources/Ol1nDevBot/internal/config"
	"github.com/lioilsources/Ol1nDevBot/internal/events"
	"github.com/lioilsources/Ol1nDevBot/internal/outbox"
	"github.com/lioilsources/Ol1nDevBot/internal/store"
)

func setupPublisher(t *testing.T) (*fakeAPI, *store.Store, *outbox.Outbox, *Publisher) {
	t.Helper()
	ctx := context.Background()
	api, st := newFakeAPI(), testStore(t)
	api.roles = []*discordgo.Role{{ID: "777", Name: "ops"}}
	if _, err := NewReconciler(api, st, "g", testLog()).Reconcile(ctx, []config.App{{Slug: "storyteller", Name: "Storyteller"}}); err != nil {
		t.Fatal(err)
	}
	ob := outbox.New(st, testLog())
	pub := NewPublisher(api, st, "g", time.UTC, ob.Notify(), func() string { return "ops" }, testLog())
	return api, st, ob, pub
}

func TestPublisherDeliversFanOutThreadsAndPings(t *testing.T) {
	ctx := context.Background()
	api, _, ob, pub := setupPublisher(t)
	var sent []string
	pub.OnSent = func(_ context.Context, _ events.Event, target string) { sent = append(sent, target) }

	ob.Emit(ctx, events.DeployEvent{Service: "storyteller-api", Env: "prod", Version: "v1", Commit: "abc1234", Status: "failed", AppSlug: "storyteller", At: time.Unix(0, 0)})
	ob.Emit(ctx, events.StoreVersionEvent{AppSlug: "storyteller", AppName: "Storyteller", Store: events.StoreAppStore, Version: "1.5.0", StoreURL: "https://apps.apple.com/app/id1"})
	ob.Emit(ctx, events.NewAppEvent{AppSlug: "storyteller", AppName: "Storyteller", Everyone: true})
	pub.Drain(ctx)

	if len(sent) != 4 {
		t.Fatalf("delivered targets: %v", sent)
	}
	deploys := api.messages[api.byName("deploys").ID]
	if len(deploys) != 1 || deploys[0].Content != "<@&777> deploy selhal" || deploys[0].Embeds[0].Color != ColorRed {
		t.Fatalf("failed deploy should ping ops: %+v", deploys)
	}
	releases := api.messages[api.byName("storyteller-releases").ID]
	if len(releases) != 2 {
		t.Fatalf("releases: %d", len(releases))
	}
	if releases[1].Content != "" {
		t.Fatal("ops ping must only go to #deploys")
	}
	if len(api.threads) != 1 || api.threads[0] != "Storyteller 1.5.0" {
		t.Fatalf("threads: %v", api.threads)
	}
	ann := api.messages[api.byName("announcements").ID]
	if len(ann) != 1 || ann[0].Content != "@everyone" || len(api.crossp) != 1 {
		t.Fatalf("announcement: %+v crosspost=%v", ann, api.crossp)
	}

	// Nothing left, nothing re-sent.
	pub.Drain(ctx)
	if len(sent) != 4 {
		t.Fatal("rows were re-sent")
	}
}

func TestPublisherRetriesWithBackoff(t *testing.T) {
	ctx := context.Background()
	api, st, ob, pub := setupPublisher(t)
	api.failSends = 1
	now := time.Unix(1_700_000_000, 0)
	store.Now = func() time.Time { return now }
	t.Cleanup(func() { store.Now = time.Now })

	ob.Emit(ctx, events.BotLogEvent{Level: "info", Title: "x", At: now})
	pub.Drain(ctx)
	if n := len(api.messages[api.byName("bot-log").ID]); n != 0 {
		t.Fatalf("should have failed, sent %d", n)
	}
	stats, _ := st.OutboxStats(ctx)
	if stats.Pending != 1 {
		t.Fatalf("expected pending retry: %+v", stats)
	}
	pub.Drain(ctx) // not due yet
	if n := len(api.messages[api.byName("bot-log").ID]); n != 0 {
		t.Fatal("retried before backoff elapsed")
	}
	now = now.Add(baseBackoff)
	pub.Drain(ctx)
	if n := len(api.messages[api.byName("bot-log").ID]); n != 1 {
		t.Fatalf("not retried after backoff: %d", n)
	}
}

func TestStatusBoardCreatesPinsThenEdits(t *testing.T) {
	ctx := context.Background()
	api, st, _, _ := setupPublisher(t)
	reg := config.NewStaticRegistry(&config.AppsFile{Apps: []config.App{{Slug: "storyteller", Name: "Storyteller"}}})
	b := NewStatusBoard(api, st, reg, time.UTC, testLog())
	if err := b.Update(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.Update(ctx); err != nil {
		t.Fatal(err)
	}
	if len(api.messages[api.byName("metrics").ID]) != 1 || len(api.pins) != 1 || len(api.edits) != 1 {
		t.Fatalf("sent=%d pins=%d edits=%d", len(api.messages[api.byName("metrics").ID]), len(api.pins), len(api.edits))
	}
}
