package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Seeyko/relay/internal/api"
	"github.com/Seeyko/relay/internal/core"
	"github.com/Seeyko/relay/internal/mcp"
	"github.com/Seeyko/relay/internal/store"
)

func connectFakeX(t *testing.T, e *env, h http.Handler) *store.Account {
	t.Helper()
	xs := httptest.NewServer(h)
	t.Cleanup(xs.Close)
	acc, err := e.svc.ConnectAccount(context.Background(), "x", "", map[string]string{"consumer_key": "a", "consumer_secret": "b", "access_token": "c", "access_token_secret": "d", "base_url": xs.URL})
	if err != nil {
		t.Fatal(err)
	}
	return acc
}

func TestAnalyticsAcrossTime(t *testing.T) {
	e := newEnv(t)
	likes := int64(1)
	views := int64(10)
	acc := connectFakeX(t, e, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/2/users/me" && strings.Contains(r.URL.RawQuery, "public_metrics"):
			w.Write([]byte(`{"data":{"id":"42","username":"alice","public_metrics":{"followers_count":100,"following_count":5,"tweet_count":12}}}`))
		case r.URL.Path == "/2/users/me":
			w.Write([]byte(`{"data":{"id":"42","username":"alice"}}`))
		case r.URL.Path == "/2/tweets" && r.Method == "GET":
			var data []map[string]any
			for _, id := range strings.Split(r.URL.Query().Get("ids"), ",") {
				data = append(data, map[string]any{"id": id, "public_metrics": map[string]int64{"like_count": atomic.LoadInt64(&likes), "reply_count": 1, "retweet_count": 2}, "non_public_metrics": map[string]int64{"impression_count": atomic.LoadInt64(&views)}})
			}
			json.NewEncoder(w).Encode(map[string]any{"data": data})
		default:
			w.WriteHeader(404)
		}
	}))
	ctx := context.Background()
	base := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC) // a Monday
	clock := base
	e.svc.Now = func() time.Time { return clock }

	mk := func(text string, at time.Time, opts map[string]any, metrics map[string]int64, rid string) *store.Post {
		p := &store.Post{AccountID: acc.ID, Status: store.StatusPublished, Parts: []string{text}, Options: opts, PublishedAt: &at, RemoteID: rid, Metrics: metrics}
		if err := e.svc.Store.CreatePost(ctx, p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	video := mk("video post #golang", base, map[string]any{"kind": "video"}, nil, "1")
	mk("text post #golang https://example.com", base.Add(2*time.Hour), map[string]any{"kind": "text"}, map[string]int64{"like_count": 3, "impression_count": 100}, "2")
	mk("RT @someone: not mine", base.Add(3*time.Hour), nil, map[string]int64{"like_count": 999}, "3")
	mk("old post", base.Add(-30*time.Hour), map[string]any{"kind": "text"}, map[string]int64{"like_count": 1, "impression_count": 10}, "4") // previous period

	// collector: first pass fetches everything due, then growth over time
	clock = base.Add(30 * time.Minute)
	e.svc.Collect(ctx, 30*24*time.Hour)
	atomic.StoreInt64(&likes, 5)
	atomic.StoreInt64(&views, 50)
	clock = base.Add(26 * time.Hour)
	e.svc.Collect(ctx, 30*24*time.Hour)

	tl, err := e.svc.PostTimeline(ctx, video.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tl.Points) != 2 || tl.AtAge["24h"]["likes"] != 5 || tl.AtAge["24h"]["views"] != 50 {
		t.Fatalf("timeline: %+v", tl)
	}

	// cadence: a fresh collection right after must not hit the API again for 2h-old posts
	clock = base.Add(26*time.Hour + 10*time.Minute)
	if !core.MetricsDue(&store.Post{PublishedAt: &base, MetricsAt: &clock}, clock.Add(7*time.Hour)) {
		t.Fatal("post should be due after 6h when older than 48h")
	}

	growth, err := e.svc.AccountGrowth(ctx, acc.ID, base.Add(-time.Hour), false)
	if err != nil || len(growth) != 1 || growth[0].Last == nil || growth[0].Last.Metrics["followers"] != 100 {
		t.Fatalf("growth: %v %+v", err, growth)
	}

	a, err := e.svc.Analyze(ctx, core.AnalyzeInput{Since: base.Add(-time.Hour), Until: base.Add(48 * time.Hour), GroupBy: "format", Metric: "likes"})
	if err != nil {
		t.Fatal(err)
	}
	if a.Posts != 2 { // repost excluded
		t.Fatalf("posts: %d", a.Posts)
	}
	if a.Totals["likes"] != 10 || a.Previous["likes"] != 5 || a.Change["likes"] != "+100%" {
		t.Fatalf("totals: %+v prev %+v change %+v", a.Totals, a.Previous, a.Change)
	}
	if len(a.Groups) != 2 || a.Groups[0].Avg != 5 {
		t.Fatalf("groups: %+v", a.Groups)
	}
	h, _ := e.svc.Analyze(ctx, core.AnalyzeInput{Since: base.Add(-time.Hour), Until: base.Add(48 * time.Hour), GroupBy: "hashtag", Metric: "engagement_rate"})
	if len(h.Groups) != 1 || h.Groups[0].Key != "#golang" || h.Groups[0].Posts != 2 {
		t.Fatalf("hashtag groups: %+v", h.Groups)
	}
	if _, err := e.svc.Analyze(ctx, core.AnalyzeInput{GroupBy: "nope"}); err == nil {
		t.Fatal("bad group_by should fail")
	}

	// read-only SQL
	text, isErr := e.callTool("sql_query", map[string]any{"query": "SELECT count(*) AS n FROM metric_snapshots"})
	if isErr || !strings.Contains(text, `"n"`) {
		t.Fatalf("sql: %s", text)
	}
	for _, q := range []string{"DELETE FROM posts", "SELECT 1; DELETE FROM posts", "SELECT credentials FROM accounts", "PRAGMA table_info(posts)"} {
		if _, isErr := e.callTool("sql_query", map[string]any{"query": q}); !isErr {
			t.Fatalf("query should be refused: %s", q)
		}
	}
	text, _ = e.callTool("sql_query", map[string]any{"query": "SELECT * FROM accounts"})
	if strings.Contains(text, "credentials") {
		t.Fatalf("credentials leaked: %s", text)
	}
}

func TestDrafterKey(t *testing.T) {
	e := newEnv(t)
	acc := connectFakeX(t, e, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":{"id":"42","username":"alice"}}`))
	}))
	// a second server sharing the same service, with a drafter key
	a := &api.API{Svc: e.svc, MCP: mcp.NewServer("relay", "test", mcp.BuildTools(e.svc)), Token: token, DraftToken: "drafter-token-0123456", Log: e.svc.Log}
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	call := func(method string, params any) map[string]any {
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
		req, _ := http.NewRequest("POST", srv.URL+"/mcp", strings.NewReader(string(body)))
		req.Header.Set("Authorization", "Bearer drafter-token-0123456")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]any
		json.NewDecoder(resp.Body).Decode(&m)
		return m["result"].(map[string]any)
	}
	for _, tl := range call("tools/list", nil)["tools"].([]any) {
		name := tl.(map[string]any)["name"]
		if name == "publish_now" || name == "approve_post" || name == "connect_account" {
			t.Fatalf("drafter should not see %s", name)
		}
	}
	res := call("tools/call", map[string]any{"name": "schedule_post", "arguments": map[string]any{"account_ids": []string{acc.ID}, "text": "hi", "scheduled_at": "now"}})
	text := res["content"].([]any)[0].(map[string]any)["text"].(string)
	var posts []store.Post
	json.Unmarshal([]byte(text), &posts)
	if len(posts) != 1 || posts[0].Status != "draft" {
		t.Fatalf("drafter post must be a draft: %s", text)
	}
	res = call("tools/call", map[string]any{"name": "publish_now", "arguments": map[string]any{"id": posts[0].ID}})
	if res["isError"] != true {
		t.Fatalf("drafter must not publish: %v", res)
	}
	res = call("tools/call", map[string]any{"name": "update_post", "arguments": map[string]any{"id": posts[0].ID, "scheduled_at": "now"}})
	if res["isError"] != true {
		t.Fatalf("drafter must not schedule via update: %v", res)
	}
	// admin approves
	text, isErr := e.callTool("approve_post", map[string]any{"id": posts[0].ID})
	if isErr || !strings.Contains(text, `"status": "scheduled"`) {
		t.Fatalf("approve: %s", text)
	}
}

