// Package events defines the typed events flowing from sources to Discord.
//
// Every event has a deterministic dedup key and a list of logical targets
// (channels). The outbox stores one row per (event, target).
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Logical Discord targets, resolved to channel IDs by the reconciler mapping.
const (
	TargetAnnouncements = "announcements"
	TargetDeploys       = "infra:deploys"
	TargetChangelog     = "infra:changelog"
	TargetMetrics       = "infra:metrics"
	TargetBotLog        = "infra:bot-log"
)

// AppReleases is the per-app releases channel target.
func AppReleases(slug string) string { return "app:" + slug + ":releases" }

// AppMetricsTarget is the per-app metrics channel target.
func AppMetricsTarget(slug string) string { return "app:" + slug + ":metrics" }

// Event kinds (persisted in the outbox; do not rename).
const (
	KindDeploy       = "deploy"
	KindStoreVersion = "store_version"
	KindVersionState = "version_state"
	KindNewApp       = "new_app"
	KindMetrics      = "metrics"
	KindChangelog    = "changelog"
	KindMilestone    = "milestone"
	KindBotLog       = "bot_log"
)

// Stores.
const (
	StoreAppStore  = "appstore"
	StorePlayStore = "playstore"
)

// StoreLabel is a human label for a store id.
func StoreLabel(store string) string {
	switch store {
	case StoreAppStore:
		return "App Store"
	case StorePlayStore:
		return "Google Play"
	}
	return store
}

// Event is anything that can be published.
type Event interface {
	Kind() string
	Key() string
	Targets() []string
}

// DeployEvent comes from the deploy hook.
type DeployEvent struct {
	Service     string    `json:"service"`
	Env         string    `json:"env"`
	Version     string    `json:"version"`
	Commit      string    `json:"commit"`
	CommitURL   string    `json:"commit_url,omitempty"`
	URL         string    `json:"url,omitempty"`
	Status      string    `json:"status"` // success | failed
	Notes       string    `json:"notes,omitempty"`
	DurationSec int64     `json:"duration_sec,omitempty"`
	AppSlug     string    `json:"app_slug,omitempty"`
	At          time.Time `json:"at"`
}

func (e DeployEvent) Kind() string { return KindDeploy }
func (e DeployEvent) Key() string {
	return fmt.Sprintf("deploy:%s:%s:%s:%s:%s", e.Service, e.Env, e.Version, e.Commit, e.Status)
}
func (e DeployEvent) Targets() []string {
	t := []string{TargetDeploys}
	if e.AppSlug != "" {
		t = append(t, AppReleases(e.AppSlug))
	}
	return t
}
func (e DeployEvent) Failed() bool { return e.Status != "success" }

// StoreVersionEvent: a new version is live in a store.
type StoreVersionEvent struct {
	AppSlug      string    `json:"app_slug"`
	AppName      string    `json:"app_name"`
	Store        string    `json:"store"`
	Version      string    `json:"version"`
	Build        string    `json:"build,omitempty"`
	ReleaseNotes string    `json:"release_notes,omitempty"`
	Description  string    `json:"description,omitempty"`
	StoreURL     string    `json:"store_url,omitempty"`
	IconURL      string    `json:"icon_url,omitempty"`
	ReleaseDate  time.Time `json:"release_date,omitzero"`
}

func (e StoreVersionEvent) Kind() string { return KindStoreVersion }
func (e StoreVersionEvent) Key() string {
	return fmt.Sprintf("version:%s:%s:%s", e.Store, e.AppSlug, e.Version)
}
func (e StoreVersionEvent) Targets() []string { return []string{AppReleases(e.AppSlug)} }

// VersionStateEvent: review / rollout state change (ASC, Play tracks).
type VersionStateEvent struct {
	AppSlug   string `json:"app_slug"`
	AppName   string `json:"app_name"`
	Store     string `json:"store"`
	Version   string `json:"version"`
	State     string `json:"state"`
	PrevState string `json:"prev_state,omitempty"`
	Detail    string `json:"detail,omitempty"` // e.g. rollout "20 %"
}

func (e VersionStateEvent) Kind() string { return KindVersionState }
func (e VersionStateEvent) Key() string {
	return fmt.Sprintf("state:%s:%s:%s:%s:%s", e.Store, e.AppSlug, e.Version, e.State, e.Detail)
}
func (e VersionStateEvent) Targets() []string { return []string{AppReleases(e.AppSlug)} }

// NewAppEvent: an app appeared in a store for the first time.
type NewAppEvent struct {
	AppSlug      string `json:"app_slug"`
	AppName      string `json:"app_name"`
	Emoji        string `json:"emoji,omitempty"`
	Description  string `json:"description,omitempty"`
	IconURL      string `json:"icon_url,omitempty"`
	AppStoreURL  string `json:"app_store_url,omitempty"`
	PlayStoreURL string `json:"play_store_url,omitempty"`
	Everyone     bool   `json:"everyone,omitempty"`
}

func (e NewAppEvent) Kind() string      { return KindNewApp }
func (e NewAppEvent) Key() string       { return "newapp:" + e.AppSlug }
func (e NewAppEvent) Targets() []string { return []string{TargetAnnouncements} }

// Figures is a set of store metrics over a period.
type Figures struct {
	Downloads   int64 `json:"downloads"`
	Impressions int64 `json:"impressions"`
	PageViews   int64 `json:"page_views"`
	Uninstalls  int64 `json:"uninstalls"`
}

