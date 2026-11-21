package api_test

import (
	"context"
	"encoding/json"
	"net/url"
	"testing"
	"time"

	"github.com/Seeyko/relay/internal/core"
	"github.com/Seeyko/relay/internal/store"
)

func TestResolvePeriod(t *testing.T) {
	paris, _ := time.LoadLocation("Europe/Paris")
	now := time.Date(2026, 11, 21, 13, 0, 0, 0, paris) // a Saturday
	day := func(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, paris) }
	for _, c := range []struct {
		name         string
		since, until time.Time
	}{
		{"last_month", day(2026, 10, 1), day(2026, 11, 1)},
		{"this_month", day(2026, 11, 1), now},
		{"last_week", day(2026, 11, 9), day(2026, 11, 16)},
		{"this_week", day(2026, 11, 16), now},
		{"yesterday", day(2026, 11, 20), day(2026, 11, 21)},
		{"last_quarter", day(2026, 7, 1), day(2026, 10, 1)},
		{"last_year", day(2025, 1, 1), day(2026, 1, 1)},
		{"last_7d", now.Add(-7 * 24 * time.Hour), now},
	} {
		s, u, err := core.ResolvePeriod(c.name, now, paris)
		if err != nil || !s.Equal(c.since) || !u.Equal(c.until) {
			t.Errorf("%s: %v %v %v, want %v %v", c.name, s, u, err, c.since, c.until)
		}
	}
	if _, _, err := core.ResolvePeriod("last_decade", now, paris); err == nil {
		t.Error("unknown period must fail")
	}
	if got, err := core.ParseTime("2026-10-01", now, paris); err != nil || !got.Equal(day(2026, 10, 1)) {
		t.Errorf("date: %v %v", got, err)
	}
}

type seeded struct {
	e                *env
	x, ig, tk        *store.Account
	igVideo, xOld    *store.Post
	since, until     time.Time
	insideBefore     time.Time
	lastYearInPeriod time.Time
}

