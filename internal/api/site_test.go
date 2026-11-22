package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Seeyko/relay/internal/api"
	"github.com/Seeyko/relay/internal/mcp"
	"github.com/Seeyko/relay/internal/providers/website/sitetest"
	"github.com/Seeyko/relay/internal/store"
)

const siteToken = "site-secret-v2"
const drafterToken = "drafter-token-0123456"

func connectSite(t *testing.T, e *env, s *sitetest.Site) *store.Account {
	t.Helper()
	srv := s.Start()
	t.Cleanup(srv.Close)
	text, isErr := e.callTool("connect_account", map[string]any{"provider": "website", "credentials": map[string]string{"base_url": srv.URL, "token": siteToken}})
	if isErr {
		t.Fatalf("connect: %s", text)
	}
	var acc store.Account
	json.Unmarshal([]byte(text), &acc)
	return &acc
}

func ok(t *testing.T, e *env, tool string, args map[string]any) map[string]any {
	t.Helper()
	text, isErr := e.callTool(tool, args)
	if isErr {
		t.Fatalf("%s: %s", tool, text)
	}
	var m map[string]any
	json.Unmarshal([]byte(text), &m)
	return m
}

func refused(t *testing.T, e *env, tool string, args map[string]any, want string) {
	t.Helper()
	text, isErr := e.callTool(tool, args)
	if !isErr || !strings.Contains(text, want) {
		t.Fatalf("%s: want an error with %q, got %v %s", tool, want, isErr, text)
	}
}

func extensions() []sitetest.Extension {
	return []sitetest.Extension{
		{
			Info: map[string]any{"name": "search", "description": "Full-text search.", "method": "GET", "path": "/x/search",
				"input_schema": map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}, "required": []string{"q"}}, "admin": false},
			Handler: func(r *http.Request, args map[string]any) (int, any) {
				return 200, map[string]any{"query": args["q"], "tags": args["tags"], "hits": []string{"1"}}
			},
		},
		{
			Info: map[string]any{"name": "retranslate", "description": "Machine-translate an article again.", "method": "POST", "path": "/x/posts/{id}/retranslate",
				"input_schema": map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string"}, "langs": map[string]any{"type": "array"}}, "required": []string{"id"}}, "admin": true},
			Handler: func(r *http.Request, args map[string]any) (int, any) {
				return 202, map[string]any{"path": args["_path"], "langs": args["langs"], "id_in_body": args["id"] != nil}
			},
		},
		{
			Info: map[string]any{"name": "suggest_tags", "description": "Suggest tags for a text.", "method": "POST", "path": "/x/suggest-tags",
				"input_schema": map[string]any{"type": "object"}, "admin": false, "read_only": true},
			Handler: func(r *http.Request, args map[string]any) (int, any) {
				return 200, map[string]any{"tags": []string{"bmx"}}
			},
		},
		{
			Info: map[string]any{"name": "import_image", "description": "Import an image from a URL to the CDN.", "method": "POST", "path": "/x/import-image",
				"input_schema": map[string]any{"type": "object"}, "admin": false},
			Handler: func(r *http.Request, args map[string]any) (int, any) {
				return 201, map[string]any{"url": "https://cdn.example/x.jpg"}
			},
		},
	}
}

