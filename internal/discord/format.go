package discord

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"

	"github.com/lioilsources/Ol1nDevBot/internal/events"
)

// Embed colors.
const (
	ColorGreen  = 0x2ECC71
	ColorRed    = 0xE74C3C
	ColorGold   = 0xF1C40F
	ColorBlue   = 0x3498DB
	ColorOrange = 0xE67E22
	ColorGray   = 0x95A5A6
	ColorPurple = 0x9B59B6
)

// Discord limits.
const (
	maxContent     = 2000
	maxEmbedTitle  = 256
	maxEmbedDesc   = 4096
	maxFieldValue  = 1024
	maxEmbeds      = 10
	maxThreadName  = 100
	threadArchive  = 10080 // minutes (7 days)
	deployFooter   = "olin.now"
	changelogTitle = "📝 Changelog"
)

// RenderContext carries per-guild data needed while rendering.
type RenderContext struct {
	Loc       *time.Location
	OpsRoleID string
}

// Rendered is what the publisher sends for one outbox row.
type Rendered struct {
	Messages []*discordgo.MessageSend
	// Thread, when set, starts a thread on the first message.
	Thread string
	// Crosspost publishes the first message to following servers (news channels).
	Crosspost bool
}

// Render turns an event into Discord messages for the given target.
func Render(ev events.Event, target string, rc RenderContext) (Rendered, error) {
	if rc.Loc == nil {
		rc.Loc = time.UTC
	}
	switch e := ev.(type) {
	case events.DeployEvent:
		return renderDeploy(e, target, rc), nil
	case events.StoreVersionEvent:
		return renderStoreVersion(e), nil
	case events.VersionStateEvent:
		return one(versionStateEmbed(e)), nil
	case events.NewAppEvent:
		return renderNewApp(e), nil
	case events.MetricsEvent:
		return renderMetrics(e, target, rc), nil
	case events.ChangelogEvent:
		return renderChangelog(e, rc), nil
	case events.MilestoneEvent:
		return renderMilestone(e, target), nil
	case events.BotLogEvent:
		return one(botLogEmbed(e)), nil
	}
	return Rendered{}, fmt.Errorf("no renderer for %T", ev)
}

func one(e *discordgo.MessageEmbed) Rendered {
	return Rendered{Messages: []*discordgo.MessageSend{{Embeds: []*discordgo.MessageEmbed{e}, AllowedMentions: noMentions()}}}
}

func noMentions() *discordgo.MessageAllowedMentions {
	return &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}}
}

func renderDeploy(e events.DeployEvent, target string, rc RenderContext) Rendered {
	icon, color := "🚀", ColorGreen
	if e.Failed() {
		icon, color = "❌", ColorRed
	}
	emb := &discordgo.MessageEmbed{
		Title:     trunc(fmt.Sprintf("%s %s %s → %s", icon, e.Service, e.Version, e.Env), maxEmbedTitle),
		URL:       e.URL,
		Color:     color,
		Footer:    &discordgo.MessageEmbedFooter{Text: deployFooter},
		Timestamp: e.At.Format(time.RFC3339),
	}
	if e.Commit != "" {
		v := "`" + shortSHA(e.Commit) + "`"
		if e.CommitURL != "" {
			v = fmt.Sprintf("[`%s`](%s)", shortSHA(e.Commit), e.CommitURL)
		}
		emb.Fields = append(emb.Fields, &discordgo.MessageEmbedField{Name: "Commit", Value: v, Inline: true})
	}
	if e.DurationSec > 0 {
		emb.Fields = append(emb.Fields, &discordgo.MessageEmbedField{Name: "Duration", Value: formatDuration(time.Duration(e.DurationSec) * time.Second), Inline: true})
	}
	emb.Fields = append(emb.Fields, &discordgo.MessageEmbedField{Name: "Status", Value: e.Status, Inline: true})
	if e.Notes != "" {
		emb.Fields = append(emb.Fields, &discordgo.MessageEmbedField{Name: "Notes", Value: trunc(e.Notes, maxFieldValue)})
	}
	msg := &discordgo.MessageSend{Embeds: []*discordgo.MessageEmbed{emb}, AllowedMentions: noMentions()}
	if e.Failed() && target == events.TargetDeploys && rc.OpsRoleID != "" {
		msg.Content = "<@&" + rc.OpsRoleID + "> deploy selhal"
		msg.AllowedMentions = &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}, Roles: []string{rc.OpsRoleID}}
	}
	return Rendered{Messages: []*discordgo.MessageSend{msg}}
}

