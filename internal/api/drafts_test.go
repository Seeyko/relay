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
	"github.com/Seeyko/relay/internal/mcp"
	"github.com/Seeyko/relay/internal/store"
)

// TestPlannedDrafts covers planned_at: a draft's intended time, which only approve_post turns into a schedule.
func TestPlannedDrafts(t *testing.T) {
	e := newEnv(t)
	var published int32
	acc := connectFakeX(t, e, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/2/tweets" && r.Method == "POST" {
			atomic.AddInt32(&published, 1)
			w.Write([]byte(`{"data":{"id":"77"}}`))
			return
		}
		w.Write([]byte(`{"data":{"id":"42","username":"alice"}}`))
	}))
	ctx := context.Background()
	clock := time.Date(2026, 12, 1, 8, 0, 0, 0, time.UTC)
	e.svc.Now = func() time.Time { return clock }

	post := func(text string) *store.Post {
		t.Helper()
		var ps []*store.Post
		if err := json.Unmarshal([]byte(text), &ps); err != nil || len(ps) != 1 {
			t.Fatalf("want one post: %s", text)
		}
		return ps[0]
	}
	one := func(text string) *store.Post {
		t.Helper()
		var p store.Post
		if err := json.Unmarshal([]byte(text), &p); err != nil {
			t.Fatalf("want a post: %s", text)
		}
		return &p
	}

	// A draft with a planned time: stored in UTC, never scheduled by itself.
	text, isErr := e.callTool("schedule_post", map[string]any{"account_ids": []string{acc.ID}, "text": "launch", "scheduled_at": "draft", "planned_at": "2026-12-03T10:00:00+01:00"})
	if isErr {
		t.Fatalf("schedule draft: %s", text)
	}
	d := post(text)
	want := time.Date(2026, 12, 3, 9, 0, 0, 0, time.UTC)
	if d.Status != store.StatusDraft || d.ScheduledAt != nil || d.PlannedAt == nil || !d.PlannedAt.Equal(want) {
		t.Fatalf("bad draft: %+v", d)
	}
	clock = want.Add(time.Hour) // past its planned time: still nothing published
	e.sched.RunDue(ctx)
	if atomic.LoadInt32(&published) != 0 {
		t.Fatal("a planned draft must never publish by itself")
	}
	clock = time.Date(2026, 12, 1, 8, 0, 0, 0, time.UTC)

	// planned_at is refused on a non-draft and must be RFC3339.
	if code, m, _ := e.req("POST", "/v1/posts", map[string]any{"account_ids": []string{acc.ID}, "text": "x", "scheduled_at": "2026-12-03T10:00:00Z", "planned_at": "2026-12-03T10:00:00Z"}); code != 400 || !strings.Contains(m["error"].(string), "planned_at") {
		t.Fatalf("planned_at on a scheduled post: want 400, got %d %v", code, m)
	}
	if code, _, _ := e.req("POST", "/v1/posts", map[string]any{"account_ids": []string{acc.ID}, "text": "x", "scheduled_at": "draft", "planned_at": "tomorrow"}); code != 400 {
		t.Fatalf("bad planned_at: want 400, got %d", code)
	}

	// REST get and list expose planned_at; date filters use it for drafts.
	code, m, _ := e.req("GET", "/v1/posts/"+d.ID, nil)
	if code != 200 || m["planned_at"] != "2026-12-03T09:00:00Z" {
		t.Fatalf("get_post: %d %v", code, m)
	}
	_, _, raw := e.req("GET", "/v1/posts?status=draft&from=2026-12-03T00:00:00Z&to=2026-12-04T00:00:00Z", nil)
	if !strings.Contains(string(raw), d.ID) {
		t.Fatalf("list_posts in the planned day should include the draft: %s", raw)
	}
	_, _, raw = e.req("GET", "/v1/posts?status=draft&from=2026-12-05T00:00:00Z&to=2026-12-06T00:00:00Z", nil)
	if strings.Contains(string(raw), d.ID) {
		t.Fatalf("list_posts on another day should not include the draft: %s", raw)
	}

	// update_post changes or clears it; it is refused on a scheduled post.
	text, isErr = e.callTool("update_post", map[string]any{"id": d.ID, "planned_at": "2026-12-04T18:30:00Z"})
	if p := one(text); isErr || p.PlannedAt == nil || !p.PlannedAt.Equal(time.Date(2026, 12, 4, 18, 30, 0, 0, time.UTC)) {
		t.Fatalf("update planned_at: %s", text)
	}
	text, isErr = e.callTool("update_post", map[string]any{"id": d.ID, "planned_at": "none"})
	if p := one(text); isErr || p.PlannedAt != nil || p.Status != store.StatusDraft {
		t.Fatalf("clear planned_at: %s", text)
	}
	if code, _, _ := e.req("PATCH", "/v1/posts/"+d.ID, map[string]any{"planned_at": "2026-12-04T18:30:00Z"}); code != 200 {
		t.Fatalf("PATCH planned_at: %d", code)
	}

	// approve_post without a time schedules the draft at its (future) planned time.
	text, isErr = e.callTool("approve_post", map[string]any{"id": d.ID})
	p := one(text)
	if isErr || p.Status != store.StatusScheduled || p.ScheduledAt == nil || !p.ScheduledAt.Equal(time.Date(2026, 12, 4, 18, 30, 0, 0, time.UTC)) || p.PlannedAt != nil {
		t.Fatalf("approve at planned time: %s", text)
	}
	e.sched.RunDue(ctx)
	if atomic.LoadInt32(&published) != 0 {
		t.Fatal("approved draft published before its planned time")
	}
	text, isErr = e.callTool("update_post", map[string]any{"id": d.ID, "planned_at": "2026-12-05T10:00:00Z"})
	if !isErr || !strings.Contains(text, "only for drafts") {
		t.Fatalf("planned_at on a scheduled post: %s", text)
	}

	// Moving a scheduled post back to draft keeps its time as planned_at.
	text, _ = e.callTool("update_post", map[string]any{"id": d.ID, "scheduled_at": "draft"})
	if p := one(text); p.Status != store.StatusDraft || p.ScheduledAt != nil || p.PlannedAt == nil || !p.PlannedAt.Equal(time.Date(2026, 12, 4, 18, 30, 0, 0, time.UTC)) {
		t.Fatalf("back to draft: %s", text)
	}

	// A planned time already past: approve publishes within one tick (the previous behaviour).
	clock = time.Date(2026, 12, 6, 8, 0, 0, 0, time.UTC)
	text, _ = e.callTool("approve_post", map[string]any{"id": d.ID})
	if p := one(text); p.Status != store.StatusScheduled || !p.ScheduledAt.Equal(clock) {
		t.Fatalf("approve with a past planned time: %s", text)
	}
	e.sched.RunDue(ctx)
	if atomic.LoadInt32(&published) != 1 {
		t.Fatal("approved draft should be published")
	}

	// An explicit scheduled_at wins over planned_at; an undated draft is approved for now.
	text, _ = e.callTool("schedule_post", map[string]any{"account_ids": []string{acc.ID}, "text": "two", "scheduled_at": "draft", "planned_at": "2026-12-20T09:00:00Z"})
	d2 := post(text)
	text, _ = e.callTool("approve_post", map[string]any{"id": d2.ID, "scheduled_at": "2026-12-10T09:00:00Z"})
	if p := one(text); !p.ScheduledAt.Equal(time.Date(2026, 12, 10, 9, 0, 0, 0, time.UTC)) {
		t.Fatalf("explicit scheduled_at: %s", text)
	}
	text, _ = e.callTool("schedule_post", map[string]any{"account_ids": []string{acc.ID}, "text": "three", "scheduled_at": "draft"})
	d3 := post(text)
	if d3.PlannedAt != nil {
		t.Fatalf("undated draft: %+v", d3)
	}
	text, _ = e.callTool("approve_post", map[string]any{"id": d3.ID})
	if p := one(text); !p.ScheduledAt.Equal(clock) {
		t.Fatalf("approve undated: %s", text)
	}
	if text, isErr := e.callTool("approve_post", map[string]any{"id": d3.ID}); !isErr || !strings.Contains(text, "not a draft") {
		t.Fatalf("approve twice: %s", text)
	}
}

