package apple

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/lioilsources/Ol1nDevBot/internal/config"
	"github.com/lioilsources/Ol1nDevBot/internal/events"
	"github.com/lioilsources/Ol1nDevBot/internal/outbox"
	"github.com/lioilsources/Ol1nDevBot/internal/sources/versions"
	"github.com/lioilsources/Ol1nDevBot/internal/store"
	"github.com/lioilsources/Ol1nDevBot/internal/store/db"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestParseSalesReportGzip(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "sales_daily.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write(raw)
	zw.Close()
	units, err := ParseSalesReport(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	// 6741234567: 12 (1F) + 3 (1) − 1 refund; updates (7F) and re-downloads (3F) excluded.
	if units["6741234567"] != 14 || units["6749999999"] != 5 || len(units) != 2 {
		t.Fatalf("units: %v", units)
	}
}

func TestParseVersions(t *testing.T) {
	raw, _ := os.ReadFile(filepath.Join("testdata", "app_store_versions.json"))
	vs, err := parseVersions(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 2 || vs[0].VersionString != "1.5.0" || vs[0].State != "IN_REVIEW" || vs[1].State != "READY_FOR_SALE" {
		t.Fatalf("versions: %+v", vs)
	}
}

func TestASCTokenIsValidES256(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	p8 := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	path := filepath.Join(t.TempDir(), "AuthKey.p8")
	os.WriteFile(path, p8, 0o600)

	a, err := NewASC("KEY123", "issuer-uuid", path)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := a.Token()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := jwt.Parse(tok, func(*jwt.Token) (any, error) { return &key.PublicKey, nil },
		jwt.WithValidMethods([]string{"ES256"}), jwt.WithAudience("appstoreconnect-v1"), jwt.WithIssuer("issuer-uuid"))
	if err != nil || parsed.Header["kid"] != "KEY123" {
		t.Fatalf("token invalid: %v %v", err, parsed.Header)
	}
	exp, _ := parsed.Claims.GetExpirationTime()
	if time.Until(exp.Time) > 20*time.Minute {
		t.Fatal("token lifetime must be ≤ 20 min")
	}
	again, _ := a.Token()
	if again != tok {
		t.Fatal("token should be cached")
	}
}

func TestLookupPollerSilentBaselineThenAnnounces(t *testing.T) {
	version := "2.0.0"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("bundleId") != "com.ol1n.storyteller" || r.URL.Query().Get("country") != "cz" {
			w.Write([]byte(`{"resultCount":0,"results":[]}`))
			return
		}
		raw, _ := os.ReadFile(filepath.Join("testdata", "lookup.json"))
		w.Write([]byte(strings.ReplaceAll(string(raw), "{{VERSION}}", version)))
	}))
	defer srv.Close()

	ctx := context.Background()
	st, _ := store.Open(ctx, ":memory:")
	ob := outbox.New(st, quiet())
	var got []events.StoreVersionEvent
	ob.Subscribe(func(_ context.Context, ev events.Event) {
		if sv, ok := ev.(events.StoreVersionEvent); ok {
			got = append(got, sv)
		}
	})
	reg := config.NewStaticRegistry(&config.AppsFile{Apps: []config.App{
		{Slug: "storyteller", Name: "Storyteller", Launched: true, IOS: &config.IOS{BundleID: "com.ol1n.storyteller", Country: "CZ"}},
		{Slug: "unreleased", Name: "Unreleased", IOS: &config.IOS{BundleID: "com.ol1n.none"}},
	}})
	p := &LookupPoller{Client: &LookupClient{BaseURL: srv.URL, HTTP: srv.Client()}, Apps: reg, Recorder: &versions.Recorder{St: st, Emit: ob, Log: quiet()}, Log: quiet()}

	if err := p.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatal("launched app's existing version must be recorded silently")
	}
	v, _ := st.LatestAppVersion(ctx, db.LatestAppVersionParams{Store: events.StoreAppStore, AppSlug: "storyteller"})
	if v.Version != "2.0.0" || v.Announced != 0 {
		t.Fatalf("baseline: %+v", v)
	}

	version = "2.1.0"
	p.Poll(ctx)
	p.Poll(ctx)
	if len(got) != 1 || got[0].Version != "2.1.0" || got[0].StoreURL != "https://apps.apple.com/cz/app/storyteller/id6741234567" || got[0].IconURL == "" {
		t.Fatalf("new version events: %+v", got)
	}
}

func TestVersionsPollerStateChanges(t *testing.T) {
	state := "WAITING_FOR_REVIEW"
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || r.URL.Path != "/v1/apps/674/appStoreVersions" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"data":[{"id":"v1","attributes":{"versionString":"1.5.0","appStoreState":"` + state + `","createdDate":"2026-09-20T10:00:00Z"}}]}`))
	}))
	defer srv.Close()

	ctx := context.Background()
	st, _ := store.Open(ctx, ":memory:")
	ob := outbox.New(st, quiet())
	var got []events.VersionStateEvent
	ob.Subscribe(func(_ context.Context, ev events.Event) {
		if s, ok := ev.(events.VersionStateEvent); ok {
			got = append(got, s)
		}
	})
	reg := config.NewStaticRegistry(&config.AppsFile{Apps: []config.App{{Slug: "s", Name: "S", IOS: &config.IOS{BundleID: "b", ASCAppID: "674"}}}})
	asc := &ASC{BaseURL: srv.URL, KeyID: "k", IssuerID: "i", key: key, HTTP: srv.Client()}
	p := &VersionsPoller{ASC: asc, Apps: reg, Recorder: &versions.Recorder{St: st, Emit: ob, Log: quiet()}, Log: quiet()}

	p.Poll(ctx) // first poll: silent baseline
	state = "IN_REVIEW"
	p.Poll(ctx)
	p.Poll(ctx)
	if len(got) != 1 || got[0].State != "IN_REVIEW" || got[0].PrevState != "WAITING_FOR_REVIEW" {
		t.Fatalf("state events: %+v", got)
	}
}
