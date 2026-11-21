package meta

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/Seeyko/relay/internal/providers"
)

func TestFacebookCommentsStopAndResume(t *testing.T) {
	var comments []string
	fail := true
	fb := newFB(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/v26.0/123/feed":
			if r.FormValue("message") != "post" {
				t.Errorf("message %q", r.FormValue("message"))
			}
			w.Write([]byte(`{"id":"123_9"}`))
		case r.Method == "GET" && r.URL.Path == "/v26.0/123_9":
			w.Write([]byte(`{}`))
		case r.Method == "POST" && r.URL.Path == "/v26.0/123_9/comments":
			msg := r.FormValue("message")
			if msg == "second" && fail {
				fail = false
				w.WriteHeader(500)
				w.Write([]byte(`{"error":{"message":"oops"}}`))
				return
			}
			comments = append(comments, msg)
			w.Write([]byte(`{"id":"9_` + msg + `"}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			w.WriteHeader(404)
		}
	})
	res, err := fb.Publish(context.Background(), providers.PublishRequest{Parts: []string{"post", "first", "second", "third"}})
	if err == nil || res == nil || !reflect.DeepEqual(res.RemoteIDs, []string{"123_9", "9_first"}) || !strings.Contains(err.Error(), "comment 2/3") {
		t.Fatalf("partial: %v %+v", err, res)
	}
	// Resume as the scheduler does: the missing parts, commented on the post (thread_root), not on the last comment.
	res, err = fb.Publish(context.Background(), providers.PublishRequest{Parts: []string{"second", "third"},
		Options: map[string]any{"reply_to": "9_first", "thread_root": "123_9"}})
	if err != nil || !reflect.DeepEqual(res.RemoteIDs, []string{"9_second", "9_third"}) {
		t.Fatalf("resume: %v %+v", err, res)
	}
	if !reflect.DeepEqual(comments, []string{"first", "second", "third"}) {
		t.Fatalf("comments %v", comments)
	}
}

func TestFacebookCommentValidation(t *testing.T) {
	errs := fbValidate(providers.PublishRequest{Parts: []string{"post", "", strings.Repeat("a", 8001)}})
	got := strings.Join(errs, "; ")
	for _, want := range []string{"part 2 is empty", "part 3 becomes a Facebook comment, limited to 8000 characters (got 8001)"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %v", want, errs)
		}
	}
	// The post limit applies to part 1 alone now.
	if errs := fbValidate(providers.PublishRequest{Parts: []string{strings.Repeat("a", 60000), strings.Repeat("b", 8000)}}); len(errs) != 0 {
		t.Errorf("valid post refused: %v", errs)
	}
	// Media on a comment never reaches the network, even with force.
	fb := newFB(t, func(w http.ResponseWriter, r *http.Request) { t.Errorf("no request expected: %s", r.URL) })
	_, err := fb.Publish(context.Background(), providers.PublishRequest{Parts: []string{"a", "b"},
		PartMedia: [][]providers.MediaFile{nil, {media("a.jpg", "image/jpeg", "x")}}})
	if !errors.Is(err, providers.ErrInvalidPost) {
		t.Fatalf("media on a comment: %v", err)
	}
}

func igRoutes(extra map[string]func(url.Values) string) map[string]func(url.Values) string {
	r := map[string]func(url.Values) string{
		"POST /v26.0/ig1/media":         static(`{"id":"c1"}`),
		"GET /v26.0/c1":                 static(`{"status_code":"FINISHED"}`),
		"POST /v26.0/ig1/media_publish": static(`{"id":"m1"}`),
		"GET /v26.0/m1":                 static(`{"permalink":"https://www.instagram.com/p/abc/"}`),
	}
	for k, v := range extra {
		r[k] = v
	}
	return r
}

func TestInstagramCommentsStopAndResume(t *testing.T) {
	n := 0
	f := newFakeIG(t, igRoutes(map[string]func(url.Values) string{
		"POST /v26.0/m1/comments": func(q url.Values) string {
			n++
			if n == 2 {
				return `400 {"error":{"message":"Invalid parameter","code":100}}`
			}
			return `{"id":"k` + q.Get("message") + `"}`
		},
	}))
	ig := newTestIG(t, f, nil, nil)
	res, err := ig.Publish(context.Background(), providers.PublishRequest{Parts: []string{"cap", "1", "2", "3"}, Media: []providers.MediaFile{jpeg("a.jpg")}})
	if err == nil || res == nil || !reflect.DeepEqual(res.RemoteIDs, []string{"m1", "k1"}) {
		t.Fatalf("partial: %v %+v", err, res)
	}
	expect(t, f.form("POST /v26.0/ig1/media", 0), map[string]string{"caption": "cap"})
	res, err = ig.Publish(context.Background(), providers.PublishRequest{Parts: []string{"2", "3"}, Options: map[string]any{"reply_to": "k1", "thread_root": "m1"}})
	if err != nil || !reflect.DeepEqual(res.RemoteIDs, []string{"k2", "k3"}) {
		t.Fatalf("resume: %v %+v", err, res)
	}
	if f.count("POST /v26.0/ig1/media") != 1 || f.count("POST /v26.0/m1/comments") != 4 {
		t.Fatalf("calls %v", f.calls)
	}
}

func TestInstagramStoryAndCommentValidation(t *testing.T) {
	story := providers.PublishRequest{Parts: []string{"a", "b"}, Media: []providers.MediaFile{jpeg("a.jpg")}, Options: map[string]any{"media_type": "STORIES"}}
	if errs := strings.Join(validateInstagram(story), "; "); !strings.Contains(errs, "story cannot carry comments") {
		t.Errorf("story with comments: %v", errs)
	}
	ig := newTestIG(t, newFakeIG(t, nil), nil, nil)
	if _, err := ig.Publish(context.Background(), story); !errors.Is(err, providers.ErrInvalidPost) {
		t.Errorf("story with comments published: %v", err)
	}
	long := providers.PublishRequest{Parts: []string{"a", strings.Repeat("é", 2201)}, Media: []providers.MediaFile{jpeg("a.jpg")}}
	if errs := strings.Join(validateInstagram(long), "; "); !strings.Contains(errs, "part 2 becomes a Instagram comment, limited to 2200 characters") {
		t.Errorf("long comment: %v", errs)
	}
	// The caption limit applies to part 1 only.
	ok := providers.PublishRequest{Parts: []string{strings.Repeat("a", 2000), strings.Repeat("b", 2000)}, Media: []providers.MediaFile{jpeg("a.jpg")}}
	if errs := validateInstagram(ok); len(errs) != 0 {
		t.Errorf("valid post refused: %v", errs)
	}
}

func TestInstagramTrialReel(t *testing.T) {
	vid := providers.MediaFile{Filename: "v.mp4", Mime: "video/mp4", PublicURL: "https://relay.example/media/v.mp4"}
	f := newFakeIG(t, igRoutes(nil))
	ig := newTestIG(t, f, nil, nil)
	_, err := ig.Publish(context.Background(), providers.PublishRequest{Parts: []string{"reel"}, Media: []providers.MediaFile{vid},
		Options: map[string]any{"trial_reel": true, "trial_graduation": "ss_performance"}})
	if err != nil {
		t.Fatal(err)
	}
	expect(t, f.form("POST /v26.0/ig1/media", 0), map[string]string{"media_type": "REELS", "video_url": vid.PublicURL,
		"trial_params": `{"graduation_strategy":"SS_PERFORMANCE"}`})

	// Default strategy: MANUAL. Not a trial: no trial_params.
	f = newFakeIG(t, igRoutes(nil))
	ig = newTestIG(t, f, nil, nil)
	if _, err := ig.Publish(context.Background(), providers.PublishRequest{Media: []providers.MediaFile{vid}, Options: map[string]any{"trial_reel": "true"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := ig.Publish(context.Background(), providers.PublishRequest{Media: []providers.MediaFile{vid}}); err != nil {
		t.Fatal(err)
	}
	expect(t, f.form("POST /v26.0/ig1/media", 0), map[string]string{"trial_params": `{"graduation_strategy":"MANUAL"}`})
	expect(t, f.form("POST /v26.0/ig1/media", 1), map[string]string{"trial_params": ""})

	bad := []struct {
		req  providers.PublishRequest
		want string
	}{
		{providers.PublishRequest{Media: []providers.MediaFile{jpeg("a.jpg")}, Options: map[string]any{"trial_reel": true}}, "trial Reel (options.trial_reel) needs exactly one video"},
		{providers.PublishRequest{Media: []providers.MediaFile{vid, vid}, Options: map[string]any{"trial_reel": true}}, "needs exactly one video"},
		{providers.PublishRequest{Media: []providers.MediaFile{vid}, Options: map[string]any{"trial_reel": true, "media_type": "STORIES"}}, "needs exactly one video"},
		{providers.PublishRequest{Media: []providers.MediaFile{vid}, Options: map[string]any{"trial_reel": true, "trial_graduation": "AUTO"}}, "must be MANUAL or SS_PERFORMANCE"},
		{providers.PublishRequest{Media: []providers.MediaFile{vid}, Options: map[string]any{"trial_graduation": "MANUAL"}}, "needs options.trial_reel=true"},
	}
	ig = newTestIG(t, newFakeIG(t, nil), nil, nil)
	for _, b := range bad {
		if errs := strings.Join(validateInstagram(b.req), "; "); !strings.Contains(errs, b.want) {
			t.Errorf("%+v: %v", b.req.Options, errs)
		}
		if _, err := ig.Publish(context.Background(), b.req); !errors.Is(err, providers.ErrInvalidPost) {
			t.Errorf("published anyway %+v: %v", b.req.Options, err)
		}
	}
}
