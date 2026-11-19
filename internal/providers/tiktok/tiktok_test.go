package tiktok

import (
	"context"
	"encoding/json"
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

type hit struct {
	Method, Path, Query, Auth, CType, Range string
	Body                                    []byte
}

type fake struct {
	t      *testing.T
	mu     sync.Mutex
	hits   []hit
	routes map[string]func(w http.ResponseWriter, h hit)
	srv    *httptest.Server
}

func newFake(t *testing.T) *fake {
	f := &fake{t: t, routes: map[string]func(http.ResponseWriter, hit){}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		h := hit{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization"), r.Header.Get("Content-Type"), r.Header.Get("Content-Range"), b}
		f.mu.Lock()
		f.hits = append(f.hits, h)
		fn := f.routes[r.Method+" "+r.URL.Path]
		f.mu.Unlock()
		if fn == nil {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.Error(w, "no route", 404)
			return
		}
		fn(w, h)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fake) on(route string, fn func(http.ResponseWriter, hit)) { f.routes[route] = fn }

func (f *fake) find(route string) []hit {
	var out []hit
	for _, h := range f.hits {
		if h.Method+" "+h.Path == route {
			out = append(out, h)
		}
	}
	return out
}

func ok(data string) func(http.ResponseWriter, hit) {
	return func(w http.ResponseWriter, _ hit) {
		io.WriteString(w, `{"data":`+data+`,"error":{"code":"ok","message":"","log_id":"x"}}`)
	}
}

func newClient(t *testing.T, f *fake, extra map[string]string) (*Client, map[string]string) {
	creds := map[string]string{"access_token": "AT", "base_url": f.srv.URL}
	for k, v := range extra {
		creds[k] = v
	}
	fac, err := providers.Get("tiktok")
	if err != nil {
		t.Fatal(err)
	}
	cl, err := fac.New(creds, func(map[string]string) error { return nil }, f.srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	c := cl.(*Client)
	c.PollInterval = time.Millisecond
	return c, creds
}

func jsonOf(t *testing.T, b []byte) map[string]any {
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("bad json %q: %v", b, err)
	}
	return m
}

func TestTokenRefreshRotates(t *testing.T) {
	f := newFake(t)
	f.on("POST /v2/oauth/token/", func(w http.ResponseWriter, h hit) {
		if h.CType != "application/x-www-form-urlencoded" {
			t.Errorf("content-type %q", h.CType)
		}
		v, _ := url.ParseQuery(string(h.Body))
		want := url.Values{"client_key": {"CK"}, "client_secret": {"CS"}, "grant_type": {"refresh_token"}, "refresh_token": {"RT1"}}
		if v.Encode() != want.Encode() {
			t.Errorf("form = %v", v)
		}
		io.WriteString(w, `{"access_token":"AT2","expires_in":86400,"refresh_token":"RT2","refresh_expires_in":31536000,"open_id":"o","scope":"user.info.basic,video.publish","token_type":"Bearer"}`)
	})
	f.on("GET /v2/user/info/", func(w http.ResponseWriter, h hit) {
		if h.Auth != "Bearer AT2" {
			t.Errorf("auth %q", h.Auth)
		}
		ok(`{"user":{"open_id":"o1","display_name":"Name","username":"handle"}}`)(w, h)
	})
	c, creds := newClient(t, f, map[string]string{"client_key": "CK", "client_secret": "CS", "refresh_token": "RT1", "expires_at": "1"})
	id, err := c.Identify(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if id.RemoteUserID != "o1" || id.Handle != "handle" || id.Name != "Name" {
		t.Errorf("identity %+v", id)
	}
	if creds["refresh_token"] != "RT2" || creds["access_token"] != "AT2" || creds["refresh_expires_at"] == "" {
		t.Errorf("creds not rotated: %v", creds["refresh_token"])
	}
	if q := f.find("GET /v2/user/info/")[0].Query; q != "fields=open_id,display_name,username" {
		t.Errorf("query %q", q)
	}
}

func TestRetryOn401(t *testing.T) {
	f := newFake(t)
	f.on("POST /v2/oauth/token/", func(w http.ResponseWriter, h hit) {
		io.WriteString(w, `{"access_token":"AT2","expires_in":86400,"refresh_token":"RT2"}`)
	})
	f.on("GET /v2/user/info/", func(w http.ResponseWriter, h hit) {
		if h.Auth == "Bearer AT" {
			w.WriteHeader(401)
			io.WriteString(w, `{"error":{"code":"access_token_invalid","message":"expired","log_id":"l"}}`)
			return
		}
		ok(`{"user":{"follower_count":10,"following_count":2,"likes_count":30,"video_count":4}}`)(w, h)
	})
	c, _ := newClient(t, f, map[string]string{"client_key": "CK", "client_secret": "CS", "refresh_token": "RT1"})
	m, err := c.AccountMetrics(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if m["followers"] != 10 || m["following"] != 2 || m["likes_total"] != 30 || m["posts"] != 4 {
		t.Errorf("metrics %v", m)
	}
	if q := f.find("GET /v2/user/info/")[1].Query; q != "fields=follower_count,following_count,likes_count,video_count" {
		t.Errorf("query %q", q)
	}
}

func TestErrorEnvelopeClassification(t *testing.T) {
	f := newFake(t)
	f.on("POST /v2/post/publish/creator_info/query/", func(w http.ResponseWriter, h hit) {
		io.WriteString(w, `{"data":{},"error":{"code":"spam_risk_too_many_posts","message":"cap","log_id":"l"}}`)
	})
	c, _ := newClient(t, f, nil)
	_, err := c.Publish(context.Background(), videoReq(1000, map[string]any{"privacy_level": "SELF_ONLY"}))
	var he *providers.HTTPError
	if err == nil || !strings.Contains(err.Error(), "spam_risk_too_many_posts") || !errorsAs(err, &he) || he.Status != 403 {
		t.Fatalf("err = %v", err)
	}
}

func errorsAs(err error, he **providers.HTTPError) bool {
	h, ok := err.(*providers.HTTPError)
	*he = h
	return ok
}

// fakeVideo returns a MediaFile of the given size whose bytes are i%251.
func fakeVideo(size int64) providers.MediaFile {
	return providers.MediaFile{Filename: "v.mp4", Mime: "video/mp4", Size: size, Open: func() (io.ReadCloser, error) {
		return io.NopCloser(io.LimitReader(&pattern{}, size)), nil
	}}
}

type pattern struct{ n int64 }

func (p *pattern) Read(b []byte) (int, error) {
	for i := range b {
		b[i] = byte(p.n % 251)
		p.n++
	}
	return len(b), nil
}

func videoReq(size int64, o map[string]any) providers.PublishRequest {
	return providers.PublishRequest{Parts: []string{"hello #go"}, Media: []providers.MediaFile{fakeVideo(size)}, Options: o}
}

const creator = `{"creator_username":"handle","creator_nickname":"N","privacy_level_options":["PUBLIC_TO_EVERYONE","MUTUAL_FOLLOW_FRIENDS","SELF_ONLY"],"comment_disabled":false,"duet_disabled":true,"stitch_disabled":false,"max_video_post_duration_sec":600}`

func TestChunkPlan(t *testing.T) {
	const mib = 1 << 20
	cases := []struct{ size, chunk, count int64 }{
		{1000, 1000, 1},
		{4 * mib, 4 * mib, 1},
		{20 * mib, 20 * mib, 1},
		{20*mib + 1, 10 * mib, 2},
		{100*mib + 7, 10 * mib, 10},
		{4 << 30, 10 * mib, 409},
	}
	for _, tc := range cases {
		cs, n := chunkPlan(tc.size)
		if cs != tc.chunk || n != tc.count {
			t.Errorf("chunkPlan(%d) = %d,%d want %d,%d", tc.size, cs, n, tc.chunk, tc.count)
		}
		last := tc.size - cs*(n-1)
		if n > 1 && (cs < 5*mib || cs > 64*mib || last < cs || last > 128*mib) {
			t.Errorf("size %d: chunk %d last %d violate TikTok rules", tc.size, cs, last)
		}
		if n < 1 || n > 1000 {
			t.Errorf("size %d: %d chunks", tc.size, n)
		}
	}
}

func publishVideo(t *testing.T, size int64, status func(i int) string) (*fake, *providers.PublishResult) {
	f := newFake(t)
	f.on("POST /v2/post/publish/creator_info/query/", ok(creator))
	f.on("POST /v2/post/publish/video/init/", func(w http.ResponseWriter, h hit) {
		ok(`{"publish_id":"v_pub_1","upload_url":"`+f.srv.URL+`/upload/?upload_id=1&upload_token=x"}`)(w, h)
	})
	var mu sync.Mutex
	var got int64
	f.on("PUT /upload/", func(w http.ResponseWriter, h hit) {
		mu.Lock()
		got += int64(len(h.Body))
		done := got == size
		mu.Unlock()
		if done {
			w.WriteHeader(201)
		} else {
			w.WriteHeader(206)
		}
	})
	n := 0
	f.on("POST /v2/post/publish/status/fetch/", func(w http.ResponseWriter, h hit) {
		n++
		ok(status(n))(w, h)
	})
	c, _ := newClient(t, f, nil)
	res, err := c.Publish(context.Background(), videoReq(size, map[string]any{"privacy_level": "PUBLIC_TO_EVERYONE", "disable_comment": false, "is_aigc": true}))
	if err != nil {
		t.Fatal(err)
	}
	return f, res
}

func TestPublishSmallVideo(t *testing.T) {
	f, res := publishVideo(t, 3000, func(i int) string {
		if i < 2 {
			return `{"status":"PROCESSING_UPLOAD","uploaded_bytes":10}`
		}
		return `{"status":"PUBLISH_COMPLETE","publicaly_available_post_id":[7311234567890123456]}`
	})
	if res.RemoteID != "7311234567890123456" || res.URL != "https://www.tiktok.com/@handle/video/7311234567890123456" {
		t.Errorf("result %+v", res)
	}
	ci := f.find("POST /v2/post/publish/creator_info/query/")[0]
	if ci.CType != "application/json; charset=UTF-8" || ci.Auth != "Bearer AT" || len(ci.Body) != 0 {
		t.Errorf("creator_info %+v", ci)
	}
	init := jsonOf(t, f.find("POST /v2/post/publish/video/init/")[0].Body)
	pi := init["post_info"].(map[string]any)
	si := init["source_info"].(map[string]any)
	wantPI := map[string]any{"privacy_level": "PUBLIC_TO_EVERYONE", "title": "hello #go", "disable_comment": false,
		"disable_duet": true, "disable_stitch": true, "is_aigc": true, "brand_content_toggle": false, "brand_organic_toggle": false}
	for k, v := range wantPI {
		if pi[k] != v {
			t.Errorf("post_info.%s = %v want %v", k, pi[k], v)
		}
	}
	if len(pi) != len(wantPI) {
		t.Errorf("post_info has extra fields: %v", pi)
	}
	if si["source"] != "FILE_UPLOAD" || si["video_size"] != 3000.0 || si["chunk_size"] != 3000.0 || si["total_chunk_count"] != 1.0 {
		t.Errorf("source_info %v", si)
	}
	puts := f.find("PUT /upload/")
	if len(puts) != 1 || puts[0].Range != "bytes 0-2999/3000" || puts[0].CType != "video/mp4" || puts[0].Query != "upload_id=1&upload_token=x" {
		t.Errorf("puts %+v", puts)
	}
	st := f.find("POST /v2/post/publish/status/fetch/")
	if len(st) != 2 || string(st[0].Body) != `{"publish_id":"v_pub_1"}` {
		t.Errorf("status calls %d %s", len(st), st[0].Body)
	}
}

func TestPublishChunkedVideo(t *testing.T) {
	const mib = 1 << 20
	size := int64(45*mib + 123)
	f, res := publishVideo(t, size, func(int) string { return `{"status":"PUBLISH_COMPLETE"}` })
	if res.RemoteID != "publish:v_pub_1" {
		t.Errorf("private post id %+v", res)
	}
	si := jsonOf(t, f.find("POST /v2/post/publish/video/init/")[0].Body)["source_info"].(map[string]any)
	if si["chunk_size"] != float64(10*mib) || si["total_chunk_count"] != 4.0 {
		t.Errorf("source_info %v", si)
	}
	puts := f.find("PUT /upload/")
	want := []string{
		"bytes 0-10485759/47186043",
		"bytes 10485760-20971519/47186043",
		"bytes 20971520-31457279/47186043",
		"bytes 31457280-47186042/47186043",
	}
	if len(puts) != len(want) {
		t.Fatalf("%d puts", len(puts))
	}
	var pos int64
	for i, p := range puts {
		if p.Range != want[i] {
			t.Errorf("chunk %d range %q want %q", i, p.Range, want[i])
		}
		for j, b := range p.Body {
			if b != byte((pos+int64(j))%251) {
				t.Fatalf("chunk %d byte %d corrupted", i, j)
			}
		}
		pos += int64(len(p.Body))
	}
}

func TestPublishFailed(t *testing.T) {
	f := newFake(t)
	f.on("POST /v2/post/publish/creator_info/query/", ok(creator))
	f.on("POST /v2/post/publish/content/init/", ok(`{"publish_id":"p_1"}`))
	f.on("POST /v2/post/publish/status/fetch/", ok(`{"status":"FAILED","fail_reason":"picture_size_check_failed"}`))
	c, _ := newClient(t, f, nil)
	req := photoReq()
	_, err := c.Publish(context.Background(), req)
	if err == nil || providers.Retryable(err) || !strings.Contains(err.Error(), "picture_size_check_failed") {
		t.Fatalf("err %v", err)
	}
}

func photoReq() providers.PublishRequest {
	return providers.PublishRequest{
		Parts: []string{"First line title\nmore text"},
		Media: []providers.MediaFile{
			{Mime: "image/jpeg", PublicURL: "https://media.example.com/a.jpg"},
			{Mime: "image/webp", PublicURL: "https://media.example.com/b.webp"},
		},
		Options: map[string]any{"media_type": "PHOTO", "privacy_level": "SELF_ONLY", "is_aigc": false, "photo_cover_index": 1.0},
	}
}

func TestPublishPhoto(t *testing.T) {
	f := newFake(t)
	f.on("POST /v2/post/publish/creator_info/query/", ok(creator))
	f.on("POST /v2/post/publish/content/init/", ok(`{"publish_id":"p_1"}`))
	f.on("POST /v2/post/publish/status/fetch/", ok(`{"status":"PUBLISH_COMPLETE","publicaly_available_post_id":[]}`))
	c, _ := newClient(t, f, nil)
	res, err := c.Publish(context.Background(), photoReq())
	if err != nil {
		t.Fatal(err)
	}
	if res.RemoteID != "publish:p_1" {
		t.Errorf("res %+v", res)
	}
	body := jsonOf(t, f.find("POST /v2/post/publish/content/init/")[0].Body)
	if body["media_type"] != "PHOTO" || body["post_mode"] != "DIRECT_POST" || body["is_aigc"] != false {
		t.Errorf("body %v", body)
	}
	pi := body["post_info"].(map[string]any)
	if pi["title"] != "First line title" || pi["description"] != "First line title\nmore text" || pi["privacy_level"] != "SELF_ONLY" ||
		pi["disable_comment"] != true || pi["auto_add_music"] != false || pi["disable_duet"] != nil || pi["is_aigc"] != nil {
		t.Errorf("post_info %v", pi)
	}
	si := body["source_info"].(map[string]any)
	imgs := si["photo_images"].([]any)
	if si["source"] != "PULL_FROM_URL" || si["photo_cover_index"] != 1.0 || len(imgs) != 2 || imgs[0] != "https://media.example.com/a.jpg" {
		t.Errorf("source_info %v", si)
	}
}

func TestPrivacyNotOffered(t *testing.T) {
	f := newFake(t)
	f.on("POST /v2/post/publish/creator_info/query/", ok(`{"privacy_level_options":["SELF_ONLY"]}`))
	c, _ := newClient(t, f, nil)
	_, err := c.Publish(context.Background(), videoReq(10, map[string]any{"privacy_level": "PUBLIC_TO_EVERYONE"}))
	if err == nil || providers.Retryable(err) {
		t.Fatalf("err %v", err)
	}
	if len(f.find("POST /v2/post/publish/video/init/")) != 0 {
		t.Error("init must not be called")
	}
}

func TestMetricsAndHistory(t *testing.T) {
	f := newFake(t)
	f.on("POST /v2/video/query/", func(w http.ResponseWriter, h hit) {
		ok(`{"videos":[{"id":"111","view_count":100,"like_count":5,"comment_count":2,"share_count":1}],"cursor":0,"has_more":false}`)(w, h)
	})
	page := 0
	f.on("POST /v2/video/list/", func(w http.ResponseWriter, h hit) {
		page++
		if page == 1 {
			ok(`{"videos":[{"id":"2","title":"t2","video_description":"d2","create_time":1700000200,"share_url":"https://www.tiktok.com/@h/video/2","view_count":9}],"cursor":1700000200000,"has_more":true}`)(w, h)
			return
		}
		ok(`{"videos":[{"id":"1","title":"t1","create_time":1700000100},{"id":"0","create_time":1600000000}],"cursor":1600000000000,"has_more":true}`)(w, h)
	})
	c, _ := newClient(t, f, nil)
	ids := []string{"publish:x"}
	for i := 0; i < 21; i++ {
		ids = append(ids, "111")
	}
	m, err := c.Metrics(context.Background(), ids)
	if err != nil {
		t.Fatal(err)
	}
	if m["111"]["view_count"] != 100 || m["111"]["like_count"] != 5 || m["111"]["comment_count"] != 2 || m["111"]["share_count"] != 1 {
		t.Errorf("metrics %v", m)
	}
	q := f.find("POST /v2/video/query/")
	if len(q) != 2 || q[0].Query != "fields="+videoFields {
		t.Fatalf("query calls %+v", q)
	}
	if n := len(jsonOf(t, q[0].Body)["filters"].(map[string]any)["video_ids"].([]any)); n != 20 {
		t.Errorf("batch of %d ids", n)
	}

	items, err := c.History(context.Background(), time.Unix(1650000000, 0), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].RemoteID != "2" || items[0].Text != "d2" || items[1].Text != "t1" || items[0].Kind != "video" || items[0].Metrics["view_count"] != 9 {
		t.Errorf("items %+v", items)
	}
	l := f.find("POST /v2/video/list/")
	if b := jsonOf(t, l[0].Body); b["max_count"] != 20.0 || b["cursor"] != nil {
		t.Errorf("first page %v", b)
	}
	if b := jsonOf(t, l[1].Body); b["cursor"] != 1700000200000.0 {
		t.Errorf("second page %v", b)
	}
}

func TestUnsupported(t *testing.T) {
	c := &Client{}
	if _, err := c.Comments(context.Background(), "1"); err != providers.ErrUnsupported {
		t.Error(err)
	}
	if _, err := c.Reply(context.Background(), "1", "x"); err != providers.ErrUnsupported {
		t.Error(err)
	}
}

func TestValidate(t *testing.T) {
	pub := map[string]any{"privacy_level": "PUBLIC_TO_EVERYONE"}
	img := func(mime string) providers.MediaFile { return providers.MediaFile{Mime: mime, Filename: "f"} }
	cases := []struct {
		name string
		req  providers.PublishRequest
		want string
	}{
		{"ok video", videoReq(10, pub), ""},
		{"no privacy", videoReq(10, nil), "privacy_level"},
		{"bad privacy", videoReq(10, map[string]any{"privacy_level": "PUBLIC"}), "privacy_level"},
		{"branded private", videoReq(10, map[string]any{"privacy_level": "SELF_ONLY", "brand_content_toggle": true}), "branded"},
		{"no media", providers.PublishRequest{Options: pub}, "needs a video"},
		{"bad video mime", providers.PublishRequest{Media: []providers.MediaFile{{Mime: "video/x-msvideo"}}, Options: pub}, "MP4"},
		{"too big", videoReq(5<<30, pub), "4 GB"},
		{"two videos", providers.PublishRequest{Media: []providers.MediaFile{fakeVideo(1), fakeVideo(1)}, Options: pub}, "exactly one"},
		{"caption utf16", providers.PublishRequest{Parts: []string{strings.Repeat("😀", 1101)}, Media: []providers.MediaFile{fakeVideo(1)}, Options: pub}, "2200"},
		{"caption ok", providers.PublishRequest{Parts: []string{strings.Repeat("😀", 1100)}, Media: []providers.MediaFile{fakeVideo(1)}, Options: pub}, ""},
		{"ok photo", providers.PublishRequest{Media: []providers.MediaFile{img("image/jpeg")}, Options: pub}, ""},
		{"png photo", providers.PublishRequest{Media: []providers.MediaFile{img("image/png")}, Options: pub}, "JPEG or WebP"},
		{"photo title", providers.PublishRequest{Media: []providers.MediaFile{img("image/webp")}, Options: map[string]any{"privacy_level": "SELF_ONLY", "title": strings.Repeat("a", 91)}}, "90"},
		{"photo desc", providers.PublishRequest{Parts: []string{strings.Repeat("a", 4001)}, Media: []providers.MediaFile{img("image/webp")}, Options: pub}, "4000"},
		{"36 photos", providers.PublishRequest{Media: make36(img("image/jpeg")), Options: pub}, "35"},
		{"photo with video", providers.PublishRequest{Media: []providers.MediaFile{img("image/jpeg"), fakeVideo(1)}, Options: map[string]any{"privacy_level": "SELF_ONLY", "media_type": "PHOTO"}}, "JPEG or WebP"},
	}
	for _, tc := range cases {
		errs := strings.Join(validate(tc.req), "; ")
		if (tc.want == "") != (errs == "") || !strings.Contains(errs, tc.want) {
			t.Errorf("%s: got %q want %q", tc.name, errs, tc.want)
		}
	}
}

func make36(m providers.MediaFile) []providers.MediaFile {
	out := make([]providers.MediaFile, 36)
	for i := range out {
		out[i] = m
	}
	return out
}

func TestFirstLineUTF16(t *testing.T) {
	if got := firstLine(strings.Repeat("😀", 50), 90); len([]rune(got)) != 45 {
		t.Errorf("got %d runes", len([]rune(got)))
	}
}
