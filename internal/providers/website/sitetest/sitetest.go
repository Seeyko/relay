// Package sitetest is an in-memory site that implements the Relay Site Protocol v2, for tests.
// Routes of capabilities the site does not declare answer 404, and every request is recorded, so a
// test can check that Relay refused an operation without calling the site.
package sitetest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Article struct {
	ID          string         `json:"id"`
	URL         string         `json:"url"`
	Title       string         `json:"title"`
	Slug        string         `json:"slug"`
	Excerpt     string         `json:"excerpt"`
	BodyHTML    string         `json:"body_html"`
	BodyMD      string         `json:"body_markdown,omitempty"`
	Tags        []string       `json:"tags"`
	Category    string         `json:"category"`
	Lang        string         `json:"lang"`
	CoverURL    string         `json:"cover_url"`
	AuthorID    string         `json:"author_id"`
	Status      string         `json:"status"`
	PublishedAt *time.Time     `json:"published_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
	Extra       map[string]any `json:"extra,omitempty"`
}

type Translation struct {
	Lang      string    `json:"lang"`
	Title     string    `json:"title"`
	Excerpt   string    `json:"excerpt"`
	BodyHTML  string    `json:"body_html"`
	Mode      string    `json:"mode"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Call is one request received by the site.
type Call struct {
	Method string
	Path   string
	Query  string
	Body   map[string]any
}

// Extension is a site-defined action: its /info description and its handler.
type Extension struct {
	Info    map[string]any
	Handler func(r *http.Request, args map[string]any) (int, any)
}

type Site struct {
	Token        string
	Name         string
	Capabilities []string
	Languages    []string
	Extensions   []Extension
	// MachineTranslate gives every article a machine translation in the other languages.
	MachineTranslate bool

	mu       sync.Mutex
	URL      string // set by the test (the httptest server URL); used in article URLs
	articles map[string]*Article
	order    []string
	trans    map[string]map[string]*Translation
	media    []string
	calls    []Call
	next     int
	clock    time.Time
}

// New builds a site with the given capabilities ("articles" is always added).
func New(token string, caps ...string) *Site {
	s := &Site{Token: token, Name: "Fake site", Languages: []string{"en", "fr"},
		articles: map[string]*Article{}, trans: map[string]map[string]*Translation{},
		clock: time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)}
	s.Capabilities = append([]string{"articles"}, caps...)
	return s
}

// All is every capability of the protocol.
var All = []string{"media", "drafts", "edit", "delete", "schedule", "translations", "taxonomy", "authors", "metrics", "comments"}

func (s *Site) has(c string) bool {
	for _, x := range s.Capabilities {
		if x == c {
			return true
		}
	}
	return false
}

func (s *Site) tick() time.Time {
	s.clock = s.clock.Add(time.Minute)
	return s.clock
}

// Calls returns the requests received so far ("GET /relay/v2/posts" ...).
func (s *Site) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Call(nil), s.calls...)
}

// Called says whether a request "METHOD /path" (path without query) was received.
func (s *Site) Called(methodPath string) bool {
	for _, c := range s.Calls() {
		if c.Method+" "+c.Path == methodPath {
			return true
		}
	}
	return false
}

// Article returns a copy of an article.
func (s *Site) Article(id string) (Article, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.articles[id]
	if !ok {
		return Article{}, false
	}
	return *a, true
}

// Add stores an article directly (status published unless set).
func (s *Site) Add(a Article) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.add(&a)
}

func (s *Site) add(a *Article) string {
	s.next++
	a.ID = strconv.Itoa(s.next)
	if a.Slug == "" {
		a.Slug = strings.ReplaceAll(strings.ToLower(a.Title), " ", "-")
	}
	a.URL = s.URL + "/blog/" + a.Slug
	if a.Status == "" {
		a.Status = "published"
	}
	if a.Lang == "" {
		a.Lang = s.Languages[0]
	}
	if a.Tags == nil {
		a.Tags = []string{}
	}
	a.UpdatedAt = s.tick()
	if a.Status == "published" && a.PublishedAt == nil {
		t := a.UpdatedAt
		a.PublishedAt = &t
	}
	s.articles[a.ID] = a
	s.order = append(s.order, a.ID)
	s.trans[a.ID] = map[string]*Translation{}
	if s.MachineTranslate {
		for _, l := range s.Languages {
			if l != a.Lang {
				s.trans[a.ID][l] = &Translation{Lang: l, Title: "[" + l + "] " + a.Title, Excerpt: a.Excerpt, BodyHTML: a.BodyHTML, Mode: "machine", UpdatedAt: a.UpdatedAt}
			}
		}
	}
	return a.ID
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, format string, a ...any) {
	writeJSON(w, status, map[string]string{"error": fmt.Sprintf(format, a...)})
}

