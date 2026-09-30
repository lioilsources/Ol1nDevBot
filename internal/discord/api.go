// Package discord publishes events to Discord, reconciles the guild layout
// with apps.yaml and handles slash commands.
package discord

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/bwmarrin/discordgo"

	"github.com/lioilsources/Ol1nDevBot/internal/store"
	"github.com/lioilsources/Ol1nDevBot/internal/store/db"
)

// API is the subset of *discordgo.Session the bot uses (faked in tests).
type API interface {
	ChannelMessageSendComplex(channelID string, data *discordgo.MessageSend, options ...discordgo.RequestOption) (*discordgo.Message, error)
	ChannelMessageEditComplex(m *discordgo.MessageEdit, options ...discordgo.RequestOption) (*discordgo.Message, error)
	ChannelMessagePin(channelID, messageID string, options ...discordgo.RequestOption) error
	ChannelMessageCrosspost(channelID, messageID string, options ...discordgo.RequestOption) (*discordgo.Message, error)
	MessageThreadStart(channelID, messageID, name string, archiveDuration int, options ...discordgo.RequestOption) (*discordgo.Channel, error)
	GuildChannels(guildID string, options ...discordgo.RequestOption) ([]*discordgo.Channel, error)
	GuildChannelCreateComplex(guildID string, data discordgo.GuildChannelCreateData, options ...discordgo.RequestOption) (*discordgo.Channel, error)
	ChannelEdit(channelID string, data *discordgo.ChannelEdit, options ...discordgo.RequestOption) (*discordgo.Channel, error)
	GuildRoles(guildID string, options ...discordgo.RequestOption) ([]*discordgo.Role, error)
}

var _ API = (*discordgo.Session)(nil)

// Mapping kinds in discord_objects.
const (
	objCategory = "category"
	objChannel  = "channel"
	objMessage  = "message"
)

// ChannelID resolves a logical target (e.g. "infra:deploys") to a channel ID.
func ChannelID(ctx context.Context, st *store.Store, target string) (string, error) {
	o, err := st.GetDiscordObject(ctx, db.GetDiscordObjectParams{Kind: objChannel, Key: target})
	if store.IsNotFound(err) {
		return "", fmt.Errorf("channel %q not reconciled yet", target)
	}
	if err != nil {
		return "", err
	}
	return o.DiscordID, nil
}

func saveObject(ctx context.Context, st *store.Store, kind, key, id string) error {
	return st.UpsertDiscordObject(ctx, db.UpsertDiscordObjectParams{Kind: kind, Key: key, DiscordID: id, UpdatedAt: store.Unix()})
}

// IsNotFound reports a Discord 404 (unknown channel/message).
func IsNotFound(err error) bool {
	if re, ok := err.(*discordgo.RESTError); ok && re.Response != nil {
		return re.Response.StatusCode == 404
	}
	return false
}

func dbObj(kind, key string) db.GetDiscordObjectParams {
	return db.GetDiscordObjectParams{Kind: kind, Key: key}
}

func nullInt(v int64) sql.NullInt64 { return sql.NullInt64{Int64: v, Valid: true} }