func renderStoreVersion(e events.StoreVersionEvent) Rendered {
	emb := &discordgo.MessageEmbed{
		Title: trunc(fmt.Sprintf("📱 %s %s — %s", e.AppName, e.Version, events.StoreLabel(e.Store)), maxEmbedTitle),
		URL:   e.StoreURL,
		Color: ColorGreen,
		Fields: []*discordgo.MessageEmbedField{
			{Name: "Stav", Value: "🟢 Live", Inline: true},
		},
	}
	if e.Build != "" {
		emb.Fields = append(emb.Fields, &discordgo.MessageEmbedField{Name: "Build", Value: e.Build, Inline: true})
	}
	if !e.ReleaseDate.IsZero() {
		emb.Timestamp = e.ReleaseDate.Format(time.RFC3339)
	}
	if e.IconURL != "" {
		emb.Thumbnail = &discordgo.MessageEmbedThumbnail{URL: e.IconURL}
	}
	if notes := strings.TrimSpace(e.ReleaseNotes); notes != "" {
		emb.Fields = append(emb.Fields, &discordgo.MessageEmbedField{Name: "Release notes", Value: trunc(notes, maxFieldValue)})
	}
	msg := &discordgo.MessageSend{Embeds: []*discordgo.MessageEmbed{emb}, AllowedMentions: noMentions()}
	if e.StoreURL != "" {
		msg.Components = linkButtons(linkButton{"Otevřít v storu", e.StoreURL})
	}
	return Rendered{
		Messages: []*discordgo.MessageSend{msg},
		Thread:   trunc(fmt.Sprintf("%s %s", e.AppName, e.Version), maxThreadName),
	}
}

var stateLabels = map[string]string{
	"PREPARE_FOR_SUBMISSION":        "Příprava",
	"WAITING_FOR_REVIEW":            "Čeká na review",
	"IN_REVIEW":                     "V review",
	"PENDING_DEVELOPER_RELEASE":     "Schváleno, čeká na vydání",
	"PENDING_APPLE_RELEASE":         "Schváleno, čeká na Apple",
	"PROCESSING_FOR_APP_STORE":      "Zpracovává se",
	"READY_FOR_SALE":                "Live",
	"REJECTED":                      "Zamítnuto",
	"METADATA_REJECTED":             "Metadata zamítnuta",
	"DEVELOPER_REJECTED":            "Staženo vývojářem",
	"DEVELOPER_REMOVED_FROM_SALE":   "Staženo z prodeje",
	"completed":                     "Vydáno 100 %",
	"inProgress":                    "Postupný rollout",
	"halted":                        "Rollout zastaven",
	"draft":                         "Koncept",
	"INVALID_BINARY":                "Neplatný binár",
	"REPLACED_WITH_NEW_VERSION":     "Nahrazeno novou verzí",
	"WAITING_FOR_EXPORT_COMPLIANCE": "Čeká na export compliance",
}

// StateLabel returns a Czech label for an ASC / Play state.
func StateLabel(s string) string {
	if l, ok := stateLabels[s]; ok {
		return l
	}
	return s
}

