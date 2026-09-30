package github

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestClient(t *testing.T) {
	since := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	until := since.AddDate(0, 0, 7)
	in := since.Add(24 * time.Hour).Format(time.RFC3339)
	before := since.Add(-24 * time.Hour).Format(time.RFC3339)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "unauthorized", 401)
			return
		}
		switch r.URL.Path {
		case "/repos/o/a/commits":
			if r.URL.Query().Get("sha") != "main" || r.URL.Query().Get("since") == "" {
				http.Error(w, "bad query", 400)
				return
			}
			fmt.Fprintf(w, `[
				{"sha":"aaa111","html_url":"u1","commit":{"message":"feat: x\n\nbody","author":{"name":"Ol1n","date":%q}},"author":{"login":"ol1n"},"parents":[{}]},
				{"sha":"bbb222","html_url":"u2","commit":{"message":"Merge pull request #3","author":{"name":"Ol1n","date":%q}},"parents":[{},{}]}
			]`, in, in)
		case "/repos/o/a/pulls":
			fmt.Fprintf(w, `[
				{"number":3,"title":"PR in window","html_url":"p3","merged_at":%q,"updated_at":%q,"user":{"login":"ol1n"}},
				{"number":2,"title":"closed unmerged","html_url":"p2","merged_at":null,"updated_at":%q,"user":{"login":"ol1n"}},
				{"number":1,"title":"old","html_url":"p1","merged_at":%q,"updated_at":%q,"user":{"login":"ol1n"}}
			]`, in, in, in, before, before)
		case "/repos/o/a/releases":
			fmt.Fprintf(w, `[{"tag_name":"v1.0.0","name":"","html_url":"r1","draft":false,"published_at":%q,"author":{"login":"ol1n"}},
				{"tag_name":"v2","draft":true,"published_at":%q}]`, in, in)
		case "/repos/o/empty/commits":
			http.Error(w, `{"message":"Git Repository is empty."}`, http.StatusConflict)
		case "/user/repos":
			page := r.URL.Query().Get("page")
			if page == "1" {
				var items []string
				for i := range perPage {
					items = append(items, fmt.Sprintf(`{"full_name":"o/r%d","owner":{"login":"o"},"archived":%v}`, i, i == 0))
				}
				w.Write([]byte("[" + strings.Join(items, ",") + "]"))
				return
			}
			w.Write([]byte(`[{"full_name":"x/other","owner":{"login":"x"}},{"full_name":"o/fork","owner":{"login":"o"},"fork":true},{"full_name":"o/last","owner":{"login":"o"}}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, Token: "tok", HTTP: srv.Client()}
	ctx := context.Background()

	commits, err := c.Commits(ctx, "o/a", "main", since, until)
	if err != nil || len(commits) != 1 || commits[0].Title != "feat: x" || commits[0].Author != "ol1n" {
		t.Fatalf("commits: %+v %v", commits, err)
	}
	prs, err := c.MergedPulls(ctx, "o/a", since, until)
	if err != nil || len(prs) != 1 || prs[0].Ref != "3" {
		t.Fatalf("prs: %+v %v", prs, err)
	}
	rels, err := c.Releases(ctx, "o/a", since, until)
	if err != nil || len(rels) != 1 || rels[0].Title != "v1.0.0" {
		t.Fatalf("releases: %+v %v", rels, err)
	}
	if empty, err := c.Commits(ctx, "o/empty", "main", since, until); err != nil || len(empty) != 0 {
		t.Fatalf("empty repo: %v %v", empty, err)
	}
	repos, err := c.OwnerRepos(ctx, "o")
	if err != nil || len(repos) != perPage { // 99 non-archived from page 1 + o/last
		t.Fatalf("owner repos: %d %v", len(repos), err)
	}
}