// Add returns the sum of two figure sets.
func (f Figures) Add(o Figures) Figures {
	return Figures{f.Downloads + o.Downloads, f.Impressions + o.Impressions, f.PageViews + o.PageViews, f.Uninstalls + o.Uninstalls}
}

// AppFigures is one app's metrics for the digest.
type AppFigures struct {
	AppSlug  string             `json:"app_slug"`
	AppName  string             `json:"app_name"`
	Emoji    string             `json:"emoji,omitempty"`
	Current  Figures            `json:"current"`
	Previous Figures            `json:"previous"`
	ByStore  map[string]Figures `json:"by_store,omitempty"`
}

// MetricsEvent is a daily line or a weekly digest.
type MetricsEvent struct {
	Period string       `json:"period"` // daily | weekly
	From   string       `json:"from"`   // YYYY-MM-DD
	To     string       `json:"to"`
	Apps   []AppFigures `json:"apps"`
}

func (e MetricsEvent) Kind() string { return KindMetrics }
func (e MetricsEvent) Key() string  { return fmt.Sprintf("metrics:%s:%s:%s", e.Period, e.From, e.To) }
func (e MetricsEvent) Targets() []string {
	t := []string{TargetMetrics}
	if e.Period == "weekly" {
		for _, a := range e.Apps {
			t = append(t, AppMetricsTarget(a.AppSlug))
		}
	}
	return t
}

// RepoChangelog is the summary of one repository.
type RepoChangelog struct {
	Repo     string `json:"repo"`
	Markdown string `json:"markdown"`
}

// ChangelogEvent is the weekly (or on-demand) GitHub summary.
type ChangelogEvent struct {
	From  time.Time       `json:"from"`
	To    time.Time       `json:"to"`
	Repos []RepoChangelog `json:"repos"`
	// Nonce distinguishes on-demand runs (/changelog now) from the weekly one.
	Nonce string `json:"nonce,omitempty"`
}

func (e ChangelogEvent) Kind() string { return KindChangelog }
func (e ChangelogEvent) Key() string {
	k := fmt.Sprintf("changelog:%s:%s", e.From.Format(time.DateOnly), e.To.Format(time.DateOnly))
	if e.Nonce != "" {
		k += ":" + e.Nonce
	}
	return k
}
func (e ChangelogEvent) Targets() []string { return []string{TargetChangelog} }

// MilestoneEvent: an app reached a milestone.
type MilestoneEvent struct {
	AppSlug      string `json:"app_slug"`
	AppName      string `json:"app_name"`
	Emoji        string `json:"emoji,omitempty"`
	MilestoneKey string `json:"key"`
	Title        string `json:"title"`
	Detail       string `json:"detail,omitempty"`
	Everyone     bool   `json:"everyone,omitempty"`
}

func (e MilestoneEvent) Kind() string { return KindMilestone }
func (e MilestoneEvent) Key() string  { return "milestone:" + e.AppSlug + ":" + e.MilestoneKey }
func (e MilestoneEvent) Targets() []string {
	t := []string{TargetAnnouncements}
	if e.AppSlug != "" {
		t = append(t, AppReleases(e.AppSlug))
	}
	return t
}

// BotLogEvent is an operational message for #bot-log.
type BotLogEvent struct {
	Level   string    `json:"level"` // info | warn | error | ok
	Title   string    `json:"title"`
	Message string    `json:"message"`
	At      time.Time `json:"at"`
}

func (e BotLogEvent) Kind() string { return KindBotLog }
func (e BotLogEvent) Key() string {
	return fmt.Sprintf("botlog:%d:%s", e.At.UnixNano(), e.Title)
}
func (e BotLogEvent) Targets() []string { return []string{TargetBotLog} }

// Decode restores an event from its outbox kind and JSON payload.
func Decode(kind, payload string) (Event, error) {
	var ev Event
	switch kind {
	case KindDeploy:
		ev = &DeployEvent{}
	case KindStoreVersion:
		ev = &StoreVersionEvent{}
	case KindVersionState:
		ev = &VersionStateEvent{}
	case KindNewApp:
		ev = &NewAppEvent{}
	case KindMetrics:
		ev = &MetricsEvent{}
	case KindChangelog:
		ev = &ChangelogEvent{}
	case KindMilestone:
		ev = &MilestoneEvent{}
	case KindBotLog:
		ev = &BotLogEvent{}
	default:
		return nil, fmt.Errorf("unknown event kind %q", kind)
	}
	if err := json.Unmarshal([]byte(payload), ev); err != nil {
		return nil, fmt.Errorf("decode %s: %w", kind, err)
	}
	return deref(ev), nil
}

func deref(ev Event) Event {
	switch e := ev.(type) {
	case *DeployEvent:
		return *e
	case *StoreVersionEvent:
		return *e
	case *VersionStateEvent:
		return *e
	case *NewAppEvent:
		return *e
	case *MetricsEvent:
		return *e
	case *ChangelogEvent:
		return *e
	case *MilestoneEvent:
		return *e
	case *BotLogEvent:
		return *e
	}
	return ev
}

// Emitter accepts events for publication. Emit reports whether the event was
// new (false = duplicate, already emitted before).
type Emitter interface {
	Emit(ctx context.Context, ev Event) (bool, error)
}
