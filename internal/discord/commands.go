package discord

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/lioilsources/Ol1nDevBot/internal/config"
	"github.com/lioilsources/Ol1nDevBot/internal/events"
	"github.com/lioilsources/Ol1nDevBot/internal/store"
	"github.com/lioilsources/Ol1nDevBot/internal/store/db"
)

// CommandDeps are the services slash commands call into. Nil functions mean
// the integration is disabled.
type CommandDeps struct {
	Reg          *config.Registry
	St           *store.Store
	Emit         events.Emitter
	Loc          *time.Location
	Log          *slog.Logger
	Sync         func(ctx context.Context) ([]string, error)
	ChangelogNow func(ctx context.Context, repo string) (events.ChangelogEvent, error)
	Metrics      func(ctx context.Context, app config.App, days int) (events.AppFigures, error)
	Status       func(ctx context.Context) (string, error)
}

// Commands registers and handles guild-scoped slash commands.
type Commands struct {
	s       *discordgo.Session
	appID   string
	guildID string
	deps    CommandDeps
	ctx     context.Context
}

// NewCommands creates the handler; ctx bounds background work of commands.
func NewCommands(ctx context.Context, s *discordgo.Session, appID, guildID string, deps CommandDeps) *Commands {
	return &Commands{s: s, appID: appID, guildID: guildID, deps: deps, ctx: ctx}
}

var adminOnly = int64(discordgo.PermissionAdministrator)

func sub(name, desc string, opts ...*discordgo.ApplicationCommandOption) *discordgo.ApplicationCommandOption {
	return &discordgo.ApplicationCommandOption{Type: discordgo.ApplicationCommandOptionSubCommand, Name: name, Description: desc, Options: opts}
}

func strOpt(name, desc string, required bool, choices ...string) *discordgo.ApplicationCommandOption {
	o := &discordgo.ApplicationCommandOption{Type: discordgo.ApplicationCommandOptionString, Name: name, Description: desc, Required: required}
	for _, c := range choices {
		o.Choices = append(o.Choices, &discordgo.ApplicationCommandOptionChoice{Name: c, Value: c})
	}
	return o
}

// Definitions returns the slash command set.
func Definitions() []*discordgo.ApplicationCommand {
	return []*discordgo.ApplicationCommand{
		{Name: "app", Description: "Správa appek", DefaultMemberPermissions: &adminOnly, Options: []*discordgo.ApplicationCommandOption{
			sub("add", "Přidat appku (formulář)", strOpt("slug", "slug, např. storyteller", true)),
			sub("remove", "Odebrat appku přidanou přes /app add", strOpt("slug", "slug appky", true)),
			sub("sync", "Znovu načíst apps.yaml a srovnat kanály"),
			sub("list", "Seznam appek a verzí"),
		}},
		{Name: "metrics", Description: "Metriky appky", DefaultMemberPermissions: &adminOnly, Options: []*discordgo.ApplicationCommandOption{
			strOpt("slug", "slug appky", true),
			strOpt("period", "období", false, "7d", "30d"),
		}},
		{Name: "changelog", Description: "GitHub changelog", DefaultMemberPermissions: &adminOnly, Options: []*discordgo.ApplicationCommandOption{
			sub("now", "Vygenerovat changelog za posledních 7 dní", strOpt("repo", "jen jeden repozitář (owner/name)", false)),
		}},
		{Name: "deploy", Description: "Deploye", DefaultMemberPermissions: &adminOnly, Options: []*discordgo.ApplicationCommandOption{
			sub("last", "Poslední deploy služby", strOpt("service", "název služby z deploy hooku", true)),
		}},
		{Name: "bot", Description: "Ol1nBot", DefaultMemberPermissions: &adminOnly, Options: []*discordgo.ApplicationCommandOption{
			sub("status", "Zdraví pollerů a outboxu"),
		}},
	}
}

// Register overwrites the guild commands (guild scope = instant propagation).
func (c *Commands) Register() error {
	_, err := c.s.ApplicationCommandBulkOverwrite(c.appID, c.guildID, Definitions())
	return err
}

