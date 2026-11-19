package meta

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Seeyko/relay/internal/providers"
)

// fakeIG records every request and answers from a route table keyed by "METHOD /path".
type fakeIG struct {
	t      *testing.T
	mu     sync.Mutex
	calls  []string
	forms  map[string][]url.Values
	routes map[string]func(q url.Values) string
	srv    *httptest.Server
}

func newFakeIG(t *testing.T, routes map[string]func(q url.Values) string) *fakeIG {
	f := &fakeIG{t: t, routes: routes, forms: map[string][]url.Values{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		key := r.Method + " " + r.URL.Path
		f.mu.Lock()
		f.calls = append(f.calls, key)
		f.forms[key] = append(f.forms[key], r.Form)
		f.mu.Unlock()
		if r.Form.Get("access_token") == "" {
			t.Errorf("%s: missing access_token", key)
		}
		h, ok := routes[key]
		if !ok {
			t.Errorf("unexpected request %s?%s", key, r.URL.RawQuery)
			w.WriteHeader(404)
			return
		}
		body := h(r.Form)
		if strings.HasPrefix(body, "400 ") {
			w.WriteHeader(400)
			body = body[4:]
		}
		io.WriteString(w, body)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeIG) form(key string, i int) url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.forms[key]) <= i {
		f.t.Fatalf("no call #%d to %s (calls: %v)", i, key, f.calls)
	}
	return f.forms[key][i]
}

func (f *fakeIG) count(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.forms[key])
}

func static(s string) func(url.Values) string { return func(url.Values) string { return s } }

func newTestIG(t *testing.T, f *fakeIG, creds map[string]string, persist func(map[string]string) error) *Instagram {
	t.Helper()
	c := map[string]string{"access_token": "tok", "base_url": f.srv.URL, "user_id": "ig1", "username": "me.ig",
		"token_refreshed_at": time.Now().UTC().Format(time.RFC3339)}
	for k, v := range creds {
		c[k] = v
	}
	fac, err := providers.Get("instagram")
	if err != nil {
		t.Fatal(err)
	}
	cl, err := fac.New(c, persist, f.srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	ig := cl.(*Instagram)
	ig.PollInterval = time.Millisecond
	return ig
}

func jpeg(name string) providers.MediaFile {
	return providers.MediaFile{Filename: name, Mime: "image/jpeg", PublicURL: "https://relay.example/media/" + name}
}

func expect(t *testing.T, got url.Values, want map[string]string) {
	t.Helper()
	for k, v := range want {
		if got.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, got.Get(k), v)
		}
	}
}

func TestInstagramPublishImage(t *testing.T) {
	f := newFakeIG(t, map[string]func(url.Values) string{
		"POST /v26.0/ig1/media":         static(`{"id":"c1"}`),
		"GET /v26.0/c1":                 static(`{"status_code":"FINISHED"}`),
		"POST /v26.0/ig1/media_publish": static(`{"id":"m1"}`),
		"GET /v26.0/m1":                 static(`{"permalink":"https://www.instagram.com/p/abc/"}`),
	})
	ig := newTestIG(t, f, nil, nil)
	img := jpeg("a.jpg")
	img.Alt = "a cat"
	res, err := ig.Publish(context.Background(), providers.PublishRequest{Parts: []string{"hello", "world"}, Media: []providers.MediaFile{img},
		Options: map[string]any{"location_id": "123", "collaborators": []any{"bob"}}})
	if err != nil {
		t.Fatal(err)
	}
	if res.RemoteID != "m1" || res.URL != "https://www.instagram.com/p/abc/" {
		t.Fatalf("result %+v", res)
	}
	c := f.form("POST /v26.0/ig1/media", 0)
	expect(t, c, map[string]string{"image_url": "https://relay.example/media/a.jpg", "caption": "hello\n\nworld", "alt_text": "a cat",
		"location_id": "123", "collaborators": `["bob"]`, "media_type": ""})
	expect(t, f.form("GET /v26.0/c1", 0), map[string]string{"fields": "status_code,status"})
	expect(t, f.form("POST /v26.0/ig1/media_publish", 0), map[string]string{"creation_id": "c1"})
	expect(t, f.form("GET /v26.0/m1", 0), map[string]string{"fields": "permalink"})
}

