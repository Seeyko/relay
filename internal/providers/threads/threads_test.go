package threads

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Seeyko/relay/internal/providers"
)

// fake records every request and answers from a route table keyed by "METHOD /path".
type fake struct {
	t      *testing.T
	mu     sync.Mutex
	calls  []string
	forms  map[string][]url.Values
	routes map[string]func(q url.Values) string
	srv    *httptest.Server
}

func newFake(t *testing.T, routes map[string]func(q url.Values) string) *fake {
	f := &fake{t: t, routes: routes, forms: map[string][]url.Values{}}
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

func (f *fake) form(key string, i int) url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.forms[key]) <= i {
		f.t.Fatalf("no call #%d to %s (calls: %v)", i, key, f.calls)
	}
	return f.forms[key][i]
}

func (f *fake) count(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.forms[key])
}

func static(s string) func(url.Values) string { return func(url.Values) string { return s } }

// seq answers with id prefix + call number: c1, c2...
func seq(prefix string) func(url.Values) string {
	n := 0
	var mu sync.Mutex
	return func(url.Values) string {
		mu.Lock()
		defer mu.Unlock()
		n++
		return fmt.Sprintf(`{"id":"%s%d"}`, prefix, n)
	}
}

func newTest(t *testing.T, f *fake, creds map[string]string, persist func(map[string]string) error) *Client {
	t.Helper()
	c := map[string]string{"access_token": "tok", "base_url": f.srv.URL, "user_id": "u1", "username": "rmp",
		"token_refreshed_at": time.Now().UTC().Format(time.RFC3339)}
	for k, v := range creds {
		c[k] = v
	}
	fac, err := providers.Get("threads")
	if err != nil {
		t.Fatal(err)
	}
	cl, err := fac.New(c, persist, f.srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	th := cl.(*Client)
	th.PollInterval = time.Millisecond
	return th
}

func expect(t *testing.T, got url.Values, want map[string]string) {
	t.Helper()
	for k, v := range want {
		if got.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, got.Get(k), v)
		}
	}
}

func img(name string) providers.MediaFile {
	return providers.MediaFile{Filename: name, Mime: "image/jpeg", PublicURL: "https://relay.example/media/" + name}
}

const (
	create  = "POST /v1.0/u1/threads"
	publish = "POST /v1.0/u1/threads_publish"
)

