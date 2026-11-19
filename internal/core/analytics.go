package core

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Seeyko/relay/internal/store"
)

// Normalized metric names, comparable across networks.
var NormalizedMetrics = []string{"views", "likes", "comments", "shares", "saves", "engagement"}

// aliases maps each normalized metric to the provider keys that feed it (summed).
var aliases = map[string][]string{
	"views":    {"impression_count", "view_count", "views"},
	"likes":    {"like_count"},
	"comments": {"reply_count", "comment_count"},
	"shares":   {"retweet_count", "quote_count", "share_count", "repost_count", "shares"},
	"saves":    {"bookmark_count", "saved"},
}

// Normalize maps raw provider metrics to views/likes/comments/shares/saves/engagement.
// views is -1 when the network did not report any.
func Normalize(raw map[string]int64) map[string]int64 {
	out := map[string]int64{}
	for k, keys := range aliases {
		var v int64
		found := false
		for _, rk := range keys {
			if x, ok := raw[rk]; ok {
				v += x
				found = true
			}
		}
		if k == "views" && !found {
			v = -1
		}
		out[k] = v
	}
	out["engagement"] = out["likes"] + out["comments"] + out["shares"] + out["saves"]
	return out
}

func engagementRate(n map[string]int64) float64 {
	if n["views"] <= 0 {
		return 0
	}
	return float64(n["engagement"]) / float64(n["views"])
}

var hashtagRe = regexp.MustCompile(`(?:^|\s)#([\p{L}\p{N}_]+)`)
var urlRe = regexp.MustCompile(`https?://\S+`)

// Format classifies a post: thread, reply, repost, quote, video, image, carousel, article, text.
func Format(p *store.Post, provider string) string {
	if k, _ := p.Options["kind"].(string); k != "" {
		return k
	}
	if provider == "website" {
		return "article"
	}
	if _, ok := p.Options["reply_to"]; ok {
		return "reply"
	}
	if len(p.Parts) > 1 {
		return "thread"
	}
	text := ""
	if len(p.Parts) > 0 {
		text = p.Parts[0]
	}
	if strings.HasPrefix(text, "RT @") {
		return "repost"
	}
	if t, _ := p.Options["media_types"].(string); t != "" {
		return t
	}
	if len(p.Media) > 1 {
		return "carousel"
	}
	if len(p.Media) == 1 {
		return "media"
	}
	return "text"
}

func lengthBucket(text string) string {
	n := len([]rune(urlRe.ReplaceAllString(text, "")))
	switch {
	case n < 80:
		return "0-79"
	case n < 160:
		return "80-159"
	case n < 280:
		return "160-279"
	case n < 1000:
		return "280-999"
	default:
		return "1000+"
	}
}

type AnalyzeInput struct {
	Since     time.Time
	Until     time.Time
	AccountID string
	Provider  string
	GroupBy   string // network, account, day, week, month, weekday, hour, format, length, hashtag, has_link
	Metric    string // views, likes, comments, shares, saves, engagement, engagement_rate (or any raw key)
	Top       int
	Location  *time.Location
	// IncludeReposts keeps reposts (RTs) in the analysis; their counters belong to someone else.
	IncludeReposts bool
}

type GroupRow struct {
	Key            string           `json:"key"`
	Posts          int              `json:"posts"`
	Totals         map[string]int64 `json:"totals"`
	Avg            float64          `json:"avg"`
	Median         float64          `json:"median"`
	EngagementRate float64          `json:"engagement_rate"`
}

type RankedPost struct {
	ID      string           `json:"id"`
	Network string           `json:"network"`
	Account string           `json:"account"`
	URL     string           `json:"url"`
	Text    string           `json:"text"`
	Format  string           `json:"format"`
	At      *time.Time       `json:"published_at"`
	Value   float64          `json:"value"`
	Metrics map[string]int64 `json:"metrics"`
}

