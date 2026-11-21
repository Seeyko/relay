package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Seeyko/relay/internal/api"
	"github.com/Seeyko/relay/internal/core"
	"github.com/Seeyko/relay/internal/mcp"
	"github.com/Seeyko/relay/internal/secret"
	"github.com/Seeyko/relay/internal/store"

	_ "github.com/Seeyko/relay/internal/providers/linkedin"
	_ "github.com/Seeyko/relay/internal/providers/meta"
	_ "github.com/Seeyko/relay/internal/providers/threads"
	_ "github.com/Seeyko/relay/internal/providers/tiktok"
	_ "github.com/Seeyko/relay/internal/providers/website"
	_ "github.com/Seeyko/relay/internal/providers/x"
	_ "github.com/Seeyko/relay/internal/providers/youtube"
)

const token = "test-token-0123456789"

type env struct {
	t     *testing.T
	srv   *httptest.Server
	svc   *core.Service
	sched *core.Scheduler
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir + "/relay.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	box, _ := secret.New("k")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := &core.Service{Store: st, Box: box, MediaDir: dir + "/media", HTTP: http.DefaultClient, Log: log, BaseURL: "https://relay.example"}
	m := mcp.NewServer("relay", "test", mcp.BuildTools(svc))
	a := &api.API{Svc: svc, MCP: m, Token: token, Log: log}
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)
	return &env{t: t, srv: srv, svc: svc, sched: &core.Scheduler{Svc: svc}}
}

func (e *env) req(method, path string, body any) (int, map[string]any, []byte) {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	r, _ := http.NewRequest(method, e.srv.URL+path, rd)
	r.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return resp.StatusCode, m, raw
}

func (e *env) mcp(method string, params any) map[string]any {
	e.t.Helper()
	_, m, raw := e.req("POST", "/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if m["error"] != nil {
		e.t.Fatalf("mcp %s error: %s", method, raw)
	}
	return m["result"].(map[string]any)
}

func (e *env) callTool(name string, args any) (string, bool) {
	res := e.mcp("tools/call", map[string]any{"name": name, "arguments": args})
	text := res["content"].([]any)[0].(map[string]any)["text"].(string)
	isErr, _ := res["isError"].(bool)
	return text, isErr
}

// fakeX emulates the subset of X API v2 that Relay uses.
type fakeX struct {
	mu     sync.Mutex
	tweets []map[string]any
	nextID int
}

func (f *fakeX) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	check := func(r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "OAuth ") {
			t.Errorf("%s %s without OAuth header", r.Method, r.URL.Path)
		}
	}
	mux.HandleFunc("GET /2/users/me", func(w http.ResponseWriter, r *http.Request) {
		check(r)
		w.Write([]byte(`{"data":{"id":"42","name":"Alice","username":"alice"}}`))
	})
	mux.HandleFunc("POST /2/tweets", func(w http.ResponseWriter, r *http.Request) {
		check(r)
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.nextID++
		id := 1000 + f.nextID
		body["id"] = id
		f.tweets = append(f.tweets, body)
		f.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": jsonID(id), "text": body["text"]}})
	})
	mux.HandleFunc("GET /2/tweets", func(w http.ResponseWriter, r *http.Request) {
		check(r)
		var data []map[string]any
		for _, id := range strings.Split(r.URL.Query().Get("ids"), ",") {
			data = append(data, map[string]any{"id": id, "public_metrics": map[string]int{"like_count": 7, "reply_count": 1}})
		}
		json.NewEncoder(w).Encode(map[string]any{"data": data})
	})
	mux.HandleFunc("GET /2/tweets/search/recent", func(w http.ResponseWriter, r *http.Request) {
		check(r)
		w.Write([]byte(`{"data":[{"id":"5555","text":"great post!","author_id":"77","created_at":"2026-11-19T10:00:00Z"}],"includes":{"users":[{"id":"77","username":"fan"}]}}`))
	})
	mux.HandleFunc("POST /2/media/upload", func(w http.ResponseWriter, r *http.Request) {
		check(r)
		if _, _, err := r.FormFile("media"); err != nil {
			t.Errorf("media upload without file: %v", err)
		}
		w.Write([]byte(`{"data":{"id":"m1"}}`))
	})
	mux.HandleFunc("GET /2/users/42/tweets", func(w http.ResponseWriter, r *http.Request) {
		check(r)
		w.Write([]byte(`{"data":[{"id":"900","text":"old tweet","created_at":"2026-01-02T03:04:05Z","public_metrics":{"like_count":3}}],"meta":{}}`))
	})
	return mux
}

