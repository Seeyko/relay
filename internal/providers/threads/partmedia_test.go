package threads

import (
	"context"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/Seeyko/relay/internal/providers"
)

func TestPublishThreadMediaPerPart(t *testing.T) {
	f := newFake(t, map[string]func(url.Values) string{})
	f.routes["POST /v1.0/u1/threads"] = seq("c")
	f.routes["POST /v1.0/u1/threads_publish"] = seq("p")
	for _, id := range []string{"c1", "c2", "c3", "c4", "c5"} {
		f.routes["GET /v1.0/"+id] = static(`{"status":"FINISHED"}`)
	}
	f.routes["GET /v1.0/p1"] = static(`{"permalink":"https://www.threads.net/@rmp/post/1"}`)
	th := newTest(t, f, nil, nil)
	req := providers.PublishRequest{Parts: []string{"one", "two", "three"},
		PartMedia: [][]providers.MediaFile{nil, {img("b.jpg")}, {img("x.jpg"), img("y.jpg")}},
		Options:   map[string]any{"link_attachment": "https://ridemypark.com"}}
	if errs := validate(req); len(errs) != 0 {
		t.Fatalf("valid thread refused: %v", errs)
	}
	res, err := th.Publish(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.RemoteIDs, []string{"p1", "p2", "p3"}) {
		t.Fatalf("result %+v", res)
	}
	k := "POST /v1.0/u1/threads"
	expect(t, f.form(k, 0), map[string]string{"media_type": "TEXT", "text": "one", "link_attachment": "https://ridemypark.com", "reply_to_id": ""})
	expect(t, f.form(k, 1), map[string]string{"media_type": "IMAGE", "image_url": "https://relay.example/media/b.jpg", "text": "two", "reply_to_id": "p1", "link_attachment": ""})
	expect(t, f.form(k, 2), map[string]string{"is_carousel_item": "true", "image_url": "https://relay.example/media/x.jpg"})
	expect(t, f.form(k, 3), map[string]string{"is_carousel_item": "true", "image_url": "https://relay.example/media/y.jpg"})
	expect(t, f.form(k, 4), map[string]string{"media_type": "CAROUSEL", "children": "c3,c4", "text": "three", "reply_to_id": "p2"})
}

func TestPublishResumedThreadKeepsPartMedia(t *testing.T) {
	f := newFake(t, map[string]func(url.Values) string{})
	f.routes["POST /v1.0/u1/threads"] = seq("c")
	f.routes["POST /v1.0/u1/threads_publish"] = seq("p")
	f.routes["GET /v1.0/c1"] = static(`{"status":"FINISHED"}`)
	f.routes["GET /v1.0/p1"] = static(`{"permalink":"https://www.threads.net/@rmp/post/9"}`)
	th := newTest(t, f, nil, nil)
	// What the scheduler sends to resume after part 1: the rest of the parts, reply_to the last id sent.
	req := providers.PublishRequest{Parts: []string{"two"}, Media: []providers.MediaFile{img("b.jpg")},
		PartMedia: [][]providers.MediaFile{{img("b.jpg")}},
		Options:   map[string]any{"reply_to": "p0", "thread_root": "p0", "link_attachment": "https://l.example", "quote_post_id": "q1"}}
	if _, err := th.Publish(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	expect(t, f.form("POST /v1.0/u1/threads", 0), map[string]string{"image_url": "https://relay.example/media/b.jpg", "reply_to_id": "p0",
		"link_attachment": "", "quote_post_id": ""})
}

func TestValidateThreadsMediaPerPart(t *testing.T) {
	many := make([]providers.MediaFile, 21)
	for i := range many {
		many[i] = img("i.jpg")
	}
	errs := validate(providers.PublishRequest{Parts: []string{"a", "", "c"},
		PartMedia: [][]providers.MediaFile{nil, {{Filename: "g.gif", Mime: "image/gif"}}, many}})
	got := strings.Join(errs, "; ")
	for _, want := range []string{"part 2: Threads only accepts JPEG or PNG", "part 3: Threads carousels take at most 20 media"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %v", want, errs)
		}
	}
	if strings.Contains(got, "part 2 is empty") {
		t.Errorf("a part with media may have no text: %v", errs)
	}
}
