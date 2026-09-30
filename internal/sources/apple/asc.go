package apple

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/csv"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/lioilsources/Ol1nDevBot/internal/config"
	"github.com/lioilsources/Ol1nDevBot/internal/events"
	"github.com/lioilsources/Ol1nDevBot/internal/sources/versions"
	"github.com/lioilsources/Ol1nDevBot/internal/store"
	"github.com/lioilsources/Ol1nDevBot/internal/store/db"
)

// ASC is an App Store Connect API client (JWT ES256, ≤ 20 min tokens).
type ASC struct {
	BaseURL  string
	KeyID    string
	IssuerID string
	key      *ecdsa.PrivateKey
	HTTP     *http.Client

	mu       sync.Mutex
	token    string
	tokenExp time.Time
}

// NewASC loads the .p8 key from disk.
func NewASC(keyID, issuerID, keyPath string) (*ASC, error) {
	pemBytes, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("read ASC key: %w", err)
	}
	key, err := ParseP8(pemBytes)
	if err != nil {
		return nil, err
	}
	return &ASC{BaseURL: "https://api.appstoreconnect.apple.com", KeyID: keyID, IssuerID: issuerID, key: key, HTTP: &http.Client{Timeout: 60 * time.Second}}, nil
}

// ParseP8 parses an App Store Connect .p8 (PKCS#8 EC) private key.
func ParseP8(pemBytes []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("ASC key: no PEM block")
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("ASC key: %w", err)
	}
	ec, ok := k.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("ASC key: not an EC key")
	}
	return ec, nil
}

// Token returns a cached bearer token, refreshed 5 minutes before expiry.
func (a *ASC) Token() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	if a.token != "" && now.Before(a.tokenExp.Add(-5*time.Minute)) {
		return a.token, nil
	}
	exp := now.Add(20 * time.Minute)
	t := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{
		"iss": a.IssuerID,
		"iat": now.Unix(),
		"exp": exp.Unix(),
		"aud": "appstoreconnect-v1",
	})
	t.Header["kid"] = a.KeyID
	s, err := t.SignedString(a.key)
	if err != nil {
		return "", fmt.Errorf("sign ASC token: %w", err)
	}
	a.token, a.tokenExp = s, exp
	return s, nil
}

var errNotFound = errors.New("not found")

func (a *ASC) do(ctx context.Context, path string, q url.Values) ([]byte, error) {
	tok, err := a.Token()
	if err != nil {
		return nil, err
	}
	u := a.BaseURL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("asc %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, errNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("asc %s: status %d: %s", path, resp.StatusCode, truncate(string(body), 300))
	}
	return body, nil
}

// Version is an appStoreVersions resource.
type Version struct {
	ID            string
	VersionString string
	State         string
	CreatedDate   time.Time
}

// AppStoreVersions lists the most recent iOS versions of an app.
func (a *ASC) AppStoreVersions(ctx context.Context, appID string) ([]Version, error) {
	q := url.Values{
		"limit":                    {"5"},
		"filter[platform]":         {"IOS"},
		"fields[appStoreVersions]": {"versionString,appStoreState,appVersionState,createdDate"},
	}
	body, err := a.do(ctx, "/v1/apps/"+appID+"/appStoreVersions", q)
	if err != nil {
		return nil, err
	}
	return parseVersions(body)
}

