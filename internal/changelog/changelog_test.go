package changelog

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/lioilsources/Ol1nDevBot/internal/config"
	"github.com/lioilsources/Ol1nDevBot/internal/events"
	"github.com/lioilsources/Ol1nDevBot/internal/sources/github"
	"github.com/lioilsources/Ol1nDevBot/internal/store"
)

type fakeGH struct{ acts map[string][]github.Activity }

func (f *fakeGH) GetRepo(_ context.Context, n string) (github.Repo, error) {
	return github.Repo{FullName: n, DefaultBranch: "main"}, nil
}
func (f *fakeGH) OwnerRepos(context.Context, string) ([]github.Repo, error) { return nil, nil }
func (f *fakeGH) filter(repo, kind string) []github.Activity {
	var out []github.Activity
	for _, a := range f.acts[repo] {
		if a.Kind == kind {
			out = append(out, a)
		}
	}
	return out
}
func (f *fakeGH) Commits(_ context.Context, repo, _ string, _, _ time.Time) ([]github.Activity, error) {
	return f.filter(repo, github.KindCommit), nil
}
func (f *fakeGH) MergedPulls(_ context.Context, repo string, _, _ time.Time) ([]github.Activity, error) {
	return f.filter(repo, github.KindPR), nil
}
func (f *fakeGH) Releases(_ context.Context, repo string, _, _ time.Time) ([]github.Activity, error) {
	return f.filter(repo, github.KindRelease), nil
}

type fakeLLM struct {
	out    string
	err    error
	prompt string
}

func (f *fakeLLM) Complete(_ context.Context, _, user string) (string, error) {
	f.prompt = user
	return f.out, f.err
}

type rec struct{ evs []events.Event }

func (r *rec) Emit(_ context.Context, ev events.Event) (bool, error) {
	r.evs = append(r.evs, ev)
	return true, nil
}

func TestLastWeek(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/Prague")
	for _, now := range []time.Time{
		time.Date(2026, 9, 28, 9, 0, 0, 0, loc),  // Monday
		time.Date(2026, 10, 4, 23, 0, 0, 0, loc), // Sunday
	} {
		from, to := LastWeek(now, loc)
		if from.Weekday() != time.Monday || to.Sub(from) != 7*24*time.Hour || !to.Before(now) || now.Sub(to) >= 7*24*time.Hour {
			t.Errorf("now=%v → %v..%v", now, from, to)
		}
	}
}

func TestRunWithLLMAndFallback(t *testing.T) {
	ctx := context.Background()
	loc := time.UTC
	st, _ := store.Open(ctx, ":memory:")
	from := time.Date(2026, 9, 21, 0, 0, 0, 0, loc)
	to := from.AddDate(0, 0, 7)
	in := from.Add(48 * time.Hour)
	gh := &fakeGH{acts: map[string][]github.Activity{
		"o/app": {
			{Repo: "o/app", Kind: github.KindPR, Ref: "12", Title: "feat: pohádky", URL: "https://github.com/o/app/pull/12", At: in},
			{Repo: "o/app", Kind: github.KindCommit, Ref: "abcdef123", Title: "fix(api): timeout", At: in},
			{Repo: "o/app", Kind: github.KindCommit, Ref: "old", Title: "outside window", At: from.Add(-time.Hour)},
		},
		"o/quiet": {},
	}}
	reg := config.NewStaticRegistry(&config.AppsFile{GitHub: config.GitHub{Owner: "o", Repos: []string{"o/quiet"}}, Apps: []config.App{{Slug: "app", Name: "App", Repos: []string{"o/app"}}}})
	llm := &fakeLLM{out: "**✨ Features**\n- Pohádky [#12](<https://github.com/o/app/pull/12>)"}
	r := &rec{}
	s := &Service{GH: gh, LLM: llm, St: st, Reg: reg, Emit: r, Loc: loc, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	ev, err := s.Run(ctx, from, to, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ev.Repos) != 1 || ev.Repos[0].Repo != "o/app" || ev.Repos[0].Markdown != llm.out {
		t.Fatalf("event: %+v", ev)
	}
	if strings.Contains(llm.prompt, "outside window") || !strings.Contains(llm.prompt, "PR #12") {
		t.Fatalf("prompt: %s", llm.prompt)
	}
	if len(r.evs) != 1 || ev.Key() != "changelog:2026-09-21:2026-09-27" {
		t.Fatalf("emitted %d, key %s", len(r.evs), ev.Key())
	}

	llm.err = errors.New("spark down")
	ev, _ = s.Run(ctx, from, to, "", "x")
	md := ev.Repos[0].Markdown
	if !strings.Contains(md, "**✨ Features**\n- pohádky [#12]") || !strings.Contains(md, "**🐛 Fixes**\n- timeout") {
		t.Fatalf("fallback markdown:\n%s", md)
	}
}
