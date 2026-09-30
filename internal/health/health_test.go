package health

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/lioilsources/Ol1nDevBot/internal/events"
	"github.com/lioilsources/Ol1nDevBot/internal/store"
)

type rec struct{ evs []events.Event }

func (r *rec) Emit(_ context.Context, ev events.Event) (bool, error) {
	r.evs = append(r.evs, ev)
	return true, nil
}

func TestAlertOnceAfterThreeFailuresAndRecover(t *testing.T) {
	ctx := context.Background()
	st, _ := store.Open(ctx, ":memory:")
	r := &rec{}
	tr := NewTracker(st, r, slog.New(slog.NewTextHandler(io.Discard, nil)))
	boom := func(context.Context) error { return errors.New("boom") }
	for range 5 {
		tr.Run(ctx, "apple", boom)
	}
	if len(r.evs) != 1 || r.evs[0].(events.BotLogEvent).Level != "error" {
		t.Fatalf("expected exactly one error alert, got %+v", r.evs)
	}
	tr.Run(ctx, "apple", func(context.Context) error { return nil })
	if len(r.evs) != 2 || r.evs[1].(events.BotLogEvent).Level != "ok" {
		t.Fatalf("expected recovery message, got %+v", r.evs)
	}
	h, _ := st.GetSourceHealth(ctx, "apple")
	if h.ConsecutiveFailures != 0 || h.Alerted != 0 {
		t.Fatalf("state not reset: %+v", h)
	}
}
