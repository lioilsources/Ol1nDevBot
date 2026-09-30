// Package health tracks per-source success/failure and alerts #bot-log once
// after repeated failures (and once again on recovery).
package health

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/lioilsources/Ol1nDevBot/internal/events"
	"github.com/lioilsources/Ol1nDevBot/internal/outbox"
	"github.com/lioilsources/Ol1nDevBot/internal/store"
	"github.com/lioilsources/Ol1nDevBot/internal/store/db"
)

// FailureThreshold is the number of consecutive failures before alerting.
const FailureThreshold = 3

// Tracker records source runs.
type Tracker struct {
	st  *store.Store
	em  events.Emitter
	log *slog.Logger
}

// NewTracker creates a tracker.
func NewTracker(st *store.Store, em events.Emitter, log *slog.Logger) *Tracker {
	return &Tracker{st: st, em: em, log: log}
}

// Run executes fn and records the outcome for source.
func (t *Tracker) Run(ctx context.Context, source string, fn func(ctx context.Context) error) error {
	h, err := t.st.GetSourceHealth(ctx, source)
	if err != nil && !store.IsNotFound(err) {
		return err
	}
	h.Source = source
	now := store.Unix()
	h.LastRunAt = now

	runErr := fn(ctx)
	if ctx.Err() != nil && runErr != nil {
		return runErr // shutting down; not a source failure
	}
	if runErr == nil {
		if h.Alerted == 1 {
			outbox.BotLog(ctx, t.em, t.log, "ok", source+" znovu funguje", fmt.Sprintf("Po %d chybách opět úspěšný běh.", h.ConsecutiveFailures))
		}
		h.LastSuccessAt, h.ConsecutiveFailures, h.LastError, h.Alerted = now, 0, "", 0
	} else {
		t.log.Warn("source run failed", "source", source, "err", runErr)
		h.ConsecutiveFailures++
		h.LastError = truncate(runErr.Error(), 1000)
		if h.ConsecutiveFailures >= FailureThreshold && h.Alerted == 0 {
			outbox.BotLog(ctx, t.em, t.log, "error", source+" selhává",
				fmt.Sprintf("%d× po sobě. Poslední chyba:\n```\n%s\n```", h.ConsecutiveFailures, truncate(runErr.Error(), 1500)))
			h.Alerted = 1
		}
	}
	if err := t.st.UpsertSourceHealth(ctx, db.UpsertSourceHealthParams(h)); err != nil {
		t.log.Error("save source health", "err", err)
	}
	return runErr
}

// Summary renders the health table for /bot status.
func (t *Tracker) Summary(ctx context.Context, loc *time.Location) (string, error) {
	rows, err := t.st.ListSourceHealth(ctx)
	if err != nil {
		return "", err
	}
	stats, err := t.st.OutboxStats(ctx)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if len(rows) == 0 {
		b.WriteString("Zatím neproběhl žádný poller.\n")
	}
	for _, r := range rows {
		icon := "🟢"
		switch {
		case r.ConsecutiveFailures >= FailureThreshold:
			icon = "🔴"
		case r.ConsecutiveFailures > 0:
			icon = "🟠"
		}
		fmt.Fprintf(&b, "%s **%s** — běh %s, úspěch %s", icon, r.Source, ago(r.LastRunAt, loc), ago(r.LastSuccessAt, loc))
		if r.ConsecutiveFailures > 0 {
			fmt.Fprintf(&b, " · %d× chyba: `%s`", r.ConsecutiveFailures, truncate(r.LastError, 150))
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "\nOutbox: %d čeká, %d odesláno, %d mrtvých", stats.Pending, stats.Sent, stats.Dead)
	return b.String(), nil
}

func ago(ts int64, loc *time.Location) string {
	if ts == 0 {
		return "nikdy"
	}
	return time.Unix(ts, 0).In(loc).Format("2. 1. 15:04")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
