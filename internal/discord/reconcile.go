package discord

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/bwmarrin/discordgo"

	"github.com/lioilsources/Ol1nDevBot/internal/config"
	"github.com/lioilsources/Ol1nDevBot/internal/events"
	"github.com/lioilsources/Ol1nDevBot/internal/store"
)

const archivePrefix = "zz-"

// CategorySpec / ChannelSpec describe the desired guild layout.
type CategorySpec struct {
	Key      string
	Name     string
	Channels []ChannelSpec
}

type ChannelSpec struct {
	Key   string // logical target, e.g. "infra:deploys"
	Name  string
	Type  discordgo.ChannelType
	Topic string
}

// Desired returns the layout derived from the apps.
func Desired(apps []config.App) []CategorySpec {
	specs := []CategorySpec{
		{Key: "announcements", Name: "📢 ANNOUNCEMENTS", Channels: []ChannelSpec{
			{Key: events.TargetAnnouncements, Name: "announcements", Type: discordgo.ChannelTypeGuildNews, Topic: "Nové appky a milníky."},
		}},
		{Key: "infra", Name: "🛠 INFRA", Channels: []ChannelSpec{
			{Key: events.TargetDeploys, Name: "deploys", Type: discordgo.ChannelTypeGuildText, Topic: "Všechny deploye (olin.now, GitHub Actions)."},
			{Key: events.TargetChangelog, Name: "changelog", Type: discordgo.ChannelTypeGuildText, Topic: "Týdenní souhrn GitHub repozitářů (pondělí 09:00)."},
			{Key: events.TargetMetrics, Name: "metrics", Type: discordgo.ChannelTypeGuildText, Topic: "Denní a týdenní metriky + status board."},
			{Key: events.TargetBotLog, Name: "bot-log", Type: discordgo.ChannelTypeGuildText, Topic: "Co bot dělá: chyby pollerů, reconcile diff."},
		}},
	}
	for _, a := range apps {
		specs = append(specs, CategorySpec{
			Key:  "app:" + a.Slug,
			Name: a.DisplayEmoji() + " " + a.Name,
			Channels: []ChannelSpec{
				{Key: events.AppReleases(a.Slug), Name: a.Slug + "-releases", Type: discordgo.ChannelTypeGuildText, Topic: a.Name + ": nové verze, stavy review, deploye."},
				{Key: events.AppMetricsTarget(a.Slug), Name: a.Slug + "-metrics", Type: discordgo.ChannelTypeGuildText, Topic: a.Name + ": týdenní metriky."},
				{Key: "app:" + a.Slug + ":feedback", Name: a.Slug + "-feedback", Type: discordgo.ChannelTypeGuildText, Topic: a.Name + ": zpětná vazba."},
			},
		})
	}
	return specs
}

// Reconciler makes the guild match apps.yaml. It creates and adopts, never
// deletes: removed apps are moved to the ARCHIVE category with a zz- prefix.
type Reconciler struct {
	api     API
	st      *store.Store
	guildID string
	log     *slog.Logger
	mu      sync.Mutex
}

// NewReconciler creates a reconciler.
func NewReconciler(api API, st *store.Store, guildID string, log *slog.Logger) *Reconciler {
	return &Reconciler{api: api, st: st, guildID: guildID, log: log}
}

type guildState struct {
	byID map[string]*discordgo.Channel
	all  []*discordgo.Channel
}

func (g *guildState) add(c *discordgo.Channel) {
	g.byID[c.ID] = c
	g.all = append(g.all, c)
}

// Reconcile applies the layout and returns a human-readable diff.
func (r *Reconciler) Reconcile(ctx context.Context, apps []config.App) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	chans, err := r.api.GuildChannels(r.guildID)
	if err != nil {
		return nil, fmt.Errorf("list guild channels: %w", err)
	}
	g := &guildState{byID: map[string]*discordgo.Channel{}}
	for _, c := range chans {
		g.add(c)
	}

	var diff []string
	desiredApps := map[string]bool{}
	for _, a := range apps {
		desiredApps[a.Slug] = true
	}
	for _, spec := range Desired(apps) {
		catID, err := r.ensureCategory(ctx, g, spec, &diff)
		if err != nil {
			return diff, err
		}
		for _, ch := range spec.Channels {
			if err := r.ensureChannel(ctx, g, ch, catID, &diff); err != nil {
				return diff, err
			}
		}
	}
	if err := r.archiveRemoved(ctx, g, desiredApps, &diff); err != nil {
		return diff, err
	}
	if len(diff) > 0 {
		r.log.Info("reconcile applied", "changes", len(diff))
	}
	return diff, nil
}

func (r *Reconciler) mapped(ctx context.Context, g *guildState, kind, key string) *discordgo.Channel {
	o, err := r.st.GetDiscordObject(ctx, dbObj(kind, key))
	if err != nil {
		return nil
	}
	return g.byID[o.DiscordID]
}

func findByName(g *guildState, name string, types []discordgo.ChannelType, parentID *string) *discordgo.Channel {
	for _, c := range g.all {
		if !strings.EqualFold(c.Name, name) || !containsType(types, c.Type) {
			continue
		}
		if parentID != nil && c.ParentID != *parentID {
			continue
		}
		return c
	}
	return nil
}

func containsType(ts []discordgo.ChannelType, t discordgo.ChannelType) bool {
	for _, x := range ts {
		if x == t {
			return true
		}
	}
	return false
}

