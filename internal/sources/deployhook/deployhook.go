// Package deployhook receives deploy notifications (GitHub Actions, deploy
// scripts) signed with HMAC-SHA256.
package deployhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"github.com/lioilsources/Ol1nDevBot/internal/config"
	"github.com/lioilsources/Ol1nDevBot/internal/events"
	"github.com/lioilsources/Ol1nDevBot/internal/store"
	"github.com/lioilsources/Ol1nDevBot/internal/store/db"
)

const maxBody = 64 << 10

// Payload is the deploy hook request body.
type Payload struct {
	Service  string `json:"service"`
	Env      string `json:"env"`
	Version  string `json:"version"`
	Commit   string `json:"commit"`
	URL      string `json:"url"`
	Status   string `json:"status"`
	Notes    string `json:"notes"`
	Repo     string `json:"repo"`     // optional owner/name → commit link
	Duration int64  `json:"duration"` // optional seconds
}

var (
	nameRe = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,100}$`)
	repoRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	shaRe  = regexp.MustCompile(`^[0-9a-fA-F]{4,40}$`)
)

// Validate normalises and checks the payload.
func (p *Payload) Validate() error {
	p.Status = strings.ToLower(strings.TrimSpace(p.Status))
	if p.Env == "" {
		p.Env = "prod"
	}
	switch {
	case !nameRe.MatchString(p.Service):
		return errors.New("service: required, [A-Za-z0-9._/-]")
	case !nameRe.MatchString(p.Env):
		return errors.New("env: invalid")
	case p.Version == "" || len(p.Version) > 100:
		return errors.New("version: required")
	case p.Status != "success" && p.Status != "failed":
		return errors.New(`status: must be "success" or "failed"`)
	case p.Commit != "" && !shaRe.MatchString(p.Commit):
		return errors.New("commit: must be a hex sha")
	case p.Repo != "" && !repoRe.MatchString(p.Repo):
		return errors.New("repo: must be owner/name")
	case p.URL != "" && !strings.HasPrefix(p.URL, "https://") && !strings.HasPrefix(p.URL, "http://"):
		return errors.New("url: must be http(s)")
	case p.Duration < 0:
		return errors.New("duration: must be >= 0")
	}
	if len(p.Notes) > 2000 {
		p.Notes = p.Notes[:2000]
	}
	return nil
}

// Sign returns the hex HMAC-SHA256 of body (what callers put in X-Signature).
func Sign(secret, body []byte) string {
	m := hmac.New(sha256.New, secret)
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

// Verify checks an X-Signature header ("<hex>" or "sha256=<hex>").
func Verify(secret, body []byte, header string) bool {
	if len(secret) == 0 {
		return false
	}
	got, err := hex.DecodeString(strings.TrimPrefix(strings.TrimSpace(header), "sha256="))
	if err != nil {
		return false
	}
	m := hmac.New(sha256.New, secret)
	m.Write(body)
	return hmac.Equal(got, m.Sum(nil))
}

// Handler serves POST /discordbot/deploy.
type Handler struct {
	Secret []byte
	Store  *store.Store
	Apps   *config.Registry
	Emit   events.Emitter
	Log    *slog.Logger
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}
	if !Verify(h.Secret, body, r.Header.Get("X-Signature")) {
		h.Log.Warn("deploy hook: bad signature", "remote", r.RemoteAddr)
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}
	var p Payload
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := p.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	isNew, err := h.Accept(r.Context(), p)
	if err != nil {
		h.Log.Error("deploy hook", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if isNew {
		w.WriteHeader(http.StatusAccepted)
	}
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "duplicate": !isNew})
}

// Accept records a validated deploy and emits its event. It is also the
// entry point for other deploy detectors (e.g. a docker events watcher).
func (h *Handler) Accept(ctx context.Context, p Payload) (bool, error) {
	now := store.Now()
	if _, err := h.Store.InsertDeploy(ctx, db.InsertDeployParams{
		Service: p.Service, Env: p.Env, Version: p.Version, CommitSha: p.Commit, Url: p.URL,
		Status: p.Status, Notes: p.Notes, DurationSec: p.Duration, ReceivedAt: now.Unix(),
	}); err != nil {
		return false, fmt.Errorf("insert deploy: %w", err)
	}
	ev := events.DeployEvent{
		Service: p.Service, Env: p.Env, Version: p.Version, Commit: p.Commit,
		CommitURL: commitURL(p.Repo, p.Commit), URL: p.URL, Status: p.Status,
		Notes: p.Notes, DurationSec: p.Duration, At: now,
	}
	if app, ok := h.Apps.AppForService(p.Service); ok {
		ev.AppSlug = app.Slug
	}
	return h.Emit.Emit(ctx, ev)
}

func commitURL(repo, sha string) string {
	if repo == "" || sha == "" {
		return ""
	}
	return fmt.Sprintf("https://github.com/%s/commit/%s", repo, sha)
}