// TestDrafterPlannedAt: a drafter key asking for a date gets a draft planned at that date.
func TestDrafterPlannedAt(t *testing.T) {
	e := newEnv(t)
	acc := connectFakeX(t, e, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":{"id":"42","username":"alice"}}`))
	}))
	a := &api.API{Svc: e.svc, MCP: mcp.NewServer("relay", "test", mcp.BuildTools(e.svc)), Token: token, DraftToken: "drafter-token-0123456", Log: e.svc.Log}
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	call := func(path, method string, body any) (int, []byte) {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(string(b)))
		req.Header.Set("Authorization", "Bearer drafter-token-0123456")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var raw json.RawMessage
		json.NewDecoder(resp.Body).Decode(&raw)
		return resp.StatusCode, raw
	}
	code, raw := call("/v1/posts", "POST", map[string]any{"account_ids": []string{acc.ID}, "text": "hi", "scheduled_at": "2026-12-03T10:00:00+01:00"})
	var ps []store.Post
	json.Unmarshal(raw, &ps)
	if code != 200 || len(ps) != 1 || ps[0].Status != "draft" || ps[0].ScheduledAt != nil || ps[0].PlannedAt == nil || ps[0].PlannedAt.Format(time.RFC3339) != "2026-12-03T09:00:00Z" {
		t.Fatalf("drafter date should become planned_at: %d %s", code, raw)
	}
	// The drafter can move the planned time but still cannot schedule.
	if code, raw := call("/v1/posts/"+ps[0].ID, "PATCH", map[string]any{"planned_at": "2026-12-04T09:00:00Z"}); code != 200 || !strings.Contains(string(raw), "2026-12-04T09:00:00Z") {
		t.Fatalf("drafter planned_at update: %d %s", code, raw)
	}
	if code, _ := call("/v1/posts/"+ps[0].ID, "PATCH", map[string]any{"scheduled_at": "2026-12-04T09:00:00Z"}); code != 403 {
		t.Fatalf("drafter scheduling: want 403, got %d", code)
	}
	if code, _ := call("/v1/posts/"+ps[0].ID+"/approve", "POST", map[string]any{}); code != 403 {
		t.Fatalf("drafter approve: want 403, got %d", code)
	}
}
