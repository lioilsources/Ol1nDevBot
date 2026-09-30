// Package gplay polls Google Play: production track releases (Android
// Publisher API) and daily install/store-performance CSV exports (GCS bucket).
package gplay

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"golang.org/x/oauth2/google"

	"github.com/lioilsources/Ol1nDevBot/internal/config"
	"github.com/lioilsources/Ol1nDevBot/internal/events"
	"github.com/lioilsources/Ol1nDevBot/internal/sources/versions"
	"github.com/lioilsources/Ol1nDevBot/internal/store"
	"github.com/lioilsources/Ol1nDevBot/internal/store/db"
)

const (
	scopePublisher = "https://www.googleapis.com/auth/androidpublisher"
	scopeStorage   = "https://www.googleapis.com/auth/devstorage.read_only"
)

// Client talks to the Android Publisher and Cloud Storage JSON APIs.
type Client struct {
	PublisherURL string
	StorageURL   string
	HTTP         *http.Client
}

// NewClient authenticates with a service-account JSON key.
func NewClient(ctx context.Context, saJSONPath string) (*Client, error) {
	data, err := os.ReadFile(saJSONPath)
	if err != nil {
		return nil, fmt.Errorf("read service account: %w", err)
	}
	cfg, err := google.JWTConfigFromJSON(data, scopePublisher, scopeStorage)
	if err != nil {
		return nil, fmt.Errorf("service account: %w", err)
	}
	hc := cfg.Client(ctx)
	hc.Timeout = 60 * time.Second
	return &Client{
		PublisherURL: "https://androidpublisher.googleapis.com/androidpublisher/v3",
		StorageURL:   "https://storage.googleapis.com",
		HTTP:         hc,
	}, nil
}

var errNotFound = errors.New("not found")

func (c *Client) do(ctx context.Context, method, u string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNotFound {
		return errNotFound
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s %s: status %d: %s", method, u, resp.StatusCode, truncate(string(body), 300))
	}
	switch o := out.(type) {
	case nil:
		return nil
	case *[]byte:
		*o = body
		return nil
	default:
		return json.Unmarshal(body, out)
	}
}

// Release is one release of a track.
type Release struct {
	Name         string   `json:"name"`
	VersionCodes []string `json:"versionCodes"`
	Status       string   `json:"status"` // completed | inProgress | halted | draft
	UserFraction float64  `json:"userFraction"`
	ReleaseNotes []struct {
		Language string `json:"language"`
		Text     string `json:"text"`
	} `json:"releaseNotes"`
}

// ProductionTrack reads the production track via a throw-away edit which is
// deleted, never committed.
func (c *Client) ProductionTrack(ctx context.Context, pkg string) ([]Release, error) {
	base := c.PublisherURL + "/applications/" + url.PathEscape(pkg) + "/edits"
	var edit struct {
		ID string `json:"id"`
	}
	if err := c.do(ctx, http.MethodPost, base, &edit); err != nil {
		return nil, fmt.Errorf("edits.insert %s: %w", pkg, err)
	}
	defer func() {
		// Use a fresh context: the edit must be deleted even on cancellation.
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		_ = c.do(dctx, http.MethodDelete, base+"/"+edit.ID, nil)
	}()
	var track struct {
		Releases []Release `json:"releases"`
	}
	if err := c.do(ctx, http.MethodGet, base+"/"+edit.ID+"/tracks/production", &track); err != nil {
		return nil, fmt.Errorf("edits.tracks.get %s: %w", pkg, err)
	}
	return track.Releases, nil
}

// Object downloads a GCS object; (nil, nil) if it does not exist.
func (c *Client) Object(ctx context.Context, bucket, name string) ([]byte, error) {
	u := fmt.Sprintf("%s/storage/v1/b/%s/o/%s?alt=media", c.StorageURL, url.PathEscape(bucket), url.PathEscape(name))
	var body []byte
	err := c.do(ctx, http.MethodGet, u, &body)
	if errors.Is(err, errNotFound) {
		return nil, nil
	}
	return body, err
}

// ListObjects lists object names under prefix.
func (c *Client) ListObjects(ctx context.Context, bucket, prefix string) ([]string, error) {
	u := fmt.Sprintf("%s/storage/v1/b/%s/o?prefix=%s&fields=items(name)", c.StorageURL, url.PathEscape(bucket), url.QueryEscape(prefix))
	var out struct {
		Items []struct {
			Name string `json:"name"`
		} `json:"items"`
	}
	if err := c.do(ctx, http.MethodGet, u, &out); err != nil {
		return nil, err
	}
	var names []string
	for _, it := range out.Items {
		names = append(names, it.Name)
	}
	return names, nil
}

// TracksPoller detects new production releases and rollout changes.
type TracksPoller struct {
	Client   *Client
	Apps     *config.Registry
	Recorder *versions.Recorder
	Log      *slog.Logger
}

