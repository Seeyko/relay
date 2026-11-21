package api_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/Seeyko/relay/internal/core"
	"github.com/Seeyko/relay/internal/store"
)

// xThreadServer answers like X: an upload returns "m-<file content>", tweets are t1, t2... The tweet
// numbered failAt fails once with a 403.
type xThreadServer struct {
	mu     sync.Mutex
	n      int
	failAt int
	tweets []map[string]any // successful POST /2/tweets bodies, in order
}

func (x *xThreadServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	x.mu.Lock()
	defer x.mu.Unlock()
	switch {
	case r.URL.Path == "/2/users/me":
		w.Write([]byte(`{"data":{"id":"42","username":"alice"}}`))
	case r.URL.Path == "/2/media/upload":
		f, _, err := r.FormFile("media")
		if err != nil {
			w.WriteHeader(400)
			return
		}
		b, _ := io.ReadAll(f)
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "m-" + string(b)}})
	case r.URL.Path == "/2/tweets" && r.Method == "POST":
		x.n++
		if x.n == x.failAt {
			x.failAt = 0
			x.n--
			w.WriteHeader(403)
			w.Write([]byte(`{"title":"Forbidden"}`))
			return
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		x.tweets = append(x.tweets, body)
		w.Write([]byte(`{"data":{"id":"t` + string(rune('0'+x.n)) + `"}}`))
	default:
		w.WriteHeader(404)
	}
}

func tweetMedia(b map[string]any) []string {
	var out []string
	if m, ok := b["media"].(map[string]any); ok {
		for _, id := range m["media_ids"].([]any) {
			out = append(out, id.(string))
		}
	}
	return out
}