func (r *Reconciler) ensureCategory(ctx context.Context, g *guildState, spec CategorySpec, diff *[]string) (string, error) {
	cats := []discordgo.ChannelType{discordgo.ChannelTypeGuildCategory}
	c := r.mapped(ctx, g, objCategory, spec.Key)
	if c == nil {
		c = findByName(g, spec.Name, cats, nil)
		if c != nil {
			*diff = append(*diff, "adopted category "+spec.Name)
		}
	}
	if c == nil {
		created, err := r.api.GuildChannelCreateComplex(r.guildID, discordgo.GuildChannelCreateData{Name: spec.Name, Type: discordgo.ChannelTypeGuildCategory})
		if err != nil {
			return "", fmt.Errorf("create category %s: %w", spec.Name, err)
		}
		g.add(created)
		c = created
		*diff = append(*diff, "created category "+spec.Name)
	} else if c.Name != spec.Name {
		// Renamed app or restored from archive.
		if _, err := r.api.ChannelEdit(c.ID, &discordgo.ChannelEdit{Name: spec.Name}); err != nil {
			return "", fmt.Errorf("rename category %s: %w", c.Name, err)
		}
		*diff = append(*diff, fmt.Sprintf("renamed category %s → %s", c.Name, spec.Name))
		c.Name = spec.Name
	}
	return c.ID, saveObject(ctx, r.st, objCategory, spec.Key, c.ID)
}

func (r *Reconciler) ensureChannel(ctx context.Context, g *guildState, spec ChannelSpec, parentID string, diff *[]string) error {
	types := []discordgo.ChannelType{discordgo.ChannelTypeGuildText, discordgo.ChannelTypeGuildNews}
	c := r.mapped(ctx, g, objChannel, spec.Key)
	if c == nil {
		if c = findByName(g, spec.Name, types, &parentID); c == nil {
			c = findByName(g, spec.Name, types, nil)
		}
		if c != nil {
			*diff = append(*diff, "adopted #"+spec.Name)
		}
	}
	if c == nil {
		data := discordgo.GuildChannelCreateData{Name: spec.Name, Type: spec.Type, ParentID: parentID, Topic: spec.Topic}
		created, err := r.api.GuildChannelCreateComplex(r.guildID, data)
		if err != nil && spec.Type == discordgo.ChannelTypeGuildNews {
			// News channels require a Community server; fall back to a text channel.
			data.Type = discordgo.ChannelTypeGuildText
			created, err = r.api.GuildChannelCreateComplex(r.guildID, data)
			if err == nil {
				*diff = append(*diff, "#"+spec.Name+": announcement channel needs Community server, created as text")
			}
		}
		if err != nil {
			return fmt.Errorf("create #%s: %w", spec.Name, err)
		}
		g.add(created)
		*diff = append(*diff, "created #"+spec.Name)
		return saveObject(ctx, r.st, objChannel, spec.Key, created.ID)
	}
	edit := &discordgo.ChannelEdit{}
	changed := false
	if c.ParentID != parentID {
		edit.ParentID = parentID
		changed = true
		*diff = append(*diff, fmt.Sprintf("moved #%s into its category", spec.Name))
	}
	if c.Name != spec.Name {
		edit.Name = spec.Name
		changed = true
		*diff = append(*diff, fmt.Sprintf("renamed #%s → #%s", c.Name, spec.Name))
	}
	if changed {
		if _, err := r.api.ChannelEdit(c.ID, edit); err != nil {
			return fmt.Errorf("edit #%s: %w", c.Name, err)
		}
		c.ParentID, c.Name = parentID, spec.Name
	}
	return saveObject(ctx, r.st, objChannel, spec.Key, c.ID)
}

func (r *Reconciler) archiveRemoved(ctx context.Context, g *guildState, desired map[string]bool, diff *[]string) error {
	objs, err := r.st.ListDiscordObjects(ctx)
	if err != nil {
		return err
	}
	var archiveID string
	for _, o := range objs {
		if o.Kind != objChannel || !strings.HasPrefix(o.Key, "app:") {
			continue
		}
		slug := strings.Split(o.Key, ":")[1]
		c := g.byID[o.DiscordID]
		if desired[slug] || c == nil {
			continue
		}
		if archiveID == "" {
			if archiveID, err = r.ensureCategory(ctx, g, CategorySpec{Key: "archive", Name: "🗄 ARCHIVE"}, diff); err != nil {
				return err
			}
		}
		if c.ParentID == archiveID && strings.HasPrefix(c.Name, archivePrefix) {
			continue
		}
		name := c.Name
		if !strings.HasPrefix(name, archivePrefix) {
			name = archivePrefix + name
		}
		if _, err := r.api.ChannelEdit(c.ID, &discordgo.ChannelEdit{Name: name, ParentID: archiveID}); err != nil {
			return fmt.Errorf("archive #%s: %w", c.Name, err)
		}
		*diff = append(*diff, fmt.Sprintf("archived #%s → #%s", c.Name, name))
		c.Name, c.ParentID = name, archiveID
	}
	for _, o := range objs {
		if o.Kind != objCategory || !strings.HasPrefix(o.Key, "app:") {
			continue
		}
		c := g.byID[o.DiscordID]
		if desired[strings.TrimPrefix(o.Key, "app:")] || c == nil || strings.HasPrefix(c.Name, archivePrefix) {
			continue
		}
		name := archivePrefix + c.Name
		if _, err := r.api.ChannelEdit(c.ID, &discordgo.ChannelEdit{Name: name}); err != nil {
			return fmt.Errorf("archive category %s: %w", c.Name, err)
		}
		*diff = append(*diff, fmt.Sprintf("archived category %s → %s", c.Name, name))
		c.Name = name
	}
	return nil
}