// Poll checks every Android app once.
func (p *TracksPoller) Poll(ctx context.Context) error {
	var errs []string
	for _, app := range p.Apps.Apps() {
		if app.Android == nil {
			continue
		}
		if err := p.pollApp(ctx, app); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", app.Slug, err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("play tracks: %s", strings.Join(errs, "; "))
	}
	return nil
}

func (p *TracksPoller) pollApp(ctx context.Context, app config.App) error {
	releases, err := p.Client.ProductionTrack(ctx, app.Android.Package)
	if err != nil {
		return err
	}
	first, err := p.Recorder.FirstStatePoll(ctx, "play", app)
	if err != nil {
		return err
	}
	var live *Release
	for i := range releases {
		r := &releases[i]
		if r.Status == "draft" {
			continue
		}
		name := ReleaseName(*r)
		detail := ""
		if r.Status == "inProgress" && r.UserFraction > 0 {
			detail = fmt.Sprintf("%g %%", r.UserFraction*100)
		}
		if err := p.Recorder.RecordState(ctx, app, versions.State{Store: events.StorePlayStore, Version: name, State: r.Status, Detail: detail}, first); err != nil {
			return err
		}
		if live == nil && (r.Status == "completed" || r.Status == "inProgress") {
			live = r
		}
	}
	if live == nil {
		return nil
	}
	return p.Recorder.RecordLive(ctx, app, versions.Live{
		Store:        events.StorePlayStore,
		Version:      ReleaseName(*live),
		Build:        strings.Join(live.VersionCodes, ", "),
		ReleaseNotes: pickNotes(*live),
		StoreURL:     versions.PlayStoreURL(app),
	})
}

// ReleaseName returns the release name, falling back to the version code.
func ReleaseName(r Release) string {
	if r.Name != "" {
		// Play defaults names to "123 (1.2.3)"; prefer the human version.
		if i, j := strings.Index(r.Name, "("), strings.LastIndex(r.Name, ")"); i >= 0 && j > i {
			return strings.TrimSpace(r.Name[i+1 : j])
		}
		return r.Name
	}
	if len(r.VersionCodes) > 0 {
		return r.VersionCodes[len(r.VersionCodes)-1]
	}
	return "?"
}

func pickNotes(r Release) string {
	for _, lang := range []string{"cs-CZ", "en-US", "en-GB"} {
		for _, n := range r.ReleaseNotes {
			if n.Language == lang {
				return n.Text
			}
		}
	}
	if len(r.ReleaseNotes) > 0 {
		return r.ReleaseNotes[0].Text
	}
	return ""
}

// StatsPoller imports the monthly CSV exports from the Play reports bucket.
type StatsPoller struct {
	Client *Client
	Bucket string
	Apps   *config.Registry
	St     *store.Store
	Loc    *time.Location
	Log    *slog.Logger
}

