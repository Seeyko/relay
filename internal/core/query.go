package core

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Periods lists the named periods accepted by ResolvePeriod. Calendar periods use the
// instance timezone; "last_month" on 2026-11-21 is 2026-10-01 00:00 to 2026-11-01 00:00.
var Periods = []string{
	"today", "yesterday", "this_week", "last_week", "this_month", "last_month",
	"this_quarter", "last_quarter", "this_year", "last_year",
	"last_7d", "last_14d", "last_30d", "last_90d", "last_365d", "all",
}

// ResolvePeriod turns a named period into [since, until). Weeks start on Monday.
func ResolvePeriod(name string, now time.Time, loc *time.Location) (since, until time.Time, err error) {
	if loc == nil {
		loc = time.UTC
	}
	n := now.In(loc)
	day := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, loc)
	week := day.AddDate(0, 0, -((int(day.Weekday()) + 6) % 7))
	month := time.Date(n.Year(), n.Month(), 1, 0, 0, 0, 0, loc)
	quarter := time.Date(n.Year(), time.Month((int(n.Month())-1)/3*3+1), 1, 0, 0, 0, 0, loc)
	year := time.Date(n.Year(), 1, 1, 0, 0, 0, 0, loc)
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "today":
		return day, now, nil
	case "yesterday":
		return day.AddDate(0, 0, -1), day, nil
	case "this_week":
		return week, now, nil
	case "last_week":
		return week.AddDate(0, 0, -7), week, nil
	case "this_month":
		return month, now, nil
	case "last_month":
		return month.AddDate(0, -1, 0), month, nil
	case "this_quarter":
		return quarter, now, nil
	case "last_quarter":
		return quarter.AddDate(0, -3, 0), quarter, nil
	case "this_year":
		return year, now, nil
	case "last_year":
		return year.AddDate(-1, 0, 0), year, nil
	case "last_7d":
		return now.Add(-7 * 24 * time.Hour), now, nil
	case "last_14d":
		return now.Add(-14 * 24 * time.Hour), now, nil
	case "last_30d":
		return now.Add(-30 * 24 * time.Hour), now, nil
	case "last_90d":
		return now.Add(-90 * 24 * time.Hour), now, nil
	case "last_365d":
		return now.Add(-365 * 24 * time.Hour), now, nil
	case "all":
		return time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), now, nil
	}
	return time.Time{}, time.Time{}, invalid("period must be one of %s", strings.Join(Periods, ", "))
}

// ParseTime accepts RFC 3339, a date (2026-10-01, midnight in loc) or a duration ago (720h).
func ParseTime(s string, now time.Time, loc *time.Location) (time.Time, error) {
	if loc == nil {
		loc = time.UTC
	}
	s = strings.TrimSpace(s)
	if d, err := time.ParseDuration(s); err == nil {
		return now.Add(-d), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation("2006-01-02", s, loc); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("%q is not RFC 3339, a date (2006-01-02) or a duration (720h)", s)
}

// Filter narrows the posts an analysis looks at. Empty fields match everything.
// Include lists are OR'ed inside a field; fields are AND'ed together.
type Filter struct {
	Providers         []string
	ExcludeProviders  []string
	AccountIDs        []string
	ExcludeAccountIDs []string
	Formats           []string
	ExcludeFormats    []string
	Hashtags          []string // any of, with or without '#'
	TextContains      string   // case-insensitive
	HasLink           *bool
	HasMedia          *bool
}

func lowerSet(v []string, trim string) map[string]bool {
	if len(v) == 0 {
		return nil
	}
	m := map[string]bool{}
	for _, s := range v {
		s = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(s), trim))
		if s != "" {
			m[s] = true
		}
	}
	return m
}

func (f Filter) match(e enriched, accountID string) bool {
	in := func(v []string, x string) bool {
		s := lowerSet(v, "")
		return s == nil || s[strings.ToLower(x)]
	}
	out := func(v []string, x string) bool { s := lowerSet(v, ""); return s != nil && s[strings.ToLower(x)] }
	if !in(f.Providers, e.provider) || out(f.ExcludeProviders, e.provider) {
		return false
	}
	if !in(f.AccountIDs, accountID) || out(f.ExcludeAccountIDs, accountID) {
		return false
	}
	if !in(f.Formats, e.format) || out(f.ExcludeFormats, e.format) {
		return false
	}
	if tags := lowerSet(f.Hashtags, "#"); tags != nil {
		ok := false
		for _, m := range hashtagRe.FindAllStringSubmatch(e.text, -1) {
			if tags[strings.ToLower(m[1])] {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	if f.TextContains != "" && !strings.Contains(strings.ToLower(e.text), strings.ToLower(f.TextContains)) {
		return false
	}
	if f.HasLink != nil && urlRe.MatchString(e.text) != *f.HasLink {
		return false
	}
	if f.HasMedia != nil && (len(e.p.Media) > 0 || mediaFormat(e.format)) != *f.HasMedia {
		return false
	}
	return true
}

func mediaFormat(f string) bool {
	switch f {
	case "image", "video", "carousel", "media", "reel", "story", "short", "document":
		return true
	}
	return false
}

// activity replaces each post's lifetime counters with what they gained inside [since, until),
// read from metric snapshots. A post published before since needs a snapshot at or before
// since as its baseline; posts without one are dropped and counted in the returned number.
func (s *Service) activity(ctx context.Context, es []enriched, since, until time.Time) ([]enriched, int, error) {
	var out []enriched
	missing := 0
	for _, e := range es {
		snaps, err := s.Store.PostSnapshots(ctx, e.p.ID)
		if err != nil {
			return nil, 0, err
		}
		var start, end map[string]int64
		for _, sn := range snaps {
			if !sn.At.After(since) {
				start = sn.Metrics
			}
			if sn.At.Before(until) {
				end = sn.Metrics
			}
		}
		publishedInside := e.p.PublishedAt != nil && !e.p.PublishedAt.Before(since)
		if start == nil {
			if !publishedInside {
				missing++
				continue
			}
			start = map[string]int64{}
		}
		if end == nil {
			// published inside the period but never measured before until: counters unknown
			missing++
			continue
		}
		a, b := Normalize(start), Normalize(end)
		d := map[string]int64{}
		for k, v := range b {
			if v < 0 {
				d[k] = -1
				continue
			}
			d[k] = max(v-max(a[k], 0), 0)
		}
		e.norm = d
		out = append(out, e)
	}
	return out, missing, nil
}

// describe echoes the filters that were applied, so an agent can check how it was understood.
func (f Filter) describe(in AnalyzeInput) map[string]any {
	m := map[string]any{}
	add := func(k string, v []string) {
		if len(v) > 0 {
			m[k] = v
		}
	}
	if in.Provider != "" {
		m["provider"] = in.Provider
	}
	if in.AccountID != "" {
		m["account_id"] = in.AccountID
	}
	add("providers", f.Providers)
	add("exclude_providers", f.ExcludeProviders)
	add("account_ids", f.AccountIDs)
	add("exclude_account_ids", f.ExcludeAccountIDs)
	add("formats", f.Formats)
	add("exclude_formats", f.ExcludeFormats)
	add("hashtags", f.Hashtags)
	if f.TextContains != "" {
		m["text_contains"] = f.TextContains
	}
	if f.HasLink != nil {
		m["has_link"] = *f.HasLink
	}
	if f.HasMedia != nil {
		m["has_media"] = *f.HasMedia
	}
	if in.IncludeReposts {
		m["include_reposts"] = true
	}
	if len(m) == 0 {
		return nil
	}
	return m
}
