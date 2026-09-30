package discord

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/lioilsources/Ol1nDevBot/internal/events"
	"github.com/lioilsources/Ol1nDevBot/internal/store"
	"github.com/lioilsources/Ol1nDevBot/internal/store/db"
)

const (
	publishBatch    = 20
	pollInterval    = 5 * time.Second
	maxAttempts     = 12
	baseBackoff     = 30 * time.Second
	maxBackoff      = time.Hour
	roleCacheMaxAge = 10 * time.Minute
)

// Publisher drains the outbox into Discord with retry and backoff.
// discordgo itself honours 429 Retry-After; anything else is retried here.
type Publisher struct {
	api     API
	st      *store.Store
	guildID string
	loc     *time.Location
	log     *slog.Logger
	notify  <-chan struct{}
	opsRole func() string

	// OnSent is called after an event has been delivered to a target.
	OnSent func(ctx context.Context, ev events.Event, target string)

	roleMu     sync.Mutex
	roleID     string
	roleLoaded time.Time
}

// NewPublisher creates a publisher. opsRole returns the configured ops role name.
func NewPublisher(api API, st *store.Store, guildID string, loc *time.Location, notify <-chan struct{}, opsRole func() string, log *slog.Logger) *Publisher {
	return &Publisher{api: api, st: st, guildID: guildID, loc: loc, notify: notify, opsRole: opsRole, log: log}
}

// Run publishes until ctx is cancelled.
func (p *Publisher) Run(ctx context.Context) {
	t := time.NewTicker(pollInterval)
	defer t.Stop()
	for {
		p.Drain(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-p.notify:
		}
	}
}

// Drain publishes all rows that are due now.
func (p *Publisher) Drain(ctx context.Context) {
	for ctx.Err() == nil {
		rows, err := p.st.DueOutbox(ctx, db.DueOutboxParams{NextAttemptAt: store.Unix(), Limit: publishBatch})
		if err != nil {
			p.log.Error("outbox query", "err", err)
			return
		}
		for _, row := range rows {
			p.deliver(ctx, row)
		}
		if len(rows) < publishBatch {
			return
		}
	}
}

func (p *Publisher) deliver(ctx context.Context, row db.Outbox) {
	ev, err := events.Decode(row.Kind, row.Payload)
	if err != nil {
		p.fail(ctx, row, err, true)
		return
	}
	chID, err := ChannelID(ctx, p.st, row.Target)
	if err != nil {
		p.fail(ctx, row, err, false)
		return
	}
	r, err := Render(ev, row.Target, RenderContext{Loc: p.loc, OpsRoleID: p.opsRoleID()})
	if err != nil {
		p.fail(ctx, row, err, true)
		return
	}
	var firstID string
	for i, m := range r.Messages {
		msg, err := p.api.ChannelMessageSendComplex(chID, m)
		if err != nil {
			p.fail(ctx, row, fmt.Errorf("send to %s: %w", row.Target, err), false)
			return
		}
		if i == 0 {
			firstID = msg.ID
		}
	}
	if firstID != "" {
		if r.Thread != "" {
			if _, err := p.api.MessageThreadStart(chID, firstID, r.Thread, threadArchive); err != nil {
				p.log.Warn("thread start failed", "target", row.Target, "err", err)
			}
		}
		if r.Crosspost {
			// Only works in announcement (news) channels; harmless failure otherwise.
			if _, err := p.api.ChannelMessageCrosspost(chID, firstID); err != nil {
				p.log.Debug("crosspost skipped", "err", err)
			}
		}
	}
	if err := p.st.MarkOutboxSent(ctx, db.MarkOutboxSentParams{ID: row.ID, MessageID: firstID, SentAt: nullInt(store.Unix())}); err != nil {
		p.log.Error("mark sent", "id", row.ID, "err", err)
	}
	p.log.Info("published", "kind", row.Kind, "target", row.Target)
	if p.OnSent != nil {
		p.OnSent(ctx, ev, row.Target)
	}
}

func (p *Publisher) fail(ctx context.Context, row db.Outbox, cause error, permanent bool) {
	attempts := row.Attempts + 1
	status := "pending"
	if permanent || attempts >= maxAttempts {
		status = "dead"
	}
	backoff := min(baseBackoff<<min(row.Attempts, 10), maxBackoff)
	p.log.Warn("publish failed", "kind", row.Kind, "target", row.Target, "attempt", attempts, "status", status, "err", cause)
	if err := p.st.MarkOutboxRetry(ctx, db.MarkOutboxRetryParams{
		ID: row.ID, NextAttemptAt: store.Now().Add(backoff).Unix(), LastError: truncErr(cause), Status: status,
	}); err != nil {
		p.log.Error("mark retry", "id", row.ID, "err", err)
	}
}

func (p *Publisher) opsRoleID() string {
	p.roleMu.Lock()
	defer p.roleMu.Unlock()
	if time.Since(p.roleLoaded) < roleCacheMaxAge {
		return p.roleID
	}
	p.roleLoaded = time.Now()
	name := p.opsRole()
	roles, err := p.api.GuildRoles(p.guildID)
	if err != nil {
		p.log.Warn("list roles", "err", err)
		return p.roleID
	}
	p.roleID = ""
	for _, r := range roles {
		if strings.EqualFold(r.Name, name) {
			p.roleID = r.ID
		}
	}
	return p.roleID
}

func truncErr(err error) string {
	s := err.Error()
	if len(s) > 500 {
		s = s[:500]
	}
	return s
}
