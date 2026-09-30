// Package changelog builds the weekly GitHub summary: activity → LLM → Discord.
package changelog

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/lioilsources/Ol1nDevBot/internal/config"
	"github.com/lioilsources/Ol1nDevBot/internal/events"
	"github.com/lioilsources/Ol1nDevBot/internal/sources/github"
	"github.com/lioilsources/Ol1nDevBot/internal/store"
	"github.com/lioilsources/Ol1nDevBot/internal/store/db"
)

// Summarizer turns a prompt into markdown (the LLM client). May be nil.
type Summarizer interface {
	Complete(ctx context.Context, system, user string) (string, error)
}

// GitHub is the subset of the GitHub client used here.
type GitHub interface {
	GetRepo(ctx context.Context, fullName string) (github.Repo, error)
	OwnerRepos(ctx context.Context, owner string) ([]github.Repo, error)
	Commits(ctx context.Context, repo, branch string, since, until time.Time) ([]github.Activity, error)
	MergedPulls(ctx context.Context, repo string, since, until time.Time) ([]github.Activity, error)
	Releases(ctx context.Context, repo string, since, until time.Time) ([]github.Activity, error)
}

// Service produces changelog events.
type Service struct {
	GH   GitHub
	LLM  Summarizer
	St   *store.Store
	Reg  *config.Registry
	Emit events.Emitter
	Loc  *time.Location
	Log  *slog.Logger
}

const maxRepoMarkdown = 1800

// SystemPrompt instructs the LLM (Czech output, fixed sections, no invention).
const SystemPrompt = `Jsi technický redaktor. Z aktivity jednoho GitHub repozitáře za poslední týden napiš stručný changelog v češtině pro Discord (markdown).
Pravidla:
- Použij pouze tyto sekce v tomto pořadí a vynech prázdné: **✨ Features**, **🐛 Fixes**, **🔧 Infra**.
- Každá položka je jedna krátká odrážka ("- ..."), max 10 odrážek celkem; související commity slouč.
- U položek z PR přidej odkaz ve tvaru [#123](<url>).
- Releasy zmiň jako první odrážku v Features ve tvaru "Vydána verze X".
- Nevymýšlej nic, co v datech není. Žádný úvod, závěr ani nadpis repozitáře.`

// LastWeek returns the previous Monday 00:00 .. this Monday 00:00 in loc.
func LastWeek(now time.Time, loc *time.Location) (from, to time.Time) {
	now = now.In(loc)
	daysSinceMonday := (int(now.Weekday()) + 6) % 7
	to = time.Date(now.Year(), now.Month(), now.Day()-daysSinceMonday, 0, 0, 0, 0, loc)
	return to.AddDate(0, 0, -7), to
}

// Weekly runs the scheduled changelog for the previous week.
func (s *Service) Weekly(ctx context.Context) error {
	from, to := LastWeek(store.Now(), s.Loc)
	_, err := s.Run(ctx, from, to, "", "")
	return err
}

// Now runs an on-demand changelog for the last 7 days (optionally one repo).
func (s *Service) Now(ctx context.Context, repo string) (events.ChangelogEvent, error) {
	to := store.Now().In(s.Loc)
	from := to.AddDate(0, 0, -7)
	return s.Run(ctx, from, to, repo, fmt.Sprint(to.Unix()))
}

// Run collects activity in [from, to) and emits one ChangelogEvent.
func (s *Service) Run(ctx context.Context, from, to time.Time, onlyRepo, nonce string) (events.ChangelogEvent, error) {
	repos, err := s.repos(ctx, onlyRepo)
	if err != nil {
		return events.ChangelogEvent{}, err
	}
	// The header shows the inclusive last day (Sunday for the weekly run).
	ev := events.ChangelogEvent{From: from, To: to.Add(-time.Second), Nonce: nonce}
	var errs []string
	for _, r := range repos {
		acts, err := s.collect(ctx, r, from, to)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", r.FullName, err))
			continue
		}
		acts = filterWindow(acts, from, to)
		if len(acts) == 0 {
			continue
		}
		s.record(ctx, acts)
		ev.Repos = append(ev.Repos, events.RepoChangelog{Repo: r.FullName, Markdown: s.summarize(ctx, r.FullName, acts)})
	}
	if len(repos) > 0 && len(errs) == len(repos) {
		return ev, fmt.Errorf("changelog failed for all repos: %s", strings.Join(errs, "; "))
	}
	if len(errs) > 0 {
		s.Log.Warn("changelog partial failure", "errors", errs)
	}
	sort.Slice(ev.Repos, func(i, j int) bool { return ev.Repos[i].Repo < ev.Repos[j].Repo })
	if _, err := s.Emit.Emit(ctx, ev); err != nil {
		return ev, err
	}
	return ev, nil
}

