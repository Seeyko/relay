package api_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/Seeyko/relay/internal/store"
)

// Importing an account's history copies each post's visuals into the media library and links them
// to the post; a re-import fills what is missing without downloading twice, a failed download never
// fails the import, and posts created in Relay are left alone.
func TestImportHistoryVisuals(t *testing.T) {
	jpeg := []byte("\xff\xd8\xff\xe0 fake jpeg")
	png := []byte("\x89PNG\r\n\x1a\n fake png")
	var mu sync.Mutex
	hits := map[string]int{}
	brokenFixed := false
	imgs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits[r.URL.Path]++
		fixed := brokenFixed
		mu.Unlock()
		switch {
		case r.URL.Path == "/a.jpg" || strings.HasPrefix(r.URL.Path, "/p"):
			w.Header().Set("Content-Type", "image/jpeg")
			w.Write(jpeg)
		case r.URL.Path == "/b": // no extension, generic type: sniffed
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Write(png)
		case r.URL.Path == "/cover.jpg" && fixed:
			w.Header().Set("Content-Type", "image/jpeg")
			w.Write(jpeg)
		case r.URL.Path == "/cover.jpg":
			w.WriteHeader(500)
		case r.URL.Path == "/huge.jpg":
			w.Header().Set("Content-Type", "image/jpeg")
			w.Write(bytes.Repeat([]byte{0xff}, 16<<20))
		case r.URL.Path == "/clip.mp4":
			w.Header().Set("Content-Type", "video/mp4")
			w.Write([]byte("\x00\x00\x00\x18ftypmp42"))
		default:
			w.WriteHeader(404)
		}
	}))
	defer imgs.Close()

	// Posts 1..6 with their media; "4" was published from Relay.
	photos := []string{}
	for i := 0; i < 12; i++ {
		photos = append(photos, fmt.Sprintf(`{"media_key":"p%d","type":"photo","url":"%s/p%d.jpg"}`, i, imgs.URL, i))
	}
	keys := []string{}
	for i := 0; i < 12; i++ {
		keys = append(keys, fmt.Sprintf(`"p%d"`, i))
	}
	tweets := `{"data":[
		{"id":"1","text":"two pics","created_at":"2026-01-01T00:00:00Z","attachments":{"media_keys":["a","b"]}},
		{"id":"2","text":"clip","created_at":"2026-01-02T00:00:00Z","attachments":{"media_keys":["v"]}},
		{"id":"3","text":"many","created_at":"2026-01-03T00:00:00Z","attachments":{"media_keys":[` + strings.Join(keys, ",") + `]}},
		{"id":"4","text":"from relay","created_at":"2026-01-04T00:00:00Z","attachments":{"media_keys":["a"]}},
		{"id":"5","text":"huge","created_at":"2026-01-05T00:00:00Z","attachments":{"media_keys":["h"]}},
		{"id":"6","text":"not an image","created_at":"2026-01-06T00:00:00Z","attachments":{"media_keys":["m"]}}],
		"includes":{"media":[
		{"media_key":"a","type":"photo","url":"` + imgs.URL + `/a.jpg"},
		{"media_key":"b","type":"photo","url":"` + imgs.URL + `/b"},
		{"media_key":"v","type":"video","url":"` + imgs.URL + `/clip.mp4","preview_image_url":"` + imgs.URL + `/cover.jpg"},
		{"media_key":"h","type":"photo","url":"` + imgs.URL + `/huge.jpg"},
		{"media_key":"m","type":"photo","url":"` + imgs.URL + `/clip.mp4"},
		` + strings.Join(photos, ",") + `]},"meta":{}}`
	xs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/2/users/me":
			w.Write([]byte(`{"data":{"id":"42","username":"alice"}}`))
		case "/2/users/42/tweets":
			w.Write([]byte(tweets))
		case "/2/tweets":
			w.Write([]byte(`{"data":[]}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer xs.Close()

	e := newEnv(t)
	ctx := t.Context()
	text, isErr := e.callTool("connect_account", map[string]any{"provider": "x", "credentials": map[string]string{
		"consumer_key": "a", "consumer_secret": "b", "access_token": "c", "access_token_secret": "d", "base_url": xs.URL}})
	if isErr {
		t.Fatalf("connect: %s", text)
	}
	var acc store.Account
	json.Unmarshal([]byte(text), &acc)
	own := &store.Post{AccountID: acc.ID, Status: store.StatusPublished, Parts: []string{"from relay"}, RemoteID: "4", RemoteIDs: []string{"4"}}
	if err := e.svc.Store.CreatePost(ctx, own); err != nil {
		t.Fatal(err)
	}

	byRemote := func() map[string]*store.Post {
		ps, err := e.svc.Store.ListPosts(ctx, store.PostFilter{AccountID: acc.ID})
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]*store.Post{}
		for _, p := range ps {
			out[p.RemoteID] = p
		}
		return out
	}
	result := func(text string) map[string]int {
		var r map[string]int
		if err := json.Unmarshal([]byte(text), &r); err != nil {
			t.Fatalf("result %s", text)
		}
		return r
	}

	text, isErr = e.callTool("import_history", map[string]any{"account_id": acc.ID})
	if isErr {
		t.Fatalf("import: %s", text)
	}
	if r := result(text); r["created"] != 5 || r["updated"] != 1 || r["visuals"] != 12 || r["visuals_failed"] != 3 {
		t.Fatalf("import result %v", r)
	}
	ps := byRemote()
	if n := len(ps["1"].Media); n != 2 {
		t.Fatalf("post 1 media %v", ps["1"].Media)
	}
	for i, want := range []struct {
		mime string
		body []byte
	}{{"image/jpeg", jpeg}, {"image/png", png}} {
		m, err := e.svc.Store.GetMedia(ctx, ps["1"].Media[i])
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(m.Path)
		if err != nil || m.Mime != want.mime || !bytes.Equal(b, want.body) || !strings.HasPrefix(m.Filename, "x-1-") {
			t.Fatalf("media %d: %+v %v", i, m, err)
		}
	}
	if len(ps["2"].Media) != 0 || ps["2"].Parts[0] != "clip" {
		t.Fatalf("failed cover: post 2 should be imported without media: %+v", ps["2"])
	}
	if len(ps["3"].Media) != 10 {
		t.Fatalf("post 3: want 10 visuals (cap), got %d", len(ps["3"].Media))
	}
	if len(ps["4"].Media) != 0 || ps["4"].Source != "relay" {
		t.Fatalf("post created in Relay was changed: %+v", ps["4"])
	}
	if len(ps["5"].Media) != 0 || len(ps["6"].Media) != 0 {
		t.Fatalf("oversized or non-image visuals kept: %v %v", ps["5"].Media, ps["6"].Media)
	}
	mu.Lock()
	if hits["/p10.jpg"] != 0 || hits["/a.jpg"] != 1 {
		t.Fatalf("downloads %v", hits)
	}
	brokenFixed = true
	mu.Unlock()
	ms, _ := e.svc.Store.ListMedia(ctx)
	before := len(ms)

	// Re-import: post 2 gets its cover now, nothing else is downloaded again.
	text, _ = e.callTool("import_history", map[string]any{"account_id": acc.ID})
	if r := result(text); r["updated"] != 6 || r["visuals"] != 1 {
		t.Fatalf("re-import result %v", r)
	}
	ps2 := byRemote()
	if len(ps2["2"].Media) != 1 || strings.Join(ps2["1"].Media, ",") != strings.Join(ps["1"].Media, ",") || len(ps2["4"].Media) != 0 {
		t.Fatalf("re-import media: 1=%v 2=%v 4=%v", ps2["1"].Media, ps2["2"].Media, ps2["4"].Media)
	}
	ms, _ = e.svc.Store.ListMedia(ctx)
	mu.Lock()
	if len(ms) != before+1 || hits["/a.jpg"] != 1 || hits["/p0.jpg"] != 1 {
		t.Fatalf("re-import downloaded again: %d media (was %d), hits %v", len(ms), before, hits)
	}
	mu.Unlock()

	// visuals:false imports text and metrics only.
	text, _ = e.callTool("import_history", map[string]any{"account_id": acc.ID, "visuals": false})
	if r := result(text); r["visuals"] != 0 || r["visuals_failed"] != 0 {
		t.Fatalf("visuals:false result %v", r)
	}
}
