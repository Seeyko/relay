package youtube

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Seeyko/relay/internal/providers"
)

type fake struct {
	t   *testing.T
	srv *httptest.Server
	mu  sync.Mutex
	log []string
	// routes are keyed by "METHOD /path".
	routes map[string]http.HandlerFunc
}

func newFake(t *testing.T) *fake {
	f := &fake{t: t, routes: map[string]http.HandlerFunc{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		f.mu.Lock()
		f.log = append(f.log, key)
		h := f.routes[key]
		f.mu.Unlock()
		if h == nil {
			t.Errorf("unexpected request %s", key)
			http.Error(w, "nope", 404)
			return
		}
		h(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fake) client(t *testing.T, creds map[string]string, persist func(map[string]string) error) *Client {
	t.Helper()
	c := map[string]string{
		"client_id": "cid", "client_secret": "csecret", "refresh_token": "rt",
		"access_token": "at", "expires_at": "9999999999",
		"base_url": f.srv.URL, "upload_base_url": f.srv.URL, "analytics_base_url": f.srv.URL, "token_url": f.srv.URL + "/token",
	}
	for k, v := range creds {
		c[k] = v
	}
	fac, err := providers.Get("youtube")
	if err != nil {
		t.Fatal(err)
	}
	cl, err := fac.New(c, persist, f.srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	yc := cl.(*Client)
	yc.RetryWait = time.Millisecond
	return yc
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func checkBearer(t *testing.T, r *http.Request, want string) {
	t.Helper()
	if got := r.Header.Get("Authorization"); got != "Bearer "+want {
		t.Errorf("%s %s: Authorization = %q, want Bearer %s", r.Method, r.URL.Path, got, want)
	}
}

func TestRefreshOn401AndPersist(t *testing.T) {
	f := newFake(t)
	calls := 0
	f.routes["POST /token"] = func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
			t.Errorf("token content-type %q", ct)
		}
		_ = r.ParseForm()
		want := map[string]string{"client_id": "cid", "client_secret": "csecret", "grant_type": "refresh_token", "refresh_token": "rt"}
		for k, v := range want {
			if r.PostForm.Get(k) != v {
				t.Errorf("token form %s = %q, want %q", k, r.PostForm.Get(k), v)
			}
		}
		writeJSON(w, map[string]any{"access_token": "new", "expires_in": 3599, "scope": "x", "token_type": "Bearer"})
	}
	f.routes["GET /youtube/v3/channels"] = func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			checkBearer(t, r, "old")
			w.WriteHeader(401)
			writeJSON(w, map[string]any{"error": map[string]any{"code": 401, "errors": []map[string]string{{"reason": "authError"}}}})
			return
		}
		checkBearer(t, r, "new")
		q := r.URL.Query()
		if q.Get("mine") != "true" || q.Get("part") != "id,snippet,contentDetails" {
			t.Errorf("channels query %v", q)
		}
		writeJSON(w, map[string]any{"items": []any{map[string]any{
			"id":             "UC123",
			"snippet":        map[string]any{"title": "My Channel", "customUrl": "@me"},
			"contentDetails": map[string]any{"relatedPlaylists": map[string]any{"uploads": "UU123"}},
		}}})
	}
	var saved map[string]string
	c := f.client(t, map[string]string{"access_token": "old"}, func(m map[string]string) error { saved = m; return nil })
	id, err := c.Identify(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if id.RemoteUserID != "UC123" || id.Handle != "@me" || id.Name != "My Channel" {
		t.Errorf("identity %+v", id)
	}
	if saved["access_token"] != "new" || saved["refresh_token"] != "rt" || saved["expires_at"] == "" {
		t.Errorf("persisted creds not updated")
	}
	if c.creds["uploads_playlist_id"] != "UU123" {
		t.Errorf("uploads playlist not cached")
	}
}

func TestRefreshWhenExpired(t *testing.T) {
	f := newFake(t)
	f.routes["POST /token"] = func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"access_token": "fresh", "expires_in": 3599})
	}
	f.routes["GET /youtube/v3/channels"] = func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r, "fresh")
		if r.URL.Query().Get("part") != "statistics,snippet" {
			t.Errorf("part %q", r.URL.Query().Get("part"))
		}
		writeJSON(w, map[string]any{"items": []any{map[string]any{"id": "UC1", "statistics": map[string]any{
			"viewCount": "12345", "subscriberCount": "678", "hiddenSubscriberCount": false, "videoCount": "42",
		}}}})
	}
	c := f.client(t, map[string]string{"expires_at": "1"}, nil)
	m, err := c.AccountMetrics(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if m["followers"] != 678 || m["posts"] != 42 || m["views_total"] != 12345 {
		t.Errorf("account metrics %v", m)
	}
}

