package x

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/Seeyko/relay/internal/providers"
)

func mediaIDs(t *testing.T, body []byte) []string {
	t.Helper()
	m := mustJSON(t, body)
	md, ok := m["media"].(map[string]any)
	if !ok {
		return nil
	}
	var out []string
	for _, id := range md["media_ids"].([]any) {
		out = append(out, id.(string))
	}
	return out
}

func TestPublishThreadMediaPerPart(t *testing.T) {
	uploads, tweets := 0, 0
	c, calls := fakeX(t, func(cl call, w http.ResponseWriter) {
		switch {
		case cl.Method == "POST" && cl.Path == "/2/media/upload":
			uploads++
			// the id tells which file was uploaded
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "m-" + string(cl.File)}})
		case cl.Method == "POST" && cl.Path == "/2/tweets":
			tweets++
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": []string{"", "t1", "t2", "t3"}[tweets]}})
		default:
			t.Errorf("unexpected %s %s", cl.Method, cl.Path)
			w.WriteHeader(404)
		}
	})
	a := file([]byte("a"), "a.png", "image/png")
	b := file([]byte("b"), "b.jpg", "image/jpeg")
	cc := file([]byte("c"), "c.jpg", "image/jpeg")
	req := providers.PublishRequest{Parts: []string{"one", "two", ""}, Media: []providers.MediaFile{a},
		PartMedia: [][]providers.MediaFile{{a}, nil, {b, cc}}}
	if errs := validate(req); len(errs) != 0 {
		t.Fatalf("a media-only last part is valid: %v", errs)
	}
	res, err := c.Publish(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.RemoteIDs, []string{"t1", "t2", "t3"}) || uploads != 3 {
		t.Fatalf("result %+v uploads %d", res, uploads)
	}
	var bodies [][]byte
	for _, cl := range *calls {
		if cl.Path == "/2/tweets" {
			bodies = append(bodies, cl.Body)
		}
	}
	// Every upload happens before the first post: a bad file never leaves half a thread.
	if (*calls)[3].Path != "/2/tweets" {
		t.Fatalf("uploads must come first: %+v", *calls)
	}
	want := [][]string{{"m-a"}, nil, {"m-b", "m-c"}}
	for i, w := range want {
		if got := mediaIDs(t, bodies[i]); !reflect.DeepEqual(got, w) {
			t.Errorf("part %d media %v, want %v", i+1, got, w)
		}
	}
	last := mustJSON(t, bodies[2])
	if _, hasText := last["text"]; hasText || last["reply"].(map[string]any)["in_reply_to_tweet_id"] != "t2" {
		t.Fatalf("third part %v", last)
	}
}

func TestValidateMediaPerPart(t *testing.T) {
	img := file([]byte("i"), "i.jpg", "image/jpeg")
	vid := file([]byte("v"), "v.mp4", "video/mp4")
	cases := []struct {
		name string
		pm   [][]providers.MediaFile
		want string
	}{
		{"five images on part 2", [][]providers.MediaFile{nil, {img, img, img, img, img}}, "part 2: X allows at most 4 media per post"},
		{"video and image on part 3", [][]providers.MediaFile{nil, nil, {vid, img}}, "part 3: a video or GIF must be the only media"},
		{"pdf on part 2", [][]providers.MediaFile{nil, {file([]byte("p"), "p.pdf", "application/pdf")}}, "part 2: p.pdf: X accepts images"},
	}
	for _, c := range cases {
		errs := validate(providers.PublishRequest{Parts: []string{"a", "b", "c"}, PartMedia: c.pm})
		if !strings.Contains(strings.Join(errs, "; "), c.want) {
			t.Errorf("%s: %v", c.name, errs)
		}
	}
	// The same media on part 1 keeps the old messages (no prefix).
	errs := validate(providers.PublishRequest{Parts: []string{"a"}, Media: []providers.MediaFile{img, img, img, img, img}})
	if len(errs) != 1 || errs[0] != "X allows at most 4 media per post" {
		t.Fatalf("part 1: %v", errs)
	}
	// Publish refuses more than 4 media on a later part even when validation was skipped (force).
	c, _ := fakeX(t, func(cl call, w http.ResponseWriter) { t.Errorf("no request expected: %s", cl.Path) })
	_, err := c.Publish(context.Background(), providers.PublishRequest{Parts: []string{"a", "b"}, PartMedia: [][]providers.MediaFile{nil, {img, img, img, img, img}}})
	if err == nil || !strings.Contains(err.Error(), "part 2 has 5 media") {
		t.Fatalf("publish: %v", err)
	}
}
