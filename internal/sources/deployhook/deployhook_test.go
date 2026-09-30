package deployhook

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lioilsources/Ol1nDevBot/internal/config"
	"github.com/lioilsources/Ol1nDevBot/internal/events"
	"github.com/lioilsources/Ol1nDevBot/internal/outbox"
	"github.com/lioilsources/Ol1nDevBot/internal/store"
)

func TestVerify(t *testing.T) {
	secret, body := []byte("s3cret"), []byte(`{"a":1}`)
	sig := Sign(secret, body)
	if !Verify(secret, body, sig) || !Verify(secret, body, "sha256="+sig) {
		t.Fatal("valid signature rejected")
	}
	if Verify(secret, []byte(`{"a":2}`), sig) || Verify(secret, body, "zz") || Verify(nil, body, sig) {
		t.Fatal("invalid signature accepted")
	}
}

func newHandler(t *testing.T) (*Handler, *store.Store, *[]events.Event) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ob := outbox.New(st, log)
	var got []events.Event
	ob.Subscribe(func(_ context.Context, ev events.Event) { got = append(got, ev) })
	reg := config.NewStaticRegistry(&config.AppsFile{Apps: []config.App{{Slug: "storyteller", Name: "Storyteller", Services: []string{"storyteller-api"}}}})
	return &Handler{Secret: []byte("s3cret"), Store: st, Apps: reg, Emit: ob, Log: log}, st, &got
}

func post(h http.Handler, body, sig string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/discordbot/deploy", strings.NewReader(body))
	req.Header.Set("X-Signature", sig)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHandler(t *testing.T) {
	h, st, got := newHandler(t)
	body := `{"service":"storyteller-api","env":"prod","version":"v1.4.2","commit":"abc1234","url":"https://storyteller.ol1n.com","status":"success","notes":"hi","repo":"lioilsources/storyteller-api","duration":42}`
	sig := Sign(h.Secret, []byte(body))

	if rec := post(h, body, "sha256=00"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad sig: %d", rec.Code)
	}
	if rec := post(h, body, sig); rec.Code != http.StatusAccepted {
		t.Fatalf("first: %d %s", rec.Code, rec.Body)
	}
	if rec := post(h, body, sig); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"duplicate":true`) {
		t.Fatalf("duplicate: %d %s", rec.Code, rec.Body)
	}
	if len(*got) != 1 {
		t.Fatalf("events: %d", len(*got))
	}
	ev := (*got)[0].(events.DeployEvent)
	if ev.AppSlug != "storyteller" || ev.CommitURL != "https://github.com/lioilsources/storyteller-api/commit/abc1234" {
		t.Fatalf("event: %+v", ev)
	}
	n, _ := st.CountSuccessfulDeploys(context.Background(), "storyteller-api")
	if n != 1 {
		t.Fatalf("deploy rows: %d", n)
	}

	bad := `{"service":"x","version":"1","status":"meh"}`
	if rec := post(h, bad, Sign(h.Secret, []byte(bad))); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid status: %d", rec.Code)
	}
	unknown := `{"service":"x","version":"1","status":"success","extra":1}`
	if rec := post(h, unknown, Sign(h.Secret, []byte(unknown))); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown field: %d", rec.Code)
	}
}
