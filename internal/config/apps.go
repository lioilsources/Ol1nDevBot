package config

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"slices"
	"sync"

	"gopkg.in/yaml.v3"
)

// AppsFile is the parsed apps.yaml.
type AppsFile struct {
	GuildID  string   `yaml:"guild_id"`
	Timezone string   `yaml:"timezone"`
	Announce Announce `yaml:"announce"`
	GitHub   GitHub   `yaml:"github"`
	Discord  Discord  `yaml:"discord"`
	Apps     []App    `yaml:"apps"`
}

type Announce struct {
	EveryoneOnNewApp    bool `yaml:"everyone_on_new_app"`
	EveryoneOnMilestone bool `yaml:"everyone_on_milestone"`
}

type GitHub struct {
	Owner    string `yaml:"owner"`
	WatchAll bool   `yaml:"watch_all"`
	// Repos not tied to any app (infra, dotfiles, …) that still belong in the changelog.
	Repos []string `yaml:"repos"`
}

type Discord struct {
	// OpsRole is the role name pinged on failed deploys (no ping if the role does not exist).
	OpsRole string `yaml:"ops_role"`
}

// App is one application; its Discord category and channels derive from it.
type App struct {
	Slug  string `yaml:"slug"`
	Name  string `yaml:"name"`
	Emoji string `yaml:"emoji,omitempty"`
	// Launched marks an app that is already public: state that exists when the
	// bot first sees it (live versions, reached milestones) is recorded silently.
	Launched   bool     `yaml:"launched,omitempty"`
	IOS        *IOS     `yaml:"ios,omitempty"`
	Android    *Android `yaml:"android,omitempty"`
	Repos      []string `yaml:"repos,omitempty"`
	Services   []string `yaml:"services,omitempty"`
	Milestones []string `yaml:"milestones,omitempty"`
	// DownloadsBaseline adds lifetime downloads from before the bot tracked the
	// app, so downloads_N milestones count from the real total.
	DownloadsBaseline int64 `yaml:"downloads_baseline,omitempty"`
}

type IOS struct {
	BundleID string `yaml:"bundle_id"`
	ASCAppID string `yaml:"asc_app_id,omitempty"`
	// Country is the storefront used for the public lookup API (default "us").
	Country string `yaml:"country,omitempty"`
}

type Android struct {
	Package string `yaml:"package"`
}

// DefaultMilestones applies when an app does not list its own.
var DefaultMilestones = []string{"first_release", "v1_0", "both_stores", "downloads_100", "downloads_1000", "downloads_10000", "weekly_growth_2x", "deploys_100"}

