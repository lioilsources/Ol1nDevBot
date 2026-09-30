package store

import (
	"context"
	"testing"

	"github.com/lioilsources/Ol1nDevBot/internal/store/db"
)

func TestOpenMigratesAndDedups(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	p := db.InsertOutboxParams{DedupKey: "k", Kind: "deploy", Target: "infra:deploys", Payload: "{}", NextAttemptAt: 1, CreatedAt: 1}
	n1, err := s.InsertOutbox(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	n2, err := s.InsertOutbox(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if n1 != 1 || n2 != 0 {
		t.Fatalf("dedup failed: %d %d", n1, n2)
	}

	first, err := s.Baseline(ctx, "versions", "appstore:x")
	if err != nil || !first {
		t.Fatalf("first baseline: %v %v", first, err)
	}
	again, _ := s.Baseline(ctx, "versions", "appstore:x")
	if again {
		t.Fatal("baseline should be created only once")
	}
}