func jsonID(i int) string { b, _ := json.Marshal(i); return string(b) }

func TestEndToEndX(t *testing.T) {
	e := newEnv(t)
	fx := &fakeX{}
	xs := httptest.NewServer(fx.handler(t))
	defer xs.Close()

	// unauthenticated
	r, _ := http.Get(e.srv.URL + "/v1/accounts")
	if r.StatusCode != 401 {
		t.Fatalf("want 401, got %d", r.StatusCode)
	}

	// MCP handshake
	init := e.mcp("initialize", map[string]any{"protocolVersion": "2025-06-18"})
	if init["protocolVersion"] != "2025-06-18" {
		t.Fatalf("protocol: %v", init["protocolVersion"])
	}
	tools := e.mcp("tools/list", nil)["tools"].([]any)
	if len(tools) < 10 {
		t.Fatalf("expected tools, got %d", len(tools))
	}

	// connect X
	text, isErr := e.callTool("connect_account", map[string]any{"provider": "x", "credentials": map[string]string{
		"consumer_key": "a", "consumer_secret": "b", "access_token": "c", "access_token_secret": "d", "base_url": xs.URL}})
	if isErr {
		t.Fatalf("connect: %s", text)
	}
	var acc store.Account
	json.Unmarshal([]byte(text), &acc)
	if acc.Handle != "alice" || acc.ID == "" {
		t.Fatalf("account: %+v", acc)
	}

	// upload an image through REST (multipart)
	var buf bytes.Buffer
	buf.WriteString("--b\r\nContent-Disposition: form-data; name=\"file\"; filename=\"pic.png\"\r\nContent-Type: image/png\r\n\r\nPNGDATA\r\n--b--\r\n")
	req, _ := http.NewRequest("POST", e.srv.URL+"/v1/media", &buf)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "multipart/form-data; boundary=b")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 201 {
		t.Fatalf("upload: %v %v", err, resp.StatusCode)
	}
	var up struct {
		Media store.Media `json:"media"`
		URL   string      `json:"url"`
	}
	json.NewDecoder(resp.Body).Decode(&up)
	if !strings.HasPrefix(up.URL, "https://relay.example/media/med_") {
		t.Fatalf("media url: %s", up.URL)
	}
	// public media route serves it without auth
	pub, _ := http.Get(e.srv.URL + "/media/" + up.URL[strings.LastIndex(up.URL, "/")+1:])
	if b, _ := io.ReadAll(pub.Body); string(b) != "PNGDATA" {
		t.Fatalf("public media: %q", b)
	}

	// schedule a thread in the past -> due now
	past := time.Now().Add(-time.Minute).Format(time.RFC3339)
	text, isErr = e.callTool("schedule_post", map[string]any{"account_ids": []string{acc.ID}, "thread": []string{"one", "two"}, "media": []string{up.Media.ID}, "scheduled_at": past})
	if isErr {
		t.Fatalf("schedule: %s", text)
	}
	var posts []store.Post
	json.Unmarshal([]byte(text), &posts)
	if len(posts) != 1 || posts[0].Status != "scheduled" {
		t.Fatalf("posts: %s", text)
	}

	// future post must not be published
	code, _, raw := e.req("POST", "/v1/posts", map[string]any{"account_ids": []string{acc.ID}, "text": "later", "scheduled_at": time.Now().Add(time.Hour).Format(time.RFC3339)})
	if code != 200 {
		t.Fatalf("rest schedule: %d %s", code, raw)
	}

	e.sched.RunDue(context.Background())

	p, err := e.svc.Store.GetPost(context.Background(), posts[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != "published" || len(p.RemoteIDs) != 2 || p.RemoteURL != "https://x.com/alice/status/"+p.RemoteID {
		t.Fatalf("published post: %+v", p)
	}
	if len(fx.tweets) != 2 {
		t.Fatalf("tweets sent: %d", len(fx.tweets))
	}
	second := fx.tweets[1]["reply"].(map[string]any)["in_reply_to_tweet_id"]
	if second != p.RemoteIDs[0] {
		t.Fatalf("thread not chained: %v", fx.tweets[1])
	}
	if fx.tweets[0]["media"] == nil || fx.tweets[1]["media"] != nil {
		t.Fatalf("media should be on first tweet only: %v", fx.tweets)
	}

	// metrics + stats
	text, isErr = e.callTool("refresh_metrics", map[string]any{})
	if isErr || !strings.Contains(text, `"updated": 1`) {
		t.Fatalf("metrics: %s", text)
	}
	code, stats, raw := e.req("GET", "/v1/analytics?group_by=format", nil)
	if code != 200 || stats["posts"].(float64) != 1 {
		t.Fatalf("stats: %s", raw)
	}

	// comments + reply
	text, isErr = e.callTool("list_comments", map[string]any{"post_id": p.ID, "refresh": true})
	if isErr || !strings.Contains(text, "great post!") {
		t.Fatalf("comments: %s", text)
	}
	text, isErr = e.callTool("reply_comment", map[string]any{"account_id": acc.ID, "remote_id": "5555", "text": "thanks!"})
	if isErr {
		t.Fatalf("reply: %s", text)
	}
	text, _ = e.callTool("list_comments", map[string]any{"pending_only": true})
	if strings.Contains(text, "5555") {
		t.Fatalf("comment should be marked replied: %s", text)
	}

	// history import, idempotent
	text, _ = e.callTool("import_history", map[string]any{"account_id": acc.ID})
	if !strings.Contains(text, `"created": 1`) {
		t.Fatalf("import: %s", text)
	}
	text, _ = e.callTool("import_history", map[string]any{"account_id": acc.ID})
	if !strings.Contains(text, `"updated": 1`) {
		t.Fatalf("re-import: %s", text)
	}

	// bad input is a 400
	code, _, _ = e.req("POST", "/v1/posts", map[string]any{"account_ids": []string{acc.ID}, "text": "x", "scheduled_at": "tomorrow"})
	if code != 400 {
		t.Fatalf("want 400, got %d", code)
	}
}

func TestRetryOnServerError(t *testing.T) {
	e := newEnv(t)
	calls := 0
	xs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/2/users/me" {
			w.Write([]byte(`{"data":{"id":"42","username":"alice"}}`))
			return
		}
		calls++
		w.WriteHeader(503)
	}))
	defer xs.Close()
	acc, err := e.svc.ConnectAccount(context.Background(), "x", "", map[string]string{"consumer_key": "a", "consumer_secret": "b", "access_token": "c", "access_token_secret": "d", "base_url": xs.URL})
	if err != nil {
		t.Fatal(err)
	}
	ps, err := e.svc.Schedule(context.Background(), core.ScheduleInput{AccountIDs: []string{acc.ID}, Text: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	e.sched.RunDue(context.Background())
	p, _ := e.svc.Store.GetPost(context.Background(), ps[0].ID)
	if p.Status != "scheduled" || p.Attempts != 1 || p.NextAttemptAt == nil || !strings.Contains(p.Error, "503") {
		t.Fatalf("expected retry later: %+v", p)
	}
	// not due again before backoff
	e.sched.RunDue(context.Background())
	if calls != 1 {
		t.Fatalf("retried too early: %d calls", calls)
	}
}

func TestWebsiteProtocol(t *testing.T) {
	e := newEnv(t)
	var got map[string]any
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer site-secret" {
			w.WriteHeader(401)
			return
		}
		switch {
		case r.URL.Path == "/relay/v1/info":
			w.Write([]byte(`{"name":"Example site","url":"https://example.com","version":"1","capabilities":["articles","metrics"]}`))
		case r.URL.Path == "/relay/v1/posts" && r.Method == "POST":
			json.NewDecoder(r.Body).Decode(&got)
			w.Write([]byte(`{"id":"a1","url":"https://example.com/blog/hello"}`))
		case r.URL.Path == "/relay/v1/metrics":
			w.Write([]byte(`{"metrics":{"a1":{"views":120}}}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer site.Close()
	text, isErr := e.callTool("connect_account", map[string]any{"provider": "website", "credentials": map[string]string{"base_url": site.URL, "token": "site-secret"}})
	if isErr {
		t.Fatalf("connect: %s", text)
	}
	var acc store.Account
	json.Unmarshal([]byte(text), &acc)
	text, isErr = e.callTool("schedule_post", map[string]any{"account_ids": []string{acc.ID}, "text": "# Hello\n\nBody", "options": map[string]any{"title": "Hello", "tags": []string{"go"}}})
	if isErr {
		t.Fatalf("schedule: %s", text)
	}
	var posts []store.Post
	json.Unmarshal([]byte(text), &posts)
	text, isErr = e.callTool("publish_now", map[string]any{"id": posts[0].ID})
	if isErr || !strings.Contains(text, "https://example.com/blog/hello") {
		t.Fatalf("publish: %s", text)
	}
	if got["title"] != "Hello" || got["body_markdown"] != "# Hello\n\nBody" || got["status"] != "published" {
		t.Fatalf("article sent: %v", got)
	}
	text, _ = e.callTool("refresh_metrics", map[string]any{})
	if !strings.Contains(text, `"updated": 1`) {
		t.Fatalf("metrics: %s", text)
	}
	// comments capability absent -> unsupported is reported as an error, not a crash
	_, isErr = e.callTool("list_comments", map[string]any{"post_id": posts[0].ID, "refresh": true})
	if !isErr {
		t.Fatalf("expected unsupported comments")
	}
}

func TestMetaProviders(t *testing.T) {
	e := newEnv(t)
	var igSteps []string
	meta := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.FormValue("access_token") == "" && r.URL.Query().Get("access_token") == "" {
			if err := r.ParseMultipartForm(1 << 20); err != nil || r.FormValue("access_token") == "" {
				t.Errorf("no token on %s", r.URL.Path)
			}
		}
		p := r.URL.Path
		switch {
		// Facebook
		case p == "/v26.0/123" && r.Method == "GET":
			w.Write([]byte(`{"id":"123","name":"Alice Page","username":"alicepage"}`))
		case p == "/v26.0/123/picture": // a Page without a picture: nothing to download
			w.Write([]byte(`{"data":{"url":"https://cdn.fb/silhouette.png","is_silhouette":true}}`))
		case p == "/v26.0/123/feed":
			if r.FormValue("message") != "hello fb" {
				t.Errorf("fb message: %q", r.FormValue("message"))
			}
			w.Write([]byte(`{"id":"123_9"}`))
		case p == "/v26.0/123_9" && r.URL.Query().Get("fields") == "permalink_url":
			w.Write([]byte(`{"permalink_url":"https://facebook.com/p/9"}`))
		case p == "/v26.0/123_9/comments" && r.Method == "GET":
			w.Write([]byte(`{"data":[{"id":"c1","message":"nice","created_time":"2026-11-19T10:00:00+0000","from":{"id":"5","name":"Ann"}},{"id":"c2","message":"mine","from":{"id":"123","name":"Alice Page"}}]}`))
		// Instagram
		case p == "/v26.0/me":
			w.Write([]byte(`{"id":"app1","user_id":"ig1","username":"alice.ig"}`))
		case p == "/v26.0/ig1/media" && r.Method == "POST":
			igSteps = append(igSteps, "container:"+r.FormValue("image_url"))
			w.Write([]byte(`{"id":"cont1"}`))
		case p == "/v26.0/cont1":
			w.Write([]byte(`{"status_code":"FINISHED"}`))
		case p == "/v26.0/ig1/media_publish":
			igSteps = append(igSteps, "publish:"+r.FormValue("creation_id"))
			w.Write([]byte(`{"id":"igm1"}`))
		case p == "/v26.0/igm1" && r.URL.Query().Get("fields") == "permalink":
			w.Write([]byte(`{"permalink":"https://instagram.com/p/x"}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, p)
			w.WriteHeader(404)
		}
	}))
	defer meta.Close()
	ctx := context.Background()

	fb, err := e.svc.ConnectAccount(ctx, "facebook", "", map[string]string{"page_id": "123", "page_access_token": "tok", "base_url": meta.URL})
	if err != nil {
		t.Fatal(err)
	}
	ps, _ := e.svc.Schedule(ctx, core.ScheduleInput{AccountIDs: []string{fb.ID}, Text: "hello fb"})
	p, err := e.svc.PublishNow(ctx, ps[0].ID)
	if err != nil || p.Status != "published" || p.RemoteURL != "https://facebook.com/p/9" {
		t.Fatalf("fb publish: %v %+v", err, p)
	}
	cs, err := e.svc.RefreshComments(ctx, p.ID)
	if err != nil || len(cs) != 1 || cs[0].Author != "Ann" {
		t.Fatalf("fb comments: %v %+v", err, cs)
	}

	ig, err := e.svc.ConnectAccount(ctx, "instagram", "", map[string]string{"access_token": "tok", "base_url": meta.URL, "token_refreshed_at": time.Now().Format(time.RFC3339)})
	if err != nil {
		t.Fatal(err)
	}
	m, err := e.svc.SaveMedia(ctx, strings.NewReader("img"), "a.jpg", "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	// Instagram refuses text-only posts
	// Instagram refuses text-only posts: caught by the pre-flight check, and at publish time with force
	if _, err := e.svc.Schedule(ctx, core.ScheduleInput{AccountIDs: []string{ig.ID}, Text: "no media"}); err == nil || !strings.Contains(err.Error(), "pre-flight") {
		t.Fatalf("ig text-only should be rejected: %v", err)
	}
	ps, _ = e.svc.Schedule(ctx, core.ScheduleInput{AccountIDs: []string{ig.ID}, Text: "no media", Force: true})
	if p, _ := e.svc.PublishNow(ctx, ps[0].ID); p.Status != "failed" {
		t.Fatalf("ig text-only should fail: %+v", p)
	}
	ps, _ = e.svc.Schedule(ctx, core.ScheduleInput{AccountIDs: []string{ig.ID}, Text: "caption", Media: []string{m.ID}})
	p, err = e.svc.PublishNow(ctx, ps[0].ID)
	if err != nil || p.Status != "published" || p.RemoteURL != "https://instagram.com/p/x" {
		t.Fatalf("ig publish: %v %+v", err, p)
	}
	want := "container:https://relay.example/media/" + m.ID + ".jpg"
	if len(igSteps) != 2 || igSteps[0] != want || igSteps[1] != "publish:cont1" {
		t.Fatalf("ig steps: %v", igSteps)
	}
}