func TestSiteV2ContentTools(t *testing.T) {
	e := newEnv(t)
	s := sitetest.New(siteToken, sitetest.All...)
	s.MachineTranslate = true
	s.Extensions = extensions()
	acc := connectSite(t, e, s)
	id := func(m map[string]any) string { v, _ := m["id"].(string); return v }
	A := func(extra map[string]any) map[string]any {
		m := map[string]any{"account_id": acc.ID}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}

	info := ok(t, e, "site_info", A(nil))
	if info["version"] != "2" || len(info["extensions"].([]any)) != 4 {
		t.Fatalf("info: %v", info)
	}

	// Create: draft by default, cover from Relay's media library.
	m, err := e.svc.SaveMedia(context.Background(), strings.NewReader("img"), "cover.jpg", "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	c := ok(t, e, "site_create_article", A(map[string]any{"title": "Hello", "body_html": "<p>Hi</p>", "tags": []string{"bmx"}, "category": "news",
		"author_id": "7", "cover_media": m.ID, "extra": map[string]any{"series": "s1"}}))
	aid := id(c)
	if aid == "" || c["status"] != "draft" {
		t.Fatalf("create: %v", c)
	}
	art, _ := s.Article(aid)
	if art.Status != "draft" || art.Category != "news" || art.AuthorID != "7" || !strings.Contains(art.CoverURL, "cover.jpg") || art.Extra["series"] != "s1" {
		t.Fatalf("stored article: %+v", art)
	}
	got := ok(t, e, "site_get_article", A(map[string]any{"article_id": aid}))
	if got["body_html"] != "<p>Hi</p>" {
		t.Fatalf("get: %v", got)
	}

	// Update with optimistic concurrency.
	up := ok(t, e, "site_update_article", A(map[string]any{"article_id": aid, "title": "Hello 2", "expected_updated_at": got["updated_at"]}))
	if up["title"] != "Hello 2" {
		t.Fatalf("update: %v", up)
	}
	refused(t, e, "site_update_article", A(map[string]any{"article_id": aid, "title": "stale", "expected_updated_at": got["updated_at"]}), "409")
	refused(t, e, "site_update_article", A(map[string]any{"article_id": aid, "status": "published"}), "publish or unpublish")

	// Publish, list, unpublish.
	if p := ok(t, e, "site_publish_article", A(map[string]any{"article_id": aid})); p["status"] != "published" {
		t.Fatalf("publish: %v", p)
	}
	if p := ok(t, e, "site_publish_article", A(map[string]any{"article_id": aid, "published_at": "2099-01-01T00:00:00Z"})); p["status"] != "scheduled" {
		t.Fatalf("schedule: %v", p)
	}
	ok(t, e, "site_publish_article", A(map[string]any{"article_id": aid}))
	list := ok(t, e, "site_list_articles", A(map[string]any{"status": "published"}))
	if items := list["items"].([]any); len(items) != 1 || id(items[0].(map[string]any)) != aid {
		t.Fatalf("list: %v", list)
	}
	if p := ok(t, e, "site_unpublish_article", A(map[string]any{"article_id": aid})); p["status"] != "draft" {
		t.Fatalf("unpublish: %v", p)
	}
	if list := ok(t, e, "site_list_articles", A(map[string]any{"status": "published"})); len(list["items"].([]any)) != 0 {
		t.Fatalf("list after unpublish: %v", list)
	}

	// Taxonomy and authors.
	tax := ok(t, e, "site_taxonomy", A(nil))
	if !strings.Contains(mustJSON(tax), `"name":"bmx"`) || !strings.Contains(mustJSON(tax), `"name":"news"`) {
		t.Fatalf("taxonomy: %v", tax)
	}
	if au := ok(t, e, "site_authors", A(nil)); !strings.Contains(mustJSON(au), "Jane Doe") {
		t.Fatalf("authors: %v", au)
	}

	// Translations: machine, then manual, then back to machine.
	tr := ok(t, e, "site_list_translations", A(map[string]any{"article_id": aid}))
	if !strings.Contains(mustJSON(tr), `"mode":"machine"`) {
		t.Fatalf("translations: %v", tr)
	}
	set := ok(t, e, "site_set_translation", A(map[string]any{"article_id": aid, "lang": "fr", "title": "Bonjour", "body_html": "<p>Salut</p>"}))
	if set["mode"] != "manual" || set["title"] != "Bonjour" {
		t.Fatalf("set translation: %v", set)
	}
	refused(t, e, "site_set_translation", A(map[string]any{"article_id": aid, "lang": "de", "title": "Hallo"}), "not one of the site's languages")
	ok(t, e, "site_delete_translation", A(map[string]any{"article_id": aid, "lang": "fr"}))
	if tr := ok(t, e, "site_list_translations", A(map[string]any{"article_id": aid})); strings.Contains(mustJSON(tr), "manual") {
		t.Fatalf("translation should be machine again: %v", tr)
	}

	// Media: from Relay's library and from a URL.
	if mm := ok(t, e, "site_upload_media", A(map[string]any{"media": m.ID, "alt": "a cover"})); !strings.HasPrefix(mm["url"].(string), "https://cdn.example/") {
		t.Fatalf("upload: %v", mm)
	}
	if mm := ok(t, e, "site_upload_media", A(map[string]any{"url": "https://images.example/skate.jpg"})); !strings.HasSuffix(mm["url"].(string), "skate.jpg") {
		t.Fatalf("import: %v", mm)
	}
	refused(t, e, "site_upload_media", A(nil), "media or url")

	// Extensions.
	text, isErr := e.callTool("site_actions", A(nil))
	if isErr || !strings.Contains(text, `"retranslate"`) || !strings.Contains(text, `"allowed": true`) {
		t.Fatalf("site_actions: %s", text)
	}
	sr := ok(t, e, "site_action", A(map[string]any{"action": "search", "args": map[string]any{"q": "bmx park", "tags": []string{"a", "b"}}}))
	if sr["query"] != "bmx park" || sr["tags"] != "a,b" {
		t.Fatalf("search (GET args as query): %v", sr)
	}
	rt := ok(t, e, "site_action", A(map[string]any{"action": "retranslate", "args": map[string]any{"id": aid, "langs": []string{"fr"}}}))
	if rt["path"] != "/x/posts/"+aid+"/retranslate" || rt["id_in_body"] != false || mustJSON(rt["langs"]) != `["fr"]` {
		t.Fatalf("retranslate (path param, JSON body): %v", rt)
	}
	refused(t, e, "site_action", A(map[string]any{"action": "search", "args": map[string]any{}}), "missing required argument(s) q")
	refused(t, e, "site_action", A(map[string]any{"action": "nope"}), "available: search, retranslate")

	// Delete.
	ok(t, e, "site_delete_article", A(map[string]any{"article_id": aid}))
	refused(t, e, "site_get_article", A(map[string]any{"article_id": aid}), "404")

	// /info is cached: connect reads it once, the tools above reuse it.
	n := 0
	for _, c := range s.Calls() {
		if c.Path == "/relay/v2/info" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("/info read %d times, want 1 (cached)", n)
	}
	ok(t, e, "site_info", A(map[string]any{"refresh": true}))
	if c := s.Calls(); c[len(c)-1].Path != "/relay/v2/info" {
		t.Fatalf("refresh should read /info again")
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestSiteV2Publish(t *testing.T) {
	e := newEnv(t)
	s := sitetest.New(siteToken, sitetest.All...)
	acc := connectSite(t, e, s)
	m, _ := e.svc.SaveMedia(context.Background(), strings.NewReader("img"), "a.jpg", "image/jpeg")
	text, isErr := e.callTool("schedule_post", map[string]any{"account_ids": []string{acc.ID}, "text": "<p>Body</p>", "media": []string{m.ID},
		"options": map[string]any{"title": "Launch", "slug": "launch", "excerpt": "ex", "tags": []string{"go"}, "category": "news", "lang": "fr",
			"author_id": "7", "status": "draft", "body_format": "html", "cover": m.ID}})
	if isErr {
		t.Fatalf("schedule: %s", text)
	}
	var posts []store.Post
	json.Unmarshal([]byte(text), &posts)
	if text, isErr = e.callTool("publish_now", map[string]any{"id": posts[0].ID}); isErr {
		t.Fatalf("publish: %s", text)
	}
	var sent map[string]any
	for _, c := range s.Calls() {
		if c.Method == "POST" && c.Path == "/relay/v2/posts" {
			sent = c.Body
		}
	}
	if sent["body_html"] != "<p>Body</p>" || sent["body_markdown"] != nil || sent["category"] != "news" || sent["author_id"] != "7" ||
		sent["status"] != "draft" || sent["lang"] != "fr" || sent["media"] != nil || !strings.Contains(sent["cover_url"].(string), "a.jpg") {
		t.Fatalf("POST /posts body: %v", sent)
	}
	var p store.Post
	_, _, raw := e.req("GET", "/v1/posts/"+posts[0].ID, nil)
	json.Unmarshal(raw, &p)
	if p.Status != "published" || p.RemoteID != "1" {
		t.Fatalf("relay post: %+v", p)
	}

	// Validation: bad status, scheduled without date.
	refused(t, e, "schedule_post", map[string]any{"account_ids": []string{acc.ID}, "text": "x", "options": map[string]any{"title": "T", "status": "live"}}, "status")
	refused(t, e, "schedule_post", map[string]any{"account_ids": []string{acc.ID}, "text": "x", "options": map[string]any{"title": "T", "status": "scheduled"}}, "published_at")

	// History reads published articles only; metrics come from /metrics.
	s.Add(sitetest.Article{Title: "Old one", Status: "published"})
	s.Add(sitetest.Article{Title: "Hidden draft", Status: "draft"})
	if text, isErr := e.callTool("import_history", map[string]any{"account_id": acc.ID}); isErr {
		t.Fatalf("import: %s", text)
	}
	imported := 0
	for _, c := range s.Calls() {
		if c.Method == "GET" && c.Path == "/relay/v2/posts" {
			if !strings.Contains(c.Query, "status=published") {
				t.Fatalf("history query: %s", c.Query)
			}
			imported++
		}
	}
	_, _, raw = e.req("GET", "/v1/posts?source=imported", nil)
	if imported == 0 || !strings.Contains(string(raw), "Old one") || strings.Contains(string(raw), "Hidden draft") {
		t.Fatalf("imported: %s", raw)
	}

	// delete_post with delete_on_site removes the article on the site too.
	if text, isErr := e.callTool("delete_post", map[string]any{"id": posts[0].ID, "delete_on_site": true}); isErr || !strings.Contains(text, "deleted_on_site") {
		t.Fatalf("delete_post: %s", text)
	}
	if _, found := s.Article("1"); found {
		t.Fatalf("article should be deleted on the site")
	}
}

func TestSiteCapabilityRefusals(t *testing.T) {
	e := newEnv(t)
	s := sitetest.New(siteToken) // articles only
	acc := connectSite(t, e, s)
	aid := s.Add(sitetest.Article{Title: "Only", Status: "published"})
	A := map[string]any{"account_id": acc.ID, "article_id": aid}
	with := func(extra map[string]any) map[string]any {
		m := map[string]any{}
		for k, v := range A {
			m[k] = v
		}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	before := len(s.Calls())
	refused(t, e, "site_update_article", with(map[string]any{"title": "x"}), `"edit"`)
	refused(t, e, "site_publish_article", A, `"drafts"`)
	refused(t, e, "site_unpublish_article", A, `"drafts"`)
	refused(t, e, "site_delete_article", A, `"delete"`)
	refused(t, e, "site_taxonomy", A, `"taxonomy"`)
	refused(t, e, "site_authors", A, `"authors"`)
	refused(t, e, "site_list_translations", A, `"translations"`)
	refused(t, e, "site_set_translation", with(map[string]any{"lang": "fr", "title": "x"}), `"translations"`)
	refused(t, e, "site_delete_translation", with(map[string]any{"lang": "fr"}), `"translations"`)
	refused(t, e, "site_upload_media", with(map[string]any{"url": "https://x.example/a.jpg"}), `"media"`)
	refused(t, e, "site_create_article", with(map[string]any{"title": "x"}), `"drafts"`) // default status draft
	refused(t, e, "site_create_article", with(map[string]any{"title": "x", "status": "scheduled", "published_at": "2099-01-01T00:00:00Z"}), `"schedule"`)
	refused(t, e, "site_action", with(map[string]any{"action": "search"}), "no extensions")
	refused(t, e, "delete_post", map[string]any{"id": "nope", "delete_on_site": true}, "not found")
	for _, c := range s.Calls()[before:] {
		if c.Path != "/relay/v2/posts/"+aid || c.Method != "GET" {
			t.Fatalf("the site was called for a refused operation: %s %s", c.Method, c.Path)
		}
	}
	// What the site supports still works; REST maps refusals to 400.
	ok(t, e, "site_create_article", with(map[string]any{"title": "Now", "status": "published", "body_markdown": "hi"}))
	if code, _, raw := e.req("GET", "/v1/sites/"+acc.ID+"/taxonomy", nil); code != 400 {
		t.Fatalf("REST refusal: %d %s", code, raw)
	}
	// Scheduling a draft on a site without drafts fails without retry.
	text, _ := e.callTool("schedule_post", map[string]any{"account_ids": []string{acc.ID}, "text": "x", "options": map[string]any{"title": "T", "status": "draft"}})
	var posts []store.Post
	json.Unmarshal([]byte(text), &posts)
	if text, isErr := e.callTool("publish_now", map[string]any{"id": posts[0].ID}); isErr || !strings.Contains(text, `"failed"`) || !strings.Contains(text, "drafts") {
		t.Fatalf("draft on a site without drafts: %s", text)
	}

	// Not a website account.
	x := connectFakeX(t, e, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":{"id":"42","username":"alice"}}`))
	}))
	refused(t, e, "site_info", map[string]any{"account_id": x.ID}, "need a website account")
}

