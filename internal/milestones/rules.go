// Package milestones evaluates declarative milestone rules over versions,
// metrics and deploys. Each (app, key) is reached — and announced — once.
package milestones

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/lioilsources/Ol1nDevBot/internal/events"
	"github.com/lioilsources/Ol1nDevBot/internal/store/db"
)

// Facts is the data a rule looks at.
type Facts struct {
	// Versions of the app (all stores). Announced=0 marks versions that were
	// already live when the bot started tracking a launched app.
	Versions []db.AppVersion
	// TotalDownloads is cumulative downloads (Apple units + Play user installs)
	// including the configured downloads_baseline.
	TotalDownloads int64
	// WeekDownloads / PrevWeekDownloads are the last two full 7-day windows.
	WeekDownloads, PrevWeekDownloads int64
	// SuccessfulDeploys per service.
	SuccessfulDeploys map[string]int64
}

// Hit is a reached milestone.
type Hit struct {
	Key    string // stored key (deploys_100 is per service: "deploys_100:<service>")
	Title  string
	Detail string
}

// WeeklyGrowthMin is the minimum weekly downloads for weekly_growth_2x.
const WeeklyGrowthMin = 50

// Evaluate returns the hits for one configured milestone key.
//
// Version rules only fire when they hold on all versions but did not already
// hold on the pre-existing (silently recorded) ones, so launching the bot for
// an established app does not re-announce history.
func Evaluate(key string, f Facts) []Hit {
	switch {
	case key == "first_release":
		if firstRelease(f.Versions) && !firstRelease(preexisting(f.Versions)) {
			v := announced(f.Versions)[0]
			return []Hit{{Key: key, Title: "První vydání! 🎉", Detail: fmt.Sprintf("Verze %s je live v %s.", v.Version, events.StoreLabel(v.Store))}}
		}
	case key == "v1_0":
		if v, ok := major1(f.Versions); ok {
			if _, pre := major1(preexisting(f.Versions)); !pre {
				return []Hit{{Key: key, Title: "Verze 1.0!", Detail: fmt.Sprintf("%s %s je venku.", events.StoreLabel(v.Store), v.Version)}}
			}
		}
	case key == "both_stores":
		if bothStores(f.Versions) && !bothStores(preexisting(f.Versions)) {
			return []Hit{{Key: key, Title: "Live na iOS i Androidu 📱🤖", Detail: "Appka je teď v App Store i na Google Play."}}
		}
	case strings.HasPrefix(key, "downloads_"):
		n, err := strconv.ParseInt(strings.TrimPrefix(key, "downloads_"), 10, 64)
		if err == nil && n > 0 && f.TotalDownloads >= n {
			return []Hit{{Key: key, Title: FormatCount(n) + " stažení!", Detail: fmt.Sprintf("Celkem %s stažení napříč story.", FormatCount(f.TotalDownloads))}}
		}
	case key == "weekly_growth_2x":
		if f.WeekDownloads >= WeeklyGrowthMin && f.PrevWeekDownloads > 0 && f.WeekDownloads >= 2*f.PrevWeekDownloads {
			return []Hit{{Key: key, Title: "Stažení za týden 2× vyšší 📈", Detail: fmt.Sprintf("%s stažení za posledních 7 dní vs. %s týden předtím.", FormatCount(f.WeekDownloads), FormatCount(f.PrevWeekDownloads))}}
		}
	case key == "deploys_100":
		var hits []Hit
		for svc, n := range f.SuccessfulDeploys {
			if n >= 100 {
				hits = append(hits, Hit{Key: "deploys_100:" + svc, Title: "100 úspěšných deployů " + svc + " 🚀", Detail: fmt.Sprintf("%s má za sebou %d úspěšných deployů.", svc, n)})
			}
		}
		return hits
	}
	return nil
}

func preexisting(vs []db.AppVersion) []db.AppVersion {
	var out []db.AppVersion
	for _, v := range vs {
		if v.Announced == 0 {
			out = append(out, v)
		}
	}
	return out
}

func announced(vs []db.AppVersion) []db.AppVersion {
	var out []db.AppVersion
	for _, v := range vs {
		if v.Announced == 1 {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		return vs
	}
	return out
}

func firstRelease(vs []db.AppVersion) bool { return len(vs) > 0 }

func major1(vs []db.AppVersion) (db.AppVersion, bool) {
	for _, v := range vs {
		if Major(v.Version) >= 1 {
			return v, true
		}
	}
	return db.AppVersion{}, false
}

func bothStores(vs []db.AppVersion) bool {
	var ios, android bool
	for _, v := range vs {
		ios = ios || v.Store == events.StoreAppStore
		android = android || v.Store == events.StorePlayStore
	}
	return ios && android
}

// Major returns the leading numeric component of a version ("v1.2.3" → 1), or -1.
func Major(v string) int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	end := 0
	for end < len(v) && v[end] >= '0' && v[end] <= '9' {
		end++
	}
	n, err := strconv.Atoi(v[:end])
	if err != nil {
		return -1
	}
	return n
}

// FormatCount formats with a non-breaking space thousands separator.
func FormatCount(n int64) string {
	s := strconv.FormatInt(n, 10)
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 && s[0] != '-' {
			b.WriteRune(' ')
		}
		b.WriteRune(r)
	}
	return b.String()
}