// Handle is the discordgo InteractionCreate handler.
func (c *Commands) Handle(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.GuildID != c.guildID {
		return
	}
	switch i.Type {
	case discordgo.InteractionApplicationCommand:
		c.handleCommand(i)
	case discordgo.InteractionModalSubmit:
		c.handleModal(i)
	}
}

func (c *Commands) handleCommand(i *discordgo.InteractionCreate) {
	data := i.ApplicationCommandData()
	opts := data.Options
	subName := ""
	if len(opts) == 1 && opts[0].Type == discordgo.ApplicationCommandOptionSubCommand {
		subName, opts = opts[0].Name, opts[0].Options
	}
	args := map[string]string{}
	for _, o := range opts {
		args[o.Name] = o.StringValue()
	}
	c.deps.Log.Info("slash command", "cmd", data.Name, "sub", subName, "user", userName(i))

	switch data.Name + " " + subName {
	case "app add":
		c.appAdd(i, args["slug"])
	case "app remove":
		c.deferred(i, func(ctx context.Context) (string, []*discordgo.MessageEmbed, error) {
			return c.appRemove(ctx, args["slug"])
		})
	case "app sync":
		c.deferred(i, func(ctx context.Context) (string, []*discordgo.MessageEmbed, error) {
			diff, err := c.deps.Sync(ctx)
			if err != nil {
				return "", nil, err
			}
			return "🔄 Sync hotový.\n" + diffText(diff), nil, nil
		})
	case "app list":
		c.deferred(i, c.appList)
	case "metrics ":
		c.deferred(i, func(ctx context.Context) (string, []*discordgo.MessageEmbed, error) {
			return c.metrics(ctx, args["slug"], args["period"])
		})
	case "changelog now":
		if c.deps.ChangelogNow == nil {
			c.reply(i, "GitHub integrace není nakonfigurovaná (`GITHUB_TOKEN`).", true)
			return
		}
		c.deferred(i, func(ctx context.Context) (string, []*discordgo.MessageEmbed, error) {
			ev, err := c.deps.ChangelogNow(ctx, args["repo"])
			if err != nil {
				return "", nil, err
			}
			return fmt.Sprintf("📝 Changelog odeslán do #changelog (%d repozitářů s aktivitou).", len(ev.Repos)), nil, nil
		})
	case "deploy last":
		c.deferred(i, func(ctx context.Context) (string, []*discordgo.MessageEmbed, error) {
			return c.deployLast(ctx, args["service"])
		})
	case "bot status":
		c.deferred(i, func(ctx context.Context) (string, []*discordgo.MessageEmbed, error) {
			s, err := c.deps.Status(ctx)
			if err != nil {
				return "", nil, err
			}
			return "", []*discordgo.MessageEmbed{{Title: "🤖 Ol1nBot status", Description: trunc(s, maxEmbedDesc), Color: ColorBlue}}, nil
		})
	default:
		c.reply(i, "Neznámý příkaz.", true)
	}
}

// deferred acknowledges within 3 s and edits the response when fn finishes.
func (c *Commands) deferred(i *discordgo.InteractionCreate, fn func(ctx context.Context) (string, []*discordgo.MessageEmbed, error)) {
	if err := c.s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseDeferredChannelMessageWithSource}); err != nil {
		c.deps.Log.Error("defer interaction", "err", err)
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(c.ctx, 5*time.Minute)
		defer cancel()
		content, embeds, err := fn(ctx)
		if err != nil {
			c.deps.Log.Warn("command failed", "err", err)
			content, embeds = "❌ "+trunc(err.Error(), 1800), nil
		}
		if content == "" && len(embeds) == 0 {
			content = "✅"
		}
		edit := &discordgo.WebhookEdit{Content: &content, AllowedMentions: noMentions()}
		if embeds != nil {
			edit.Embeds = &embeds
		}
		if _, err := c.s.InteractionResponseEdit(i.Interaction, edit); err != nil {
			c.deps.Log.Error("edit interaction response", "err", err)
		}
	}()
}