func TestSiteV1Fallback(t *testing.T) {
	e := newEnv(t)
	v1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/relay/v1/info":
			w.Write([]byte(`{"name":"Old site","url":"https://old.example","version":"1","capabilities":["articles"]}`))
		case "/relay/v1/posts":
			w.Write([]byte(`{"id":"a1","url":"https://old.example/a1"}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer v1.Close()
	text, isErr := e.callTool("connect_account", map[string]any{"provider": "website", "credentials": map[string]string{"base_url": v1.URL, "token": "t"}})
	if isErr {
		t.Fatalf("connect v1: %s", text)
	}
	var acc store.Account
	json.Unmarshal([]byte(text), &acc)
	refused(t, e, "site_list_articles", map[string]any{"account_id": acc.ID}, "v1")
	if info := ok(t, e, "site_info", map[string]any{"account_id": acc.ID}); info["version"] != "1" {
		t.Fatalf("v1 info: %v", info)
	}
	text, _ = e.callTool("schedule_post", map[string]any{"account_ids": []string{acc.ID}, "text": "hi", "options": map[string]any{"title": "T"}})
	var posts []store.Post
	json.Unmarshal([]byte(text), &posts)
	if text, isErr := e.callTool("publish_now", map[string]any{"id": posts[0].ID}); isErr || !strings.Contains(text, "https://old.example/a1") {
		t.Fatalf("publish v1: %s", text)
	}
}

func TestSiteDrafterLimits(t *testing.T) {
	e := newEnv(t)
	s := sitetest.New(siteToken, sitetest.All...)
	s.Extensions = extensions()
	acc := connectSite(t, e, s)
	pub := s.Add(sitetest.Article{Title: "Live", Status: "published"})
	draft := s.Add(sitetest.Article{Title: "Wip", Status: "draft"})
	a := &api.API{Svc: e.svc, MCP: mcp.NewServer("relay", "test", mcp.BuildTools(e.svc)), Token: token, DraftToken: drafterToken, Log: e.svc.Log}
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	call := func(method, path string, body any) (int, string) {
		t.Helper()
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, srv.URL+path, rd)
		req.Header.Set("Authorization", "Bearer "+drafterToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	base := "/v1/sites/" + acc.ID
	expect := func(code int, method, path string, body any) string {
		t.Helper()
		got, raw := call(method, path, body)
		if got != code {
			t.Fatalf("%s %s: want %d, got %d %s", method, path, code, got, raw)
		}
		return raw
	}
	// Reads are fine.
	expect(200, "GET", base, nil)
	expect(200, "GET", base+"/articles", nil)
	expect(200, "GET", base+"/articles/"+pub, nil)
	expect(200, "GET", base+"/taxonomy", nil)
	// Drafts only.
	expect(200, "POST", base+"/articles", map[string]any{"title": "New draft"})
	expect(403, "POST", base+"/articles", map[string]any{"title": "Sneaky", "status": "published"})
	expect(200, "PATCH", base+"/articles/"+draft, map[string]any{"title": "Wip 2"})
	expect(403, "PATCH", base+"/articles/"+pub, map[string]any{"title": "Defaced"})
	expect(200, "PUT", base+"/articles/"+draft+"/translations/fr", map[string]any{"title": "Brouillon"})
	expect(403, "PUT", base+"/articles/"+pub+"/translations/fr", map[string]any{"title": "Non"})
	expect(403, "POST", base+"/articles/"+draft+"/publish", nil)
	expect(403, "POST", base+"/articles/"+pub+"/unpublish", nil)
	expect(403, "DELETE", base+"/articles/"+draft, nil)
	// Extensions: read-only and not admin only.
	raw := expect(200, "GET", base+"/actions", nil)
	if !strings.Contains(raw, `"name":"retranslate"`) || !strings.Contains(raw, `"allowed":false`) {
		t.Fatalf("actions for a drafter: %s", raw)
	}
	expect(200, "POST", base+"/actions/search", map[string]any{"q": "bmx"})
	expect(200, "POST", base+"/actions/suggest_tags", map[string]any{"text": "bmx"})
	expect(403, "POST", base+"/actions/retranslate", map[string]any{"id": pub})
	expect(403, "POST", base+"/actions/import_image", map[string]any{"url": "https://x.example/a.jpg"})
	if live, _ := s.Article(pub); live.Title != "Live" || live.Status != "published" {
		t.Fatalf("the published article changed: %+v", live)
	}
	if s.Called("POST /relay/v2/x/posts/" + pub + "/retranslate") {
		t.Fatalf("admin extension was called with a drafter key")
	}
	// The admin key may do all of it, and REST keeps the site's 409.
	got := ok(t, e, "site_get_article", map[string]any{"account_id": acc.ID, "article_id": draft})
	if code, _, raw := e.req("PATCH", base+"/articles/"+draft, map[string]any{"title": "x", "expected_updated_at": "2000-01-01T00:00:00Z"}); code != 409 {
		t.Fatalf("REST conflict: %d %s", code, raw)
	}
	if code, _, raw := e.req("POST", base+"/actions/retranslate", map[string]any{"id": draft}); code != 200 || !strings.Contains(string(raw), "retranslate") {
		t.Fatalf("admin extension: %d %s", code, raw)
	}
	if code, _, raw := e.req("DELETE", base+"/articles/"+draft, nil); code != 200 {
		t.Fatalf("admin delete: %d %s", code, raw)
	}
	_ = got
}

// An existing draft is scheduled in Relay (the site has no schedule capability) and published by the scheduler.
func TestSiteScheduleExistingDraft(t *testing.T) {
	e := newEnv(t)
	s := sitetest.New(siteToken, "drafts", "edit", "translations")
	acc := connectSite(t, e, s)
	id := s.Add(sitetest.Article{Title: "Competitor review", Status: "draft"})
	pub := s.Add(sitetest.Article{Title: "Already out", Status: "published"})
	now := time.Now().UTC()
	e.svc.Now = func() time.Time { return now }
	at := now.Add(time.Hour).Format(time.RFC3339)

	refused(t, e, "site_publish_article", map[string]any{"account_id": acc.ID, "article_id": id, "scheduled_at": now.Add(-time.Hour).Format(time.RFC3339)}, "future")
	refused(t, e, "site_publish_article", map[string]any{"account_id": acc.ID, "article_id": id, "scheduled_at": at, "published_at": at}, "not both")
	refused(t, e, "site_publish_article", map[string]any{"account_id": acc.ID, "article_id": pub, "scheduled_at": at}, "not a draft")
	refused(t, e, "site_publish_article", map[string]any{"account_id": acc.ID, "article_id": id, "published_at": at}, "schedule")

	text, isErr := e.callTool("site_publish_article", map[string]any{"account_id": acc.ID, "article_id": id, "scheduled_at": at})
	if isErr {
		t.Fatalf("schedule: %s", text)
	}
	var p store.Post
	json.Unmarshal([]byte(text), &p)
	if p.Status != "scheduled" || p.Options["article_id"] != id || len(p.Parts) != 1 || p.Parts[0] != "Competitor review" {
		t.Fatalf("relay post: %s", text)
	}
	// REST: same tool on the publish route.
	if code, _, raw := e.req("POST", "/v1/sites/"+acc.ID+"/articles/"+id+"/publish", map[string]any{"scheduled_at": at}); code != 200 {
		t.Fatalf("REST schedule: %d %s", code, raw)
	} else {
		var p2 store.Post
		json.Unmarshal(raw, &p2)
		e.req("DELETE", "/v1/posts/"+p2.ID, nil)
	}

	e.sched.RunDue(context.Background())
	if a, _ := s.Article(id); a.Status != "draft" {
		t.Fatalf("published too early: %s", a.Status)
	}

	now = now.Add(2 * time.Hour)
	e.sched.RunDue(context.Background())
	a, _ := s.Article(id)
	if a.Status != "published" {
		t.Fatalf("article status after due time: %s", a.Status)
	}
	publishes := 0
	for _, c := range s.Calls() {
		if c.Method == "POST" && c.Path == "/relay/v2/posts" {
			t.Fatalf("a new article was created: %v", c.Body)
		}
		if c.Method == "POST" && c.Path == "/relay/v2/posts/"+id+"/publish" {
			publishes++
		}
	}
	if publishes != 1 {
		t.Fatalf("publish calls: %d", publishes)
	}
	_, _, raw := e.req("GET", "/v1/posts/"+p.ID, nil)
	json.Unmarshal(raw, &p)
	if p.Status != "published" || p.RemoteID != id {
		t.Fatalf("relay post after publish: %s", raw)
	}

	// A retry on an article already published does not publish it again.
	if text, isErr := e.callTool("schedule_post", map[string]any{"account_ids": []string{acc.ID}, "text": "x", "options": map[string]any{"article_id": pub}}); isErr {
		t.Fatalf("schedule_post article_id: %s", text)
	}
	e.sched.RunDue(context.Background())
	for _, c := range s.Calls() {
		if c.Path == "/relay/v2/posts/"+pub+"/publish" {
			t.Fatalf("already published article published again")
		}
	}
}