func versionStateEmbed(e events.VersionStateEvent) *discordgo.MessageEmbed {
	color := ColorBlue
	switch e.State {
	case "READY_FOR_SALE", "completed", "PENDING_DEVELOPER_RELEASE":
		color = ColorGreen
	case "REJECTED", "METADATA_REJECTED", "INVALID_BINARY", "halted":
		color = ColorRed
	case "WAITING_FOR_REVIEW", "IN_REVIEW", "inProgress":
		color = ColorOrange
	}
	label := StateLabel(e.State)
	if e.Detail != "" {
		label += " (" + e.Detail + ")"
	}
	emb := &discordgo.MessageEmbed{
		Title: trunc(fmt.Sprintf("🔄 %s %s — %s: %s", e.AppName, e.Version, events.StoreLabel(e.Store), label), maxEmbedTitle),
		Color: color,
	}
	if e.PrevState != "" {
		emb.Description = fmt.Sprintf("%s → **%s**", StateLabel(e.PrevState), label)
	}
	return emb
}

func renderNewApp(e events.NewAppEvent) Rendered {
	emb := &discordgo.MessageEmbed{
		Title:       trunc(fmt.Sprintf("🎉 Nová appka: %s %s", e.Emoji, e.AppName), maxEmbedTitle),
		Description: trunc(e.Description, 1000),
		Color:       ColorPurple,
	}
	if e.IconURL != "" {
		emb.Thumbnail = &discordgo.MessageEmbedThumbnail{URL: e.IconURL}
	}
	var links []linkButton
	if e.AppStoreURL != "" {
		links = append(links, linkButton{"App Store", e.AppStoreURL})
	}
	if e.PlayStoreURL != "" {
		links = append(links, linkButton{"Google Play", e.PlayStoreURL})
	}
	msg := &discordgo.MessageSend{Embeds: []*discordgo.MessageEmbed{emb}, Components: linkButtons(links...), AllowedMentions: noMentions()}
	if e.Everyone {
		msg.Content = "@everyone"
		msg.AllowedMentions = &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{discordgo.AllowedMentionTypeEveryone}}
	}
	return Rendered{Messages: []*discordgo.MessageSend{msg}, Crosspost: true}
}

func renderMilestone(e events.MilestoneEvent, target string) Rendered {
	emb := &discordgo.MessageEmbed{
		Title:       trunc(fmt.Sprintf("🏆 %s: %s", e.AppName, e.Title), maxEmbedTitle),
		Description: e.Detail,
		Color:       ColorGold,
	}
	msg := &discordgo.MessageSend{Embeds: []*discordgo.MessageEmbed{emb}, AllowedMentions: noMentions()}
	r := Rendered{Messages: []*discordgo.MessageSend{msg}}
	if target == events.TargetAnnouncements {
		r.Crosspost = true
		if e.Everyone {
			msg.Content = "@everyone"
			msg.AllowedMentions = &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{discordgo.AllowedMentionTypeEveryone}}
		}
	}
	return r
}

func renderMetrics(e events.MetricsEvent, target string, rc RenderContext) Rendered {
	if e.Period == "daily" {
		return Rendered{Messages: []*discordgo.MessageSend{{Content: trunc(dailyLine(e), maxContent), AllowedMentions: noMentions()}}}
	}
	from, _ := time.ParseInLocation(time.DateOnly, e.From, rc.Loc)
	to, _ := time.ParseInLocation(time.DateOnly, e.To, rc.Loc)
	period := FormatDateRange(from, to)

	if target != events.TargetMetrics {
		for _, a := range e.Apps {
			if events.AppMetricsTarget(a.AppSlug) == target {
				emb := appMetricsEmbed(a)
				emb.Title = fmt.Sprintf("📊 %s — týden %s", a.AppName, period)
				return one(emb)
			}
		}
		return Rendered{}
	}

	var cur, prev events.Figures
	for _, a := range e.Apps {
		cur, prev = cur.Add(a.Current), prev.Add(a.Previous)
	}
	summary := &discordgo.MessageEmbed{
		Title: "📊 Metriky — týden " + period,
		Color: ColorBlue,
		Fields: []*discordgo.MessageEmbedField{
			{Name: "Downloads celkem", Value: fmt.Sprintf("**%s** %s", FormatInt(cur.Downloads), Delta(cur.Downloads, prev.Downloads)), Inline: true},
			{Name: "Page views", Value: FormatInt(cur.PageViews), Inline: true},
			{Name: "Appek", Value: strconv.Itoa(len(e.Apps)), Inline: true},
		},
	}
	embeds := []*discordgo.MessageEmbed{summary}
	for i, a := range e.Apps {
		if len(embeds) == maxEmbeds {
			summary.Footer = &discordgo.MessageEmbedFooter{Text: fmt.Sprintf("+ %d dalších appek v per-app kanálech", len(e.Apps)-i)}
			break
		}
		embeds = append(embeds, appMetricsEmbed(a))
	}
	return Rendered{Messages: []*discordgo.MessageSend{{Embeds: embeds, AllowedMentions: noMentions()}}}
}