func TestPublishText(t *testing.T) {
	f := newFake(t, map[string]func(url.Values) string{
		create:         static(`{"id":"c1"}`),
		"GET /v1.0/c1": static(`{"status":"FINISHED"}`),
		publish:        static(`{"id":"p1"}`),
		"GET /v1.0/p1": static(`{"id":"p1","permalink":"https://www.threads.net/@rmp/post/abc"}`),
	})
	th := newTest(t, f, nil, nil)
	res, err := th.Publish(context.Background(), providers.PublishRequest{Parts: []string{"Session au skatepark 🛹"},
		Options: map[string]any{"link_attachment": "https://ridemypark.com", "reply_control": "followers_only",
			"topic_tag": "skate", "made_with_ai": true, "quote_post_id": "q9"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.RemoteID != "p1" || res.URL != "https://www.threads.net/@rmp/post/abc" || len(res.RemoteIDs) != 1 {
		t.Fatalf("result %+v", res)
	}
	expect(t, f.form(create, 0), map[string]string{"media_type": "TEXT", "text": "Session au skatepark 🛹",
		"link_attachment": "https://ridemypark.com", "reply_control": "followers_only", "topic_tag": "skate",
		"is_made_with_ai": "true", "quote_post_id": "q9", "reply_to_id": ""})
	expect(t, f.form("GET /v1.0/c1", 0), map[string]string{"fields": "status,error_message"})
	expect(t, f.form(publish, 0), map[string]string{"creation_id": "c1"})
}

func TestPublishThreadChain(t *testing.T) {
	f := newFake(t, map[string]func(url.Values) string{
		create:         seq("c"),
		"GET /v1.0/c1": static(`{"status":"FINISHED"}`),
		"GET /v1.0/c2": static(`{"status":"FINISHED"}`),
		"GET /v1.0/c3": static(`{"status":"FINISHED"}`),
		publish:        seq("p"),
		"GET /v1.0/p1": static(`{"permalink":"https://www.threads.net/@rmp/post/one"}`),
	})
	th := newTest(t, f, nil, nil)
	res, err := th.Publish(context.Background(), providers.PublishRequest{Parts: []string{"one", "two", "three"},
		Media: []providers.MediaFile{img("a.jpg")}, Options: map[string]any{"reply_to": "x0", "quote_post_id": "q1"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.RemoteIDs, ",") != "p1,p2,p3" || res.RemoteID != "p1" || res.URL != "https://www.threads.net/@rmp/post/one" {
		t.Fatalf("result %+v", res)
	}
	// media and quote on the first part only; each part replies to the previous one
	expect(t, f.form(create, 0), map[string]string{"media_type": "IMAGE", "image_url": "https://relay.example/media/a.jpg", "text": "one", "reply_to_id": "x0", "quote_post_id": "q1"})
	expect(t, f.form(create, 1), map[string]string{"media_type": "TEXT", "text": "two", "reply_to_id": "p1", "image_url": "", "quote_post_id": ""})
	expect(t, f.form(create, 2), map[string]string{"media_type": "TEXT", "text": "three", "reply_to_id": "p2"})
	for i, c := range []string{"c1", "c2", "c3"} {
		expect(t, f.form(publish, i), map[string]string{"creation_id": c})
	}
}

func TestPublishThreadStopsMidway(t *testing.T) {
	n := 0
	f := newFake(t, map[string]func(url.Values) string{
		create: func(url.Values) string {
			n++
			if n == 2 {
				return `400 {"error":{"message":"restricted","code":10,"error_subcode":2207051}}`
			}
			return `{"id":"c1"}`
		},
		"GET /v1.0/c1": static(`{"status":"FINISHED"}`),
		publish:        static(`{"id":"p1"}`),
	})
	th := newTest(t, f, nil, nil)
	res, err := th.Publish(context.Background(), providers.PublishRequest{Parts: []string{"one", "two"}})
	if err == nil || !strings.Contains(err.Error(), "part 2/2") || providers.Retryable(err) {
		t.Fatalf("err %v", err)
	}
	if res == nil || res.RemoteID != "p1" {
		t.Fatalf("partial result %+v", res)
	}
}

func TestPublishImageAlt(t *testing.T) {
	f := newFake(t, map[string]func(url.Values) string{
		create:         static(`{"id":"c1"}`),
		"GET /v1.0/c1": static(`{"status":"FINISHED"}`),
		publish:        static(`{"id":"p1"}`),
		"GET /v1.0/p1": static(`{"permalink":"https://www.threads.net/@rmp/post/i"}`),
	})
	th := newTest(t, f, nil, nil)
	m := img("a.png")
	m.Mime, m.Alt = "image/png", "a bowl"
	if _, err := th.Publish(context.Background(), providers.PublishRequest{Media: []providers.MediaFile{m}}); err != nil {
		t.Fatal(err)
	}
	expect(t, f.form(create, 0), map[string]string{"media_type": "IMAGE", "image_url": "https://relay.example/media/a.png", "alt_text": "a bowl", "text": ""})
	if _, err := th.Publish(context.Background(), providers.PublishRequest{Media: []providers.MediaFile{{Filename: "x.jpg", Mime: "image/jpeg"}}}); !errors.Is(err, providers.ErrInvalidPost) {
		t.Fatalf("missing public URL must be invalid: %v", err)
	}
}

func TestPublishCarousel(t *testing.T) {
	f := newFake(t, map[string]func(url.Values) string{
		create:         seq("c"),
		"GET /v1.0/c1": static(`{"status":"FINISHED"}`),
		"GET /v1.0/c2": static(`{"status":"FINISHED"}`),
		"GET /v1.0/c3": static(`{"status":"FINISHED"}`),
		publish:        static(`{"id":"p1"}`),
		"GET /v1.0/p1": static(`{"permalink":"https://www.threads.net/@rmp/post/car"}`),
	})
	th := newTest(t, f, nil, nil)
	vid := providers.MediaFile{Filename: "b.mp4", Mime: "video/mp4", PublicURL: "https://relay.example/media/b.mp4"}
	if _, err := th.Publish(context.Background(), providers.PublishRequest{Parts: []string{"album"},
		Media: []providers.MediaFile{img("a.jpg"), vid}, Options: map[string]any{"made_with_ai": true}}); err != nil {
		t.Fatal(err)
	}
	expect(t, f.form(create, 0), map[string]string{"media_type": "IMAGE", "image_url": "https://relay.example/media/a.jpg", "is_carousel_item": "true", "is_made_with_ai": "", "text": ""})
	expect(t, f.form(create, 1), map[string]string{"media_type": "VIDEO", "video_url": "https://relay.example/media/b.mp4", "is_carousel_item": "true"})
	expect(t, f.form(create, 2), map[string]string{"media_type": "CAROUSEL", "children": "c1,c2", "text": "album", "is_made_with_ai": "true"})
	expect(t, f.form(publish, 0), map[string]string{"creation_id": "c3"})
}

func TestPublishVideoPolling(t *testing.T) {
	polls, publishes := 0, 0
	f := newFake(t, map[string]func(url.Values) string{
		create: static(`{"id":"c1"}`),
		"GET /v1.0/c1": func(url.Values) string {
			polls++
			if polls < 3 {
				return `{"status":"IN_PROGRESS"}`
			}
			return `{"status":"FINISHED"}`
		},
		publish: func(url.Values) string {
			publishes++
			if publishes == 1 {
				return `400 {"error":{"message":"The requested resource does not exist","code":24,"error_subcode":4279009}}`
			}
			return `{"id":"p1"}`
		},
		"GET /v1.0/p1": static(`{"permalink":"https://www.threads.net/@rmp/post/v"}`),
	})
	th := newTest(t, f, nil, nil)
	vid := providers.MediaFile{Filename: "r.mp4", Mime: "video/mp4", PublicURL: "https://relay.example/media/r.mp4"}
	res, err := th.Publish(context.Background(), providers.PublishRequest{Parts: []string{"clip"}, Media: []providers.MediaFile{vid}})
	if err != nil {
		t.Fatal(err)
	}
	if res.URL != "https://www.threads.net/@rmp/post/v" || polls != 3 || publishes != 2 {
		t.Fatalf("res=%+v polls=%d publishes=%d", res, polls, publishes)
	}
	expect(t, f.form(create, 0), map[string]string{"media_type": "VIDEO", "video_url": "https://relay.example/media/r.mp4", "text": "clip"})
}

func TestPublishContainerErrors(t *testing.T) {
	status := `{"status":"ERROR","error_message":"INVALID_DURATION"}`
	f := newFake(t, map[string]func(url.Values) string{
		create:         static(`{"id":"c1"}`),
		"GET /v1.0/c1": func(url.Values) string { return status },
	})
	th := newTest(t, f, nil, nil)
	vid := providers.MediaFile{Filename: "r.mp4", Mime: "video/mp4", PublicURL: "https://relay.example/media/r.mp4"}
	if _, err := th.Publish(context.Background(), providers.PublishRequest{Media: []providers.MediaFile{vid}}); !errors.Is(err, providers.ErrInvalidPost) {
		t.Fatalf("invalid duration must not be retried: %v", err)
	}
	status = `{"status":"ERROR","error_message":"FAILED_DOWNLOADING_VIDEO"}`
	if _, err := th.Publish(context.Background(), providers.PublishRequest{Media: []providers.MediaFile{vid}}); err == nil || !providers.Retryable(err) {
		t.Fatalf("download failure should be retryable: %v", err)
	}
	status = `{"status":"EXPIRED"}`
	if _, err := th.Publish(context.Background(), providers.PublishRequest{Media: []providers.MediaFile{vid}}); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired: %v", err)
	}
	if f.count(publish) != 0 {
		t.Fatal("must not publish a failed container")
	}
}

func TestRateLimitRetryable(t *testing.T) {
	f := newFake(t, map[string]func(url.Values) string{
		create: static(`400 {"error":{"message":"Application request limit reached","code":4}}`),
	})
	th := newTest(t, f, nil, nil)
	if _, err := th.Publish(context.Background(), providers.PublishRequest{Parts: []string{"x"}}); err == nil || !providers.Retryable(err) {
		t.Fatalf("rate limit must be retryable: %v", err)
	}
}

func TestRefresh(t *testing.T) {
	f := newFake(t, map[string]func(url.Values) string{
		"GET /refresh_access_token": func(q url.Values) string {
			if q.Get("grant_type") != "th_refresh_token" || q.Get("access_token") != "old" {
				t.Errorf("refresh query %v", q)
			}
			return `{"access_token":"new","token_type":"bearer","expires_in":5184000}`
		},
		"GET /v1.0/me": func(q url.Values) string {
			if q.Get("access_token") != "new" {
				t.Errorf("me called with %q", q.Get("access_token"))
			}
			return `{"id":"u1","username":"rmp","name":"RideMyPark"}`
		},
	})
	var saved map[string]string
	th := newTest(t, f, map[string]string{"access_token": "old", "token_refreshed_at": time.Now().Add(-31 * 24 * time.Hour).Format(time.RFC3339)},
		func(c map[string]string) error { saved = c; return nil })
	id, err := th.Identify(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if id.RemoteUserID != "u1" || id.Handle != "rmp" || id.Name != "RideMyPark" {
		t.Fatalf("identity %+v", id)
	}
	expect(t, f.form("GET /v1.0/me", 0), map[string]string{"fields": "id,username,name,threads_profile_picture_url"})
	if saved["access_token"] != "new" || saved["token_expires_at"] == "" {
		t.Fatalf("persisted %v", saved)
	}
	exp, _ := time.Parse(time.RFC3339, saved["token_expires_at"])
	if d := time.Until(exp); d < 59*24*time.Hour || d > 61*24*time.Hour {
		t.Fatalf("expiry %v", exp)
	}
	// fresh token: no second refresh
	if _, err := th.Identify(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.count("GET /refresh_access_token") != 1 {
		t.Fatalf("refreshed %d times", f.count("GET /refresh_access_token"))
	}
}

func TestRefreshTooYoung(t *testing.T) {
	f := newFake(t, map[string]func(url.Values) string{
		"GET /refresh_access_token": static(`400 {"error":{"message":"Session has not been valid long enough","code":190}}`),
		"GET /v1.0/me":              static(`{"id":"u1","username":"rmp"}`),
	})
	var saved map[string]string
	th := newTest(t, f, map[string]string{"token_refreshed_at": ""}, func(c map[string]string) error { saved = c; return nil })
	id, err := th.Identify(context.Background())
	if err != nil || id.Name != "rmp" {
		t.Fatalf("%v %+v", err, id)
	}
	last, _ := time.Parse(time.RFC3339, saved["token_refreshed_at"])
	if saved["access_token"] != "tok" || time.Since(last) < 28*24*time.Hour {
		t.Fatalf("should retry in about a day: %v", saved)
	}
}

func TestIdentifyNameFallback(t *testing.T) {
	f := newFake(t, map[string]func(url.Values) string{
		"GET /v1.0/me": func(q url.Values) string {
			if strings.Contains(q.Get("fields"), "name,") || strings.HasSuffix(q.Get("fields"), ",name") {
				return `400 {"error":{"message":"Tried accessing nonexisting field (name)","code":100}}`
			}
			return `{"id":"u1","username":"rmp"}`
		},
	})
	th := newTest(t, f, map[string]string{"user_id": ""}, nil)
	id, err := th.Identify(context.Background())
	if err != nil || id.RemoteUserID != "u1" || id.Name != "rmp" {
		t.Fatalf("%v %+v", err, id)
	}
}

func TestMetrics(t *testing.T) {
	f := newFake(t, map[string]func(url.Values) string{
		"GET /v1.0/p1/insights": static(`{"data":[{"name":"views","period":"lifetime","values":[{"value":900}]},{"name":"likes","values":[{"value":40}]},` +
			`{"name":"replies","values":[{"value":5}]},{"name":"reposts","values":[{"value":3}]},{"name":"quotes","values":[{"value":2}]},{"name":"shares","values":[{"value":1}]}]}`),
		"GET /v1.0/p2/insights": func(q url.Values) string {
			if strings.Contains(q.Get("metric"), "shares") {
				return `400 {"error":{"message":"(#100) metric shares is not supported","code":100}}`
			}
			return `{"data":[{"name":"views","values":[{"value":7}]},{"name":"likes","values":[{"value":1}]}]}`
		},
	})
	th := newTest(t, f, nil, nil)
	got, err := th.Metrics(context.Background(), []string{"p1", "p2"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int64{"views": 900, "like_count": 40, "reply_count": 5, "repost_count": 3, "quote_count": 2, "shares": 1}
	for k, v := range want {
		if got["p1"][k] != v {
			t.Errorf("p1 %s = %d, want %d", k, got["p1"][k], v)
		}
	}
	if got["p2"]["views"] != 7 || got["p2"]["like_count"] != 1 {
		t.Fatalf("p2 %v", got["p2"])
	}
	expect(t, f.form("GET /v1.0/p1/insights", 0), map[string]string{"metric": "views,likes,replies,reposts,quotes,shares"})
	expect(t, f.form("GET /v1.0/p2/insights", 1), map[string]string{"metric": "views,likes,replies,reposts,quotes"})
}

func TestAccountMetrics(t *testing.T) {
	f := newFake(t, map[string]func(url.Values) string{
		"GET /v1.0/u1/threads_insights": func(q url.Values) string {
			if q.Get("metric") == "followers_count" {
				return `{"data":[{"name":"followers_count","period":"day","total_value":{"value":1234}}]}`
			}
			if q.Get("since") != "1712991600" || q.Get("until") == "" {
				t.Errorf("range %v", q)
			}
			return `{"data":[{"name":"views","period":"day","values":[{"value":10,"end_time":"x"},{"value":15,"end_time":"y"}]},{"name":"likes","total_value":{"value":99}}]}`
		},
	})
	th := newTest(t, f, nil, nil)
	m, err := th.AccountMetrics(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if m["followers"] != 1234 || m["views_total"] != 25 || m["likes_total"] != 99 {
		t.Fatalf("metrics %v", m)
	}
}

func TestComments(t *testing.T) {
	f := newFake(t, map[string]func(url.Values) string{
		"GET /v1.0/p1/conversation": func(q url.Values) string {
			if q.Get("fields") != replyFields || q.Get("reverse") != "false" {
				t.Errorf("query %v", q)
			}
			if q.Get("after") == "" {
				return `{"data":[{"id":"r1","text":"trop bien","username":"ann","timestamp":"2026-11-19T10:00:00+0000","permalink":"https://www.threads.net/@ann/post/r1"},` +
					`{"id":"r2","text":"merci","username":"rmp","is_reply_owned_by_me":true}],"paging":{"cursors":{"before":"B","after":"A1"}}}`
			}
			return `{"data":[{"id":"r3","text":"nested","username":"bob"}],"paging":{"cursors":{"before":"A1","after":"A1"}}}`
		},
		"GET /v1.0/p2/conversation": static(`400 {"error":{"message":"only root posts","code":100}}`),
		"GET /v1.0/p2/replies":      static(`{"data":[{"id":"r9","text":"direct","username":"cat"}]}`),
	})
	th := newTest(t, f, nil, nil)
	cs, err := th.Comments(context.Background(), "p1")
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 2 || cs[0].RemoteID != "r1" || cs[0].Author != "@ann" || cs[0].CreatedAt == nil || cs[0].URL == "" || cs[1].RemoteID != "r3" {
		t.Fatalf("comments %+v", cs)
	}
	cs, err = th.Comments(context.Background(), "p2")
	if err != nil || len(cs) != 1 || cs[0].Text != "direct" {
		t.Fatalf("fallback %v %+v", err, cs)
	}
}

func TestReply(t *testing.T) {
	f := newFake(t, map[string]func(url.Values) string{
		create:         static(`{"id":"c1"}`),
		"GET /v1.0/c1": static(`{"status":"FINISHED"}`),
		publish:        static(`{"id":"p7"}`),
		"GET /v1.0/p7": static(`{"permalink":"https://www.threads.net/@rmp/post/p7"}`),
	})
	th := newTest(t, f, nil, nil)
	res, err := th.Reply(context.Background(), "r1", "merci !")
	if err != nil {
		t.Fatal(err)
	}
	if res.RemoteID != "p7" || res.URL != "https://www.threads.net/@rmp/post/p7" {
		t.Fatalf("res %+v", res)
	}
	expect(t, f.form(create, 0), map[string]string{"media_type": "TEXT", "text": "merci !", "reply_to_id": "r1"})
}

func TestHistory(t *testing.T) {
	routes := map[string]func(url.Values) string{
		"GET /v1.0/u1/threads": func(q url.Values) string {
			if q.Get("limit") != "100" || q.Get("since") != "1767225600" {
				t.Errorf("query %v", q)
			}
			if q.Get("after") == "" {
				return `{"data":[{"id":"1","text":"hello","media_type":"TEXT_POST","permalink":"https://t/1","timestamp":"2026-01-02T05:42:03+0000"},` +
					`{"id":"2","media_type":"CAROUSEL_ALBUM","timestamp":"2026-01-03T00:00:00+0000","children":{"data":[` +
					`{"id":"2a","media_type":"IMAGE","media_url":"https://th.cdn/2a.jpg"},{"id":"2b","media_type":"VIDEO","media_url":"https://th.cdn/2b.mp4","thumbnail_url":"https://th.cdn/2b.jpg"}]}},` +
					`{"id":"3","media_type":"REPOST_FACADE","media_url":"https://th.cdn/other.jpg"},{"id":"4","media_type":"TEXT_POST","is_quote_post":true},` +
					`{"id":"5","media_type":"IMAGE","media_url":"https://th.cdn/5.jpg"}],"paging":{"cursors":{"after":"A"}}}`
			}
			if q.Get("fields") != "id,text,media_type,permalink,timestamp,media_url,thumbnail_url,children{media_type,media_url,thumbnail_url},is_quote_post" {
				t.Errorf("fields %q", q.Get("fields"))
			}
			return `{"data":[{"id":"6","media_type":"VIDEO","media_url":"https://th.cdn/6.mp4","thumbnail_url":"https://th.cdn/6.jpg"}],"paging":{"cursors":{"before":"A"}}}`
		},
	}
	for _, id := range []string{"1", "2", "4", "5", "6"} { // no insights call for the repost facade (3)
		routes["GET /v1.0/"+id+"/insights"] = static(`{"data":[{"name":"views","values":[{"value":` + id + `0}]},{"name":"likes","values":[{"value":` + id + `}]}]}`)
	}
	f := newFake(t, routes)
	th := newTest(t, f, nil, nil)
	items, err := th.History(context.Background(), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), 0)
	if err != nil {
		t.Fatal(err)
	}
	kinds := []string{}
	for _, it := range items {
		kinds = append(kinds, it.Kind)
	}
	if strings.Join(kinds, ",") != "text,carousel,repost,quote,image,video" {
		t.Fatalf("kinds %v", kinds)
	}
	if items[0].Metrics["views"] != 10 || items[0].Metrics["like_count"] != 1 || items[2].Metrics != nil || items[5].Metrics["views"] != 60 {
		t.Fatalf("metrics %v %v %v", items[0].Metrics, items[2].Metrics, items[5].Metrics)
	}
	if items[0].CreatedAt.IsZero() || items[0].URL != "https://t/1" || items[0].Text != "hello" {
		t.Fatalf("item %+v", items[0])
	}
	wantVis := [][]providers.Visual{
		nil,
		{{URL: "https://th.cdn/2a.jpg"}, {URL: "https://th.cdn/2b.jpg", Cover: true}},
		nil, // a repost shows someone else's post
		nil,
		{{URL: "https://th.cdn/5.jpg"}},
		{{URL: "https://th.cdn/6.jpg", Cover: true}},
	}
	for i, w := range wantVis {
		if !reflect.DeepEqual(items[i].Visuals, w) {
			t.Errorf("item %s visuals %+v want %+v", items[i].RemoteID, items[i].Visuals, w)
		}
	}
	items, _ = th.History(context.Background(), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), 2)
	if len(items) != 2 {
		t.Fatalf("max: %d", len(items))
	}
}

func TestTextLen(t *testing.T) {
	cases := map[string]int{"abc": 3, "éàü": 3, "🛹": 4, "ok 👍🏽": 3 + 4 + 4, "❤️": 3 + 3}
	for s, want := range cases {
		if got := TextLen(s); got != want {
			t.Errorf("TextLen(%q) = %d, want %d", s, got, want)
		}
	}
}

func TestValidate(t *testing.T) {
	ok := []providers.PublishRequest{
		{Parts: []string{strings.Repeat("é", 500)}},
		{Parts: []string{"a", "b"}},
		{Media: []providers.MediaFile{img("a.jpg")}},
		{Parts: []string{"x"}, Options: map[string]any{"link_attachment": "https://a.b", "reply_control": "everyone", "topic_tag": "Skate Park"}},
	}
	for i, r := range ok {
		if errs := validate(r); len(errs) != 0 {
			t.Errorf("ok #%d: %v", i, errs)
		}
	}
	many := make([]providers.MediaFile, 21)
	for i := range many {
		many[i] = img("a.jpg")
	}
	bad := map[string]providers.PublishRequest{
		"500":           {Parts: []string{strings.Repeat("a", 501)}},
		"emojis":        {Parts: []string{strings.Repeat("🛹", 126)}},
		"links":         {Parts: []string{"https://a https://b https://c https://d https://e https://f"}},
		"empty":         {},
		"empty part":    {Parts: []string{"a", " "}},
		"carousel":      {Media: many},
		"link w/ media": {Media: []providers.MediaFile{img("a.jpg")}, Options: map[string]any{"link_attachment": "https://a.b"}},
		"reply_control": {Parts: []string{"x"}, Options: map[string]any{"reply_control": "friends"}},
		"topic":         {Parts: []string{"x"}, Options: map[string]any{"topic_tag": "a.b"}},
		"gif":           {Media: []providers.MediaFile{{Filename: "a.gif", Mime: "image/gif"}}},
		"webm":          {Media: []providers.MediaFile{{Filename: "a.webm", Mime: "video/webm"}}},
		"big image":     {Media: []providers.MediaFile{{Filename: "a.jpg", Mime: "image/jpeg", Size: 9 << 20}}},
		"alt":           {Media: []providers.MediaFile{{Filename: "a.jpg", Mime: "image/jpeg", Alt: strings.Repeat("a", 1001)}}},
	}
	for name, r := range bad {
		if errs := validate(r); len(errs) == 0 {
			t.Errorf("%s: expected a problem", name)
		}
	}
}