// seed: three networks, posts in October 2026 (the "last month" of the fixed clock),
// one post in September, one in October 2025.
func seed(t *testing.T) *seeded {
	e := newEnv(t)
	ctx := context.Background()
	e.svc.Location = time.UTC
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	e.svc.Now = func() time.Time { return now }
	s := &seeded{e: e, since: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), until: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}
	acc := func(p string) *store.Account {
		a := &store.Account{Provider: p, Handle: "rmp-" + p}
		if err := e.svc.Store.CreateAccount(ctx, a); err != nil {
			t.Fatal(err)
		}
		return a
	}
	s.x, s.ig, s.tk = acc("x"), acc("instagram"), acc("tiktok")
	post := func(a *store.Account, at time.Time, text, kind string, m map[string]int64) *store.Post {
		p := &store.Post{AccountID: a.ID, Status: store.StatusPublished, Parts: []string{text}, Options: map[string]any{"kind": kind}, PublishedAt: &at, RemoteID: text, Metrics: m}
		if err := e.svc.Store.CreatePost(ctx, p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	oct := func(d int) time.Time { return time.Date(2026, 10, d, 18, 0, 0, 0, time.UTC) }
	post(s.x, oct(2), "x text https://ridemypark.com", "text", map[string]int64{"impression_count": 1000, "like_count": 10})
	s.igVideo = post(s.ig, oct(3), "ig reel #skatepark", "video", map[string]int64{"views": 500, "like_count": 50, "comments_count": 0, "comment_count": 5})
	post(s.ig, oct(20), "ig photo #bmx", "image", map[string]int64{"views": 200, "like_count": 20})
	post(s.tk, oct(25), "tk video #skatepark lyon", "video", map[string]int64{"views": 3000, "like_count": 300})
	s.xOld = post(s.x, time.Date(2026, 9, 10, 18, 0, 0, 0, time.UTC), "x september", "text", map[string]int64{"impression_count": 400, "like_count": 4})
	post(s.ig, time.Date(2025, 10, 5, 18, 0, 0, 0, time.UTC), "ig last year", "image", map[string]int64{"views": 100, "like_count": 1})
	return s
}

func analyze(t *testing.T, e *env, args map[string]any) *core.Analysis {
	t.Helper()
	text, isErr := e.callTool("analyze", args)
	if isErr {
		t.Fatalf("analyze %v: %s", args, text)
	}
	var a core.Analysis
	if err := json.Unmarshal([]byte(text), &a); err != nil {
		t.Fatalf("%v: %s", err, text)
	}
	return &a
}

func TestAnalyzeFiltersAndPeriods(t *testing.T) {
	s := seed(t)
	// "views on every platform except X last month"
	a := analyze(t, s.e, map[string]any{"period": "last_month", "exclude_providers": []string{"x"}, "metric": "views"})
	if a.Posts != 3 || a.Totals["views"] != 3700 || !a.Period.Since.Equal(s.since) || !a.Period.Until.Equal(s.until) {
		t.Fatalf("except x: posts=%d totals=%v period=%+v", a.Posts, a.Totals, a.Period)
	}
	if a.Filters["exclude_providers"] == nil {
		t.Fatalf("filters not echoed: %v", a.Filters)
	}
	// comma separated lists work too (REST query strings)
	a = analyze(t, s.e, map[string]any{"period": "last_month", "providers": "instagram, tiktok", "formats": "video", "hashtags": []string{"#SkatePark"}})
	if a.Posts != 2 || a.Totals["views"] != 3500 {
		t.Fatalf("ig+tk skatepark videos: %d %v", a.Posts, a.Totals)
	}
	a = analyze(t, s.e, map[string]any{"period": "last_month", "text_contains": "LYON"})
	if a.Posts != 1 || a.Top[0].Network != "tiktok" {
		t.Fatalf("text: %+v", a)
	}
	a = analyze(t, s.e, map[string]any{"period": "last_month", "has_link": true})
	if a.Posts != 1 || a.Top[0].Network != "x" {
		t.Fatalf("has_link: %+v", a)
	}
	a = analyze(t, s.e, map[string]any{"period": "last_month", "exclude_account_ids": []string{s.ig.ID}, "exclude_formats": []string{"text"}})
	if a.Posts != 1 {
		t.Fatalf("exclude account + format: %d", a.Posts)
	}
	// comparisons
	a = analyze(t, s.e, map[string]any{"period": "last_month", "providers": []string{"x"}})
	if a.Previous["posts"] != 1 || a.Previous["views"] != 400 || a.Change["views"] != "+150%" {
		t.Fatalf("previous: %v %v", a.Previous, a.Change)
	}
	a = analyze(t, s.e, map[string]any{"period": "last_month", "providers": []string{"instagram"}, "compare": "previous_year"})
	if a.Previous["posts"] != 1 || a.Previous["views"] != 100 || a.PrevPer == nil || a.PrevPer.Since.Year() != 2025 {
		t.Fatalf("previous_year: %v %+v", a.Previous, a.PrevPer)
	}
	a = analyze(t, s.e, map[string]any{"period": "last_month", "compare": "none"})
	if a.Previous != nil || len(a.Change) != 0 {
		t.Fatalf("none: %v %v", a.Previous, a.Change)
	}
	// two dimensions
	a = analyze(t, s.e, map[string]any{"since": "2026-09-01", "until": "2026-11-01", "group_by": "network,month", "metric": "views"})
	keys := map[string]int64{}
	for _, g := range a.Groups {
		keys[g.Key] = g.Totals["views"]
		if len(g.Keys) != 2 {
			t.Fatalf("keys: %+v", g)
		}
	}
	if keys["x | 2026-09"] != 400 || keys["x | 2026-10"] != 1000 || keys["instagram | 2026-10"] != 700 || len(keys) != 4 {
		t.Fatalf("network,month: %v", keys)
	}
	// errors
	for _, bad := range []map[string]any{
		{"period": "last_month", "since": "720h"},
		{"period": "fortnight"},
		{"basis": "lifetime"},
		{"compare": "yesterday"},
		{"group_by": "network,colour"},
		{"since": "2026-11-01", "until": "2026-10-01"},
	} {
		if text, isErr := s.e.callTool("analyze", bad); !isErr {
			t.Errorf("%v should fail: %s", bad, text)
		}
	}
}

func TestAnalyzeActivityBasis(t *testing.T) {
	s := seed(t)
	ctx := context.Background()
	st := s.e.svc.Store
	// The September X post was measured before October (baseline 300 views), then grew to 400.
	st.AddPostSnapshot(ctx, s.xOld.ID, time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC), map[string]int64{"impression_count": 300, "like_count": 3})
	st.AddPostSnapshot(ctx, s.xOld.ID, time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC), map[string]int64{"impression_count": 400, "like_count": 4})
	// The October reel: published inside, starts at 0, reached 450 views by the end of October
	// (and 500 later, which must not count).
	st.AddPostSnapshot(ctx, s.igVideo.ID, time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC), map[string]int64{"views": 450, "like_count": 40})
	st.AddPostSnapshot(ctx, s.igVideo.ID, time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC), map[string]int64{"views": 500, "like_count": 50})
	a := analyze(t, s.e, map[string]any{"period": "last_month", "basis": "activity", "metric": "views", "group_by": "network", "compare": "none"})
	if a.Basis != "activity" || a.Totals["views"] != 550 || a.Totals["likes"] != 41 || a.Posts != 2 {
		t.Fatalf("activity totals: posts=%d %v", a.Posts, a.Totals)
	}
	// The other October posts and last year's post have no snapshot: reported, not invented.
	found := false
	for _, n := range a.Notes {
		if len(n) > 0 && n[:14] == "basis=activity" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing-baseline note: %v", a.Notes)
	}
}

func TestAnalyticsRESTQuery(t *testing.T) {
	s := seed(t)
	q := url.Values{"period": {"last_month"}, "exclude_providers": {"x,tiktok"}, "metric": {"views"}}
	code, body, raw := s.e.req("GET", "/v1/analytics?"+q.Encode(), nil)
	if code != 200 || body["posts"].(float64) != 2 {
		t.Fatalf("%d %s", code, raw)
	}
}
