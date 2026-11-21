package youtube

import (
	"context"
	"net/http"
	"testing"
)

func TestAvatar(t *testing.T) {
	f := newFake(t)
	calls := 0
	f.routes["GET /youtube/v3/channels"] = func(w http.ResponseWriter, r *http.Request) {
		calls++
		q := r.URL.Query()
		if q.Get("mine") != "true" {
			t.Errorf("mine %q", q.Get("mine"))
		}
		writeJSON(w, map[string]any{"items": []any{map[string]any{"id": "UC1",
			"snippet":    map[string]any{"title": "RMP", "thumbnails": map[string]any{"default": map[string]any{"url": "https://yt3/88.jpg"}, "high": map[string]any{"url": "https://yt3/800.jpg"}}},
			"statistics": map[string]any{"subscriberCount": "5", "videoCount": "1", "viewCount": "9"}}}})
	}
	c := f.client(t, nil, nil)
	// The counters call carries the snippet: the picture costs no extra request.
	if _, err := c.AccountMetrics(context.Background()); err != nil {
		t.Fatal(err)
	}
	if u, err := c.AvatarURL(context.Background()); err != nil || u != "https://yt3/800.jpg" || calls != 1 {
		t.Fatalf("avatar %q %v, %d calls", u, err, calls)
	}
	c2 := f.client(t, nil, nil)
	if u, err := c2.AvatarURL(context.Background()); err != nil || u != "https://yt3/800.jpg" || calls != 2 {
		t.Fatalf("lookup %q %v, %d calls", u, err, calls)
	}
}