func appMetricsEmbed(a events.AppFigures) *discordgo.MessageEmbed {
	c, p := a.Current, a.Previous
	emb := &discordgo.MessageEmbed{
		Title: strings.TrimSpace(a.Emoji + " " + a.AppName),
		Color: ColorBlue,
		Fields: []*discordgo.MessageEmbedField{
			{Name: "Downloads (7d)", Value: fmt.Sprintf("**%s** %s", FormatInt(c.Downloads), Delta(c.Downloads, p.Downloads)), Inline: true},
			{Name: "Impressions", Value: fmt.Sprintf("%s %s", FormatInt(c.Impressions), Delta(c.Impressions, p.Impressions)), Inline: true},
			{Name: "Page views", Value: fmt.Sprintf("%s %s", FormatInt(c.PageViews), Delta(c.PageViews, p.PageViews)), Inline: true},
			{Name: "CVR", Value: CVR(c.Downloads, c.PageViews), Inline: true},
		},
	}
	if c.Uninstalls > 0 {
		emb.Fields = append(emb.Fields, &discordgo.MessageEmbedField{Name: "Odinstalace", Value: FormatInt(c.Uninstalls), Inline: true})
	}
	if len(a.ByStore) > 0 {
		var parts []string
		for _, s := range []string{events.StoreAppStore, events.StorePlayStore} {
			if f, ok := a.ByStore[s]; ok {
				parts = append(parts, fmt.Sprintf("%s %s", events.StoreLabel(s), FormatInt(f.Downloads)))
			}
		}
		if len(parts) > 0 {
			emb.Footer = &discordgo.MessageEmbedFooter{Text: strings.Join(parts, " · ")}
		}
	}
	return emb
}

func dailyLine(e events.MetricsEvent) string {
	d, _ := time.Parse(time.DateOnly, e.To)
	var parts []string
	for _, a := range e.Apps {
		p := fmt.Sprintf("%s %s **%s**", a.Emoji, a.AppName, FormatInt(a.Current.Downloads))
		ios, iosOK := a.ByStore[events.StoreAppStore]
		and, andOK := a.ByStore[events.StorePlayStore]
		if iosOK && andOK {
			p += fmt.Sprintf(" (iOS %s · Android %s)", FormatInt(ios.Downloads), FormatInt(and.Downloads))
		}
		parts = append(parts, strings.TrimSpace(p))
	}
	if len(parts) == 0 {
		parts = append(parts, "žádná data")
	}
	return fmt.Sprintf("📊 **%d. %d.** stažení: %s", d.Day(), d.Month(), strings.Join(parts, " · "))
}

func renderChangelog(e events.ChangelogEvent, rc RenderContext) Rendered {
	header := fmt.Sprintf("# %s %s", changelogTitle, FormatDateRange(e.From.In(rc.Loc), e.To.In(rc.Loc)))
	var b strings.Builder
	b.WriteString(header)
	if len(e.Repos) == 0 {
		b.WriteString("\n\nTento týden žádná aktivita. 😴")
	}
	for _, r := range e.Repos {
		fmt.Fprintf(&b, "\n\n## [%s](<https://github.com/%s>)\n%s", r.Repo, r.Repo, strings.TrimSpace(r.Markdown))
	}
	var msgs []*discordgo.MessageSend
	for _, chunk := range SplitMessage(b.String(), maxContent) {
		msgs = append(msgs, &discordgo.MessageSend{Content: chunk, AllowedMentions: noMentions(), Flags: discordgo.MessageFlagsSuppressEmbeds})
	}
	return Rendered{Messages: msgs}
}

