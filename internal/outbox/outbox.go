// Package outbox persists events before publication so restarts lose nothing,
// and deduplicates them by their deterministic key.
package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"

	"github.com/lioilsources/Ol1nDevBot/internal/events"
	"github.com/lioilsources/Ol1nDevBot/internal/store"
	"github.com/lioilsources/Ol1nDevBot/internal/store/db"
)

// Subscriber is called in-process for every newly emitted event
// (e.g. the milestone engine reacting to versions and metrics).
type Subscriber func(ctx context.Context, ev events.Event)

// Outbox implements events.Emitter on top of the outbox table.
type Outbox struct {
	st     *store.Store
	log    *slog.Logger
	notify chan struct{}

	mu   sync.RWMutex
	subs []Subscriber
}

// New creates an outbox.
func New(st *store.Store, log *slog.Logger) *Outbox {
	return &Outbox{st: st, log: log, notify: make(chan struct{}, 1)}
}

// Subscribe registers an in-process subscriber.
func (o *Outbox) Subscribe(s Subscriber) {
	o.mu.Lock()
	o.subs = append(o.subs, s)
	o.mu.Unlock()
}

// Notify returns a channel signalled whenever new rows are ready.
func (o *Outbox) Notify() <-chan struct{} { return o.notify }

// Emit stores one row per target. The event counts as new if at least one
// row was inserted (dedup key = event key + "@" + target).
func (o *Outbox) Emit(ctx context.Context, ev events.Event) (bool, error) {
	payload, err := json.Marshal(ev)
	if err != nil {
		return false, fmt.Errorf("marshal %s: %w", ev.Kind(), err)
	}
	now := store.Unix()
	inserted := false
	for _, target := range ev.Targets() {
		n, err := o.st.InsertOutbox(ctx, db.InsertOutboxParams{
			DedupKey:      ev.Key() + "@" + target,
			Kind:          ev.Kind(),
			Target:        target,
			Payload:       string(payload),
			NextAttemptAt: now,
			CreatedAt:     now,
		})
		if err != nil {
			return inserted, fmt.Errorf("outbox insert %s: %w", ev.Key(), err)
		}
		inserted = inserted || n > 0
	}
	if !inserted {
		o.log.Debug("duplicate event skipped", "key", ev.Key())
		return false, nil
	}
	o.log.Info("event emitted", "kind", ev.Kind(), "key", ev.Key())
	select {
	case o.notify <- struct{}{}:
	default:
	}
	o.mu.RLock()
	subs := append([]Subscriber(nil), o.subs...)
	o.mu.RUnlock()
	for _, s := range subs {
		s(ctx, ev)
	}
	return true, nil
}

// BotLog is a convenience for emitting an operational message; errors are logged only.
func BotLog(ctx context.Context, em events.Emitter, log *slog.Logger, level, title, msg string) {
	ev := events.BotLogEvent{Level: level, Title: title, Message: msg, At: store.Now()}
	if _, err := em.Emit(ctx, ev); err != nil {
		log.Error("emit bot-log", "err", err)
	}
}