func TestInvalidGrant(t *testing.T) {
	f := newFake(t)
	f.routes["POST /token"] = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		writeJSON(w, map[string]any{"error": "invalid_grant", "error_description": "Token has been expired or revoked."})
	}
	c := f.client(t, map[string]string{"access_token": "", "expires_at": ""}, nil)
	_, err := c.AccountMetrics(context.Background())
	if !errors.Is(err, providers.ErrInvalidPost) {
		t.Fatalf("want non-retryable error, got %v", err)
	}
}

func videoBytes(n int) []byte { return bytes.Repeat([]byte("v"), n) }

func media(data []byte) providers.MediaFile {
	return providers.MediaFile{
		ID: "m1", Filename: "clip.mp4", Mime: "video/mp4", Size: int64(len(data)),
		Open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil },
	}
}

func TestPublishResumableUpload(t *testing.T) {
	f := newFake(t)
	data := videoBytes(1000)
	f.routes["POST /upload/youtube/v3/videos"] = func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r, "at")
		q := r.URL.Query()
		if q.Get("uploadType") != "resumable" || q.Get("part") != "snippet,status" || q.Get("notifySubscribers") != "false" {
			t.Errorf("init query %v", q)
		}
		if r.Header.Get("X-Upload-Content-Length") != "1000" || r.Header.Get("X-Upload-Content-Type") != "video/mp4" {
			t.Errorf("init headers %v", r.Header)
		}
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			t.Errorf("init content-type %q", r.Header.Get("Content-Type"))
		}
		var body struct {
			Snippet struct {
				Title, Description, CategoryID string
				Tags                           []string
			}
			Status map[string]any
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		if body.Snippet.Title != "My first Short" {
			t.Errorf("title %q", body.Snippet.Title)
		}
		if body.Snippet.Description != "My first Short\nmore text\n\n#Shorts" {
			t.Errorf("description %q", body.Snippet.Description)
		}
		if !strings.Contains(string(raw), `"categoryId":"22"`) || len(body.Snippet.Tags) != 2 {
			t.Errorf("snippet %s", raw)
		}
		if body.Status["privacyStatus"] != "unlisted" || body.Status["selfDeclaredMadeForKids"] != false {
			t.Errorf("status %v", body.Status)
		}
		w.Header().Set("Location", f.srv.URL+"/upload/session?upload_id=abc")
		w.WriteHeader(200)
	}
	puts := 0
	f.routes["PUT /upload/session"] = func(w http.ResponseWriter, r *http.Request) {
		puts++
		checkBearer(t, r, "at")
		b, _ := io.ReadAll(r.Body)
		switch puts {
		case 1: // full upload: fail with 503 after receiving part of it
			if r.ContentLength != 1000 || r.Header.Get("Content-Type") != "video/mp4" || r.Header.Get("Content-Range") != "" {
				t.Errorf("first PUT len=%d headers=%v", r.ContentLength, r.Header)
			}
			w.WriteHeader(503)
		case 2: // status query
			if r.Header.Get("Content-Range") != "bytes */1000" || len(b) != 0 {
				t.Errorf("status PUT range %q body %d", r.Header.Get("Content-Range"), len(b))
			}
			w.Header().Set("Range", "bytes=0-399")
			w.WriteHeader(308)
		case 3: // resume
			if r.Header.Get("Content-Range") != "bytes 400-999/1000" || len(b) != 600 || r.ContentLength != 600 {
				t.Errorf("resume PUT range %q len %d", r.Header.Get("Content-Range"), len(b))
			}
			w.WriteHeader(201)
			writeJSON(w, map[string]any{"id": "vid12345678", "status": map[string]any{"uploadStatus": "uploaded"}})
		default:
			t.Errorf("too many PUTs")
		}
	}
	c := f.client(t, nil, nil)
	res, err := c.Publish(context.Background(), providers.PublishRequest{
		Parts: []string{"My first Short\nmore text"},
		Media: []providers.MediaFile{media(data)},
		Options: map[string]any{
			"privacy_status": "unlisted", "category_id": "22", "tags": []any{"go", "relay app"},
			"shorts": true, "notify_subscribers": false,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.RemoteID != "vid12345678" || res.URL != "https://www.youtube.com/shorts/vid12345678" {
		t.Errorf("result %+v", res)
	}
}

func TestPublishRejectsBadMetadata(t *testing.T) {
	f := newFake(t)
	f.routes["POST /upload/youtube/v3/videos"] = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		writeJSON(w, map[string]any{"error": map[string]any{"code": 400, "errors": []map[string]string{{"reason": "invalidCategoryId"}}}})
	}
	c := f.client(t, nil, nil)
	_, err := c.Publish(context.Background(), providers.PublishRequest{Parts: []string{"t"}, Media: []providers.MediaFile{media(videoBytes(10))}})
	if !errors.Is(err, providers.ErrInvalidPost) {
		t.Fatalf("want invalid, got %v", err)
	}
}

func TestQuotaIsRetryable(t *testing.T) {
	f := newFake(t)
	f.routes["GET /youtube/v3/videos"] = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		writeJSON(w, map[string]any{"error": map[string]any{"code": 403, "errors": []map[string]string{{"reason": "quotaExceeded"}}}})
	}
	c := f.client(t, nil, nil)
	_, err := c.Metrics(context.Background(), []string{"a"})
	if !providers.Retryable(err) {
		t.Fatalf("quota error should be retryable: %v", err)
	}
}