// ServeHTTP implements the protocol under /relay/v2.
func (s *Site) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		b, _ := io.ReadAll(r.Body)
		if len(b) > 0 && json.Unmarshal(b, &body) != nil {
			fail(w, 400, "invalid JSON")
			return
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, Call{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Body: body})
	if r.Header.Get("Authorization") != "Bearer "+s.Token {
		fail(w, 401, "unauthorized")
		return
	}
	p, ok := strings.CutPrefix(r.URL.Path, "/relay/v2")
	if !ok {
		fail(w, 404, "not found")
		return
	}
	seg := strings.Split(strings.Trim(p, "/"), "/")
	need := func(c string) bool {
		if !s.has(c) {
			fail(w, 404, "not found")
			return false
		}
		return true
	}
	switch {
	case p == "/info" && r.Method == "GET":
		exts := []map[string]any{}
		for _, e := range s.Extensions {
			exts = append(exts, e.Info)
		}
		writeJSON(w, 200, map[string]any{"name": s.Name, "url": s.URL, "version": "2", "capabilities": s.Capabilities, "languages": s.Languages, "extensions": exts})
	case p == "/posts" && r.Method == "GET":
		s.list(w, r)
	case p == "/posts" && r.Method == "POST":
		s.create(w, body)
	case p == "/media" && r.Method == "POST":
		if !need("media") {
			return
		}
		name := ""
		if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
			f, h, err := r.FormFile("file")
			if err != nil {
				fail(w, 400, "file is required")
				return
			}
			f.Close()
			name = h.Filename
		} else if u, _ := body["url"].(string); u != "" {
			name = u[strings.LastIndex(u, "/")+1:]
		} else {
			fail(w, 400, "send a file or a url")
			return
		}
		s.media = append(s.media, name)
		id := fmt.Sprintf("img_%d", len(s.media))
		writeJSON(w, 201, map[string]string{"id": id, "url": "https://cdn.example/" + id + "/" + name})
	case p == "/taxonomy" && r.Method == "GET":
		if need("taxonomy") {
			s.taxonomy(w)
		}
	case p == "/authors" && r.Method == "GET":
		if need("authors") {
			writeJSON(w, 200, map[string]any{"items": []map[string]string{{"id": "7", "name": "Jane Doe"}}})
		}
	case p == "/metrics" && r.Method == "GET":
		if need("metrics") {
			m := map[string]any{}
			for _, id := range strings.Split(r.URL.Query().Get("ids"), ",") {
				if _, ok := s.articles[id]; ok {
					m[id] = map[string]int64{"views": 100}
				}
			}
			writeJSON(w, 200, map[string]any{"metrics": m})
		}
	case len(seg) == 3 && seg[0] == "comments" && seg[2] == "replies" && r.Method == "POST":
		if need("comments") {
			writeJSON(w, 201, map[string]string{"id": "r1", "url": s.URL + "#r1"})
		}
	case len(seg) >= 2 && seg[0] == "posts":
		s.article(w, r, seg[1:], body, need)
	case len(seg) >= 2 && seg[0] == "x":
		s.extension(w, r, p, body)
	default:
		fail(w, 404, "not found")
	}
}

func (s *Site) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	start, _ := strconv.Atoi(q.Get("cursor"))
	var all []*Article
	for i := len(s.order) - 1; i >= 0; i-- { // newest first
		a := s.articles[s.order[i]]
		if a == nil || (q.Get("status") != "" && a.Status != q.Get("status")) || (q.Get("lang") != "" && a.Lang != q.Get("lang")) {
			continue
		}
		if since := q.Get("since"); since != "" {
			if t, err := time.Parse(time.RFC3339, since); err == nil && a.UpdatedAt.Before(t) {
				continue
			}
		}
		all = append(all, a)
	}
	items := []map[string]any{}
	next := ""
	for i := start; i < len(all); i++ {
		if len(items) == limit {
			next = strconv.Itoa(i)
			break
		}
		a := all[i]
		items = append(items, map[string]any{"id": a.ID, "url": a.URL, "title": a.Title, "slug": a.Slug, "excerpt": a.Excerpt, "status": a.Status,
			"lang": a.Lang, "published_at": a.PublishedAt, "updated_at": a.UpdatedAt, "metrics": map[string]int64{"views": 100}})
	}
	writeJSON(w, 200, map[string]any{"items": items, "next_cursor": next})
}