func (s *Service) repos(ctx context.Context, onlyRepo string) ([]github.Repo, error) {
	if onlyRepo != "" {
		if !strings.Contains(onlyRepo, "/") {
			onlyRepo = s.Reg.File().GitHub.Owner + "/" + onlyRepo
		}
		r, err := s.GH.GetRepo(ctx, onlyRepo)
		if err != nil {
			return nil, err
		}
		return []github.Repo{r}, nil
	}
	gh := s.Reg.File().GitHub
	var out []github.Repo
	names := s.Reg.Repos()
	if gh.WatchAll && gh.Owner != "" {
		all, err := s.GH.OwnerRepos(ctx, gh.Owner)
		if err != nil {
			return nil, err
		}
		for _, r := range all {
			out = append(out, r)
			names = slices.DeleteFunc(names, func(n string) bool { return strings.EqualFold(n, r.FullName) })
		}
	}
	for _, n := range names {
		r, err := s.GH.GetRepo(ctx, n)
		if err != nil {
			s.Log.Warn("github repo lookup failed", "repo", n, "err", err)
			continue
		}
		if !r.Archived {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *Service) collect(ctx context.Context, r github.Repo, from, to time.Time) ([]github.Activity, error) {
	commits, err := s.GH.Commits(ctx, r.FullName, r.DefaultBranch, from, to)
	if err != nil {
		return nil, err
	}
	prs, err := s.GH.MergedPulls(ctx, r.FullName, from, to)
	if err != nil {
		return nil, err
	}
	rels, err := s.GH.Releases(ctx, r.FullName, from, to)
	if err != nil {
		return nil, err
	}
	return append(append(rels, prs...), commits...), nil
}

func filterWindow(acts []github.Activity, from, to time.Time) []github.Activity {
	var out []github.Activity
	for _, a := range acts {
		if !a.At.Before(from) && a.At.Before(to) {
			out = append(out, a)
		}
	}
	return out
}

func (s *Service) record(ctx context.Context, acts []github.Activity) {
	for _, a := range acts {
		if _, err := s.St.InsertGithubActivity(ctx, db.InsertGithubActivityParams{
			Repo: a.Repo, Kind: a.Kind, RefID: a.Ref, Title: a.Title, Url: a.URL, Author: a.Author, OccurredAt: a.At.Unix(),
		}); err != nil {
			s.Log.Warn("record github activity", "err", err)
		}
	}
}

func (s *Service) summarize(ctx context.Context, repo string, acts []github.Activity) string {
	if s.LLM != nil {
		out, err := s.LLM.Complete(ctx, SystemPrompt, Prompt(repo, acts))
		if err == nil {
			return clip(out, maxRepoMarkdown)
		}
		s.Log.Warn("llm summary failed, using raw list", "repo", repo, "err", err)
	}
	return clip(Fallback(acts), maxRepoMarkdown)
}

// Prompt renders activity as the LLM user message.
func Prompt(repo string, acts []github.Activity) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Repozitář: %s\n", repo)
	for _, kind := range []string{github.KindRelease, github.KindPR, github.KindCommit} {
		for _, a := range acts {
			if a.Kind != kind {
				continue
			}
			switch kind {
			case github.KindRelease:
				fmt.Fprintf(&b, "\nRELEASE %s: %s\n", a.Ref, a.Title)
			case github.KindPR:
				fmt.Fprintf(&b, "\nPR #%s (%s): %s\n", a.Ref, a.URL, a.Title)
			default:
				fmt.Fprintf(&b, "\nCOMMIT %s: %s\n", shortSHA(a.Ref), a.Title)
			}
			if a.Body != "" {
				fmt.Fprintf(&b, "  %s\n", strings.ReplaceAll(a.Body, "\n", "\n  "))
			}
		}
	}
	return b.String()
}

var conventional = regexp.MustCompile(`^(\w+)(\([^)]*\))?!?:\s*`)

// Fallback groups activity by conventional-commit type without an LLM.
func Fallback(acts []github.Activity) string {
	sections := map[string][]string{}
	for _, a := range acts {
		sec, title := classify(a.Title)
		line := "- " + title
		switch a.Kind {
		case github.KindRelease:
			sec, line = "features", fmt.Sprintf("- Vydána verze [%s](<%s>)", a.Title, a.URL)
		case github.KindPR:
			line += fmt.Sprintf(" [#%s](<%s>)", a.Ref, a.URL)
		}
		sections[sec] = append(sections[sec], line)
	}
	var b strings.Builder
	for _, s := range []struct{ key, title string }{{"features", "**✨ Features**"}, {"fixes", "**🐛 Fixes**"}, {"infra", "**🔧 Infra**"}} {
		lines := sections[s.key]
		if len(lines) == 0 {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(s.title + "\n")
		const maxLines = 12
		for i, l := range lines {
			if i == maxLines {
				fmt.Fprintf(&b, "- … a %d dalších\n", len(lines)-maxLines)
				break
			}
			b.WriteString(l + "\n")
		}
	}
	return strings.TrimSpace(b.String())
}

func classify(title string) (section, clean string) {
	m := conventional.FindStringSubmatch(title)
	if m == nil {
		return "features", title
	}
	clean = strings.TrimSpace(title[len(m[0]):])
	switch strings.ToLower(m[1]) {
	case "feat", "feature":
		return "features", clean
	case "fix", "bugfix", "hotfix":
		return "fixes", clean
	}
	return "infra", clean
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := strings.LastIndex(s[:n], "\n")
	if cut < n/2 {
		for cut = n; cut > 0 && !utf8.RuneStart(s[cut]); cut-- {
		}
	}
	return s[:cut] + "\n- …"
}

func shortSHA(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}