func TestMetricsBatchesAndAnalytics(t *testing.T) {
	f := newFake(t)
	ids := make([]string, 60)
	for i := range ids {
		ids[i] = "v" + string(rune('A'+i%26)) + string(rune('a'+i/26))
	}
	var batches []int
	f.routes["GET /youtube/v3/videos"] = func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("part") != "statistics" {
			t.Errorf("part %q", q.Get("part"))
		}
		got := strings.Split(q.Get("id"), ",")
		batches = append(batches, len(got))
		var items []any
		for _, id := range got {
			items = append(items, map[string]any{"id": id, "statistics": map[string]any{
				"viewCount": "100", "likeCount": "7", "favoriteCount": "0", "commentCount": "3",
			}})
		}
		writeJSON(w, map[string]any{"items": items})
	}
	f.routes["GET /v2/reports"] = func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("ids") != "channel==MINE" || q.Get("dimensions") != "video" || !strings.HasPrefix(q.Get("filters"), "video==") ||
			q.Get("startDate") == "" || q.Get("endDate") == "" || !strings.Contains(q.Get("metrics"), "estimatedMinutesWatched") {
			t.Errorf("analytics query %v", q)
		}
		writeJSON(w, map[string]any{
			"columnHeaders": []any{
				map[string]any{"name": "video"}, map[string]any{"name": "views"}, map[string]any{"name": "estimatedMinutesWatched"},
				map[string]any{"name": "averageViewDuration"}, map[string]any{"name": "subscribersGained"}, map[string]any{"name": "shares"},
			},
			"rows": []any{[]any{ids[0], 100, 250.0, 41, 2, 5}},
		})
	}
	c := f.client(t, nil, nil)
	m, err := c.Metrics(context.Background(), ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(batches) != 2 || batches[0] != 50 || batches[1] != 10 {
		t.Errorf("batches %v", batches)
	}
	got := m[ids[0]]
	if got["view_count"] != 100 || got["like_count"] != 7 || got["comment_count"] != 3 || got["estimated_minutes_watched"] != 250 ||
		got["average_view_duration"] != 41 || got["subscribers_gained"] != 2 || got["share_count"] != 5 {
		t.Errorf("metrics %v", got)
	}
	if _, ok := m[ids[1]]["estimated_minutes_watched"]; ok || len(m) != 60 {
		t.Errorf("unexpected analytics merge")
	}
}