func saveMedia(t *testing.T, e *env, content, name, mime string) *store.Media {
	t.Helper()
	m, err := e.svc.SaveMedia(context.Background(), strings.NewReader(content), name, mime)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// A thread scheduled over REST with media on several parts, whose third part fails: the resume sends
// the missing parts with their own media, chained to the last part published.
func TestThreadMediaPerPartAndResume(t *testing.T) {
	e := newEnv(t)
	xs := &xThreadServer{failAt: 3}
	acc := connectFakeX(t, e, xs)
	m1 := saveMedia(t, e, "one", "1.jpg", "image/jpeg")
	m2 := saveMedia(t, e, "two", "2.jpg", "image/jpeg")
	m3 := saveMedia(t, e, "three", "3.png", "image/png")
	m4 := saveMedia(t, e, "four", "4.png", "image/png")

	code, _, raw := e.req("POST", "/v1/posts", map[string]any{"account_ids": []string{acc.ID}, "scheduled_at": "draft", "media": []string{m1.ID},
		"thread": []any{"first", map[string]any{"text": "second", "media": []string{m2.ID}}, map[string]any{"text": "third", "media": []string{m3.ID, m4.ID}}, "fourth"}})
	if code != 200 {
		t.Fatalf("schedule: %d %s", code, raw)
	}
	var posts []store.Post
	if err := json.Unmarshal(raw, &posts); err != nil || len(posts) != 1 {
		t.Fatalf("%s", raw)
	}
	p := posts[0]
	want := [][]string{{m1.ID}, {m2.ID}, {m3.ID, m4.ID}}
	if !reflect.DeepEqual(p.Parts, []string{"first", "second", "third", "fourth"}) || !reflect.DeepEqual(p.Media, []string{m1.ID}) || !reflect.DeepEqual(p.PartMedia, want) {
		t.Fatalf("stored post %+v", p)
	}
	if !strings.Contains(string(raw), `"part_media":[["`+m1.ID+`"],["`+m2.ID+`"],["`+m3.ID+`","`+m4.ID+`"]]`) {
		t.Fatalf("part_media JSON: %s", raw)
	}

	ctx := context.Background()
	got, err := e.svc.PublishNow(ctx, p.ID)
	if err != nil || got.Status != "failed" || !reflect.DeepEqual(got.RemoteIDs, []string{"t1", "t2"}) {
		t.Fatalf("partial: %v %+v", err, got)
	}
	got, err = e.svc.PublishNow(ctx, p.ID)
	if err != nil || got.Status != "published" || !reflect.DeepEqual(got.RemoteIDs, []string{"t1", "t2", "t3", "t4"}) {
		t.Fatalf("resume: %v %+v", err, got)
	}
	if len(xs.tweets) != 4 {
		t.Fatalf("tweets %v", xs.tweets)
	}
	wantMedia := [][]string{{"m-one"}, {"m-two"}, {"m-three", "m-four"}, nil}
	wantReply := []string{"", "t1", "t2", "t3"}
	for i, tw := range xs.tweets {
		if got := tweetMedia(tw); !reflect.DeepEqual(got, wantMedia[i]) {
			t.Errorf("tweet %d media %v, want %v", i+1, got, wantMedia[i])
		}
		reply, _ := tw["reply"].(map[string]any)
		if r, _ := reply["in_reply_to_tweet_id"].(string); r != wantReply[i] {
			t.Errorf("tweet %d replies to %q, want %q", i+1, r, wantReply[i])
		}
	}
}

func TestThreadInputValidation(t *testing.T) {
	e := newEnv(t)
	acc := connectFakeX(t, e, &xThreadServer{})
	m := saveMedia(t, e, "a", "a.jpg", "image/jpeg")
	cases := []struct {
		body map[string]any
		want string
	}{
		{map[string]any{"media": []string{m.ID}, "thread": []any{map[string]any{"text": "a", "media": []string{m.ID}}}}, "media is given twice for part 1"},
		{map[string]any{"thread": []any{"a", map[string]any{"text": "b", "media": []string{"med_nope"}}}}, `unknown media "med_nope" (thread part 2)`},
		{map[string]any{"thread": []any{"a", 42}}, "a thread part is a string or"},
		{map[string]any{"thread": []any{"a", map[string]any{"txt": "typo"}}}, "a thread part is a string or"},
	}
	for _, c := range cases {
		c.body["account_ids"] = []string{acc.ID}
		code, body, raw := e.req("POST", "/v1/posts", c.body)
		if code != 400 || !strings.Contains(body["error"].(string), c.want) {
			t.Errorf("want 400 %q, got %d %s", c.want, code, raw)
		}
	}
	// thread[0].media alone is the media of part 1.
	code, _, raw := e.req("POST", "/v1/posts", map[string]any{"account_ids": []string{acc.ID}, "scheduled_at": "draft",
		"thread": []any{map[string]any{"text": "a", "media": []string{m.ID}}, "b"}})
	var ps []store.Post
	json.Unmarshal(raw, &ps)
	if code != 200 || len(ps) != 1 || !reflect.DeepEqual(ps[0].Media, []string{m.ID}) || ps[0].PartMedia != nil {
		t.Fatalf("thread[0].media: %d %s", code, raw)
	}
	// X validates every part: five images on part 2.
	five := []string{m.ID, m.ID, m.ID, m.ID, m.ID}
	code, _, raw = e.req("POST", "/v1/posts/validate", map[string]any{"account_ids": []string{acc.ID}, "thread": []any{"a", map[string]any{"text": "b", "media": five}}})
	if code != 200 || !strings.Contains(string(raw), "part 2: X allows at most 4 media per post") {
		t.Fatalf("validate: %d %s", code, raw)
	}
}

// Facebook: parts 2..n become comments. Media on them is refused before publishing, and at publish
// time when the pre-flight check was skipped (force), without retries. A failed comment resumes on the post.
func TestFacebookFirstCommentsThroughTheScheduler(t *testing.T) {
	e := newEnv(t)
	var mu sync.Mutex
	var comments []string
	var targets []string
	failOnce := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		r.ParseForm()
		switch {
		case r.URL.Path == "/v26.0/123" && r.Method == "GET":
			w.Write([]byte(`{"id":"123","name":"RMP"}`))
		case r.URL.Path == "/v26.0/123/feed":
			if r.FormValue("message") != "post" {
				t.Errorf("message %q", r.FormValue("message"))
			}
			w.Write([]byte(`{"id":"123_9"}`))
		case strings.HasSuffix(r.URL.Path, "/comments") && r.Method == "POST":
			if r.FormValue("message") == "c2" && failOnce {
				failOnce = false
				w.WriteHeader(500)
				return
			}
			targets = append(targets, r.URL.Path)
			comments = append(comments, r.FormValue("message"))
			w.Write([]byte(`{"id":"9_` + r.FormValue("message") + `"}`))
		case r.Method == "GET":
			w.Write([]byte(`{}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	fb, err := e.svc.ConnectAccount(ctx, "facebook", "", map[string]string{"page_id": "123", "page_access_token": "tok", "base_url": srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	m := saveMedia(t, e, "a", "a.jpg", "image/jpeg")
	withMedia := core.Thread{{Text: "post"}, {Text: "c1", Media: []string{m.ID}}}
	if _, err := e.svc.Schedule(ctx, core.ScheduleInput{AccountIDs: []string{fb.ID}, Thread: withMedia}); err == nil || !strings.Contains(err.Error(), "part 2 has media, but facebook only takes media on the first part") {
		t.Fatalf("pre-flight: %v", err)
	}
	ps, err := e.svc.Schedule(ctx, core.ScheduleInput{AccountIDs: []string{fb.ID}, Thread: withMedia, Force: true})
	if err != nil {
		t.Fatal(err)
	}
	p, _ := e.svc.PublishNow(ctx, ps[0].ID)
	if p.Status != "failed" || p.Attempts != 1 || !strings.Contains(p.Error, "only the first part can carry media") || len(comments) != 0 {
		t.Fatalf("forced media on a comment: %+v", p)
	}

	ps, err = e.svc.Schedule(ctx, core.ScheduleInput{AccountIDs: []string{fb.ID}, Thread: core.TextThread("post", "c1", "c2", "c3")})
	if err != nil {
		t.Fatal(err)
	}
	p, _ = e.svc.PublishNow(ctx, ps[0].ID)
	if p.Status != "failed" || !reflect.DeepEqual(p.RemoteIDs, []string{"123_9", "9_c1"}) || p.RemoteID != "123_9" {
		t.Fatalf("partial: %+v", p)
	}
	p, _ = e.svc.PublishNow(ctx, ps[0].ID)
	if p.Status != "published" || !reflect.DeepEqual(p.RemoteIDs, []string{"123_9", "9_c1", "9_c2", "9_c3"}) {
		t.Fatalf("resume: %+v", p)
	}
	if !reflect.DeepEqual(comments, []string{"c1", "c2", "c3"}) {
		t.Fatalf("comments %v", comments)
	}
	for _, tg := range targets {
		if tg != "/v26.0/123_9/comments" {
			t.Fatalf("every comment goes on the post, got %v", targets)
		}
	}

	// The preview shows the parts as comments.
	pv := e.svc.PreviewPost(ctx, p, fb)
	if pv.Layout != "post_comments" || len(pv.Texts) != 4 || pv.Texts[0].Label != "caption" || pv.Texts[1].Label != "comment" || pv.Texts[1].Limit != 8000 {
		t.Fatalf("preview %+v", pv)
	}
}

func TestUpdatePostThreadObjects(t *testing.T) {
	e := newEnv(t)
	acc := connectFakeX(t, e, &xThreadServer{})
	m1 := saveMedia(t, e, "1", "1.jpg", "image/jpeg")
	m2 := saveMedia(t, e, "2", "2.jpg", "image/jpeg")
	ctx := context.Background()
	ps, err := e.svc.Schedule(ctx, core.ScheduleInput{AccountIDs: []string{acc.ID}, Text: "old", Media: []string{m1.ID}, ScheduledAt: "draft"})
	if err != nil {
		t.Fatal(err)
	}
	id := ps[0].ID
	code, _, raw := e.req("PATCH", "/v1/posts/"+id, map[string]any{"thread": []any{"a", map[string]any{"text": "b", "media": []string{m2.ID}}}})
	var p store.Post
	json.Unmarshal(raw, &p)
	// Part 1 keeps its media (thread[0] gave none), part 2 gets m2.
	if code != 200 || !reflect.DeepEqual(p.PartMedia, [][]string{{m1.ID}, {m2.ID}}) || !reflect.DeepEqual(p.Media, []string{m1.ID}) {
		t.Fatalf("update thread: %d %s", code, raw)
	}
	if used, _ := e.svc.Store.MediaInUse(ctx, m2.ID); !used {
		t.Fatal("m2 is used by part 2")
	}
	// The preview gives each part its media.
	acct, _ := e.svc.Store.GetAccount(ctx, acc.ID)
	full, _ := e.svc.Store.GetPost(ctx, id)
	pv := e.svc.PreviewPost(ctx, full, acct)
	if len(pv.PartMedia) != 2 || pv.PartMedia[1][0].ID != m2.ID || len(pv.Texts[1].Media) != 1 || pv.Texts[1].Media[0].ID != m2.ID || len(pv.Media) != 1 {
		t.Fatalf("preview %+v", pv)
	}
	if code, body, _ := e.req("PATCH", "/v1/posts/"+id, map[string]any{"media": []string{m2.ID}, "thread": []any{map[string]any{"text": "a", "media": []string{m1.ID}}}}); code != 400 {
		t.Fatalf("media twice: %d %v", code, body)
	}
	// Back to a single text: no part media left.
	code, _, raw = e.req("PATCH", "/v1/posts/"+id, map[string]any{"text": "single"})
	p = store.Post{}
	json.Unmarshal(raw, &p)
	if code != 200 || p.PartMedia != nil || !reflect.DeepEqual(p.Media, []string{m1.ID}) || strings.Contains(string(raw), "part_media") {
		t.Fatalf("update text: %d %s", code, raw)
	}
}

func TestPreviewLinkedInDocument(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	m1 := saveMedia(t, e, "1", "1.jpg", "image/jpeg")
	m2 := saveMedia(t, e, "2", "2.jpg", "image/jpeg")
	acc := &store.Account{ID: "acc_li", Provider: "linkedin-page", Name: "RMP"}
	p := &store.Post{ID: "post_li", AccountID: acc.ID, Status: "draft", Parts: []string{"Top 10 skateparks\nla suite"}, Media: []string{m1.ID, m2.ID},
		Options: map[string]any{"images_as_document": true}}
	pv := e.svc.PreviewPost(ctx, p, acc)
	if pv.Document == nil || pv.Document.Title != "Top 10 skateparks" || pv.Document.Pages != 2 || len(pv.Problems) != 0 {
		t.Fatalf("preview %+v %+v", pv.Document, pv.Problems)
	}
	p.Options["document_title"] = "Guide"
	if pv := e.svc.PreviewPost(ctx, p, acc); pv.Document.Title != "Guide" {
		t.Fatalf("title %+v", pv.Document)
	}
	delete(p.Options, "images_as_document")
	if pv := e.svc.PreviewPost(ctx, p, acc); pv.Document != nil {
		t.Fatalf("not a document: %+v", pv.Document)
	}
}