func TestInstagramPublishCarousel(t *testing.T) {
	n := 0
	f := newFakeIG(t, map[string]func(url.Values) string{
		"POST /v26.0/ig1/media": func(url.Values) string {
			n++
			return `{"id":"c` + string(rune('0'+n)) + `"}`
		},
		"GET /v26.0/c1":                 static(`{"status_code":"FINISHED"}`),
		"GET /v26.0/c2":                 static(`{"status_code":"FINISHED"}`),
		"GET /v26.0/c3":                 static(`{"status_code":"FINISHED"}`),
		"POST /v26.0/ig1/media_publish": static(`{"id":"m1"}`),
		"GET /v26.0/m1":                 static(`{"permalink":"https://www.instagram.com/p/car/"}`),
	})
	ig := newTestIG(t, f, nil, nil)
	vid := providers.MediaFile{Filename: "b.mp4", Mime: "video/mp4", PublicURL: "https://relay.example/media/b.mp4"}
	if _, err := ig.Publish(context.Background(), providers.PublishRequest{Parts: []string{"album"}, Media: []providers.MediaFile{jpeg("a.jpg"), vid}}); err != nil {
		t.Fatal(err)
	}
	k := "POST /v26.0/ig1/media"
	expect(t, f.form(k, 0), map[string]string{"image_url": "https://relay.example/media/a.jpg", "is_carousel_item": "true", "caption": "", "media_type": ""})
	expect(t, f.form(k, 1), map[string]string{"video_url": "https://relay.example/media/b.mp4", "media_type": "VIDEO", "is_carousel_item": "true", "caption": ""})
	expect(t, f.form(k, 2), map[string]string{"media_type": "CAROUSEL", "children": "c1,c2", "caption": "album"})
	expect(t, f.form("POST /v26.0/ig1/media_publish", 0), map[string]string{"creation_id": "c3"})
}

