package discord

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/lioilsources/Ol1nDevBot/internal/config"
	"github.com/lioilsources/Ol1nDevBot/internal/events"
	"github.com/lioilsources/Ol1nDevBot/internal/store"
	"github.com/lioilsources/Ol1nDevBot/internal/store/db"
)

const statusBoardKey = "status_board"

// StatusBoard maintains one pinned, continuously edited message in #metrics
// with the current version / last deploy / 7-day downloads of every app.
type StatusBoard struct {
	api API
	st  *store.Store
	reg *config.Registry
	loc *time.Location
	log *slog.Logger
	mu  sync.Mutex
}

// NewStatusBoard creates the status board updater.
func NewStatusBoard(api API, st *store.Store, reg *config.Registry, loc *time.Location, log *slog.Logger) *StatusBoard {
	return &StatusBoard{api: api, st: st, reg: reg, loc: loc, log: log}
}

// Build renders the board embed from the database.
func (b *StatusBoard) Build(ctx context.Context) (*discordgo.MessageEmbed, error) {
	now := store.Now().In(b.loc)
	to := now.AddDate(0, 0, -1).Format(time.DateOnly)
	from := now.AddDate(0, 0, -7).Format(time.DateOnly)
	emb := &discordgo.MessageEmbed{
		Title:     "📋 Status board",
		Color:     ColorBlue,
		Footer:    &discordgo.MessageEmbedFooter{Text: "Aktualizováno"},
		Timestamp: now.Format(time.RFC3339),
	}
	apps := b.reg.Apps()
	if len(apps) == 0 {
		emb.Description = "Žádné appky v apps.yaml."
	}
	for i, a := range apps {
		if i == 25 {
			break
		}
		var lines []string
		var vers []string
		for _, s := range []string{events.StoreAppStore, events.StorePlayStore} {
			v, err := b.st.LatestAppVersion(ctx, db.LatestAppVersionParams{Store: s, AppSlug: a.Slug})
			if err == nil {
				label := "iOS"
				if s == events.StorePlayStore {
					label = "Android"
				}
				vers = append(vers, fmt.Sprintf("%s **%s**", label, v.Version))
			} else if !store.IsNotFound(err) {
				return nil, err
			}
		}
		if len(vers) == 0 {
			vers = append(vers, "zatím ve storu nevyšla")
		}
		lines = append(lines, strings.Join(vers, " · "))
		for _, svc := range a.Services {
			d, err := b.st.LastDeploy(ctx, svc)
			if store.IsNotFound(err) {
				continue
			} else if err != nil {
				return nil, err
			}
			icon := "✅"
			if d.Status != "success" {
				icon = "❌"
			}
			at := time.Unix(d.ReceivedAt, 0).In(b.loc)
			lines = append(lines, fmt.Sprintf("%s %s %s (%d. %d. %02d:%02d)", icon, svc, d.Version, at.Day(), at.Month(), at.Hour(), at.Minute()))
		}
		sum, err := b.st.SumMetrics(ctx, db.SumMetricsParams{AppSlug: a.Slug, Date: from, Date_2: to})
		if err != nil {
			return nil, err
		}
		if sum.Downloads > 0 || sum.PageViews > 0 {
			lines = append(lines, "Downloads 7d: "+FormatInt(sum.Downloads))
		}
		emb.Fields = append(emb.Fields, &discordgo.MessageEmbedField{Name: a.DisplayEmoji() + " " + a.Name, Value: trunc(strings.Join(lines, "\n"), maxFieldValue)})
	}
	return emb, nil
}

// Update edits (or creates and pins) the board message.
func (b *StatusBoard) Update(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	chID, err := ChannelID(ctx, b.st, events.TargetMetrics)
	if err != nil {
		return err
	}
	emb, err := b.Build(ctx)
	if err != nil {
		return err
	}
	embeds := []*discordgo.MessageEmbed{emb}
	if o, err := b.st.GetDiscordObject(ctx, dbObj(objMessage, statusBoardKey)); err == nil {
		_, err := b.api.ChannelMessageEditComplex(&discordgo.MessageEdit{ID: o.DiscordID, Channel: chID, Embeds: &embeds})
		if err == nil {
			return nil
		}
		if !IsNotFound(err) {
			return fmt.Errorf("edit status board: %w", err)
		}
		b.log.Info("status board message gone, recreating")
	}
	msg, err := b.api.ChannelMessageSendComplex(chID, &discordgo.MessageSend{Embeds: embeds, AllowedMentions: noMentions()})
	if err != nil {
		return fmt.Errorf("send status board: %w", err)
	}
	if err := b.api.ChannelMessagePin(chID, msg.ID); err != nil {
		b.log.Warn("pin status board", "err", err)
	}
	return saveObject(ctx, b.st, objMessage, statusBoardKey, msg.ID)
}