type Analysis struct {
	Period struct {
		Since time.Time `json:"since"`
		Until time.Time `json:"until"`
	} `json:"period"`
	Metric   string            `json:"metric"`
	GroupBy  string            `json:"group_by"`
	Posts    int               `json:"posts"`
	Totals   map[string]int64  `json:"totals"`
	Previous map[string]int64  `json:"previous_period_totals"`
	Change   map[string]string `json:"change_vs_previous"`
	Groups   []GroupRow        `json:"groups,omitempty"`
	Top      []RankedPost      `json:"top"`
	Bottom   []RankedPost      `json:"bottom"`
	Notes    []string          `json:"notes,omitempty"`
}

type enriched struct {
	p        *store.Post
	provider string
	account  string
	norm     map[string]int64
	text     string
	format   string
}

func (s *Service) loadEnriched(ctx context.Context, in AnalyzeInput, since, until time.Time) ([]enriched, error) {
	accs, err := s.Store.ListAccounts(ctx)
	if err != nil {
		return nil, err
	}
	byID := map[string]*store.Account{}
	for _, a := range accs {
		byID[a.ID] = a
	}
	var out []enriched
	for offset := 0; ; offset += 1000 {
		posts, err := s.Store.ListPosts(ctx, store.PostFilter{Status: store.StatusPublished, AccountID: in.AccountID, From: &since, To: &until, Limit: 1000, Offset: offset})
		if err != nil {
			return nil, err
		}
		for _, p := range posts {
			a := byID[p.AccountID]
			if a == nil || (in.Provider != "" && a.Provider != in.Provider) {
				continue
			}
			e := enriched{p: p, provider: a.Provider, account: a.Handle, norm: Normalize(p.Metrics), format: Format(p, a.Provider)}
			if e.account == "" {
				e.account = a.Name
			}
			if len(p.Parts) > 0 {
				e.text = strings.Join(p.Parts, "\n")
			}
			if e.format == "repost" && !in.IncludeReposts {
				continue
			}
			out = append(out, e)
		}
		if len(posts) < 1000 {
			break
		}
	}
	return out, nil
}

func value(e enriched, metric string) (float64, bool) {
	if metric == "engagement_rate" {
		if e.norm["views"] <= 0 {
			return 0, false
		}
		return engagementRate(e.norm), true
	}
	if v, ok := e.norm[metric]; ok {
		if v < 0 {
			return 0, false
		}
		return float64(v), true
	}
	v, ok := e.p.Metrics[metric]
	return float64(v), ok
}

func totals(es []enriched) map[string]int64 {
	t := map[string]int64{}
	for _, e := range es {
		for k, v := range e.norm {
			if v > 0 {
				t[k] += v
			}
		}
	}
	return t
}

func (s *Service) groupKeys(e enriched, by string, loc *time.Location) []string {
	at := time.Time{}
	if e.p.PublishedAt != nil {
		at = e.p.PublishedAt.In(loc)
	}
	switch by {
	case "network":
		return []string{e.provider}
	case "account":
		return []string{e.provider + ":" + e.account}
	case "day":
		return []string{at.Format("2006-01-02")}
	case "week":
		y, w := at.ISOWeek()
		return []string{fmt.Sprintf("%d-W%02d", y, w)}
	case "month":
		return []string{at.Format("2006-01")}
	case "weekday":
		return []string{fmt.Sprintf("%d-%s", (int(at.Weekday())+6)%7+1, at.Weekday())}
	case "hour":
		return []string{fmt.Sprintf("%02dh", at.Hour())}
	case "format":
		return []string{e.format}
	case "length":
		return []string{lengthBucket(e.text)}
	case "has_link":
		if urlRe.MatchString(e.text) {
			return []string{"with link"}
		}
		return []string{"no link"}
	case "hashtag":
		var keys []string
		seen := map[string]bool{}
		for _, m := range hashtagRe.FindAllStringSubmatch(e.text, -1) {
			t := "#" + strings.ToLower(m[1])
			if !seen[t] {
				seen[t] = true
				keys = append(keys, t)
			}
		}
		if len(keys) == 0 {
			return []string{"(none)"}
		}
		return keys
	}
	return nil
}

var GroupByOptions = []string{"network", "account", "day", "week", "month", "weekday", "hour", "format", "length", "hashtag", "has_link"}

