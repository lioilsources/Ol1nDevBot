// Package github reads repository activity (commits, merged PRs, releases).
package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const perPage = 100

// Client is a small read-only GitHub REST client.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// New creates a client for api.github.com.
func New(token string) *Client {
	return &Client{BaseURL: "https://api.github.com", Token: token, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

// Activity kinds.
const (
	KindCommit  = "commit"
	KindPR      = "pr"
	KindRelease = "release"
)

// Activity is one commit / merged PR / release.
type Activity struct {
	Repo   string
	Kind   string
	Ref    string // sha, PR number or tag
	Title  string
	Body   string
	URL    string
	Author string
	At     time.Time
}

// Repo metadata.
type Repo struct {
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
	Archived      bool   `json:"archived"`
	Fork          bool   `json:"fork"`
	Owner         struct {
		Login string `json:"login"`
	} `json:"owner"`
}

func (c *Client) get(ctx context.Context, path string, q url.Values, out any) error {
	u := c.BaseURL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("github %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusConflict && strings.HasSuffix(path, "/commits") {
		return nil // empty repository
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("github %s: status %d: %s", path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// paginate fetches pages until a short page, maxPages or stop() says so.
func paginate[T any](ctx context.Context, c *Client, path string, q url.Values, maxPages int, stop func([]T) bool) ([]T, error) {
	var all []T
	for page := 1; page <= maxPages; page++ {
		q.Set("per_page", fmt.Sprint(perPage))
		q.Set("page", fmt.Sprint(page))
		var items []T
		if err := c.get(ctx, path, q, &items); err != nil {
			return nil, err
		}
		all = append(all, items...)
		if len(items) < perPage || (stop != nil && stop(items)) {
			break
		}
	}
	return all, nil
}

// GetRepo returns repository metadata.
func (c *Client) GetRepo(ctx context.Context, fullName string) (Repo, error) {
	var r Repo
	err := c.get(ctx, "/repos/"+fullName, nil, &r)
	return r, err
}

// OwnerRepos lists non-archived, non-fork repos of owner (including private
// ones visible to the token).
func (c *Client) OwnerRepos(ctx context.Context, owner string) ([]Repo, error) {
	var repos []Repo
	var err error
	if c.Token != "" {
		repos, err = paginate[Repo](ctx, c, "/user/repos", url.Values{"affiliation": {"owner,organization_member"}}, 10, nil)
	} else {
		repos, err = paginate[Repo](ctx, c, "/users/"+owner+"/repos", url.Values{"type": {"owner"}}, 10, nil)
	}
	if err != nil {
		return nil, err
	}
	var out []Repo
	for _, r := range repos {
		if strings.EqualFold(r.Owner.Login, owner) && !r.Archived && !r.Fork {
			out = append(out, r)
		}
	}
	return out, nil
}

type commitJSON struct {
	SHA     string `json:"sha"`
	HTMLURL string `json:"html_url"`
	Commit  struct {
		Message string `json:"message"`
		Author  struct {
			Name string    `json:"name"`
			Date time.Time `json:"date"`
		} `json:"author"`
	} `json:"commit"`
	Author *struct {
		Login string `json:"login"`
	} `json:"author"`
	Parents []struct{} `json:"parents"`
}

// Commits returns non-merge commits on branch within [since, until).
func (c *Client) Commits(ctx context.Context, repo, branch string, since, until time.Time) ([]Activity, error) {
	q := url.Values{"sha": {branch}, "since": {since.UTC().Format(time.RFC3339)}, "until": {until.UTC().Format(time.RFC3339)}}
	items, err := paginate[commitJSON](ctx, c, "/repos/"+repo+"/commits", q, 5, nil)
	if err != nil {
		return nil, err
	}
	var out []Activity
	for _, it := range items {
		if len(it.Parents) > 1 {
			continue // merge commits are represented by their PR
		}
		title, body, _ := strings.Cut(it.Commit.Message, "\n")
		author := it.Commit.Author.Name
		if it.Author != nil && it.Author.Login != "" {
			author = it.Author.Login
		}
		out = append(out, Activity{Repo: repo, Kind: KindCommit, Ref: it.SHA, Title: strings.TrimSpace(title), Body: truncate(body, 300), URL: it.HTMLURL, Author: author, At: it.Commit.Author.Date})
	}
	return out, nil
}

type pullJSON struct {
	Number    int        `json:"number"`
	Title     string     `json:"title"`
	Body      string     `json:"body"`
	HTMLURL   string     `json:"html_url"`
	MergedAt  *time.Time `json:"merged_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	User      struct {
		Login string `json:"login"`
	} `json:"user"`
}

// MergedPulls returns PRs merged within [since, until).
func (c *Client) MergedPulls(ctx context.Context, repo string, since, until time.Time) ([]Activity, error) {
	q := url.Values{"state": {"closed"}, "sort": {"updated"}, "direction": {"desc"}}
	items, err := paginate(ctx, c, "/repos/"+repo+"/pulls", q, 5, func(page []pullJSON) bool {
		return page[len(page)-1].UpdatedAt.Before(since)
	})
	if err != nil {
		return nil, err
	}
	var out []Activity
	for _, p := range items {
		if p.MergedAt == nil || p.MergedAt.Before(since) || !p.MergedAt.Before(until) {
			continue
		}
		out = append(out, Activity{Repo: repo, Kind: KindPR, Ref: fmt.Sprint(p.Number), Title: p.Title, Body: truncate(p.Body, 600), URL: p.HTMLURL, Author: p.User.Login, At: *p.MergedAt})
	}
	return out, nil
}

type releaseJSON struct {
	TagName     string     `json:"tag_name"`
	Name        string     `json:"name"`
	Body        string     `json:"body"`
	HTMLURL     string     `json:"html_url"`
	Draft       bool       `json:"draft"`
	PublishedAt *time.Time `json:"published_at"`
	Author      struct {
		Login string `json:"login"`
	} `json:"author"`
}

// Releases returns releases published within [since, until).
func (c *Client) Releases(ctx context.Context, repo string, since, until time.Time) ([]Activity, error) {
	var items []releaseJSON
	if err := c.get(ctx, "/repos/"+repo+"/releases", url.Values{"per_page": {"30"}}, &items); err != nil {
		return nil, err
	}
	var out []Activity
	for _, r := range items {
		if r.Draft || r.PublishedAt == nil || r.PublishedAt.Before(since) || !r.PublishedAt.Before(until) {
			continue
		}
		title := r.Name
		if title == "" {
			title = r.TagName
		}
		out = append(out, Activity{Repo: repo, Kind: KindRelease, Ref: r.TagName, Title: title, Body: truncate(r.Body, 600), URL: r.HTMLURL, Author: r.Author.Login, At: *r.PublishedAt})
	}
	return out, nil
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
