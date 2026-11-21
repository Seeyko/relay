package website

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Conformance checker for Site Protocol v2 (relay site-check). It exercises the capabilities a site
// declares, read-only unless write is set; with write it creates one draft and deletes it again.

type checker struct {
	ctx   context.Context
	c     *Client
	out   io.Writer
	fails int
	warns int
}

func (k *checker) pass(format string, a ...any) {
	fmt.Fprintf(k.out, "PASS  %s\n", fmt.Sprintf(format, a...))
}
func (k *checker) fail(format string, a ...any) {
	k.fails++
	fmt.Fprintf(k.out, "FAIL  %s\n", fmt.Sprintf(format, a...))
}
func (k *checker) warn(format string, a ...any) {
	k.warns++
	fmt.Fprintf(k.out, "WARN  %s\n", fmt.Sprintf(format, a...))
}
func (k *checker) skip(format string, a ...any) {
	fmt.Fprintf(k.out, "SKIP  %s\n", fmt.Sprintf(format, a...))
}

// get calls a v2 path directly (no capability checks) and decodes the answer as a JSON object.
func (k *checker) call(method, path string, in any) (map[string]any, error) {
	var raw json.RawMessage
	if err := k.c.json(k.ctx, method, path, in, &raw); err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("not a JSON object: %v", err)
	}
	return m, nil
}

func isStr(m map[string]any, k string) bool { _, ok := m[k].(string); return ok }

func missing(m map[string]any, keys ...string) []string {
	var out []string
	for _, key := range keys {
		if !isStr(m, key) {
			out = append(out, key)
		}
	}
	return out
}

func validTime(v any, nullable bool) bool {
	if v == nil {
		return nullable
	}
	s, ok := v.(string)
	if !ok {
		return false
	}
	_, err := time.Parse(time.RFC3339, s)
	return err == nil
}

func validStatus(v any) bool {
	s, _ := v.(string)
	return s == "draft" || s == "published" || s == "scheduled"
}

// errorBody checks that a non-2xx answer carries {"error": "..."}.
func errorBody(err error) bool {
	var se *SiteError
	if !errors.As(err, &se) || se.err == nil {
		return false
	}
	var b struct {
		Error string `json:"error"`
	}
	return json.Unmarshal([]byte(se.err.Body), &b) == nil && b.Error != ""
}

func (k *checker) article(where string, a map[string]any, full bool) {
	keys := []string{"id", "url", "title", "status", "updated_at"}
	if full {
		keys = append(keys, "body_html")
	}
	var bad []string
	for _, key := range keys {
		if _, ok := a[key]; !ok {
			bad = append(bad, key)
		}
	}
	if !isStr(a, "id") {
		bad = append(bad, "id (string)")
	}
	if !validStatus(a["status"]) {
		bad = append(bad, fmt.Sprintf("status %v", a["status"]))
	}
	if !validTime(a["updated_at"], false) {
		bad = append(bad, "updated_at (RFC 3339)")
	}
	if v, ok := a["published_at"]; ok && !validTime(v, true) {
		bad = append(bad, "published_at (RFC 3339 or null)")
	}
	if v, ok := a["tags"]; ok && v != nil {
		if _, isArr := v.([]any); !isArr {
			bad = append(bad, "tags (array)")
		}
	}
	if len(bad) > 0 {
		k.fail("%s: article %v: missing or invalid %s", where, a["id"], strings.Join(bad, ", "))
		return
	}
	k.pass("%s: article %v has the expected fields", where, a["id"])
}

