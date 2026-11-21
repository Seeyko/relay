package api_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Seeyko/relay/internal/api"
	"github.com/Seeyko/relay/internal/mcp"
	"github.com/Seeyko/relay/internal/store"
)

func TestFeedbackLifecycle(t *testing.T) {
	e := newEnv(t)
	var mu sync.Mutex
	var events []string
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var ev struct{ Event string }
		json.Unmarshal(b, &ev)
		mu.Lock()
		events = append(events, ev.Event)
		mu.Unlock()
	}))
	defer hook.Close()
	e.svc.WebhookURL = hook.URL
	img := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write([]byte("\x89PNG\r\n\x1a\nfake"))
	}))
	defer img.Close()

	// An agent asks for a missing capability through MCP, with a screenshot URL.
	text, isErr := e.callTool("submit_feedback", map[string]any{
		"title":      "Filter analyze by language",
		"body":       "User asked: « views of English posts only ». analyze has no language filter; I tried text_contains.",
		"media_urls": []string{img.URL + "/shot.png"},
		"context":    map[string]any{"tool": "analyze"},
	})
	if isErr {
		t.Fatal(text)
	}
	var f store.Feedback
	json.Unmarshal([]byte(text), &f)
	if f.ID == "" || f.Status != "open" || f.Kind != "missing_feature" || len(f.Media) != 1 || f.Source != "mcp" || f.Role != "admin" || f.Context["tool"] != "analyze" {
		t.Fatalf("submit: %s", text)
	}
	// Same thing over REST, referencing an uploaded media id.
	code, body, raw := e.req("POST", "/v1/feedback", map[string]any{"kind": "bug", "title": "TikTok import stops at 30", "body": "details", "media": f.Media})
	if code != 200 || body["source"] != "rest" || body["kind"] != "bug" {
		t.Fatalf("rest submit: %d %s", code, raw)
	}
	for _, bad := range []map[string]any{
		{"title": "", "body": "x"},
		{"title": "x", "body": ""},
		{"title": "x", "body": "y", "kind": "wish"},
		{"title": "x", "body": "y", "media": []string{"med_nope"}},
		{"title": strings.Repeat("t", 201), "body": "y"},
	} {
		if code, _, raw := e.req("POST", "/v1/feedback", bad); code != 400 {
			t.Errorf("%v: %d %s", bad, code, raw)
		}
	}
	// Maintainers list open feedback, then close it with a PR.
	code, _, raw = e.req("GET", "/v1/feedback", nil)
	var open []store.Feedback
	json.Unmarshal(raw, &open)
	if code != 200 || len(open) != 2 || open[0].ID != f.ID {
		t.Fatalf("list: %d %s", code, raw)
	}
	code, body, raw = e.req("PATCH", "/v1/feedback/"+f.ID, map[string]any{"status": "done", "resolution": "added a language filter", "pr_url": "https://github.com/Seeyko/relay/pull/99"})
	if code != 200 || body["status"] != "done" || body["pr_url"] != "https://github.com/Seeyko/relay/pull/99" {
		t.Fatalf("update: %d %s", code, raw)
	}
	if code, _, _ := e.req("PATCH", "/v1/feedback/"+f.ID, map[string]any{"status": "shipped"}); code != 400 {
		t.Fatalf("bad status accepted: %d", code)
	}
	if code, _, _ := e.req("PATCH", "/v1/feedback/fb_missing", map[string]any{"status": "done"}); code != 404 {
		t.Fatalf("missing feedback: %d", code)
	}
	code, _, raw = e.req("GET", "/v1/feedback?status=all", nil)
	json.Unmarshal(raw, &open)
	if code != 200 || len(open) != 2 {
		t.Fatalf("list all: %s", raw)
	}
	code, body, _ = e.req("GET", "/v1/feedback/"+f.ID, nil)
	if code != 200 || body["resolution"] != "added a language filter" {
		t.Fatalf("get: %v", body)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := strings.Count(strings.Join(events, ","), "feedback.new")
		u := strings.Count(strings.Join(events, ","), "feedback.updated")
		mu.Unlock()
		if n == 2 && u == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("webhooks: %v", events)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestFeedbackDrafterKey(t *testing.T) {
	e := newEnv(t)
	a := &api.API{Svc: e.svc, MCP: mcp.NewServer("relay", "test", mcp.BuildTools(e.svc)), Token: token, DraftToken: "drafter-token-0123456", Log: e.svc.Log}
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	do := func(method, path, body string) (int, string) {
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer drafter-token-0123456")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	// Sub-agents with the drafts-only key can ask for features and follow their own request...
	code, body := do("POST", "/v1/feedback", `{"title":"Need Pinterest","body":"User wants to post on Pinterest"}`)
	if code != 200 || !strings.Contains(body, `"role":"drafter"`) {
		t.Fatalf("drafter submit: %d %s", code, body)
	}
	var f store.Feedback
	json.Unmarshal([]byte(body), &f)
	if code, _ := do("GET", "/v1/feedback/"+f.ID, ""); code != 200 {
		t.Fatalf("drafter get: %d", code)
	}
	// ...but not read everybody's feedback or change statuses.
	if code, _ := do("GET", "/v1/feedback", ""); code != 403 {
		t.Fatalf("drafter list: %d", code)
	}
	if code, _ := do("PATCH", "/v1/feedback/"+f.ID, `{"status":"done"}`); code != 403 {
		t.Fatalf("drafter update: %d", code)
	}
	if !strings.Contains(e.mcpInstructions(), "submit_feedback") {
		t.Fatal("MCP instructions must tell agents about submit_feedback")
	}
}

func (e *env) mcpInstructions() string {
	res := e.mcp("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "t", "version": "1"}})
	s, _ := res["instructions"].(string)
	return s
}