var (
	slugRe      = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}$`)
	milestoneRe = regexp.MustCompile(`^(first_release|v1_0|both_stores|weekly_growth_2x|deploys_100|downloads_[1-9][0-9]*)$`)
	repoRe      = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
)

// DisplayEmoji returns the configured emoji or a default phone.
func (a App) DisplayEmoji() string {
	if a.Emoji != "" {
		return a.Emoji
	}
	return "📱"
}

// MilestoneKeys returns configured milestones or the defaults.
func (a App) MilestoneKeys() []string {
	if len(a.Milestones) > 0 {
		return a.Milestones
	}
	return DefaultMilestones
}

// Validate checks a single app definition.
func (a App) Validate() error {
	if !slugRe.MatchString(a.Slug) {
		return fmt.Errorf("app %q: slug must match %s", a.Slug, slugRe)
	}
	if a.Name == "" {
		return fmt.Errorf("app %q: name is required", a.Slug)
	}
	if a.IOS != nil && a.IOS.BundleID == "" {
		return fmt.Errorf("app %q: ios.bundle_id is required", a.Slug)
	}
	if a.Android != nil && a.Android.Package == "" {
		return fmt.Errorf("app %q: android.package is required", a.Slug)
	}
	for _, r := range a.Repos {
		if !repoRe.MatchString(r) {
			return fmt.Errorf("app %q: repo %q must be owner/name", a.Slug, r)
		}
	}
	for _, m := range a.Milestones {
		if !milestoneRe.MatchString(m) {
			return fmt.Errorf("app %q: unknown milestone %q", a.Slug, m)
		}
	}
	return nil
}

// ParseAppsFile parses and validates apps.yaml content.
func ParseAppsFile(data []byte) (*AppsFile, error) {
	var f AppsFile
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse apps.yaml: %w", err)
	}
	if f.Timezone == "" {
		f.Timezone = "Europe/Prague"
	}
	if f.Discord.OpsRole == "" {
		f.Discord.OpsRole = "ops"
	}
	for _, r := range f.GitHub.Repos {
		if !repoRe.MatchString(r) {
			return nil, fmt.Errorf("github.repos: %q must be owner/name", r)
		}
	}
	if err := validateApps(f.Apps); err != nil {
		return nil, err
	}
	return &f, nil
}

func validateApps(apps []App) error {
	slugs := map[string]bool{}
	services := map[string]string{}
	for _, a := range apps {
		if err := a.Validate(); err != nil {
			return err
		}
		if slugs[a.Slug] {
			return fmt.Errorf("duplicate app slug %q", a.Slug)
		}
		slugs[a.Slug] = true
		for _, s := range a.Services {
			if other, ok := services[s]; ok {
				return fmt.Errorf("service %q belongs to both %q and %q", s, other, a.Slug)
			}
			services[s] = a.Slug
		}
	}
	return nil
}

// OverrideSource returns apps added at runtime (the apps_override table).
type OverrideSource func(ctx context.Context) ([]App, error)

// Registry is the merged, reloadable view of apps.yaml + runtime overrides.
type Registry struct {
	path      string
	overrides OverrideSource

	mu   sync.RWMutex
	file *AppsFile
	apps []App
}

// NewRegistry creates a registry; call Reload before use.
func NewRegistry(path string, overrides OverrideSource) *Registry {
	return &Registry{path: path, overrides: overrides}
}

// NewStaticRegistry builds a registry from an in-memory file (tests).
func NewStaticRegistry(f *AppsFile) *Registry {
	return &Registry{file: f, apps: slices.Clone(f.Apps)}
}

// Reload re-reads apps.yaml and overrides. Apps from apps.yaml win over
// overrides with the same slug.
func (r *Registry) Reload(ctx context.Context) error {
	data, err := os.ReadFile(r.path)
	if err != nil {
		return fmt.Errorf("read %s: %w", r.path, err)
	}
	f, err := ParseAppsFile(data)
	if err != nil {
		return err
	}
	apps := slices.Clone(f.Apps)
	if r.overrides != nil {
		ovr, err := r.overrides(ctx)
		if err != nil {
			return fmt.Errorf("load app overrides: %w", err)
		}
		for _, o := range ovr {
			if !slices.ContainsFunc(apps, func(a App) bool { return a.Slug == o.Slug }) {
				apps = append(apps, o)
			}
		}
		if err := validateApps(apps); err != nil {
			return fmt.Errorf("apps.yaml + overrides: %w", err)
		}
	}
	r.mu.Lock()
	r.file, r.apps = f, apps
	r.mu.Unlock()
	return nil
}

// File returns the global settings from apps.yaml.
func (r *Registry) File() AppsFile {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return *r.file
}

// Apps returns all apps (yaml + overrides).
func (r *Registry) Apps() []App {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return slices.Clone(r.apps)
}

// App looks up an app by slug.
func (r *Registry) App(slug string) (App, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, a := range r.apps {
		if a.Slug == slug {
			return a, true
		}
	}
	return App{}, false
}

// AppForService returns the app owning a deploy-hook service name.
func (r *Registry) AppForService(service string) (App, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, a := range r.apps {
		if slices.Contains(a.Services, service) {
			return a, true
		}
	}
	return App{}, false
}

// AppForRepo returns the app owning a GitHub repo.
func (r *Registry) AppForRepo(repo string) (App, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, a := range r.apps {
		if slices.Contains(a.Repos, repo) {
			return a, true
		}
	}
	return App{}, false
}

// Repos returns explicitly configured repos (apps + github.repos), deduplicated.
func (r *Registry) Repos() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []string
	add := func(rs []string) {
		for _, x := range rs {
			if !slices.Contains(out, x) {
				out = append(out, x)
			}
		}
	}
	for _, a := range r.apps {
		add(a.Repos)
	}
	add(r.file.GitHub.Repos)
	return out
}

// MarshalAppYAML renders an app as an apps.yaml list item (for /app add).
func MarshalAppYAML(a App) (string, error) {
	b, err := yaml.Marshal([]App{a})
	return string(b), err
}

// UnmarshalAppYAML is the inverse of MarshalAppYAML.
func UnmarshalAppYAML(s string) (App, error) {
	var apps []App
	if err := yaml.Unmarshal([]byte(s), &apps); err != nil {
		return App{}, err
	}
	if len(apps) != 1 {
		return App{}, fmt.Errorf("expected exactly one app, got %d", len(apps))
	}
	return apps[0], nil
}
