package meta

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Seeyko/relay/internal/providers"
)

func newFB(t *testing.T, h http.HandlerFunc) *Facebook {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	f, err := providers.Get("facebook")
	if err != nil {
		t.Fatal(err)
	}
	c, err := f.New(map[string]string{"page_id": "123", "page_access_token": "tok", "base_url": srv.URL, "rupload_base_url": srv.URL}, nil, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	return c.(*Facebook)
}

func media(name, mime, body string) providers.MediaFile {
	return providers.MediaFile{Filename: name, Mime: mime, Size: int64(len(body)), Open: func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(body)), nil
	}}
}

func token(r *http.Request) string {
	if v := r.URL.Query().Get("access_token"); v != "" {
		return v
	}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
		_ = r.ParseMultipartForm(1 << 20)
	}
	return r.FormValue("access_token")
}

func TestFacebookPublishTextAndLink(t *testing.T) {
	fb := newFB(t, func(w http.ResponseWriter, r *http.Request) {
		if token(r) != "tok" {
			t.Errorf("missing token on %s", r.URL.Path)
		}
		switch {
		case r.Method == "POST" && r.URL.Path == "/v26.0/123/feed":
			if r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" || r.FormValue("message") != "a\n\nb" || r.FormValue("link") != "https://ex.com" {
				t.Errorf("feed form: %v", r.Form)
			}
			w.Write([]byte(`{"id":"123_9"}`))
		case r.URL.Path == "/v26.0/123_9" && r.URL.Query().Get("fields") == "permalink_url":
			w.Write([]byte(`{"permalink_url":"https://www.facebook.com/123/posts/9"}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
		}
	})
	res, err := fb.Publish(context.Background(), providers.PublishRequest{Parts: []string{"a", "b"}, Options: map[string]any{"link": "https://ex.com"}})
	if err != nil || res.RemoteID != "123_9" || res.URL != "https://www.facebook.com/123/posts/9" {
		t.Fatalf("%v %+v", err, res)
	}
}

func TestFacebookPublishPhotos(t *testing.T) {
	var steps []string
	fb := newFB(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/v26.0/123/photos":
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Fatal(err)
			}
			f, h, err := r.FormFile("source")
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(f)
			steps = append(steps, h.Filename+":"+string(b)+":caption="+r.FormValue("caption")+":published="+r.FormValue("published")+":alt="+r.FormValue("alt_text_custom"))
			if r.FormValue("message") != "" || r.FormValue("access_token") != "tok" {
				t.Errorf("photo form: %v", r.MultipartForm.Value)
			}
			w.Write([]byte(`{"id":"p` + string(b) + `","post_id":"123_p` + string(b) + `"}`))
		case r.Method == "POST" && r.URL.Path == "/v26.0/123/feed":
			steps = append(steps, "feed:"+r.FormValue("message")+":"+r.FormValue("attached_media[0]")+","+r.FormValue("attached_media[1]"))
			w.Write([]byte(`{"id":"123_album"}`))
		case r.Method == "GET":
			w.WriteHeader(400)
			w.Write([]byte(`{"error":{"message":"no","code":100}}`))
		}
	})
	ctx := context.Background()
	one := media("a.jpg", "image/jpeg", "1")
	one.Alt = "a cat"
	res, err := fb.Publish(ctx, providers.PublishRequest{Parts: []string{"hi"}, Media: []providers.MediaFile{one}})
	if err != nil || res.RemoteID != "123_p1" || res.URL != "https://www.facebook.com/123_p1" {
		t.Fatalf("%v %+v", err, res)
	}
	res, err = fb.Publish(ctx, providers.PublishRequest{Parts: []string{"two"}, Media: []providers.MediaFile{media("a.jpg", "image/jpeg", "1"), media("b.png", "image/png", "2")}})
	if err != nil || res.RemoteID != "123_album" {
		t.Fatalf("%v %+v", err, res)
	}
	want := []string{
		"a.jpg:1:caption=hi:published=:alt=a cat",
		"a.jpg:1:caption=:published=false:alt=",
		"b.png:2:caption=:published=false:alt=",
		`feed:two:{"media_fbid":"p1"},{"media_fbid":"p2"}`,
	}
	if strings.Join(steps, "|") != strings.Join(want, "|") {
		t.Fatalf("steps:\n%v\nwant:\n%v", steps, want)
	}
}

func TestFacebookPublishVideoSimpleAndChunked(t *testing.T) {
	var got []string
	var uploaded strings.Builder
	fb := newFB(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.Write([]byte(`{"permalink_url":"/123/videos/v1/"}`))
			return
		}
		if r.URL.Path != "/v26.0/123/videos" {
			t.Errorf("unexpected %s", r.URL.Path)
		}
		if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
			r.ParseMultipartForm(1 << 20)
		} else {
			r.ParseForm()
		}
		switch r.FormValue("upload_phase") {
		case "":
			f, _, err := r.FormFile("source")
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(f)
			got = append(got, "simple:"+string(b)+":"+r.FormValue("title")+":"+r.FormValue("description"))
			w.Write([]byte(`{"id":"v1"}`))
		case "start":
			got = append(got, "start:"+r.FormValue("file_size"))
			w.Write([]byte(`{"upload_session_id":"s1","video_id":"v2","start_offset":"0","end_offset":"4"}`))
		case "transfer":
			f, _, err := r.FormFile("video_file_chunk")
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(f)
			uploaded.Write(b)
			got = append(got, "transfer:"+r.FormValue("upload_session_id")+":"+r.FormValue("start_offset")+":"+string(b))
			if r.FormValue("start_offset") == "0" {
				w.Write([]byte(`{"start_offset":"4","end_offset":"6"}`))
			} else {
				w.Write([]byte(`{"start_offset":"6","end_offset":"6"}`))
			}
		case "finish":
			got = append(got, "finish:"+r.FormValue("upload_session_id")+":"+r.FormValue("title")+":"+r.FormValue("description"))
			w.Write([]byte(`{"success":true}`))
		}
	})
	ctx := context.Background()
	res, err := fb.Publish(ctx, providers.PublishRequest{Parts: []string{"desc"}, Media: []providers.MediaFile{media("v.mp4", "video/mp4", "VID")}, Options: map[string]any{"title": "T"}})
	if err != nil || res.RemoteID != "v1" || res.URL != "https://www.facebook.com/123/videos/v1/" {
		t.Fatalf("%v %+v", err, res)
	}
	big := media("big.mp4", "video/mp4", "abcdef")
	big.Size = fbChunkedVideo + 1 // force the chunked path while the fake body stays small
	res, err = fb.Publish(ctx, providers.PublishRequest{Parts: []string{"desc"}, Media: []providers.MediaFile{big}, Options: map[string]any{"title": "T"}})
	if err != nil || res.RemoteID != "v2" {
		t.Fatalf("%v %+v", err, res)
	}
	want := "simple:VID:T:desc|start:67108865|transfer:s1:0:abcd|transfer:s1:4:ef|finish:s1:T:desc"
	if strings.Join(got, "|") != want || uploaded.String() != "abcdef" {
		t.Fatalf("got %v", got)
	}
}

func TestFacebookPublishReel(t *testing.T) {
	var got []string
	fb := newFB(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v26.0/123/video_reels":
			r.ParseForm()
			got = append(got, r.FormValue("upload_phase")+":"+r.FormValue("video_id")+":"+r.FormValue("video_state")+":"+r.FormValue("description"))
			if r.FormValue("upload_phase") == "start" {
				w.Write([]byte(`{"video_id":"r1","upload_url":"http://` + r.Host + `/video-upload/v26.0/r1"}`))
			} else {
				w.Write([]byte(`{"success":true}`))
			}
		case r.URL.Path == "/video-upload/v26.0/r1":
			b, _ := io.ReadAll(r.Body)
			if r.Header.Get("Authorization") != "OAuth tok" || r.Header.Get("offset") != "0" || r.Header.Get("file_size") != "4" || r.Header.Get("Content-Type") != "application/octet-stream" {
				t.Errorf("upload headers: %v", r.Header)
			}
			got = append(got, "upload:"+string(b))
			w.Write([]byte(`{"success":true}`))
		case r.Method == "GET":
			w.Write([]byte(`{}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
		}
	})
	res, err := fb.Publish(context.Background(), providers.PublishRequest{Parts: []string{"#reel"}, Media: []providers.MediaFile{media("r.mp4", "video/mp4", "REEL")}, Options: map[string]any{"reel": true}})
	if err != nil || res.RemoteID != "r1" || res.URL != "https://www.facebook.com/reel/r1" {
		t.Fatalf("%v %+v", err, res)
	}
	if strings.Join(got, "|") != "start:::|upload:REEL|finish:r1:PUBLISHED:#reel" {
		t.Fatalf("got %v", got)
	}
}

func TestFacebookMetrics(t *testing.T) {
	// Graph v26.0 rejects the ?ids= batch read, so every object is read with GET /{id}.
	objects := map[string]map[string]string{
		"/v26.0/123_1": {
			"reactions.limit(0).summary(true),comments.limit(0).summary(true),shares":                    `{"id":"123_1","reactions":{"data":[],"summary":{"total_count":5}},"comments":{"data":[],"summary":{"total_count":2}},"shares":{"count":1}}`,
			"insights.metric(post_media_view,post_total_media_view_unique,post_clicks).period(lifetime)": `{"insights":{"data":[{"name":"post_media_view","period":"lifetime","values":[{"value":100}]},{"name":"post_total_media_view_unique","period":"lifetime","values":[{"value":80}]},{"name":"post_clicks","period":"lifetime","values":[{"value":7}]}]}}`,
		},
		"/v26.0/123_2": {
			"reactions.limit(0).summary(true),comments.limit(0).summary(true),shares":                    `{"id":"123_2","reactions":{"summary":{"total_count":0}}}`,
			"insights.metric(post_media_view,post_total_media_view_unique,post_clicks).period(lifetime)": `{}`,
		},
		"/v26.0/v9": {
			"likes.limit(0).summary(true),comments.limit(0).summary(true)": `{"likes":{"summary":{"total_count":3}},"comments":{"summary":{"total_count":4}}}`,
		},
	}
	fb := newFB(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Has("ids") {
			w.WriteHeader(400)
			w.Write([]byte(`{"error":{"message":"The ids query parameter is deprecated in v26.0+.","code":100}}`))
			return
		}
		if r.URL.Path == "/v26.0/123_gone" {
			// A deleted post must not hide the metrics of the others.
			w.WriteHeader(400)
			w.Write([]byte(`{"error":{"message":"Unsupported get request","code":100}}`))
			return
		}
		if q.Get("fields") == "video_insights.metric(total_video_views).period(lifetime)" {
			// Missing read_insights must not break engagement metrics.
			w.WriteHeader(403)
			w.Write([]byte(`{"error":{"message":"(#10) permission","code":10}}`))
			return
		}
		body, ok := objects[r.URL.Path][q.Get("fields")]
		if !ok {
			t.Errorf("unexpected %s fields=%q", r.URL.Path, q.Get("fields"))
			w.WriteHeader(500)
			return
		}
		w.Write([]byte(body))
	})
	m, err := fb.Metrics(context.Background(), []string{"123_1", "v9", "123_2", "123_gone"})
	if err != nil {
		t.Fatal(err)
	}
	if want := (map[string]int64{"like_count": 5, "reply_count": 2, "share_count": 1, "views": 100, "reach": 80, "clicks": 7}); !equal(m["123_1"], want) {
		t.Fatalf("123_1 %v", m["123_1"])
	}
	if want := (map[string]int64{"like_count": 3, "reply_count": 4}); !equal(m["v9"], want) {
		t.Fatalf("v9 %v", m["v9"])
	}
	if m["123_2"]["like_count"] != 0 || len(m["123_2"]) != 3 {
		t.Fatalf("123_2 %v", m["123_2"])
	}
}