// Check runs the conformance checks against a site and writes a report. It returns the number of
// failed checks.
func Check(ctx context.Context, hc *http.Client, baseURL, token string, write bool, out io.Writer) int {
	c, err := New(baseURL, token, hc)
	k := &checker{ctx: ctx, c: c, out: out}
	if err != nil {
		k.fail("%v", err)
		return k.fails
	}
	fmt.Fprintf(out, "Relay Site Protocol v2 conformance: %s\n\n", c.Root)

	// /info
	var i Info
	var rawInfo map[string]any
	if err := c.raw(ctx, "GET", c.Root+"/relay/v2/info", "", nil, &rawInfo); err != nil {
		k.fail("GET /relay/v2/info: %v", err)
		var v1 Info
		if c.raw(ctx, "GET", c.Root+"/relay/v1/info", "", nil, &v1) == nil {
			fmt.Fprintf(out, "      the site speaks v1 (%q): Relay uses it, but the site tools need v2\n", v1.Name)
		}
		return k.summary()
	}
	b, _ := json.Marshal(rawInfo)
	if err := json.Unmarshal(b, &i); err != nil {
		k.fail("GET /info: invalid shape: %v", err)
		return k.summary()
	}
	if i.Version != "2" {
		k.fail("GET /info: version is %q, want \"2\"", i.Version)
		return k.summary()
	}
	if m := missing(rawInfo, "name", "url"); len(m) > 0 || i.Name == "" || i.URL == "" {
		k.fail("GET /info: name and url must be non-empty strings")
	} else {
		k.pass("GET /info: %s (%s), version 2", i.Name, i.URL)
	}
	if !i.Has(CapArticles) {
		k.fail("GET /info: capabilities must include \"articles\" (got %v)", i.Capabilities)
	} else {
		k.pass("capabilities: %s", strings.Join(i.Capabilities, ", "))
	}
	known := map[string]bool{}
	for _, cp := range KnownCapabilities {
		known[cp] = true
	}
	for _, cp := range i.Capabilities {
		if !known[cp] {
			k.warn("capability %q is not part of the protocol (Relay ignores it)", cp)
		}
	}
	if len(i.Languages) == 0 {
		k.warn("GET /info: no languages listed")
	} else {
		k.pass("languages: %s", strings.Join(i.Languages, ", "))
	}
	seen := map[string]bool{}
	for _, e := range i.Extensions {
		switch msg := CheckExtension(e); {
		case msg != "":
			k.fail("extension %q: %s", e.Name, msg)
		case seen[e.Name]:
			k.fail("extension %q is listed twice", e.Name)
		default:
			k.pass("extension %s: %s %s (admin=%v, read_only=%v)", e.Name, strings.ToUpper(e.Method), e.Path, e.Admin, e.IsReadOnly())
		}
		seen[e.Name] = true
	}
	// Seed the client's cache with what we read, so the calls below use v2.
	infoCache.Lock()
	infoCache.m[c.cacheKey()] = cached{info: &i, at: time.Now()}
	infoCache.Unlock()

	// Auth
	bad := &Client{Root: c.Root, Token: "relay-site-check-wrong-token", HC: c.HC}
	if err := bad.raw(ctx, "GET", c.Root+"/relay/v2/posts?limit=1", "", nil, nil); StatusOf(err) == 401 {
		k.pass("a wrong token gets 401")
	} else {
		k.fail("a wrong token must get 401 (got %v)", errOr(err))
	}

	// Articles
	var first map[string]any
	if list, err := k.call("GET", "/posts?limit=5", nil); err != nil {
		k.fail("GET /posts: %v", err)
	} else if items, ok := list["items"].([]any); !ok {
		k.fail("GET /posts: items must be an array")
	} else {
		k.pass("GET /posts: %d item(s)", len(items))
		if _, ok := list["next_cursor"]; !ok {
			k.warn("GET /posts: next_cursor is absent (use \"\" on the last page)")
		}
		for n, it := range items {
			a, _ := it.(map[string]any)
			if a == nil {
				k.fail("GET /posts: item %d is not an object", n)
				continue
			}
			if n == 0 {
				first = a
				k.article("GET /posts", a, false)
			}
		}
		if cur, _ := list["next_cursor"].(string); cur != "" {
			if page, err := k.call("GET", "/posts?limit=5&cursor="+url.QueryEscape(cur), nil); err != nil {
				k.fail("GET /posts (page 2): %v", err)
			} else if _, ok := page["items"].([]any); !ok {
				k.fail("GET /posts (page 2): items must be an array")
			} else {
				k.pass("GET /posts: next_cursor gives a second page")
			}
		}
	}
	if list, err := k.call("GET", "/posts?status=published&limit=5", nil); err != nil {
		k.fail("GET /posts?status=published: %v", err)
	} else {
		items, _ := list["items"].([]any)
		ok := true
		for _, it := range items {
			if a, _ := it.(map[string]any); a == nil || a["status"] != "published" {
				ok = false
			}
		}
		if ok {
			k.pass("GET /posts?status=published: only published articles")
		} else {
			k.fail("GET /posts?status=published returned other statuses")
		}
	}
	id := ""
	if first != nil {
		id, _ = first["id"].(string)
	}
	if id != "" {
		if a, err := k.call("GET", "/posts/"+url.PathEscape(id), nil); err != nil {
			k.fail("GET /posts/%s: %v", id, err)
		} else {
			k.article("GET /posts/{id}", a, true)
		}
	} else {
		k.skip("GET /posts/{id}: the site has no article yet")
	}
	if _, err := k.call("GET", "/posts/relay-site-check-unknown-id", nil); StatusOf(err) == 404 {
		if errorBody(err) {
			k.pass("an unknown id gets 404 with {\"error\"}")
		} else {
			k.fail("an unknown id gets 404 but without a {\"error\": \"...\"} body")
		}
	} else {
		k.fail("GET /posts/<unknown id> must get 404 (got %v)", errOr(err))
	}

	// Optional read-only capabilities
	if i.Has(CapTaxonomy) {
		if t, err := k.call("GET", "/taxonomy", nil); err != nil {
			k.fail("GET /taxonomy: %v", err)
		} else if !namedList(t["categories"]) || !namedList(t["tags"]) {
			k.fail("GET /taxonomy: categories and tags must be arrays of {name, count}")
		} else {
			k.pass("GET /taxonomy")
		}
	}
	if i.Has(CapAuthors) {
		if t, err := k.call("GET", "/authors", nil); err != nil {
			k.fail("GET /authors: %v", err)
		} else if !idNameList(t["items"]) {
			k.fail("GET /authors: items must be an array of {id, name}")
		} else {
			k.pass("GET /authors")
		}
	}
	if i.Has(CapTranslations) && id != "" {
		k.translations(id)
	}
	if i.Has(CapMetrics) && id != "" {
		if m, err := k.call("GET", "/metrics?ids="+url.QueryEscape(id), nil); err != nil {
			k.fail("GET /metrics: %v", err)
		} else if _, ok := m["metrics"].(map[string]any); !ok {
			k.fail("GET /metrics: metrics must be an object of counters per id")
		} else {
			k.pass("GET /metrics")
		}
	}
	if i.Has(CapComments) && id != "" {
		if m, err := k.call("GET", "/posts/"+url.PathEscape(id)+"/comments", nil); err != nil {
			k.fail("GET /posts/{id}/comments: %v", err)
		} else if _, ok := m["items"].([]any); !ok {
			k.fail("GET /posts/{id}/comments: items must be an array")
		} else {
			k.pass("GET /posts/{id}/comments")
		}
	}
	if !write {
		k.skip("write checks (run with --write to create, edit and delete a draft)")
		return k.summary()
	}
	k.writes(&i)
	return k.summary()
}

