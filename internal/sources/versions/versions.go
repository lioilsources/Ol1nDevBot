// Package versions records live store versions and review/rollout states,
// shared by the Apple and Google Play pollers.
package versions

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/lioilsources/Ol1nDevBot/internal/config"
	"github.com/lioilsources/Ol1nDevBot/internal/events"
	"github.com/lioilsources/Ol1nDevBot/internal/store"
	"github.com/lioilsources/Ol1nDevBot/internal/store/db"
)

// Live describes the version currently live in a store.
type Live struct {
	Store        string
	Version      string
	Build        string
	ReleaseNotes string
	Description  string
	StoreURL     string
	IconURL      string
	ReleaseDate  time.Time
}

// State describes the review / rollout state of one version.
type State struct {
	Store   string
	Version string
	State   string
	Detail  string
}

// Recorder deduplicates versions and emits events for new ones.
type Recorder struct {
	St   *store.Store
	Emit events.Emitter
	Log  *slog.Logger
}

// RecordLive stores a live version. The first observation of a launched app
// in a store is recorded silently (announced=0) so existing versions are not
// announced as new when the bot starts.
func (r *Recorder) RecordLive(ctx context.Context, app config.App, v Live) error {
	first, err := r.St.Baseline(ctx, "versions", v.Store+":"+app.Slug)
	if err != nil {
		return err
	}
	silent := first && app.Launched
	announced := int64(1)
	if silent {
		announced = 0
	}
	release := ""
	if !v.ReleaseDate.IsZero() {
		release = v.ReleaseDate.Format(time.DateOnly)
	}
	n, err := r.St.InsertAppVersion(ctx, db.InsertAppVersionParams{
		Store: v.Store, AppSlug: app.Slug, Version: v.Version, Build: v.Build,
		ReleaseDate: release, SeenAt: store.Unix(), Announced: announced,
	})
	if err != nil {
		return fmt.Errorf("insert version: %w", err)
	}
	if n == 0 {
		return nil
	}
	if silent {
		r.Log.Info("baseline version recorded silently", "app", app.Slug, "store", v.Store, "version", v.Version)
		return nil
	}
	_, err = r.Emit.Emit(ctx, events.StoreVersionEvent{
		AppSlug: app.Slug, AppName: app.Name, Store: v.Store, Version: v.Version, Build: v.Build,
		ReleaseNotes: v.ReleaseNotes, Description: v.Description, StoreURL: v.StoreURL, IconURL: v.IconURL, ReleaseDate: v.ReleaseDate,
	})
	return err
}

// RecordState stores a review/rollout state and emits a change event. The
// very first poll of an app records states silently.
func (r *Recorder) RecordState(ctx context.Context, app config.App, s State, firstPoll bool) error {
	prev, err := r.St.GetVersionState(ctx, db.GetVersionStateParams{Store: s.Store, AppSlug: app.Slug, Version: s.Version})
	if err != nil && !store.IsNotFound(err) {
		return err
	}
	if err == nil && prev.State == s.State && prev.Detail == s.Detail {
		return nil
	}
	if err := r.St.UpsertVersionState(ctx, db.UpsertVersionStateParams{
		Store: s.Store, AppSlug: app.Slug, Version: s.Version, State: s.State, Detail: s.Detail, ChangedAt: store.Unix(),
	}); err != nil {
		return err
	}
	if firstPoll {
		return nil
	}
	_, err = r.Emit.Emit(ctx, events.VersionStateEvent{
		AppSlug: app.Slug, AppName: app.Name, Store: s.Store, Version: s.Version,
		State: s.State, PrevState: prev.State, Detail: s.Detail,
	})
	return err
}

// FirstStatePoll marks the state baseline for (source, app) and reports
// whether this is the first poll.
func (r *Recorder) FirstStatePoll(ctx context.Context, source string, app config.App) (bool, error) {
	return r.St.Baseline(ctx, "states", source+":"+app.Slug)
}

// AppStoreURL returns the public App Store URL for an app, if known.
func AppStoreURL(app config.App) string {
	if app.IOS != nil && app.IOS.ASCAppID != "" {
		return "https://apps.apple.com/app/id" + app.IOS.ASCAppID
	}
	return ""
}

// PlayStoreURL returns the public Google Play URL for an app.
func PlayStoreURL(app config.App) string {
	if app.Android != nil {
		return "https://play.google.com/store/apps/details?id=" + app.Android.Package
	}
	return ""
}
