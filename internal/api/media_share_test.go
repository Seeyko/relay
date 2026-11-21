package api_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Seeyko/relay/internal/api"
	"github.com/Seeyko/relay/internal/core"
	"github.com/Seeyko/relay/internal/mcp"
	"github.com/Seeyko/relay/internal/store"
)

// An agent shows the media of a scheduled post to a person: get_media lists them in publishing order
// with a public URL, and that URL downloads the file without any Authorization header.
func TestGetMediaShareLinks(t *testing.T) {
	e := newEnv(t)
	acc := connectFakeX(t, e, &xThreadServer{})
	m1 := saveMedia(t, e, "one", "1.jpg", "image/jpeg")
	m2 := saveMedia(t, e, "two", "2.jpg", "image/jpeg")
	m3 := saveMedia(t, e, "three", "3.png", "image/png")
	code, _, raw := e.req("POST", "/v1/posts", map[string]any{"account_ids": []string{acc.ID}, "scheduled_at": "draft", "media": []string{m1.ID, m2.ID},
		"thread": []any{"first", map[string]any{"text": "second", "media": []string{m3.ID}}}})
	if code != 200 {
		t.Fatalf("schedule: %d %s", code, raw)
	}
	var posts []store.Post
	if err := json.Unmarshal(raw, &posts); err != nil || len(posts) != 1 {
		t.Fatalf("%s", raw)
	}

	// MCP: by post, in order, with the thread part.
	text, isErr := e.callTool("get_media", map[string]any{"post": posts[0].ID})
	if isErr {
		t.Fatalf("get_media: %s", text)
	}
	var got []core.SharedMedia
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("%v: %s", err, text)
	}
	wantIDs, wantParts := []string{m1.ID, m2.ID, m3.ID}, []int{1, 1, 2}
	if len(got) != 3 {
		t.Fatalf("got %s", text)
	}
	for i, g := range got {
		if g.ID != wantIDs[i] || g.Part != wantParts[i] || g.Missing {
			t.Fatalf("media %d: %+v", i, g)
		}
		if want := "https://relay.example/media/" + strings.Split(g.URL, "/media/")[1]; g.URL != want || !strings.HasPrefix(g.URL, "https://relay.example/media/med_") {
			t.Fatalf("media %d url %q", i, g.URL)
		}
	}
	if got[0].Filename != "1.jpg" || got[0].Mime != "image/jpeg" || got[0].Size != 3 {
		t.Fatalf("metadata %+v", got[0])
	}

	// The URL downloads the file with a plain GET (no token), the way the person opens it.
	resp, err := http.Get(e.srv.URL + strings.TrimPrefix(got[2].URL, "https://relay.example"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(b) != "three" {
		t.Fatalf("public download: %d %q", resp.StatusCode, b)
	}

	// MCP: by ids, unknown ids come back as missing instead of failing the call.
	text, isErr = e.callTool("get_media", map[string]any{"ids": []string{m2.ID, "med_nope"}})
	if isErr || json.Unmarshal([]byte(text), &got) != nil || len(got) != 2 || got[0].ID != m2.ID || got[0].URL == "" || !got[1].Missing {
		t.Fatalf("by ids: %s", text)
	}
	if text, isErr = e.callTool("get_media", map[string]any{}); !isErr {
		t.Fatalf("no ids nor post should fail: %s", text)
	}

	// REST: one media, the media of a post, and the library list all carry the url.
	if code, _, raw = e.req("GET", "/v1/media/"+m1.ID, nil); code != 200 || !strings.Contains(string(raw), `"url":"https://relay.example/media/med_`) {
		t.Fatalf("GET /v1/media/{id}: %d %s", code, raw)
	}
	if code, _, raw = e.req("GET", "/v1/posts/"+posts[0].ID+"/media", nil); code != 200 || strings.Count(string(raw), `"url":`) != 3 {
		t.Fatalf("GET /v1/posts/{id}/media: %d %s", code, raw)
	}
	code, _, raw = e.req("GET", "/v1/media", nil)
	var lib []map[string]any
	if code != 200 || json.Unmarshal(raw, &lib) != nil || len(lib) != 3 || lib[0]["url"] == nil || lib[0]["created_at"] == nil {
		t.Fatalf("GET /v1/media: %d %s", code, raw)
	}

	// A drafter key (sub-agents) can read media too.
	a := &api.API{Svc: e.svc, MCP: mcp.NewServer("relay", "test", mcp.BuildTools(e.svc)), Token: token, DraftToken: "drafter-token-0123456", Log: e.svc.Log}
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL+"/v1/media/"+m1.ID, nil)
	req.Header.Set("Authorization", "Bearer drafter-token-0123456")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(b), `"url":`) {
		t.Fatalf("drafter get_media: %d %s", resp.StatusCode, b)
	}
}