func (c *Commands) reply(i *discordgo.InteractionCreate, content string, ephemeral bool) {
	data := &discordgo.InteractionResponseData{Content: content, AllowedMentions: noMentions()}
	if ephemeral {
		data.Flags = discordgo.MessageFlagsEphemeral
	}
	if err := c.s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseChannelMessageWithSource, Data: data}); err != nil {
		c.deps.Log.Error("reply interaction", "err", err)
	}
}

const modalAppAdd = "app_add:"

func textInput(id, label, placeholder string, required bool) discordgo.ActionsRow {
	return discordgo.ActionsRow{Components: []discordgo.MessageComponent{discordgo.TextInput{
		CustomID: id, Label: label, Placeholder: placeholder, Style: discordgo.TextInputShort, Required: required, MaxLength: 200,
	}}}
}

func (c *Commands) appAdd(i *discordgo.InteractionCreate, slug string) {
	slug = strings.ToLower(strings.TrimSpace(slug))
	if err := (config.App{Slug: slug, Name: "x"}).Validate(); err != nil {
		c.reply(i, "❌ "+err.Error(), true)
		return
	}
	if _, ok := c.deps.Reg.App(slug); ok {
		c.reply(i, fmt.Sprintf("Appka `%s` už existuje.", slug), true)
		return
	}
	err := c.s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseModal,
		Data: &discordgo.InteractionResponseData{
			CustomID: modalAppAdd + slug,
			Title:    "Nová appka: " + slug,
			Components: []discordgo.MessageComponent{
				textInput("name", "Název", "Storyteller", true),
				textInput("bundle_id", "iOS bundle id", "com.ol1n.storyteller", false),
				textInput("asc_app_id", "App Store Connect app id", "6741234567", false),
				textInput("package", "Android package", "com.ol1n.storyteller", false),
				textInput("repos", "GitHub repos (čárkou)", "lioilsources/storyteller, lioilsources/storyteller-api", false),
			},
		},
	})
	if err != nil {
		c.deps.Log.Error("open modal", "err", err)
	}
}

func (c *Commands) handleModal(i *discordgo.InteractionCreate) {
	data := i.ModalSubmitData()
	if !strings.HasPrefix(data.CustomID, modalAppAdd) {
		return
	}
	slug := strings.TrimPrefix(data.CustomID, modalAppAdd)
	fields := map[string]string{}
	for _, row := range data.Components {
		if r, ok := row.(*discordgo.ActionsRow); ok {
			for _, comp := range r.Components {
				if ti, ok := comp.(*discordgo.TextInput); ok {
					fields[ti.CustomID] = strings.TrimSpace(ti.Value)
				}
			}
		}
	}
	app := config.App{Slug: slug, Name: fields["name"]}
	if fields["bundle_id"] != "" {
		app.IOS = &config.IOS{BundleID: fields["bundle_id"], ASCAppID: fields["asc_app_id"]}
	}
	if fields["package"] != "" {
		app.Android = &config.Android{Package: fields["package"]}
	}
	for _, r := range strings.Split(fields["repos"], ",") {
		if r = strings.TrimSpace(r); r != "" {
			app.Repos = append(app.Repos, r)
		}
	}
	c.deferred(i, func(ctx context.Context) (string, []*discordgo.MessageEmbed, error) {
		if err := app.Validate(); err != nil {
			return "", nil, err
		}
		y, err := config.MarshalAppYAML(app)
		if err != nil {
			return "", nil, err
		}
		if err := c.deps.St.UpsertAppOverride(ctx, db.UpsertAppOverrideParams{Slug: slug, Yaml: y, CreatedBy: userName(i), CreatedAt: store.Unix()}); err != nil {
			return "", nil, err
		}
		diff, err := c.deps.Sync(ctx)
		if err != nil {
			return "", nil, err
		}
		snippet := "Přidej do `apps.yaml` v repu (pod `apps:`):\n```yaml\n" + indentYAML(y) + "```"
		ev := events.BotLogEvent{Level: "info", Title: "Appka " + app.Name + " přidána přes /app add", Message: snippet, At: store.Now()}
		if _, err := c.deps.Emit.Emit(ctx, ev); err != nil {
			c.deps.Log.Warn("emit app add snippet", "err", err)
		}
		return fmt.Sprintf("✅ Appka **%s** přidána. YAML snippet je v #bot-log.\n%s", app.Name, diffText(diff)), nil, nil
	})
}