func TestCommentsAndReply(t *testing.T) {
	f := newFake(t)
	pages := 0
	f.routes["GET /youtube/v3/commentThreads"] = func(w http.ResponseWriter, r *http.Request) {
		pages++
		q := r.URL.Query()
		if q.Get("part") != "snippet,replies" || q.Get("videoId") != "vid12345678" || q.Get("maxResults") != "100" || q.Get("textFormat") != "plainText" {
			t.Errorf("threads query %v", q)
		}
		cm := func(id, text string) map[string]any {
			return map[string]any{"id": id, "snippet": map[string]any{"authorDisplayName": "Ann", "textDisplay": text, "publishedAt": "2026-01-02T03:04:05Z"}}
		}
		if pages == 1 {
			writeJSON(w, map[string]any{"nextPageToken": "p2", "items": []any{map[string]any{
				"snippet": map[string]any{"topLevelComment": cm("Ugx1", "nice"), "totalReplyCount": 1},
				"replies": map[string]any{"comments": []any{cm("Ugx1.r1", "thanks")}},
			}}})
			return
		}
		if q.Get("pageToken") != "p2" {
			t.Errorf("pageToken %q", q.Get("pageToken"))
		}
		writeJSON(w, map[string]any{"items": []any{map[string]any{"snippet": map[string]any{"topLevelComment": cm("Ugx2", "wow")}}}})
	}
	f.routes["POST /youtube/v3/comments"] = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("part") != "snippet" {
			t.Errorf("part %q", r.URL.Query().Get("part"))
		}
		var body map[string]map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["snippet"]["parentId"] != "Ugx1" || body["snippet"]["textOriginal"] != "hello" {
			t.Errorf("reply body %v", body)
		}
		writeJSON(w, map[string]any{"id": "Ugx1.new"})
	}
	c := f.client(t, nil, nil)
	cs, err := c.Comments(context.Background(), "vid12345678")
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 3 || cs[1].RemoteID != "Ugx1.r1" || cs[0].Text != "nice" || cs[0].Author != "Ann" || cs[0].CreatedAt == nil ||
		cs[0].URL != "https://www.youtube.com/watch?v=vid12345678&lc=Ugx1" {
		t.Errorf("comments %+v", cs)
	}
	res, err := c.Reply(context.Background(), "Ugx1.r1", "hello") // a reply to a reply goes under its top-level parent
	if err != nil || res.RemoteID != "Ugx1.new" {
		t.Fatalf("reply %v %v", res, err)
	}
}

func TestCommentsDisabled(t *testing.T) {
	f := newFake(t)
	f.routes["GET /youtube/v3/commentThreads"] = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		writeJSON(w, map[string]any{"error": map[string]any{"code": 403, "errors": []map[string]string{{"reason": "commentsDisabled"}}}})
	}
	cs, err := f.client(t, nil, nil).Comments(context.Background(), "x")
	if err != nil || len(cs) != 0 {
		t.Fatalf("got %v %v", cs, err)
	}
}

func TestTopLevelCommentOnVideo(t *testing.T) {
	f := newFake(t)
	f.routes["POST /youtube/v3/commentThreads"] = func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		want := `{"snippet":{"channelId":"UC1","topLevelComment":{"snippet":{"textOriginal":"first"}},"videoId":"vid12345678"}}`
		if string(raw) != want {
			t.Errorf("body %s", raw)
		}
		writeJSON(w, map[string]any{"id": "UgxT"})
	}
	res, err := f.client(t, map[string]string{"channel_id": "UC1"}, nil).Reply(context.Background(), "vid12345678", "first")
	if err != nil || res.RemoteID != "UgxT" {
		t.Fatalf("%v %v", res, err)
	}
}

func TestHistoryPagination(t *testing.T) {
	f := newFake(t)
	f.routes["GET /youtube/v3/channels"] = func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"items": []any{map[string]any{"id": "UC1",
			"contentDetails": map[string]any{"relatedPlaylists": map[string]any{"uploads": "UU1"}}}}})
	}
	pl := 0
	f.routes["GET /youtube/v3/playlistItems"] = func(w http.ResponseWriter, r *http.Request) {
		pl++
		q := r.URL.Query()
		if q.Get("playlistId") != "UU1" || q.Get("maxResults") != "50" || q.Get("part") != "contentDetails" {
			t.Errorf("playlist query %v", q)
		}
		item := func(id, at string) map[string]any {
			return map[string]any{"contentDetails": map[string]any{"videoId": id, "videoPublishedAt": at}}
		}
		switch pl {
		case 1:
			writeJSON(w, map[string]any{"nextPageToken": "n2", "items": []any{item("s1", "2026-03-01T00:00:00Z"), item("l1", "2026-02-01T00:00:00Z")}})
		case 2:
			if q.Get("pageToken") != "n2" {
				t.Errorf("pageToken %q", q.Get("pageToken"))
			}
			writeJSON(w, map[string]any{"nextPageToken": "n3", "items": []any{item("o1", "2025-01-01T00:00:00Z")}})
		default:
			t.Errorf("walked past since")
		}
	}
	f.routes["GET /youtube/v3/videos"] = func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("part") != "snippet,contentDetails,statistics" || q.Get("id") != "s1,l1" {
			t.Errorf("videos query %v", q)
		}
		writeJSON(w, map[string]any{"items": []any{
			map[string]any{"id": "s1", "snippet": map[string]any{"title": "Clip #shorts", "description": "", "publishedAt": "2026-03-01T00:00:00Z",
				"thumbnails": map[string]any{"default": map[string]any{"url": "https://i.ytimg.com/vi/s1/default.jpg"}, "high": map[string]any{"url": "https://i.ytimg.com/vi/s1/hqdefault.jpg"}}},
				"contentDetails": map[string]any{"duration": "PT45S"}, "statistics": map[string]any{"viewCount": "9"}},
			map[string]any{"id": "l1", "snippet": map[string]any{"title": "Long", "description": "Deep dive", "publishedAt": "2026-02-01T00:00:00Z",
				"thumbnails": map[string]any{"medium": map[string]any{"url": "https://i.ytimg.com/vi/l1/mqdefault.jpg"}, "maxres": map[string]any{"url": "https://i.ytimg.com/vi/l1/maxresdefault.jpg"}}},
				"contentDetails": map[string]any{"duration": "PT1H2M3S"}, "statistics": map[string]any{"viewCount": "90"}},
		}})
	}
	c := f.client(t, nil, nil)
	items, err := c.History(context.Background(), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].Kind != "short" || items[1].Kind != "video" || items[1].Text != "Long\n\nDeep dive" ||
		items[1].Metrics["view_count"] != 90 || items[0].URL != "https://www.youtube.com/watch?v=s1" {
		t.Errorf("history %+v", items)
	}
	if !reflect.DeepEqual(items[0].Visuals, []providers.Visual{{URL: "https://i.ytimg.com/vi/s1/hqdefault.jpg", Cover: true}}) ||
		!reflect.DeepEqual(items[1].Visuals, []providers.Visual{{URL: "https://i.ytimg.com/vi/l1/maxresdefault.jpg", Cover: true}}) {
		t.Errorf("visuals %+v %+v", items[0].Visuals, items[1].Visuals)
	}
}

