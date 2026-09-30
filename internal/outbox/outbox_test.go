package outbox

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/lioilsources/Ol1nDevBot/internal/events"
	"github.com/lioilsources/Ol1nDevBot/internal/store"
	"github.com/lioilsources/Ol1nDevBot/internal/store/db"
)

func TestEmitFanOutAndDedup(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	o := New(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	var seen int
	o.Subscribe(func(context.Context, events.Event) { seen++ })

	ev := events.DeployEvent{Service: "api", Env: "prod", Version: "v1", Commit: "abc", Status: "success", AppSlug: "storyteller"}
	isNew, err := o.Emit(ctx, ev)
	if err != nil || !isNew {
		t.Fatalf("first emit: %v %v", isNew, err)
	}
	isNew, _ = o.Emit(ctx, ev)
	if isNew {
		t.Fatal("duplicate should not be new")
	}
	if seen != 1 {
		t.Fatalf("subscriber calls = %d", seen)
	}
	rows, _ := st.DueOutbox(ctx, dueAll())
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows (deploys + app releases), got %d", len(rows))
	}
	back, err := events.Decode(rows[0].Kind, rows[0].Payload)
	if err != nil {
		t.Fatal(err)
	}
	if back.(events.DeployEvent).Commit != "abc" {
		t.Fatal("decode roundtrip")
	}
}

func dueAll() db.DueOutboxParams { return db.DueOutboxParams{NextAttemptAt: 1 << 62, Limit: 100} }
