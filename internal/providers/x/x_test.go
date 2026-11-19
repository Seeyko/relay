package x

import (
	"bytes"
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

type call struct {
	Method, Path string
	Query        url.Values
	ContentType  string
	Auth         string
	Body         []byte
	Form         map[string][]string // multipart text fields
	File         []byte              // multipart "media" part
}

// fakeX records every request and answers with handler.
func fakeX(t *testing.T, handler func(c call, w http.ResponseWriter)) (*Client, *[]call) {
	t.Helper()
	var mu sync.Mutex
	var calls []call
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := call{Method: r.Method, Path: r.URL.Path, Query: r.URL.Query(), ContentType: r.Header.Get("Content-Type"), Auth: r.Header.Get("Authorization")}
		if strings.HasPrefix(c.ContentType, "multipart/form-data") {
			if err := r.ParseMultipartForm(32 << 20); err != nil {
				t.Errorf("bad multipart: %v", err)
			}
			c.Form = r.MultipartForm.Value
			if fh := r.MultipartForm.File["media"]; len(fh) == 1 {
				f, _ := fh[0].Open()
				c.File, _ = io.ReadAll(f)
				f.Close()
			}
		} else {
			c.Body, _ = io.ReadAll(r.Body)
		}
		mu.Lock()
		calls = append(calls, c)
		mu.Unlock()
		handler(c, w)
	}))
	t.Cleanup(srv.Close)
	f, err := providers.Get("x")
	if err != nil {
		t.Fatal(err)
	}
	cl, err := f.New(map[string]string{"consumer_key": "ck", "consumer_secret": "cs", "access_token": "at", "access_token_secret": "as", "base_url": srv.URL, "user_id": "42", "username": "alice"}, nil, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	c := cl.(*Client)
	c.PollInterval = time.Millisecond
	return c, &calls
}

func mustJSON(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("body is not JSON: %q", b)
	}
	return m
}

func file(data []byte, name, mime string) providers.MediaFile {
	return providers.MediaFile{Filename: name, Mime: mime, Size: int64(len(data)), Open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }}
}

func TestPublishThreadWithImage(t *testing.T) {
	n := 0
	c, calls := fakeX(t, func(cl call, w http.ResponseWriter) {
		switch {
		case cl.Method == "POST" && cl.Path == "/2/media/upload":
			w.Write([]byte(`{"data":{"id":"m1","media_key":"3_m1","size":3,"image":{"w":1,"h":1,"image_type":"image/png"}}}`))
		case cl.Method == "POST" && cl.Path == "/2/tweets":
			n++
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": []string{"", "t1", "t2"}[n], "text": "x"}})
		default:
			t.Errorf("unexpected %s %s", cl.Method, cl.Path)
			w.WriteHeader(404)
		}
	})
	res, err := c.Publish(context.Background(), providers.PublishRequest{Parts: []string{"one", "two"}, Media: []providers.MediaFile{file([]byte("png"), "a.png", "image/png")}})
	if err != nil {
		t.Fatal(err)
	}
	if res.RemoteID != "t1" || len(res.RemoteIDs) != 2 || res.URL != "https://x.com/alice/status/t1" {
		t.Fatalf("result %+v", res)
	}
	cs := *calls
	if len(cs) != 3 {
		t.Fatalf("calls %d", len(cs))
	}
	up := cs[0]
	if !strings.HasPrefix(up.Auth, "OAuth ") || !strings.Contains(up.Auth, `oauth_consumer_key="ck"`) || !strings.Contains(up.Auth, `oauth_token="at"`) {
		t.Fatalf("auth header %q", up.Auth)
	}
	// schema allows only media, media_category, additional_owners
	if string(up.File) != "png" || len(up.Form) != 1 || up.Form["media_category"][0] != "tweet_image" {
		t.Fatalf("upload form %v file %q", up.Form, up.File)
	}
	first := mustJSON(t, cs[1].Body)
	if cs[1].ContentType != "application/json" || first["text"] != "one" || first["reply"] != nil {
		t.Fatalf("first tweet %v", first)
	}
	if ids := first["media"].(map[string]any)["media_ids"].([]any); len(ids) != 1 || ids[0] != "m1" {
		t.Fatalf("media ids %v", ids)
	}
	second := mustJSON(t, cs[2].Body)
	if second["media"] != nil || second["reply"].(map[string]any)["in_reply_to_tweet_id"] != "t1" {
		t.Fatalf("second tweet %v", second)
	}
}