func (c *Commands) appRemove(ctx context.Context, slug string) (string, []*discordgo.MessageEmbed, error) {
	n, err := c.deps.St.DeleteAppOverride(ctx, slug)
	if err != nil {
		return "", nil, err
	}
	if n == 0 {
		if _, ok := c.deps.Reg.App(slug); ok {
			return fmt.Sprintf("`%s` je v apps.yaml — odeber ji tam a dej `/app sync`.", slug), nil, nil
		}
		return fmt.Sprintf("Appka `%s` neexistuje.", slug), nil, nil
	}
	diff, err := c.deps.Sync(ctx)
	if err != nil {
		return "", nil, err
	}
	return fmt.Sprintf("🗄 Appka `%s` odebrána (kanály archivovány).\n%s", slug, diffText(diff)), nil, nil
}

func (c *Commands) appList(ctx context.Context) (string, []*discordgo.MessageEmbed, error) {
	apps := c.deps.Reg.Apps()
	if len(apps) == 0 {
		return "Žádné appky.", nil, nil
	}
	var b strings.Builder
	for _, a := range apps {
		fmt.Fprintf(&b, "%s **%s** `%s`", a.DisplayEmoji(), a.Name, a.Slug)
		for _, s := range []string{events.StoreAppStore, events.StorePlayStore} {
			v, err := c.deps.St.LatestAppVersion(ctx, db.LatestAppVersionParams{Store: s, AppSlug: a.Slug})
			if err == nil {
				fmt.Fprintf(&b, " · %s %s", events.StoreLabel(s), v.Version)
			} else if !store.IsNotFound(err) {
				return "", nil, err
			}
		}
		b.WriteString("\n")
	}
	return "", []*discordgo.MessageEmbed{{Title: "📱 Appky", Description: trunc(b.String(), maxEmbedDesc), Color: ColorBlue}}, nil
}

func (c *Commands) metrics(ctx context.Context, slug, period string) (string, []*discordgo.MessageEmbed, error) {
	app, ok := c.deps.Reg.App(slug)
	if !ok {
		return fmt.Sprintf("Appka `%s` neexistuje.", slug), nil, nil
	}
	days := 7
	if period == "30d" {
		days = 30
	}
	af, err := c.deps.Metrics(ctx, app, days)
	if err != nil {
		return "", nil, err
	}
	emb := appMetricsEmbed(af)
	emb.Title = fmt.Sprintf("📊 %s — posledních %d dní", strings.TrimSpace(af.Emoji+" "+af.AppName), days)
	emb.Fields[0].Name = fmt.Sprintf("Downloads (%dd)", days)
	return "", []*discordgo.MessageEmbed{emb}, nil
}

func (c *Commands) deployLast(ctx context.Context, service string) (string, []*discordgo.MessageEmbed, error) {
	d, err := c.deps.St.LastDeploy(ctx, service)
	if store.IsNotFound(err) {
		return fmt.Sprintf("Žádný deploy služby `%s`.", service), nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	r := renderDeploy(events.DeployEvent{
		Service: d.Service, Env: d.Env, Version: d.Version, Commit: d.CommitSha, URL: d.Url,
		Status: d.Status, Notes: d.Notes, DurationSec: d.DurationSec, At: time.Unix(d.ReceivedAt, 0),
	}, "", RenderContext{Loc: c.deps.Loc})
	return "", r.Messages[0].Embeds, nil
}

func diffText(diff []string) string {
	if len(diff) == 0 {
		return "Beze změn."
	}
	return trunc("• "+strings.Join(diff, "\n• "), 1500)
}

func indentYAML(y string) string {
	var b strings.Builder
	for _, line := range strings.SplitAfter(y, "\n") {
		if strings.TrimSpace(line) != "" {
			b.WriteString("  " + line)
		}
	}
	return b.String()
}

func userName(i *discordgo.InteractionCreate) string {
	if i.Member != nil && i.Member.User != nil {
		return i.Member.User.Username
	}
	if i.User != nil {
		return i.User.Username
	}
	return ""
}