func botLogEmbed(e events.BotLogEvent) *discordgo.MessageEmbed {
	color, icon := ColorGray, "ℹ️"
	switch e.Level {
	case "error":
		color, icon = ColorRed, "🔴"
	case "warn":
		color, icon = ColorOrange, "🟠"
	case "ok":
		color, icon = ColorGreen, "🟢"
	}
	return &discordgo.MessageEmbed{
		Title:       trunc(icon+" "+e.Title, maxEmbedTitle),
		Description: trunc(e.Message, maxEmbedDesc),
		Color:       color,
		Timestamp:   e.At.Format(time.RFC3339),
	}
}

type linkButton struct{ label, url string }

func linkButtons(links ...linkButton) []discordgo.MessageComponent {
	if len(links) == 0 {
		return nil
	}
	row := discordgo.ActionsRow{}
	for _, l := range links {
		row.Components = append(row.Components, discordgo.Button{Label: l.label, Style: discordgo.LinkButton, URL: l.url})
	}
	return []discordgo.MessageComponent{row}
}

// SplitMessage splits text into chunks of at most limit runes, preferring
// paragraph and line boundaries.
func SplitMessage(s string, limit int) []string {
	var out []string
	for utf8.RuneCountInString(s) > limit {
		cut := byteIndexOfRune(s, limit)
		head := s[:cut]
		if i := strings.LastIndex(head, "\n\n"); i > cut/2 {
			cut = i
		} else if i := strings.LastIndex(head, "\n"); i > cut/2 {
			cut = i
		} else if i := strings.LastIndex(head, " "); i > cut/2 {
			cut = i
		}
		out = append(out, strings.TrimRight(s[:cut], " \n"))
		s = strings.TrimLeft(s[cut:], " \n")
	}
	if strings.TrimSpace(s) != "" {
		out = append(out, s)
	}
	return out
}

func byteIndexOfRune(s string, n int) int {
	i := 0
	for pos := range s {
		if i == n {
			return pos
		}
		i++
	}
	return len(s)
}

func trunc(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return s[:byteIndexOfRune(s, n-1)] + "…"
}

func shortSHA(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}

func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm %02ds", int(d.Minutes()), int(d.Seconds())%60)
}

// FormatInt formats with a non-breaking space as the thousands separator (1 000).
func FormatInt(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteRune(' ')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// Delta formats week-over-week change, e.g. "(▲ 25 %)".
func Delta(cur, prev int64) string {
	if prev == 0 {
		if cur == 0 {
			return ""
		}
		return "(nové)"
	}
	pct := (float64(cur) - float64(prev)) / float64(prev) * 100
	arrow := "▲"
	if pct < 0 {
		arrow = "▼"
	}
	if math.Abs(pct) < 0.5 {
		return "(= 0 %)"
	}
	return fmt.Sprintf("(%s %d %%)", arrow, int(math.Round(math.Abs(pct))))
}

// CVR is the store listing conversion rate (downloads / page views).
func CVR(downloads, pageViews int64) string {
	if pageViews == 0 {
		return "–"
	}
	return strings.Replace(fmt.Sprintf("%.1f %%", float64(downloads)/float64(pageViews)*100), ".", ",", 1)
}

// FormatDateRange renders a Czech date range: "22.–28. 9. 2026".
func FormatDateRange(from, to time.Time) string {
	switch {
	case from.Year() != to.Year():
		return fmt.Sprintf("%d. %d. %d–%d. %d. %d", from.Day(), from.Month(), from.Year(), to.Day(), to.Month(), to.Year())
	case from.Month() != to.Month():
		return fmt.Sprintf("%d. %d.–%d. %d. %d", from.Day(), from.Month(), to.Day(), to.Month(), to.Year())
	case from.Day() != to.Day():
		return fmt.Sprintf("%d.–%d. %d. %d", from.Day(), to.Day(), to.Month(), to.Year())
	}
	return fmt.Sprintf("%d. %d. %d", to.Day(), to.Month(), to.Year())
}