func TestMediaOnlyPostOmitsText(t *testing.T) {
	c, calls := fakeX(t, func(cl call, w http.ResponseWriter) {
		if cl.Path == "/2/media/upload" {
			w.Write([]byte(`{"data":{"id":"m1"}}`))
			return
		}
		w.WriteHeader(201)
		w.Write([]byte(`{"data":{"id":"t1","text":""}}`))
	})
	if _, err := c.Publish(context.Background(), providers.PublishRequest{Parts: []string{""}, Media: []providers.MediaFile{file([]byte("j"), "a.jpg", "image/jpeg")}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := mustJSON(t, (*calls)[1].Body)["text"]; ok {
		t.Fatal("empty text must be omitted")
	}
}

func TestCreateTweetErrorsWith200(t *testing.T) {
	c, _ := fakeX(t, func(cl call, w http.ResponseWriter) {
		w.Write([]byte(`{"errors":[{"title":"Forbidden","detail":"duplicate content"}]}`))
	})
	_, err := c.Reply(context.Background(), "1", "hi")
	if err == nil || !strings.Contains(err.Error(), "duplicate content") {
		t.Fatalf("err %v", err)
	}
}

func TestChunkedVideoUpload(t *testing.T) {
	video := bytes.Repeat([]byte("v"), chunkSize+10)
	statusCalls := 0
	c, calls := fakeX(t, func(cl call, w http.ResponseWriter) {
		switch {
		case cl.Method == "POST" && cl.Path == "/2/media/upload/initialize":
			w.Write([]byte(`{"data":{"id":"77","media_key":"7_77","expires_after_secs":86400}}`))
		case cl.Method == "POST" && cl.Path == "/2/media/upload/77/append":
			w.Write([]byte(`{"data":{"expires_at":1}}`))
		case cl.Method == "POST" && cl.Path == "/2/media/upload/77/finalize":
			w.Write([]byte(`{"data":{"id":"77","processing_info":{"state":"pending","check_after_secs":1}}}`))
		case cl.Method == "GET" && cl.Path == "/2/media/upload":
			statusCalls++
			state := "in_progress"
			if statusCalls > 1 {
				state = "succeeded"
			}
			w.Write([]byte(`{"data":{"id":"77","processing_info":{"state":"` + state + `","progress_percent":50}}}`))
		default:
			t.Errorf("unexpected %s %s", cl.Method, cl.Path)
			w.WriteHeader(404)
		}
	})
	id, err := c.UploadMedia(context.Background(), file(video, "v.mp4", "video/mp4"))
	if err != nil || id != "77" {
		t.Fatalf("id %q err %v", id, err)
	}
	cs := *calls
	ini := mustJSON(t, cs[0].Body)
	if cs[0].ContentType != "application/json" || ini["media_type"] != "video/mp4" || ini["total_bytes"] != float64(len(video)) || ini["media_category"] != "tweet_video" || len(ini) != 3 {
		t.Fatalf("initialize body %v", ini)
	}
	for i, want := range []int{chunkSize, 10} {
		a := cs[1+i]
		if a.Form["segment_index"][0] != []string{"0", "1"}[i] || len(a.File) != want {
			t.Fatalf("append %d: form %v len %d", i, a.Form, len(a.File))
		}
	}
	if fin := cs[3]; len(fin.Body) != 0 || fin.ContentType != "" {
		t.Fatalf("finalize must have no body: %q %q", fin.Body, fin.ContentType)
	}
	st := cs[4]
	if st.Query.Get("command") != "STATUS" || st.Query.Get("media_id") != "77" || len(cs) != 6 {
		t.Fatalf("status query %v, %d calls", st.Query, len(cs))
	}
}

func TestChunkedUploadFailure(t *testing.T) {
	c, _ := fakeX(t, func(cl call, w http.ResponseWriter) {
		switch cl.Path {
		case "/2/media/upload/initialize":
			w.Write([]byte(`{"data":{"id":"5"}}`))
		case "/2/media/upload/5/finalize":
			w.Write([]byte(`{"data":{"id":"5","processing_info":{"state":"failed"}},"errors":[{"title":"InvalidMedia","detail":"Unsupported codec"}]}`))
		default:
			w.Write([]byte(`{}`))
		}
	})
	_, err := c.UploadMedia(context.Background(), file([]byte("gif"), "a.gif", "image/gif"))
	if err == nil || !strings.Contains(err.Error(), "Unsupported codec") || providers.Retryable(err) {
		t.Fatalf("err %v", err)
	}
}

func TestMetricsFallsBackToPublic(t *testing.T) {
	c, calls := fakeX(t, func(cl call, w http.ResponseWriter) {
		fields := cl.Query.Get("tweet.fields")
		if strings.Contains(fields, "non_public_metrics") {
			// own recent post gets private metrics, the other one a per-post error
			w.Write([]byte(`{"data":[{"id":"1","public_metrics":{"like_count":3,"impression_count":100},"non_public_metrics":{"engagements":9},"attachments":{"media_keys":["7_1"]}}],
				"includes":{"media":[{"media_key":"7_1","type":"video","public_metrics":{"view_count":40}}]},
				"errors":[{"value":"2","detail":"Sorry, you are not authorized to access 'non_public_metrics' on the Tweet with ids: [2]."}]}`))
			return
		}
		w.Write([]byte(`{"data":[{"id":"2","public_metrics":{"like_count":1}}]}`))
	})
	got, err := c.Metrics(context.Background(), []string{"1", "2"})
	if err != nil {
		t.Fatal(err)
	}
	if got["1"]["engagements"] != 9 || got["1"]["impression_count"] != 100 || got["1"]["video_view_count"] != 40 || got["2"]["like_count"] != 1 {
		t.Fatalf("metrics %v", got)
	}
	cs := *calls
	q := cs[0].Query
	if cs[0].Path != "/2/tweets" || q.Get("ids") != "1,2" || q.Get("expansions") != "attachments.media_keys" || q.Get("media.fields") != "public_metrics" {
		t.Fatalf("lookup query %v", q)
	}
	if len(cs) != 2 || cs[1].Query.Get("ids") != "2" || cs[1].Query.Get("tweet.fields") != "public_metrics,attachments" {
		t.Fatalf("fallback query %v", cs[1].Query)
	}
}

func TestMetricsFallbackOn403(t *testing.T) {
	c, _ := fakeX(t, func(cl call, w http.ResponseWriter) {
		if strings.Contains(cl.Query.Get("tweet.fields"), "non_public") {
			w.WriteHeader(403)
			return
		}
		w.Write([]byte(`{"data":[{"id":"1","public_metrics":{"like_count":2}}]}`))
	})
	got, err := c.Metrics(context.Background(), []string{"1"})
	if err != nil || got["1"]["like_count"] != 2 {
		t.Fatalf("%v %v", got, err)
	}
}

func TestCommentsPaginates(t *testing.T) {
	c, calls := fakeX(t, func(cl call, w http.ResponseWriter) {
		if cl.Query.Get("next_token") == "" {
			w.Write([]byte(`{"data":[{"id":"10","text":"nice","author_id":"7","created_at":"2026-11-01T10:00:00.000Z"},{"id":"11","text":"me","author_id":"42","created_at":"2026-11-01T10:00:00.000Z"}],
				"includes":{"users":[{"id":"7","username":"bob"}]},"meta":{"next_token":"p2"}}`))
			return
		}
		w.Write([]byte(`{"data":[{"id":"12","text":"cool","author_id":"8","created_at":"2026-11-01T11:00:00.000Z"}],"includes":{"users":[{"id":"8","username":"eve"}]},"meta":{}}`))
	})
	got, err := c.Comments(context.Background(), "99")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Author != "@bob" || got[0].URL != "https://x.com/bob/status/10" || got[1].Author != "@eve" {
		t.Fatalf("comments %+v", got)
	}
	q := (*calls)[0].Query
	if (*calls)[0].Path != "/2/tweets/search/recent" || q.Get("query") != "conversation_id:99" || q.Get("max_results") != "100" || q.Get("expansions") != "author_id" {
		t.Fatalf("query %v", q)
	}
}

func TestHistoryKinds(t *testing.T) {
	c, calls := fakeX(t, func(cl call, w http.ResponseWriter) {
		w.Write([]byte(`{"data":[
			{"id":"1","text":"hello","created_at":"2026-11-01T10:00:00.000Z","public_metrics":{"like_count":1}},
			{"id":"2","text":"@bob yes","referenced_tweets":[{"type":"replied_to","id":"9"}]},
			{"id":"3","text":"RT @bob: x","referenced_tweets":[{"type":"retweeted","id":"9"}]},
			{"id":"4","text":"look","referenced_tweets":[{"type":"quoted","id":"9"}]},
			{"id":"5","text":"pic","attachments":{"media_keys":["3_1","3_2"]}},
			{"id":"6","text":"clip","attachments":{"media_keys":["7_1"]}},
			{"id":"7","text":"gif","attachments":{"media_keys":["16_1"]}}],
			"includes":{"media":[{"media_key":"3_1","type":"photo"},{"media_key":"3_2","type":"photo"},{"media_key":"7_1","type":"video","public_metrics":{"view_count":12}},{"media_key":"16_1","type":"animated_gif"}]},
			"meta":{"result_count":7}}`))
	})
	got, err := c.History(context.Background(), time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"text", "reply", "repost", "quote", "image", "video", "video"}
	if len(got) != len(want) {
		t.Fatalf("got %d items", len(got))
	}
	for i, w := range want {
		if got[i].Kind != w {
			t.Errorf("item %s kind %q want %q", got[i].RemoteID, got[i].Kind, w)
		}
	}
	if got[5].Metrics["video_view_count"] != 12 || got[0].URL != "https://x.com/alice/status/1" {
		t.Fatalf("item %+v", got[5])
	}
	cl := (*calls)[0]
	q := cl.Query
	if cl.Path != "/2/users/42/tweets" || q.Get("max_results") != "100" || q.Get("start_time") != "2026-10-01T00:00:00Z" ||
		q.Get("tweet.fields") != "created_at,public_metrics,referenced_tweets,attachments" || q.Get("expansions") != "attachments.media_keys" || q.Get("media.fields") != "type,public_metrics" {
		t.Fatalf("query %s %v", cl.Path, q)
	}
}

func TestAccountMetrics(t *testing.T) {
	c, calls := fakeX(t, func(cl call, w http.ResponseWriter) {
		w.Write([]byte(`{"data":{"id":"42","public_metrics":{"followers_count":5,"following_count":6,"tweet_count":7,"listed_count":1,"like_count":99}}}`))
	})
	m, err := c.AccountMetrics(context.Background())
	if err != nil || m["followers"] != 5 || m["following"] != 6 || m["posts"] != 7 {
		t.Fatalf("%v %v", m, err)
	}
	if (*calls)[0].Path != "/2/users/me" || (*calls)[0].Query.Get("user.fields") != "public_metrics" {
		t.Fatal("bad request")
	}
}

func TestWeightedLength(t *testing.T) {
	for s, want := range map[string]int{
		"hello": 5,
		"see https://example.com/a/very/long/path": 4 + 23,
		"日本":            4,
		"👍":             2,
		"👍🏽":            2, // skin tone modifier
		"👨‍👩‍👧":         2, // ZWJ family
		"🇫🇷":            2, // flag
		"❤️":            2, // variation selector
		"“quoted” — ok": 13,
	} {
		if got := WeightedLength(s); got != want {
			t.Errorf("WeightedLength(%q) = %d, want %d", s, got, want)
		}
	}
}

func TestValidate(t *testing.T) {
	img := providers.MediaFile{Filename: "a.png", Mime: "image/png", Size: 6 << 20}
	vid := providers.MediaFile{Filename: "v.mp4", Mime: "video/mp4", Size: 1}
	errs := validate(providers.PublishRequest{Parts: []string{strings.Repeat("a", 281)}, Media: []providers.MediaFile{img, vid}})
	joined := strings.Join(errs, "|")
	for _, want := range []string{"281 characters", "5 MB", "only media"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %v", want, errs)
		}
	}
	if errs := validate(providers.PublishRequest{Parts: []string{""}, Media: []providers.MediaFile{vid}}); len(errs) != 0 {
		t.Fatalf("unexpected %v", errs)
	}
}