// Poll imports the current month (and the previous one during the first days).
func (p *StatsPoller) Poll(ctx context.Context) error {
	now := store.Now().In(p.Loc)
	months := []time.Time{now}
	if now.Day() <= 3 {
		months = append(months, now.AddDate(0, -1, 0))
	}
	var errs []string
	for _, app := range p.Apps.Apps() {
		if app.Android == nil {
			continue
		}
		for _, m := range months {
			if err := p.importMonth(ctx, app, m.Format("200601")); err != nil {
				errs = append(errs, fmt.Sprintf("%s %s: %v", app.Slug, m.Format("2006-01"), err))
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("play stats: %s", strings.Join(errs, "; "))
	}
	return nil
}

func (p *StatsPoller) importMonth(ctx context.Context, app config.App, yyyymm string) error {
	pkg := app.Android.Package
	body, err := p.Client.Object(ctx, p.Bucket, fmt.Sprintf("stats/installs/installs_%s_%s_overview.csv", pkg, yyyymm))
	if err != nil {
		return err
	}
	if body != nil {
		rows, err := ParseInstalls(body)
		if err != nil {
			return err
		}
		for _, r := range rows {
			if err := p.St.UpsertMetricsDaily(ctx, db.UpsertMetricsDailyParams{
				AppSlug: app.Slug, Store: events.StorePlayStore, Date: r.Date,
				Downloads: r.Installs, Uninstalls: r.Uninstalls, UpdatedAt: store.Unix(),
			}); err != nil {
				return err
			}
		}
	}

	// Store listing performance: file naming varies by breakdown; take the
	// country breakdown (or overview) and sum per day.
	names, err := p.Client.ListObjects(ctx, p.Bucket, fmt.Sprintf("stats/store_performance/store_performance_%s_%s", pkg, yyyymm))
	if err != nil {
		return err
	}
	name := pickPerformanceFile(names)
	if name == "" {
		return nil
	}
	body, err = p.Client.Object(ctx, p.Bucket, name)
	if err != nil || body == nil {
		return err
	}
	perf, err := ParseStorePerformance(body)
	if err != nil {
		return err
	}
	for date, f := range perf {
		if err := p.St.UpsertStorePerformance(ctx, db.UpsertStorePerformanceParams{
			AppSlug: app.Slug, Store: events.StorePlayStore, Date: date,
			Impressions: f.Impressions, PageViews: f.PageViews, UpdatedAt: store.Unix(),
		}); err != nil {
			return err
		}
	}
	return nil
}

func pickPerformanceFile(names []string) string {
	for _, suffix := range []string{"_overview.csv", "_country.csv"} {
		for _, n := range names {
			if strings.HasSuffix(n, suffix) {
				return n
			}
		}
	}
	return ""
}

// InstallsRow is one day of the installs overview report.
type InstallsRow struct {
	Date       string
	Installs   int64
	Uninstalls int64
}

// ParseInstalls parses installs_<pkg>_<YYYYMM>_overview.csv.
func ParseInstalls(body []byte) ([]InstallsRow, error) {
	rows, col, err := readCSV(body)
	if err != nil {
		return nil, err
	}
	dateCol := col.find("date")
	instCol := col.find("daily user installs")
	uninstCol := col.find("daily user uninstalls")
	if dateCol < 0 || instCol < 0 {
		return nil, fmt.Errorf("installs csv: unexpected header %v", col)
	}
	var out []InstallsRow
	for _, r := range rows {
		date := cell(r, dateCol)
		if date == "" {
			continue
		}
		out = append(out, InstallsRow{Date: date, Installs: parseInt(cell(r, instCol)), Uninstalls: parseInt(cell(r, uninstCol))})
	}
	return out, nil
}

// Performance is store listing traffic for one day.
type Performance struct {
	Impressions int64
	PageViews   int64
}

// ParseStorePerformance sums "Store listing visitors" (page views) and, when
// present, impressions per date across all breakdown rows.
func ParseStorePerformance(body []byte) (map[string]Performance, error) {
	rows, col, err := readCSV(body)
	if err != nil {
		return nil, err
	}
	dateCol := col.find("date")
	visitorsCol := col.find("store listing visitors")
	impressionsCol := col.find("impressions")
	if dateCol < 0 || visitorsCol < 0 {
		return nil, fmt.Errorf("store performance csv: unexpected header %v", col)
	}
	out := map[string]Performance{}
	for _, r := range rows {
		date := cell(r, dateCol)
		if date == "" {
			continue
		}
		p := out[date]
		p.PageViews += parseInt(cell(r, visitorsCol))
		p.Impressions += parseInt(cell(r, impressionsCol))
		out[date] = p
	}
	return out, nil
}

type header []string

// find returns the first column whose lower-cased name contains s.
func (h header) find(s string) int {
	for i, name := range h {
		if strings.EqualFold(name, s) {
			return i
		}
	}
	for i, name := range h {
		if strings.Contains(strings.ToLower(name), s) {
			return i
		}
	}
	return -1
}

func readCSV(body []byte) ([][]string, header, error) {
	r := csv.NewReader(bytes.NewReader(DecodeText(body)))
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	rows, err := r.ReadAll()
	if err != nil {
		return nil, nil, fmt.Errorf("parse csv: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil, errors.New("empty csv")
	}
	h := make(header, len(rows[0]))
	for i, name := range rows[0] {
		h[i] = strings.TrimSpace(name)
	}
	return rows[1:], h, nil
}

// DecodeText converts Play's UTF-16 (BOM) exports to UTF-8; UTF-8 passes through.
func DecodeText(b []byte) []byte {
	var order func([]byte) uint16
	switch {
	case len(b) >= 2 && b[0] == 0xFF && b[1] == 0xFE:
		order = func(p []byte) uint16 { return uint16(p[0]) | uint16(p[1])<<8 }
	case len(b) >= 2 && b[0] == 0xFE && b[1] == 0xFF:
		order = func(p []byte) uint16 { return uint16(p[1]) | uint16(p[0])<<8 }
	default:
		return bytes.TrimPrefix(b, []byte("\xEF\xBB\xBF"))
	}
	b = b[2:]
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		u = append(u, order(b[i:i+2]))
	}
	return []byte(string(utf16.Decode(u)))
}

func cell(r []string, i int) string {
	if i < 0 || i >= len(r) {
		return ""
	}
	return strings.TrimSpace(r[i])
}

func parseInt(s string) int64 {
	s = strings.ReplaceAll(s, ",", "")
	if s == "" {
		return 0
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n
	}
	f, _ := strconv.ParseFloat(s, 64)
	return int64(f)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
