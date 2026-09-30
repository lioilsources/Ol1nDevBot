// Package apple polls the public iTunes lookup API and App Store Connect.
package apple

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/lioilsources/Ol1nDevBot/internal/config"
	"github.com/lioilsources/Ol1nDevBot/internal/events"
	"github.com/lioilsources/Ol1nDevBot/internal/sources/versions"
)

// LookupResult is the subset of the iTunes lookup response we use.
type LookupResult struct {
	TrackID                   int64     `json:"trackId"`
	TrackName                 string    `json:"trackName"`
	BundleID                  string    `json:"bundleId"`
	Version                   string    `json:"version"`
	ReleaseNotes              string    `json:"releaseNotes"`
	Description               string    `json:"description"`
	TrackViewURL              string    `json:"trackViewUrl"`
	ArtworkURL512             string    `json:"artworkUrl512"`
	CurrentVersionReleaseDate time.Time `json:"currentVersionReleaseDate"`
}

// LookupClient calls https://itunes.apple.com/lookup (no auth, ~24h cache).
type LookupClient struct {
	BaseURL string
	HTTP    *http.Client
}

// NewLookupClient creates a lookup client.
func NewLookupClient() *LookupClient {
	return &LookupClient{BaseURL: "https://itunes.apple.com", HTTP: &http.Client{Timeout: 20 * time.Second}}
}

// Lookup returns the store listing for bundleID, or nil if it is not live.
func (c *LookupClient) Lookup(ctx context.Context, bundleID, country string) (*LookupResult, error) {
	q := url.Values{"bundleId": {bundleID}, "entity": {"software"}}
	if country != "" {
		q.Set("country", strings.ToLower(country))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/lookup?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("itunes lookup: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return nil, fmt.Errorf("itunes lookup %s: status %d: %s", bundleID, resp.StatusCode, b)
	}
	var out struct {
		ResultCount int            `json:"resultCount"`
		Results     []LookupResult `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("itunes lookup decode: %w", err)
	}
	for _, r := range out.Results {
		if strings.EqualFold(r.BundleID, bundleID) {
			return &r, nil
		}
	}
	return nil, nil
}

// LookupPoller detects new live App Store versions (primary "is it in the store" signal).
type LookupPoller struct {
	Client   *LookupClient
	Apps     *config.Registry
	Recorder *versions.Recorder
	Log      *slog.Logger
}

// Poll checks every iOS app once.
func (p *LookupPoller) Poll(ctx context.Context) error {
	var errs []string
	for _, app := range p.Apps.Apps() {
		if app.IOS == nil {
			continue
		}
		country := app.IOS.Country
		if country == "" {
			country = "us"
		}
		res, err := p.Client.Lookup(ctx, app.IOS.BundleID, country)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		if res == nil || res.Version == "" {
			p.Log.Debug("app not in App Store yet", "app", app.Slug)
			continue
		}
		storeURL := res.TrackViewURL
		if i := strings.Index(storeURL, "?"); i > 0 {
			storeURL = storeURL[:i]
		}
		if err := p.Recorder.RecordLive(ctx, app, versions.Live{
			Store: events.StoreAppStore, Version: res.Version, ReleaseNotes: res.ReleaseNotes, Description: res.Description,
			StoreURL: storeURL, IconURL: res.ArtworkURL512, ReleaseDate: res.CurrentVersionReleaseDate,
		}); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", app.Slug, err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("apple lookup: %s", strings.Join(errs, "; "))
	}
	return nil
}