func parseVersions(body []byte) ([]Version, error) {
	var out struct {
		Data []struct {
			ID         string `json:"id"`
			Attributes struct {
				VersionString   string    `json:"versionString"`
				AppStoreState   string    `json:"appStoreState"`
				AppVersionState string    `json:"appVersionState"`
				CreatedDate     time.Time `json:"createdDate"`
			} `json:"attributes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode appStoreVersions: %w", err)
	}
	var vs []Version
	for _, d := range out.Data {
		// appStoreState is deprecated but still the most familiar vocabulary.
		state := d.Attributes.AppStoreState
		if state == "" {
			state = d.Attributes.AppVersionState
		}
		vs = append(vs, Version{ID: d.ID, VersionString: d.Attributes.VersionString, State: state, CreatedDate: d.Attributes.CreatedDate})
	}
	sort.Slice(vs, func(i, j int) bool { return vs[i].CreatedDate.After(vs[j].CreatedDate) })
	return vs, nil
}

// downloadTypes are Sales report product types that count as first downloads
// (excludes updates 7*, re-downloads 3*, in-app purchases).
var downloadTypes = map[string]bool{"1": true, "1F": true, "1T": true, "F1": true, "1E": true, "1EP": true, "1EU": true}

// SalesDownloads fetches the DAILY SALES SUMMARY report for date and returns
// first-download units per Apple app id. A missing report (no sales that day
// or not yet published) returns (nil, false, nil).
func (a *ASC) SalesDownloads(ctx context.Context, vendor string, date time.Time) (map[string]int64, bool, error) {
	q := url.Values{
		"filter[frequency]":     {"DAILY"},
		"filter[reportType]":    {"SALES"},
		"filter[reportSubType]": {"SUMMARY"},
		"filter[vendorNumber]":  {vendor},
		"filter[reportDate]":    {date.Format(time.DateOnly)},
		"filter[version]":       {"1_1"},
	}
	body, err := a.do(ctx, "/v1/salesReports", q)
	if errors.Is(err, errNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	units, err := ParseSalesReport(body)
	return units, true, err
}

// ParseSalesReport parses a (gzipped) tab-separated Sales report.
func ParseSalesReport(body []byte) (map[string]int64, error) {
	if len(body) > 2 && body[0] == 0x1f && body[1] == 0x8b {
		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("sales report gunzip: %w", err)
		}
		if body, err = io.ReadAll(zr); err != nil {
			return nil, fmt.Errorf("sales report gunzip: %w", err)
		}
	}
	r := csv.NewReader(bytes.NewReader(body))
	r.Comma = '\t'
	r.LazyQuotes = true
	r.FieldsPerRecord = -1
	rows, err := r.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("sales report parse: %w", err)
	}
	if len(rows) == 0 {
		return map[string]int64{}, nil
	}
	col := map[string]int{}
	for i, h := range rows[0] {
		col[strings.TrimSpace(h)] = i
	}
	idCol, okID := col["Apple Identifier"]
	typeCol, okType := col["Product Type Identifier"]
	unitsCol, okUnits := col["Units"]
	if !okID || !okType || !okUnits {
		return nil, fmt.Errorf("sales report: unexpected header %v", rows[0])
	}
	out := map[string]int64{}
	for _, row := range rows[1:] {
		if len(row) <= max(idCol, typeCol, unitsCol) {
			continue
		}
		if !downloadTypes[strings.TrimSpace(row[typeCol])] {
			continue
		}
		n, err := strconv.ParseFloat(strings.TrimSpace(row[unitsCol]), 64)
		if err != nil {
			continue
		}
		out[strings.TrimSpace(row[idCol])] += int64(n)
	}
	return out, nil
}

// VersionsPoller tracks review states (WAITING_FOR_REVIEW → IN_REVIEW → …).
type VersionsPoller struct {
	ASC      *ASC
	Apps     *config.Registry
	Recorder *versions.Recorder
	Log      *slog.Logger
}

// Poll checks every app with an asc_app_id.
func (p *VersionsPoller) Poll(ctx context.Context) error {
	var errs []string
	for _, app := range p.Apps.Apps() {
		if app.IOS == nil || app.IOS.ASCAppID == "" {
			continue
		}
		vs, err := p.ASC.AppStoreVersions(ctx, app.IOS.ASCAppID)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", app.Slug, err))
			continue
		}
		first, err := p.Recorder.FirstStatePoll(ctx, "asc", app)
		if err != nil {
			return err
		}
		for i, v := range vs {
			if i == 3 {
				break // older versions do not change state any more
			}
			if err := p.Recorder.RecordState(ctx, app, versions.State{Store: events.StoreAppStore, Version: v.VersionString, State: v.State}, first); err != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", app.Slug, err))
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("asc versions: %s", strings.Join(errs, "; "))
	}
	return nil
}

// SalesPoller writes daily App Store downloads into metrics_daily.
type SalesPoller struct {
	ASC    *ASC
	Vendor string
	Apps   *config.Registry
	St     *store.Store
	Loc    *time.Location
	Log    *slog.Logger
	// Days is how many past days to (re)fetch each run; reports lag 1–2 days.
	Days int
}

// Poll fetches the last Days daily reports.
func (p *SalesPoller) Poll(ctx context.Context) error {
	byAppID := map[string]string{}
	for _, a := range p.Apps.Apps() {
		if a.IOS != nil && a.IOS.ASCAppID != "" {
			byAppID[a.IOS.ASCAppID] = a.Slug
		}
	}
	if len(byAppID) == 0 {
		return nil
	}
	days := p.Days
	if days == 0 {
		days = 3
	}
	today := store.Now().In(p.Loc)
	for d := 1; d <= days; d++ {
		date := today.AddDate(0, 0, -d)
		units, ok, err := p.ASC.SalesDownloads(ctx, p.Vendor, date)
		if err != nil {
			return fmt.Errorf("sales %s: %w", date.Format(time.DateOnly), err)
		}
		if !ok && d == 1 {
			continue // yesterday's report is often not published yet
		}
		for appID, slug := range byAppID {
			if err := p.St.UpsertMetricsDaily(ctx, db.UpsertMetricsDailyParams{
				AppSlug: slug, Store: events.StoreAppStore, Date: date.Format(time.DateOnly),
				Downloads: units[appID], UpdatedAt: store.Unix(),
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