// Analyze computes cross-network performance over a period, compared with the previous period of equal length.
func (s *Service) Analyze(ctx context.Context, in AnalyzeInput) (*Analysis, error) {
	if in.Metric == "" {
		in.Metric = "engagement"
	}
	if in.Top <= 0 {
		in.Top = 5
	}
	if in.Location == nil {
		in.Location = time.UTC
	}
	until := in.Until
	if until.IsZero() {
		until = s.now()
	}
	since := in.Since
	if since.IsZero() {
		since = until.Add(-30 * 24 * time.Hour)
	}
	if in.GroupBy != "" && s.groupKeys(enriched{p: &store.Post{}}, in.GroupBy, time.UTC) == nil {
		return nil, invalid("group_by must be one of %s", strings.Join(GroupByOptions, ", "))
	}
	cur, err := s.loadEnriched(ctx, in, since, until)
	if err != nil {
		return nil, err
	}
	prev, err := s.loadEnriched(ctx, in, since.Add(-until.Sub(since)), since)
	if err != nil {
		return nil, err
	}
	a := &Analysis{Metric: in.Metric, GroupBy: in.GroupBy, Posts: len(cur), Totals: totals(cur), Previous: totals(prev), Change: map[string]string{}}
	a.Period.Since, a.Period.Until = since, until
	for _, k := range NormalizedMetrics {
		p, c := a.Previous[k], a.Totals[k]
		switch {
		case p > 0:
			a.Change[k] = fmt.Sprintf("%+.0f%%", (float64(c)-float64(p))/float64(p)*100)
		case c > 0:
			a.Change[k] = "new"
		}
	}
	a.Totals["posts"], a.Previous["posts"] = int64(len(cur)), int64(len(prev))

	missingViews := map[string]bool{}
	var ranked []RankedPost
	for _, e := range cur {
		if e.norm["views"] < 0 {
			missingViews[e.provider] = true
		}
		v, ok := value(e, in.Metric)
		if !ok {
			continue
		}
		ranked = append(ranked, RankedPost{ID: e.p.ID, Network: e.provider, Account: e.account, URL: e.p.RemoteURL, Text: truncate(e.text, 140), Format: e.format, At: e.p.PublishedAt, Value: v, Metrics: e.norm})
	}
	sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].Value > ranked[j].Value })
	a.Top = ranked[:min(in.Top, len(ranked))]
	if len(ranked) > in.Top {
		rest := ranked[max(in.Top, len(ranked)-in.Top):]
		a.Bottom = make([]RankedPost, len(rest))
		for i := range rest {
			a.Bottom[i] = rest[len(rest)-1-i]
		}
	}

	if in.GroupBy != "" {
		groups := map[string][]enriched{}
		for _, e := range cur {
			for _, k := range s.groupKeys(e, in.GroupBy, in.Location) {
				groups[k] = append(groups[k], e)
			}
		}
		for k, es := range groups {
			row := GroupRow{Key: k, Posts: len(es), Totals: totals(es)}
			var vals []float64
			for _, e := range es {
				if v, ok := value(e, in.Metric); ok {
					vals = append(vals, v)
				}
			}
			row.Avg, row.Median = mean(vals), median(vals)
			if row.Totals["views"] > 0 {
				row.EngagementRate = round(float64(row.Totals["engagement"])/float64(row.Totals["views"]), 4)
			}
			a.Groups = append(a.Groups, row)
		}
		sort.Slice(a.Groups, func(i, j int) bool {
			if timeOrdered(in.GroupBy) {
				return a.Groups[i].Key < a.Groups[j].Key
			}
			return a.Groups[i].Avg > a.Groups[j].Avg
		})
		small := 0
		for _, g := range a.Groups {
			if g.Posts < 3 {
				small++
			}
		}
		if small > 0 && !timeOrdered(in.GroupBy) {
			a.Notes = append(a.Notes, fmt.Sprintf("%d group(s) have fewer than 3 posts: treat their averages as anecdotal", small))
		}
	}
	for p := range missingViews {
		a.Notes = append(a.Notes, p+" does not report views for some posts: engagement_rate excludes them")
	}
	if in.Metric == "views" || in.Metric == "engagement_rate" {
		a.Notes = append(a.Notes, "X only reports impressions for your own posts younger than 30 days")
	}
	return a, nil
}

