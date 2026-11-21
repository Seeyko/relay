package meta

import (
	"context"
	"net/http"
	"testing"
)

// The Page picture comes from /{page-id}/picture?type=large&redirect=false; a silhouette means none.
func TestFacebookAvatar(t *testing.T) {
	silhouette := false
	fb := newFB(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/v26.0/123/picture" || q.Get("type") != "large" || q.Get("redirect") != "false" || token(r) != "tok" {
			t.Errorf("unexpected %s", r.URL)
		}
		if silhouette {
			w.Write([]byte(`{"data":{"url":"https://cdn.fb/default.png","is_silhouette":true,"height":200,"width":200}}`))
			return
		}
		w.Write([]byte(`{"data":{"url":"https://cdn.fb/page.jpg","is_silhouette":false,"height":200,"width":200}}`))
	})
	if u, err := fb.AvatarURL(context.Background()); err != nil || u != "https://cdn.fb/page.jpg" {
		t.Fatalf("avatar %q %v", u, err)
	}
	silhouette = true
	if u, err := fb.AvatarURL(context.Background()); err != nil || u != "" {
		t.Fatalf("silhouette avatar %q %v", u, err)
	}
}