func str(b map[string]any, k string) string { v, _ := b[k].(string); return v }

func strs(b map[string]any, k string) []string {
	out := []string{}
	arr, _ := b[k].([]any)
	for _, x := range arr {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func (s *Site) create(w http.ResponseWriter, b map[string]any) {
	if str(b, "title") == "" {
		fail(w, 422, "title is required")
		return
	}
	a := &Article{Title: str(b, "title"), Slug: str(b, "slug"), Excerpt: str(b, "excerpt"), BodyHTML: str(b, "body_html"), BodyMD: str(b, "body_markdown"),
		Tags: strs(b, "tags"), Category: str(b, "category"), Lang: str(b, "lang"), CoverURL: str(b, "cover_url"), AuthorID: str(b, "author_id"), Status: str(b, "status")}
	if a.BodyHTML == "" && a.BodyMD != "" {
		a.BodyHTML = "<p>" + a.BodyMD + "</p>"
	}
	if ex, ok := b["extra"].(map[string]any); ok {
		a.Extra = ex
	}
	switch a.Status {
	case "published":
	case "draft":
		if !s.has("drafts") {
			fail(w, 422, "drafts are not supported")
			return
		}
	case "scheduled":
		t, err := time.Parse(time.RFC3339, str(b, "published_at"))
		if !s.has("schedule") || err != nil {
			fail(w, 422, "scheduled needs the schedule capability and published_at")
			return
		}
		a.PublishedAt = &t
	default:
		fail(w, 422, "invalid status %q", a.Status)
		return
	}
	if pa := str(b, "published_at"); pa != "" && a.Status == "published" {
		if t, err := time.Parse(time.RFC3339, pa); err == nil {
			a.PublishedAt = &t
		}
	}
	s.add(a)
	writeJSON(w, 201, map[string]any{"id": a.ID, "url": a.URL, "status": a.Status, "updated_at": a.UpdatedAt})
}

func (s *Site) taxonomy(w http.ResponseWriter) {
	cats, tags := map[string]int{}, map[string]int{}
	for _, a := range s.articles {
		if a.Category != "" {
			cats[a.Category]++
		}
		for _, t := range a.Tags {
			tags[t]++
		}
	}
	list := func(m map[string]int) []map[string]any {
		out := []map[string]any{}
		for k, v := range m {
			out = append(out, map[string]any{"name": k, "count": v})
		}
		sort.Slice(out, func(i, j int) bool { return out[i]["name"].(string) < out[j]["name"].(string) })
		return out
	}
	writeJSON(w, 200, map[string]any{"categories": list(cats), "tags": list(tags)})
}

func (s *Site) article(w http.ResponseWriter, r *http.Request, seg []string, b map[string]any, need func(string) bool) {
	a, ok := s.articles[seg[0]]
	if !ok {
		fail(w, 404, "article %s not found", seg[0])
		return
	}
	switch {
	case len(seg) == 1 && r.Method == "GET":
		writeJSON(w, 200, a)
	case len(seg) == 1 && r.Method == "PATCH":
		if !need("edit") {
			return
		}
		if _, ok := b["status"]; ok {
			fail(w, 400, "status cannot be edited")
			return
		}
		if exp := str(b, "expected_updated_at"); exp != "" {
			if t, err := time.Parse(time.RFC3339, exp); err != nil || !t.Equal(a.UpdatedAt) {
				fail(w, 409, "the article changed since %s", exp)
				return
			}
		}
		for k, v := range b {
			sv, _ := v.(string)
			switch k {
			case "title":
				a.Title = sv
			case "slug":
				a.Slug = sv
			case "excerpt":
				a.Excerpt = sv
			case "body_html":
				a.BodyHTML = sv
			case "body_markdown":
				a.BodyMD, a.BodyHTML = sv, "<p>"+sv+"</p>"
			case "category":
				a.Category = sv
			case "lang":
				a.Lang = sv
			case "cover_url":
				a.CoverURL = sv
			case "author_id":
				a.AuthorID = sv
			case "tags":
				a.Tags = strs(b, "tags")
			}
		}
		a.UpdatedAt = s.tick()
		writeJSON(w, 200, a)
	case len(seg) == 1 && r.Method == "DELETE":
		if !need("delete") {
			return
		}
		delete(s.articles, a.ID)
		delete(s.trans, a.ID)
		w.WriteHeader(204)
	case len(seg) == 2 && seg[1] == "publish" && r.Method == "POST":
		if !need("drafts") {
			return
		}
		now := s.tick()
		t := now
		if pa := str(b, "published_at"); pa != "" {
			var err error
			if t, err = time.Parse(time.RFC3339, pa); err != nil {
				fail(w, 400, "invalid published_at")
				return
			}
		}
		if t.After(time.Now()) {
			if !s.has("schedule") {
				fail(w, 422, "scheduling is not supported")
				return
			}
			a.Status = "scheduled"
		} else {
			a.Status = "published"
		}
		a.PublishedAt, a.UpdatedAt = &t, now
		writeJSON(w, 200, a)
	case len(seg) == 2 && seg[1] == "unpublish" && r.Method == "POST":
		if !need("drafts") {
			return
		}
		a.Status, a.UpdatedAt = "draft", s.tick()
		writeJSON(w, 200, a)
	case len(seg) == 2 && seg[1] == "comments" && r.Method == "GET":
		if need("comments") {
			writeJSON(w, 200, map[string]any{"items": []map[string]any{{"id": "c1", "author": "Ann", "text": "Nice!", "url": a.URL + "#c1", "created_at": "2026-11-19T19:00:00Z"}}})
		}
	case len(seg) == 2 && seg[1] == "translations" && r.Method == "GET":
		if !need("translations") {
			return
		}
		items := []*Translation{}
		for _, l := range s.Languages {
			if t := s.trans[a.ID][l]; t != nil {
				items = append(items, t)
			}
		}
		writeJSON(w, 200, map[string]any{"items": items})
	case len(seg) == 3 && seg[1] == "translations" && (r.Method == "PUT" || r.Method == "DELETE"):
		if !need("translations") {
			return
		}
		lang := seg[2]
		known := false
		for _, l := range s.Languages {
			known = known || l == lang
		}
		if !known || lang == a.Lang {
			fail(w, 400, "invalid language %s", lang)
			return
		}
		if r.Method == "PUT" {
			t := &Translation{Lang: lang, Title: str(b, "title"), Excerpt: str(b, "excerpt"), BodyHTML: str(b, "body_html"), Mode: "manual", UpdatedAt: s.tick()}
			s.trans[a.ID][lang] = t
			writeJSON(w, 200, t)
			return
		}
		t := s.trans[a.ID][lang]
		if t == nil || t.Mode != "manual" {
			fail(w, 404, "no manual translation in %s", lang)
			return
		}
		delete(s.trans[a.ID], lang)
		if s.MachineTranslate {
			s.trans[a.ID][lang] = &Translation{Lang: lang, Title: "[" + lang + "] " + a.Title, Excerpt: a.Excerpt, BodyHTML: a.BodyHTML, Mode: "machine", UpdatedAt: s.tick()}
		}
		w.WriteHeader(204)
	default:
		fail(w, 404, "not found")
	}
}

func (s *Site) extension(w http.ResponseWriter, r *http.Request, p string, body map[string]any) {
	for _, e := range s.Extensions {
		method, _ := e.Info["method"].(string)
		path, _ := e.Info["path"].(string)
		if !strings.EqualFold(method, r.Method) || !matchPath(path, p) {
			continue
		}
		args := body
		if args == nil {
			args = map[string]any{}
		}
		for k, v := range r.URL.Query() {
			args[k] = v[0]
		}
		args["_path"] = p
		s.mu.Unlock()
		status, out := e.Handler(r, args)
		s.mu.Lock()
		if out == nil {
			w.WriteHeader(status)
			return
		}
		writeJSON(w, status, out)
		return
	}
	fail(w, 404, "not found")
}

// matchPath matches /x/a/{id}/b against /x/a/42/b.
func matchPath(pattern, p string) bool {
	ps, xs := strings.Split(pattern, "/"), strings.Split(p, "/")
	if len(ps) != len(xs) {
		return false
	}
	for i := range ps {
		if strings.HasPrefix(ps[i], "{") && strings.HasSuffix(ps[i], "}") {
			continue
		}
		if ps[i] != xs[i] {
			return false
		}
	}
	return true
}

// Start serves the site on a local test server and sets its URL. Close the server when done.
func (s *Site) Start() *httptest.Server {
	srv := httptest.NewServer(s)
	s.mu.Lock()
	s.URL = srv.URL
	s.mu.Unlock()
	return srv
}