func TestValidate(t *testing.T) {
	v := media(videoBytes(1))
	img := providers.MediaFile{Mime: "image/png"}
	cases := []struct {
		name string
		req  providers.PublishRequest
		want string
	}{
		{"ok", providers.PublishRequest{Parts: []string{"Title\nbody"}, Media: []providers.MediaFile{v}}, ""},
		{"no media", providers.PublishRequest{Parts: []string{"t"}}, "exactly one video"},
		{"image", providers.PublishRequest{Parts: []string{"t"}, Media: []providers.MediaFile{img}}, "exactly one video"},
		{"no title", providers.PublishRequest{Media: []providers.MediaFile{v}}, "needs a title"},
		{"long title", providers.PublishRequest{Parts: []string{"t"}, Media: []providers.MediaFile{v}, Options: map[string]any{"title": strings.Repeat("a", 101)}}, "100 characters"},
		{"angle", providers.PublishRequest{Parts: []string{"a <b>"}, Media: []providers.MediaFile{v}}, "< or >"},
		{"desc bytes", providers.PublishRequest{Parts: []string{"t\n" + strings.Repeat("é", 2500)}, Media: []providers.MediaFile{v}}, "5000 bytes"},
		{"tags", providers.PublishRequest{Parts: []string{"t"}, Media: []providers.MediaFile{v}, Options: map[string]any{"tags": []any{strings.Repeat("a", 250), strings.Repeat("b", 250)}}}, "tags total 501"},
		{"tags space", providers.PublishRequest{Parts: []string{"t"}, Media: []providers.MediaFile{v}, Options: map[string]any{"tags": []any{strings.Repeat("a", 495), "b c"}}}, "tags total 501"},
		{"privacy", providers.PublishRequest{Parts: []string{"t"}, Media: []providers.MediaFile{v}, Options: map[string]any{"privacy_status": "friends"}}, "privacy_status"},
		{"category", providers.PublishRequest{Parts: []string{"t"}, Media: []providers.MediaFile{v}, Options: map[string]any{"category_id": "music"}}, "category_id"},
	}
	for _, tc := range cases {
		errs := strings.Join(validate(tc.req), "; ")
		if tc.want == "" && errs != "" {
			t.Errorf("%s: unexpected %q", tc.name, errs)
		}
		if tc.want != "" && !strings.Contains(errs, tc.want) {
			t.Errorf("%s: got %q, want %q", tc.name, errs, tc.want)
		}
	}
	if n := tagsLength([]string{"Foo-Baz"}); n != 7 {
		t.Errorf("Foo-Baz = %d", n)
	}
	if n := tagsLength([]string{"Foo Baz"}); n != 9 {
		t.Errorf("Foo Baz = %d", n)
	}
}

func TestParseDuration(t *testing.T) {
	for s, want := range map[string]int64{"PT15M33S": 933, "PT1H2M3S": 3723, "P1DT2H": 93600, "PT45S": 45, "": 0} {
		if got := parseDuration(s); got != want {
			t.Errorf("%s = %d, want %d", s, got, want)
		}
	}
}