func TestInstagramPublishReelPolling(t *testing.T) {
	polls := 0
	publishes := 0
	f := newFakeIG(t, map[string]func(url.Values) string{
		"POST /v26.0/ig1/media": static(`{"id":"c1"}`),
		"GET /v26.0/c1": func(url.Values) string {
			polls++
			if polls < 3 {
				return `{"status_code":"IN_PROGRESS","status":"In Progress"}`
			}
			return `{"status_code":"FINISHED"}`
		},
		"POST /v26.0/ig1/media_publish": func(url.Values) string {
			publishes++
			if publishes == 1 {
				return `400 {"error":{"message":"Media ID is not available","code":9007,"error_subcode":2207027}}`
			}
			return `{"id":"m1"}`
		},
		"GET /v26.0/m1": static(`{"permalink":"https://www.instagram.com/reel/r/"}`),
	})
	ig := newTestIG(t, f, nil, nil)
	vid := providers.MediaFile{Filename: "r.mp4", Mime: "video/mp4", PublicURL: "https://relay.example/media/r.mp4"}
	res, err := ig.Publish(context.Background(), providers.PublishRequest{Parts: []string{"reel"}, Media: []providers.MediaFile{vid},
		Options: map[string]any{"share_to_feed": true, "thumb_offset": float64(1500), "media_type": "VIDEO"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.URL != "https://www.instagram.com/reel/r/" || polls != 3 || publishes != 2 {
		t.Fatalf("res=%+v polls=%d publishes=%d", res, polls, publishes)
	}
	expect(t, f.form("POST /v26.0/ig1/media", 0), map[string]string{"media_type": "REELS", "video_url": "https://relay.example/media/r.mp4",
		"caption": "reel", "share_to_feed": "true", "thumb_offset": "1500"})
}

func TestInstagramPublishStoryAndErrors(t *testing.T) {
	status := `{"status_code":"FINISHED"}`
	f := newFakeIG(t, map[string]func(url.Values) string{
		"POST /v26.0/ig1/media":         static(`{"id":"c1"}`),
		"GET /v26.0/c1":                 func(url.Values) string { return status },
		"POST /v26.0/ig1/media_publish": static(`400 {"error":{"message":"limit","code":9,"error_subcode":2207042}}`),
	})
	ig := newTestIG(t, f, nil, nil)
	img := jpeg("s.jpg")
	img.Alt = "ignored"
	_, err := ig.Publish(context.Background(), providers.PublishRequest{Parts: []string{"no caption"}, Media: []providers.MediaFile{img}, Options: map[string]any{"media_type": "STORIES"}})
	if err == nil || !providers.Retryable(err) {
		t.Fatalf("posting limit should be retryable: %v", err)
	}
	expect(t, f.form("POST /v26.0/ig1/media", 0), map[string]string{"media_type": "STORIES", "image_url": "https://relay.example/media/s.jpg", "caption": "", "alt_text": ""})

	status = `{"status_code":"EXPIRED"}`
	if _, err := ig.Publish(context.Background(), providers.PublishRequest{Media: []providers.MediaFile{jpeg("a.jpg")}}); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired: %v", err)
	}
	if _, err := ig.Publish(context.Background(), providers.PublishRequest{Media: []providers.MediaFile{{Filename: "x.jpg", Mime: "image/jpeg"}}}); !errors.Is(err, providers.ErrInvalidPost) {
		t.Fatalf("missing public URL must be invalid: %v", err)
	}
}

func TestInstagramMetrics(t *testing.T) {
	f := newFakeIG(t, map[string]func(url.Values) string{
		"GET /v26.0/m1":          static(`{"like_count":10,"comments_count":2,"media_product_type":"REELS"}`),
		"GET /v26.0/m1/insights": static(`{"data":[{"name":"views","period":"lifetime","values":[{"value":500}]},{"name":"reach","total_value":{"value":300}},{"name":"saved","values":[{"value":4}]},{"name":"shares","values":[{"value":3}]}]}`),
		"GET /v26.0/m2":          static(`{"like_count":1,"comments_count":0,"media_product_type":"STORY"}`),
		"GET /v26.0/m2/insights": func(q url.Values) string {
			if q.Get("metric") != "views,reach" {
				return `400 {"error":{"message":"(#10) Not enough viewers","code":10}}`
			}
			return `{"data":[{"name":"views","values":[{"value":7}]}]}`
		},
	})
	ig := newTestIG(t, f, nil, nil)
	got, err := ig.Metrics(context.Background(), []string{"m1", "m2"})
	if err != nil {
		t.Fatal(err)
	}
	m1 := got["m1"]
	if m1["like_count"] != 10 || m1["reply_count"] != 2 || m1["views"] != 500 || m1["reach"] != 300 || m1["saved"] != 4 || m1["shares"] != 3 {
		t.Fatalf("m1 %v", m1)
	}
	if got["m2"]["views"] != 7 || got["m2"]["like_count"] != 1 {
		t.Fatalf("m2 %v", got["m2"])
	}
	expect(t, f.form("GET /v26.0/m1", 0), map[string]string{"fields": "like_count,comments_count,media_product_type"})
	expect(t, f.form("GET /v26.0/m1/insights", 0), map[string]string{"metric": insightMetrics("REELS")})
	expect(t, f.form("GET /v26.0/m2/insights", 0), map[string]string{"metric": insightMetrics("STORY")})
	for _, bad := range []string{"impressions", "plays", "video_views"} {
		for _, p := range []string{"FEED", "REELS", "STORY"} {
			if strings.Contains(insightMetrics(p), bad) {
				t.Errorf("deprecated metric %s requested for %s", bad, p)
			}
		}
	}
}

func TestInstagramCommentsAndReply(t *testing.T) {
	f := newFakeIG(t, map[string]func(url.Values) string{
		"GET /v26.0/m1/comments": func(q url.Values) string {
			if q.Get("after") == "" {
				return `{"data":[{"id":"c1","text":"nice","username":"ann","timestamp":"2026-11-19T10:00:00+0000",
					"replies":{"data":[{"id":"r1","text":"thanks","username":"me.ig","timestamp":"2026-11-19T10:05:00+0000"},{"id":"r2","text":"+1","username":"bob","timestamp":"2026-11-19T10:06:00+0000"}]}}],
					"paging":{"cursors":{"after":"CUR"},"next":"https://graph.instagram.com/next"}}`
			}
			return `{"data":[{"id":"c2","text":"older","username":"cy","timestamp":"2026-11-18T10:00:00+0000"}],"paging":{"cursors":{"after":"X"}}}`
		},
		"POST /v26.0/c1/replies": static(`{"id":"r9"}`),
	})
	ig := newTestIG(t, f, nil, nil)
	cs, err := ig.Comments(context.Background(), "m1")
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 3 || cs[0].RemoteID != "c1" || cs[0].Author != "@ann" || cs[1].RemoteID != "r2" || cs[2].RemoteID != "c2" {
		t.Fatalf("comments %+v", cs)
	}
	if cs[0].CreatedAt == nil || !cs[0].CreatedAt.Equal(time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("timestamp %v", cs[0].CreatedAt)
	}
	expect(t, f.form("GET /v26.0/m1/comments", 0), map[string]string{"fields": "id,text,username,timestamp,replies{id,text,username,timestamp}", "limit": "50", "after": ""})
	expect(t, f.form("GET /v26.0/m1/comments", 1), map[string]string{"after": "CUR"})

	res, err := ig.Reply(context.Background(), "c1", "merci")
	if err != nil || res.RemoteID != "r9" {
		t.Fatalf("reply %v %+v", err, res)
	}
	expect(t, f.form("POST /v26.0/c1/replies", 0), map[string]string{"message": "merci"})
}

func TestInstagramHistory(t *testing.T) {
	f := newFakeIG(t, map[string]func(url.Values) string{
		"GET /v26.0/ig1/media": func(q url.Values) string {
			if q.Get("after") == "" {
				return `{"data":[
					{"id":"1","caption":"pic","media_type":"IMAGE","media_product_type":"FEED","permalink":"https://www.instagram.com/p/1/","timestamp":"2026-11-01T08:00:00+0000","like_count":5,"comments_count":1},
					{"id":"2","media_type":"VIDEO","media_product_type":"REELS","timestamp":"2026-11-02T08:00:00+0000"}],
					"paging":{"cursors":{"after":"A"},"next":"https://graph.instagram.com/n"}}`
			}
			return `{"data":[{"id":"3","media_type":"CAROUSEL_ALBUM","media_product_type":"FEED","timestamp":"2026-11-03T08:00:00+0000"},
				{"id":"4","media_type":"VIDEO","media_product_type":"FEED","timestamp":"2026-11-03T09:00:00+0000"}]}`
		},
	})
	ig := newTestIG(t, f, nil, nil)
	since := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	items, err := ig.History(context.Background(), since, 0)
	if err != nil {
		t.Fatal(err)
	}
	kinds := []string{}
	for _, it := range items {
		kinds = append(kinds, it.Kind)
	}
	if strings.Join(kinds, ",") != "image,reel,carousel,video" {
		t.Fatalf("kinds %v", kinds)
	}
	if items[0].Text != "pic" || items[0].URL != "https://www.instagram.com/p/1/" || items[0].Metrics["like_count"] != 5 || items[0].Metrics["reply_count"] != 1 ||
		!items[0].CreatedAt.Equal(time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)) {
		t.Fatalf("item %+v", items[0])
	}
	expect(t, f.form("GET /v26.0/ig1/media", 0), map[string]string{
		"fields": "id,caption,media_type,permalink,timestamp,like_count,comments_count,media_product_type", "limit": "100", "since": "1790812800"})
	expect(t, f.form("GET /v26.0/ig1/media", 1), map[string]string{"after": "A"})
	if igKind("IMAGE", "STORY") != "story" {
		t.Fatal("story kind")
	}
}

func TestInstagramHistoryFieldFallback(t *testing.T) {
	f := newFakeIG(t, map[string]func(url.Values) string{
		"GET /v26.0/ig1/media": func(q url.Values) string {
			if strings.Contains(q.Get("fields"), "media_product_type") {
				return `400 {"error":{"message":"(#100) Tried accessing nonexisting field (media_product_type)","code":100}}`
			}
			return `{"data":[{"id":"1","media_type":"CAROUSEL_ALBUM","timestamp":"2026-11-03T08:00:00+0000"}]}`
		},
	})
	items, err := newTestIG(t, f, nil, nil).History(context.Background(), time.Time{}, 10)
	if err != nil || len(items) != 1 || items[0].Kind != "carousel" {
		t.Fatalf("%v %+v", err, items)
	}
}

func TestInstagramIdentifyAndAccountMetrics(t *testing.T) {
	f := newFakeIG(t, map[string]func(url.Values) string{
		"GET /v26.0/me": func(q url.Values) string {
			if q.Get("fields") == "followers_count,follows_count,media_count" {
				return `{"followers_count":1200,"follows_count":80,"media_count":42}`
			}
			return `{"id":"app1","user_id":"ig9","username":"alice","name":"Alice"}`
		},
	})
	ig := newTestIG(t, f, map[string]string{"user_id": "", "username": ""}, nil)
	id, err := ig.Identify(context.Background())
	if err != nil || id.RemoteUserID != "ig9" || id.Handle != "alice" || id.Name != "Alice" {
		t.Fatalf("identify %v %+v", err, id)
	}
	expect(t, f.form("GET /v26.0/me", 0), map[string]string{"fields": "id,user_id,username,name"})
	m, err := ig.AccountMetrics(context.Background())
	if err != nil || m["followers"] != 1200 || m["following"] != 80 || m["posts"] != 42 {
		t.Fatalf("account %v %v", err, m)
	}
}

func TestInstagramTokenRefresh(t *testing.T) {
	f := newFakeIG(t, map[string]func(url.Values) string{
		"GET /refresh_access_token": func(q url.Values) string {
			if q.Get("grant_type") != "ig_refresh_token" || q.Get("access_token") != "old" {
				t.Errorf("refresh query %v", q)
			}
			return `{"access_token":"new","token_type":"bearer","expires_in":5184000}`
		},
		"GET /v26.0/me": func(q url.Values) string {
			if q.Get("access_token") != "new" {
				t.Errorf("token not swapped: %q", q.Get("access_token"))
			}
			return `{"followers_count":1}`
		},
	})
	var saved map[string]string
	old := time.Now().Add(-31 * 24 * time.Hour).UTC().Format(time.RFC3339)
	ig := newTestIG(t, f, map[string]string{"access_token": "old", "token_refreshed_at": old}, func(c map[string]string) error { saved = c; return nil })
	if _, err := ig.AccountMetrics(context.Background()); err != nil {
		t.Fatal(err)
	}
	if saved["access_token"] != "new" || saved["token_refreshed_at"] == old || saved["token_expires_at"] == "" {
		t.Fatalf("persisted %v", saved)
	}
	// A fresh token is not refreshed again.
	if _, err := ig.AccountMetrics(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := f.count("GET /refresh_access_token"); n != 1 {
		t.Fatalf("refresh calls %d", n)
	}
}

func TestInstagramTokenRefreshBackoff(t *testing.T) {
	f := newFakeIG(t, map[string]func(url.Values) string{
		"GET /refresh_access_token": static(`400 {"error":{"message":"token too new","code":190}}`),
		"GET /v26.0/me":             static(`{"followers_count":1}`),
	})
	var saved map[string]string
	ig := newTestIG(t, f, map[string]string{"token_refreshed_at": ""}, func(c map[string]string) error { saved = c; return nil })
	ig.AccountMetrics(context.Background())
	ig.AccountMetrics(context.Background())
	if n := f.count("GET /refresh_access_token"); n != 1 || saved["access_token"] != "tok" || saved["token_refreshed_at"] == "" {
		t.Fatalf("calls=%d saved=%v", n, saved)
	}
}

func TestInstagramValidate(t *testing.T) {
	fac, _ := providers.Get("instagram")
	ok := providers.PublishRequest{Parts: []string{"hi #a @b"}, Media: []providers.MediaFile{jpeg("a.jpg")}}
	if errs := fac.Validate(ok); errs != nil {
		t.Fatalf("valid post rejected: %v", errs)
	}
	many := make([]providers.MediaFile, 21)
	for i := range many {
		many[i] = jpeg("x.jpg")
	}
	cases := map[string]providers.PublishRequest{
		"at least one":    {Parts: []string{"text"}},
		"at most 20":      {Media: many},
		"2200":            {Parts: []string{strings.Repeat("a", 2201)}, Media: ok.Media},
		"30 hashtags":     {Parts: []string{strings.Repeat("#tag ", 31)}, Media: ok.Media},
		"20 @ mentions":   {Parts: []string{strings.Repeat("@user ", 21)}, Media: ok.Media},
		"JPEG":            {Media: []providers.MediaFile{{Filename: "a.png", Mime: "image/png"}}},
		"8 MB":            {Media: []providers.MediaFile{{Filename: "a.jpg", Mime: "image/jpeg", Size: 9 << 20}}},
		"MP4 or MOV":      {Media: []providers.MediaFile{{Filename: "a.webm", Mime: "video/webm"}}},
		"exactly one":     {Media: []providers.MediaFile{jpeg("a"), jpeg("b")}, Options: map[string]any{"media_type": "STORIES"}},
		"needs a video":   {Media: ok.Media, Options: map[string]any{"media_type": "REELS"}},
		"alt text":        {Media: []providers.MediaFile{{Filename: "a.jpg", Mime: "image/jpeg", Alt: strings.Repeat("a", 1001)}}},
		"media_type must": {Media: ok.Media, Options: map[string]any{"media_type": "GIF"}},
	}
	for want, req := range cases {
		errs := strings.Join(fac.Validate(req), "; ")
		if !strings.Contains(errs, want) {
			t.Errorf("%s: got %q", want, errs)
		}
	}
	// A bare "#" is not a hashtag.
	if errs := fac.Validate(providers.PublishRequest{Parts: []string{strings.Repeat("# ", 40)}, Media: ok.Media}); errs != nil {
		t.Errorf("bare # counted as hashtags: %v", errs)
	}
}