func equal(a, b map[string]int64) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}

func TestFacebookCommentsAndReply(t *testing.T) {
	fb := newFB(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch {
		case r.Method == "GET" && r.URL.Path == "/v26.0/123_9/comments":
			if q.Get("filter") != "stream" || q.Get("limit") != "100" || q.Get("fields") != "id,message,created_time,permalink_url,from" {
				t.Errorf("query %v", q)
			}
			if q.Get("after") == "" {
				w.Write([]byte(`{"data":[{"id":"c1","message":"nice","created_time":"2026-11-19T10:00:00+0000","from":{"id":"5","name":"Ann"}},{"id":"c2","message":"mine","from":{"id":"123","name":"Page"}}],"paging":{"cursors":{"after":"X"},"next":"https://next"}}`))
			} else {
				w.Write([]byte(`{"data":[{"id":"c3","message":"late","created_time":"2026-11-19T11:00:00+0000"}],"paging":{"cursors":{"after":"Y"}}}`))
			}
		case r.Method == "POST" && r.URL.Path == "/v26.0/c1/comments":
			if r.FormValue("message") != "thanks" {
				t.Errorf("reply form %v", r.Form)
			}
			w.Write([]byte(`{"id":"123_9_r1"}`))
		case r.Method == "GET" && r.URL.Path == "/v26.0/123_9_r1":
			w.Write([]byte(`{"permalink_url":"https://www.facebook.com/x?comment_id=r1"}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
		}
	})
	ctx := context.Background()
	cs, err := fb.Comments(ctx, "123_9")
	if err != nil || len(cs) != 2 || cs[0].Author != "Ann" || cs[1].Author != "unknown" || cs[0].CreatedAt == nil || !cs[0].CreatedAt.Equal(time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("%v %+v", err, cs)
	}
	res, err := fb.Reply(ctx, "c1", "thanks")
	if err != nil || res.RemoteID != "123_9_r1" || res.URL != "https://www.facebook.com/x?comment_id=r1" {
		t.Fatalf("%v %+v", err, res)
	}
}

func TestFacebookHistoryKindsAndAccount(t *testing.T) {
	fb := newFB(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch r.URL.Path {
		case "/v26.0/123/posts":
			if q.Get("since") != "1700000000" || q.Get("limit") != "100" || q.Get("fields") != "id,message,created_time,permalink_url,status_type,full_picture,attachments{type,media_type,media{image{src}},subattachments{type,media_type,media{image{src}}}}" {
				t.Errorf("query %v", q)
			}
			w.Write([]byte(`{"data":[
				{"id":"123_1","message":"t","created_time":"2026-01-02T03:04:05+0000","status_type":"mobile_status_update"},
				{"id":"123_2","created_time":"2026-01-02T03:04:05+0000","status_type":"added_photos","full_picture":"https://fb.cdn/2-full.jpg","attachments":{"data":[{"type":"photo","media_type":"photo","media":{"image":{"src":"https://fb.cdn/2.jpg","width":720}}}]}},
				{"id":"123_3","created_time":"2026-01-02T03:04:05+0000","attachments":{"data":[{"type":"album","media_type":"album","media":{"image":{"src":"https://fb.cdn/3a.jpg"}},
					"subattachments":{"data":[{"type":"photo","media":{"image":{"src":"https://fb.cdn/3a.jpg"}}},{"type":"video","media_type":"video","media":{"image":{"src":"https://fb.cdn/3b-thumb.jpg"}}}]}}]}},
				{"id":"123_4","created_time":"2026-01-02T03:04:05+0000","status_type":"added_video","attachments":{"data":[{"type":"video_inline","media_type":"video","media":{"image":{"src":"https://fb.cdn/4-thumb.jpg"},"source":"https://fb.cdn/4.mp4"}}]}},
				{"id":"123_5","created_time":"2026-01-02T03:04:05+0000","status_type":"shared_story","full_picture":"https://fb.cdn/5-link.jpg","attachments":{"data":[{"type":"share","media_type":"link"}]}}
			],"paging":{"cursors":{"after":"A"}}}`))
		case "/v26.0/123_1", "/v26.0/123_2", "/v26.0/123_3", "/v26.0/123_4", "/v26.0/123_5":
			w.Write([]byte(`{}`))
		case "/v26.0/123":
			if q.Get("fields") != "fan_count,followers_count" {
				t.Errorf("fields %q", q.Get("fields"))
			}
			w.Write([]byte(`{"fan_count":10,"followers_count":12,"id":"123"}`))
		default:
			t.Errorf("unexpected %s", r.URL)
		}
	})
	ctx := context.Background()
	items, err := fb.History(ctx, time.Unix(1700000000, 0), 0)
	if err != nil || len(items) != 5 {
		t.Fatalf("%v %+v", err, items)
	}
	var kinds []string
	for _, it := range items {
		kinds = append(kinds, it.Kind)
	}
	if strings.Join(kinds, ",") != "text,image,carousel,video,text" || items[0].CreatedAt.Year() != 2026 {
		t.Fatalf("kinds %v %v", kinds, items[0].CreatedAt)
	}
	wantVis := [][]providers.Visual{
		nil,
		{{URL: "https://fb.cdn/2.jpg"}},
		{{URL: "https://fb.cdn/3a.jpg"}, {URL: "https://fb.cdn/3b-thumb.jpg", Cover: true}},
		{{URL: "https://fb.cdn/4-thumb.jpg", Cover: true}},
		{{URL: "https://fb.cdn/5-link.jpg"}}, // link preview through full_picture
	}
	for i, w := range wantVis {
		if !reflect.DeepEqual(items[i].Visuals, w) {
			t.Errorf("item %s visuals %+v want %+v", items[i].RemoteID, items[i].Visuals, w)
		}
	}
	m, err := fb.AccountMetrics(ctx)
	if err != nil || m["followers"] != 12 || m["page_likes"] != 10 {
		t.Fatalf("%v %v", err, m)
	}
}

func TestFacebookErrorClassification(t *testing.T) {
	cases := []struct {
		status int
		body   string
		retry  bool
	}{
		{400, `{"error":{"message":"(#4) Application request limit reached","type":"OAuthException","code":4}}`, true},
		{400, `{"error":{"message":"(#32) Page request limit reached","code":32}}`, true},
		{400, `{"error":{"message":"BUC rate limit","code":80001}}`, true},
		{400, `{"error":{"message":"unknown","code":2,"is_transient":true}}`, true},
		{500, `{"error":{"message":"Session has expired","type":"OAuthException","code":190,"error_subcode":463}}`, false},
		{403, `{"error":{"message":"(#200) perms","code":200}}`, false},
		{400, `{"error":{"message":"(#506) Duplicate status message","code":506}}`, false},
	}
	for _, c := range cases {
		fb := newFB(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(c.status)
			w.Write([]byte(c.body))
		})
		_, err := fb.Identify(context.Background())
		var he *providers.HTTPError
		if !errors.As(err, &he) || he.Body != c.body {
			t.Fatalf("not an HTTPError with the body: %v", err)
		}
		if got := providers.Retryable(err); got != c.retry {
			t.Errorf("%s: retryable=%v", c.body, got)
		}
	}
}

func TestFacebookValidate(t *testing.T) {
	f, _ := providers.Get("facebook")
	big := media("x.jpg", "image/jpeg", "")
	big.Size = fbMaxPhoto + 1
	cases := []struct {
		req  providers.PublishRequest
		want string
	}{
		{providers.PublishRequest{}, "empty post"},
		{providers.PublishRequest{Parts: []string{strings.Repeat("a", fbMaxText+1)}}, "63206"},
		{providers.PublishRequest{Media: []providers.MediaFile{big}}, "10 MB"},
		{providers.PublishRequest{Media: []providers.MediaFile{media("x.webp", "image/webp", "1")}}, "JPEG"},
		{providers.PublishRequest{Media: []providers.MediaFile{media("v.mp4", "video/mp4", "1"), media("a.jpg", "image/jpeg", "1")}}, "only media"},
		{providers.PublishRequest{Parts: []string{"x"}, Options: map[string]any{"reel": true}}, "Reel"},
	}
	for _, c := range cases {
		if errs := f.Validate(c.req); !strings.Contains(strings.Join(errs, ";"), c.want) {
			t.Errorf("want %q, got %v", c.want, errs)
		}
	}
	if errs := f.Validate(providers.PublishRequest{Parts: []string{"ok"}, Media: []providers.MediaFile{media("a.jpg", "image/jpeg", "1")}}); errs != nil {
		t.Errorf("unexpected %v", errs)
	}
}