func TestThreadsProvider(t *testing.T) {
	e := newEnv(t)
	var mu sync.Mutex
	var steps []string
	n := 0
	th := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.FormValue("access_token") == "" {
			t.Errorf("no token on %s", r.URL.Path)
		}
		mu.Lock()
		defer mu.Unlock()
		p := r.URL.Path
		switch {
		case p == "/v1.0/me":
			w.Write([]byte(`{"id":"th1","username":"rmp","name":"RideMyPark"}`))
		case p == "/v1.0/th1/threads" && r.Method == "POST":
			n++
			steps = append(steps, fmt.Sprintf("create:%s:%s:%s:%s", r.FormValue("media_type"), r.FormValue("image_url"), r.FormValue("reply_to_id"), r.FormValue("text")))
			fmt.Fprintf(w, `{"id":"c%d"}`, n)
		case strings.HasPrefix(p, "/v1.0/c"):
			w.Write([]byte(`{"status":"FINISHED"}`))
		case p == "/v1.0/th1/threads_publish":
			id := "p" + strings.TrimPrefix(r.FormValue("creation_id"), "c")
			steps = append(steps, "publish:"+id)
			fmt.Fprintf(w, `{"id":"%s"}`, id)
		case p == "/v1.0/p1" && r.URL.Query().Get("fields") == "permalink":
			w.Write([]byte(`{"permalink":"https://www.threads.net/@rmp/post/one"}`))
		case p == "/v1.0/p3" && r.URL.Query().Get("fields") == "permalink":
			w.Write([]byte(`{"permalink":"https://www.threads.net/@rmp/post/three"}`))
		case p == "/v1.0/p1/conversation":
			w.Write([]byte(`{"data":[{"id":"r1","text":"top","username":"ann"},{"id":"p2","text":"part two","username":"rmp","is_reply_owned_by_me":true}]}`))
		case p == "/v1.0/p1/insights":
			w.Write([]byte(`{"data":[{"name":"views","values":[{"value":100}]},{"name":"likes","values":[{"value":9}]},{"name":"reposts","values":[{"value":2}]}]}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, p)
			w.WriteHeader(404)
		}
	}))
	defer th.Close()
	ctx := context.Background()

	acc, err := e.svc.ConnectAccount(ctx, "threads", "", map[string]string{"access_token": "tok", "base_url": th.URL, "token_refreshed_at": time.Now().Format(time.RFC3339)})
	if err != nil || acc.Handle != "rmp" || acc.RemoteUserID != "th1" {
		t.Fatalf("connect: %v %+v", err, acc)
	}
	if _, err := e.svc.Schedule(ctx, core.ScheduleInput{AccountIDs: []string{acc.ID}, Text: strings.Repeat("a", 501)}); err == nil || !strings.Contains(err.Error(), "pre-flight") {
		t.Fatalf("501 characters should be rejected: %v", err)
	}
	m, err := e.svc.SaveMedia(ctx, strings.NewReader("img"), "a.jpg", "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	ps, err := e.svc.Schedule(ctx, core.ScheduleInput{AccountIDs: []string{acc.ID}, Thread: []string{"one", "two"}, Media: []string{m.ID}})
	if err != nil {
		t.Fatal(err)
	}
	p, err := e.svc.PublishNow(ctx, ps[0].ID)
	if err != nil || p.Status != "published" || p.RemoteURL != "https://www.threads.net/@rmp/post/one" || strings.Join(p.RemoteIDs, ",") != "p1,p2" {
		t.Fatalf("publish: %v %+v", err, p)
	}
	img := "https://relay.example/media/" + m.ID + ".jpg"
	want := []string{"create:IMAGE:" + img + "::one", "publish:p1", "create:TEXT::p1:two", "publish:p2"}
	if strings.Join(steps, "|") != strings.Join(want, "|") {
		t.Fatalf("steps:\n%v\nwant\n%v", steps, want)
	}
	cs, err := e.svc.RefreshComments(ctx, p.ID)
	if err != nil || len(cs) != 1 || cs[0].Author != "@ann" {
		t.Fatalf("comments: %v %+v", err, cs)
	}
	res, err := e.svc.ReplyToComment(ctx, acc.ID, "r1", "thanks")
	if err != nil || res.URL != "https://www.threads.net/@rmp/post/three" {
		t.Fatalf("reply: %v %+v", err, res)
	}
	if steps[len(steps)-2] != "create:TEXT::r1:thanks" {
		t.Fatalf("reply steps: %v", steps)
	}
	if n, err := e.svc.RefreshMetrics(ctx, acc.ID, time.Now().Add(-time.Hour)); err != nil || n != 1 {
		t.Fatalf("metrics: %d %v", n, err)
	}
	got, _ := e.svc.Store.GetPost(ctx, p.ID)
	if norm := core.Normalize(got.Metrics); norm["views"] != 100 || norm["likes"] != 9 || norm["shares"] != 2 {
		t.Fatalf("normalized %v", norm)
	}
}

func TestLinkedInPage(t *testing.T) {
	e := newEnv(t)
	li := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" || r.Header.Get("X-Restli-Protocol-Version") != "2.0.0" {
			t.Errorf("headers on %s: %v", r.URL.Path, r.Header)
		}
		b, _ := io.ReadAll(r.Body)
		switch p := r.URL.EscapedPath(); {
		case p == "/rest/organizations/42":
			w.Write([]byte(`{"id":42,"localizedName":"RideMyPark","vanityName":"ridemypark"}`))
		case p == "/rest/posts" && r.Method == "POST":
			if !strings.Contains(string(b), `"commentary":"hello li \\(page\\)"`) || !strings.Contains(string(b), `"author":"urn:li:organization:42"`) {
				t.Errorf("post body %s", b)
			}
			w.Header().Set("x-restli-id", "urn:li:share:7")
			w.WriteHeader(201)
		case p == "/v2/socialActions/urn%3Ali%3Ashare%3A7/comments" && r.Method == "POST":
			w.WriteHeader(201)
			w.Write([]byte(`{"commentUrn":"urn:li:comment:(urn:li:activity:8,1)"}`))
		case p == "/v2/socialActions/urn%3Ali%3Ashare%3A7/comments":
			w.Write([]byte(`{"elements":[{"commentUrn":"urn:li:comment:(urn:li:activity:8,2)","actor":"urn:li:person:x","message":{"text":"rad"},"created":{"time":1763700000000}},` +
				`{"commentUrn":"urn:li:comment:(urn:li:activity:8,1)","actor":"urn:li:organization:42","message":{"text":"part two"}}]}`))
		case p == "/rest/organizationalEntityShareStatistics":
			w.Write([]byte(`{"elements":[{"share":"urn:li:share:7","totalShareStatistics":{"impressionCount":120,"likeCount":9,"commentCount":1,"shareCount":2}}]}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, p)
			w.WriteHeader(404)
		}
	}))
	defer li.Close()
	ctx := context.Background()
	acc, err := e.svc.ConnectAccount(ctx, "linkedin-page", "", map[string]string{"organization_id": "42", "access_token": "tok", "base_url": li.URL})
	if err != nil {
		t.Fatal(err)
	}
	ps, err := e.svc.Schedule(ctx, core.ScheduleInput{AccountIDs: []string{acc.ID}, Thread: []string{"hello li (page)", "part two"}})
	if err != nil {
		t.Fatal(err)
	}
	p, err := e.svc.PublishNow(ctx, ps[0].ID)
	if err != nil || p.Status != "published" || p.RemoteURL != "https://www.linkedin.com/feed/update/urn:li:share:7" || len(p.RemoteIDs) != 2 {
		t.Fatalf("publish: %v %+v", err, p)
	}
	cs, err := e.svc.RefreshComments(ctx, p.ID)
	if err != nil || len(cs) != 1 || cs[0].Author != "urn:li:person:x" {
		t.Fatalf("comments: %v %+v", err, cs)
	}
	if n, err := e.svc.RefreshMetrics(ctx, acc.ID, time.Now().Add(-time.Hour)); err != nil || n != 1 {
		t.Fatalf("metrics: %d %v", n, err)
	}
}