func timeOrdered(by string) bool {
	return by == "day" || by == "week" || by == "month" || by == "weekday" || by == "hour"
}

func mean(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	var s float64
	for _, x := range v {
		s += x
	}
	return round(s/float64(len(v)), 4)
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	c := append([]float64(nil), v...)
	sort.Float64s(c)
	m := len(c) / 2
	if len(c)%2 == 1 {
		return c[m]
	}
	return round((c[m-1]+c[m])/2, 4)
}

func round(x float64, d int) float64 {
	p := math.Pow(10, float64(d))
	return math.Round(x*p) / p
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// ---------- per-post timeline and account growth ----------

type Timeline struct {
	PostID      string                      `json:"post_id"`
	PublishedAt *time.Time                  `json:"published_at"`
	Points      []TimelinePoint             `json:"points"`
	AtAge       map[string]map[string]int64 `json:"at_age"` // "1h", "24h", "7d" -> normalized metrics (first snapshot after that age)
}

type TimelinePoint struct {
	At      time.Time        `json:"at"`
	AgeH    float64          `json:"age_hours,omitempty"`
	Metrics map[string]int64 `json:"metrics"`
}

func (s *Service) PostTimeline(ctx context.Context, postID string) (*Timeline, error) {
	p, err := s.Store.GetPost(ctx, postID)
	if err != nil {
		return nil, err
	}
	snaps, err := s.Store.PostSnapshots(ctx, postID)
	if err != nil {
		return nil, err
	}
	t := &Timeline{PostID: postID, PublishedAt: p.PublishedAt, AtAge: map[string]map[string]int64{}}
	for _, sn := range snaps {
		pt := TimelinePoint{At: sn.At, Metrics: Normalize(sn.Metrics)}
		if p.PublishedAt != nil {
			pt.AgeH = round(sn.At.Sub(*p.PublishedAt).Hours(), 1)
		}
		t.Points = append(t.Points, pt)
	}
	for _, age := range []struct {
		k string
		d time.Duration
	}{{"1h", time.Hour}, {"24h", 24 * time.Hour}, {"7d", 7 * 24 * time.Hour}, {"30d", 30 * 24 * time.Hour}} {
		for _, pt := range t.Points {
			if p.PublishedAt != nil && pt.At.Sub(*p.PublishedAt) >= age.d {
				t.AtAge[age.k] = pt.Metrics
				break
			}
		}
	}
	return t, nil
}

type Growth struct {
	AccountID string             `json:"account_id"`
	Network   string             `json:"network"`
	Handle    string             `json:"handle"`
	First     *store.Snapshot    `json:"first,omitempty"`
	Last      *store.Snapshot    `json:"last,omitempty"`
	Delta     map[string]int64   `json:"delta"`
	PerDay    map[string]float64 `json:"per_day"`
	Points    []store.Snapshot   `json:"points,omitempty"`
}

func (s *Service) AccountGrowth(ctx context.Context, accountID string, since time.Time, withPoints bool) ([]Growth, error) {
	accs, err := s.accounts(ctx, accountID)
	if err != nil {
		return nil, err
	}
	var out []Growth
	for _, a := range accs {
		snaps, err := s.Store.AccountSnapshots(ctx, a.ID, since)
		if err != nil {
			return nil, err
		}
		g := Growth{AccountID: a.ID, Network: a.Provider, Handle: a.Handle, Delta: map[string]int64{}, PerDay: map[string]float64{}}
		if len(snaps) > 0 {
			f, l := snaps[0], snaps[len(snaps)-1]
			g.First, g.Last = &f, &l
			days := l.At.Sub(f.At).Hours() / 24
			for k, v := range l.Metrics {
				g.Delta[k] = v - f.Metrics[k]
				if days >= 1 {
					g.PerDay[k] = round(float64(g.Delta[k])/days, 2)
				}
			}
		}
		if withPoints {
			g.Points = snaps
		}
		out = append(out, g)
	}
	return out, nil
}