func TestThreadResume(t *testing.T) {
	e := newEnv(t)
	var n int32
	var lastReply string
	acc := connectFakeX(t, e, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/2/users/me" {
			w.Write([]byte(`{"data":{"id":"42","username":"alice"}}`))
			return
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if rp, ok := body["reply"].(map[string]any); ok {
			lastReply = rp["in_reply_to_tweet_id"].(string)
		}
		c := atomic.AddInt32(&n, 1)
		if c == 2 { // second part fails the first time
			w.WriteHeader(403)
			w.Write([]byte(`{"title":"Forbidden"}`))
			return
		}
		w.Write([]byte(`{"data":{"id":"t` + string(rune('0'+c)) + `"}}`))
	}))
	ctx := context.Background()
	ps, err := e.svc.Schedule(ctx, core.ScheduleInput{AccountIDs: []string{acc.ID}, Thread: []string{"one", "two", "three"}})
	if err != nil {
		t.Fatal(err)
	}
	p, _ := e.svc.PublishNow(ctx, ps[0].ID)
	if p.Status != "failed" || len(p.RemoteIDs) != 1 {
		t.Fatalf("expected partial failure: %+v", p)
	}
	p, _ = e.svc.PublishNow(ctx, ps[0].ID)
	if p.Status != "published" || len(p.RemoteIDs) != 3 || p.RemoteIDs[0] != "t1" || lastReply != "t3" {
		t.Fatalf("resume: %+v last reply to %s", p, lastReply)
	}
}