func (k *checker) translations(id string) bool {
	m, err := k.call("GET", "/posts/"+url.PathEscape(id)+"/translations", nil)
	if err != nil {
		k.fail("GET /posts/{id}/translations: %v", err)
		return false
	}
	items, ok := m["items"].([]any)
	if !ok {
		k.fail("GET /posts/{id}/translations: items must be an array")
		return false
	}
	for _, it := range items {
		t, _ := it.(map[string]any)
		if t == nil || !isStr(t, "lang") || (t["mode"] != "manual" && t["mode"] != "machine") {
			k.fail("GET /posts/{id}/translations: each item needs lang and mode (manual or machine): %v", it)
			return false
		}
	}
	k.pass("GET /posts/{id}/translations: %d item(s)", len(items))
	return true
}

func (k *checker) writes(i *Info) {
	if !i.Has(CapDrafts) {
		k.skip("write checks: the site has no \"drafts\" capability (site-check never publishes)")
		return
	}
	lang := ""
	if len(i.Languages) > 0 {
		lang = i.Languages[0]
	}
	body := map[string]any{"title": "Relay site-check draft", "body_html": "<p>Created by relay site-check. Safe to delete.</p>",
		"excerpt": "relay site-check", "status": "draft"}
	if lang != "" {
		body["lang"] = lang
	}
	created, err := k.call("POST", "/posts", body)
	if err != nil {
		k.fail("POST /posts (draft): %v", err)
		return
	}
	id, _ := created["id"].(string)
	if id == "" || created["status"] != "draft" || !validTime(created["updated_at"], false) {
		k.fail("POST /posts: answer needs id, status \"draft\" and updated_at: %v", created)
		if id == "" {
			return
		}
	} else {
		k.pass("POST /posts: draft %s created", id)
	}
	cleanup := func() {
		if !i.Has(CapDelete) {
			k.warn("draft %s is left on the site (no \"delete\" capability): remove it by hand", id)
			return
		}
		if err := k.c.raw(k.ctx, "DELETE", k.c.Root+"/relay/v2/posts/"+url.PathEscape(id), "", nil, nil); err != nil {
			k.fail("DELETE /posts/{id}: %v", err)
			return
		}
		k.pass("DELETE /posts/{id}")
		if _, err := k.call("GET", "/posts/"+url.PathEscape(id), nil); StatusOf(err) == 404 {
			k.pass("a deleted article gets 404")
		} else {
			k.fail("GET of a deleted article must get 404 (got %v)", errOr(err))
		}
	}
	defer cleanup()

	a, err := k.call("GET", "/posts/"+url.PathEscape(id), nil)
	if err != nil {
		k.fail("GET /posts/{id} of the new draft: %v", err)
		return
	}
	k.article("GET /posts/{id} (new draft)", a, true)
	if a["status"] != "draft" {
		k.fail("the new draft has status %v", a["status"])
	}
	if list, err := k.call("GET", "/posts?status=draft&limit=100", nil); err == nil {
		found := false
		items, _ := list["items"].([]any)
		for _, it := range items {
			if m, _ := it.(map[string]any); m != nil && m["id"] == id {
				found = true
			}
		}
		if found {
			k.pass("GET /posts?status=draft lists the new draft")
		} else {
			k.warn("GET /posts?status=draft (first 100) does not list the new draft")
		}
	}
	if i.Has(CapEdit) {
		updated, _ := a["updated_at"].(string)
		if p, err := k.call("PATCH", "/posts/"+url.PathEscape(id), map[string]any{"title": "Relay site-check draft (edited)", "expected_updated_at": updated}); err != nil {
			k.fail("PATCH /posts/{id}: %v", err)
		} else if p["title"] != "Relay site-check draft (edited)" {
			k.fail("PATCH /posts/{id}: the answer does not carry the new title")
		} else {
			k.pass("PATCH /posts/{id} with the current expected_updated_at")
		}
		if _, err := k.call("PATCH", "/posts/"+url.PathEscape(id), map[string]any{"title": "stale", "expected_updated_at": "2000-01-01T00:00:00Z"}); StatusOf(err) == 409 {
			k.pass("PATCH with a stale expected_updated_at gets 409")
		} else {
			k.fail("PATCH with a stale expected_updated_at must get 409 (got %v)", errOr(err))
		}
		_, err := k.call("PATCH", "/posts/"+url.PathEscape(id), map[string]any{"status": "draft"})
		if s := StatusOf(err); s == 400 || s == 422 {
			k.pass("PATCH with status gets %d", s)
		} else {
			k.fail("PATCH with status must get 400 (got %v)", errOr(err))
		}
	}
	if i.Has(CapTranslations) {
		other := ""
		for _, l := range i.Languages {
			if l != lang {
				other = l
				break
			}
		}
		if other == "" {
			k.skip("translation writes: the site lists a single language")
		} else {
			p := "/posts/" + url.PathEscape(id) + "/translations/" + url.PathEscape(other)
			if t, err := k.call("PUT", p, map[string]any{"title": "Relay site-check", "excerpt": "relay site-check", "body_html": "<p>relay site-check</p>"}); err != nil {
				k.fail("PUT /posts/{id}/translations/%s: %v", other, err)
			} else if t["mode"] != "manual" || t["lang"] != other {
				k.fail("PUT /posts/{id}/translations/%s: answer needs lang %q and mode \"manual\": %v", other, other, t)
			} else {
				k.pass("PUT /posts/{id}/translations/%s", other)
			}
			k.translations(id)
			if err := k.c.raw(k.ctx, "DELETE", k.c.Root+"/relay/v2"+p, "", nil, nil); err != nil {
				k.fail("DELETE /posts/{id}/translations/%s: %v", other, err)
			} else {
				k.pass("DELETE /posts/{id}/translations/%s", other)
			}
		}
	}
}

func namedList(v any) bool {
	arr, ok := v.([]any)
	if !ok {
		return false
	}
	for _, it := range arr {
		m, _ := it.(map[string]any)
		if m == nil || !isStr(m, "name") {
			return false
		}
	}
	return true
}

func idNameList(v any) bool {
	arr, ok := v.([]any)
	if !ok {
		return false
	}
	for _, it := range arr {
		m, _ := it.(map[string]any)
		if m == nil || !isStr(m, "id") || !isStr(m, "name") {
			return false
		}
	}
	return true
}

func errOr(err error) string {
	if err == nil {
		return "a 2xx answer"
	}
	return err.Error()
}

func (k *checker) summary() int {
	fmt.Fprintf(k.out, "\n%d failed, %d warning(s)\n", k.fails, k.warns)
	return k.fails
}
