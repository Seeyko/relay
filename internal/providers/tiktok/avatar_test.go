package tiktok

import (
	"context"
	"io"
	"net/http"
	"testing"
)

func TestAvatar(t *testing.T) {
	f := newFake(t)
	f.on("GET /v2/user/info/", ok(`{"user":{"open_id":"o1","display_name":"RMP","avatar_url_100":"https://cdn.tt/100.jpg","avatar_large_url":"https://cdn.tt/large.jpg"}}`))
	c, _ := newClient(t, f, nil)
	id, err := c.Identify(context.Background())
	if err != nil || id.AvatarURL != "https://cdn.tt/large.jpg" {
		t.Fatalf("identify %+v %v", id, err)
	}
	if u, err := c.AvatarURL(context.Background()); err != nil || u != "https://cdn.tt/large.jpg" || len(f.find("GET /v2/user/info/")) != 1 {
		t.Fatalf("avatar %q %v", u, err)
	}

	// A fresh client asks for the avatar fields only; avatar_url_100 when there is no large one.
	f2 := newFake(t)
	f2.on("GET /v2/user/info/", ok(`{"user":{"avatar_url_100":"https://cdn.tt/100.jpg"}}`))
	c2, _ := newClient(t, f2, nil)
	if u, err := c2.AvatarURL(context.Background()); err != nil || u != "https://cdn.tt/100.jpg" {
		t.Fatalf("avatar %q %v", u, err)
	}
	if q := f2.find("GET /v2/user/info/")[0].Query; q != "fields=avatar_large_url,avatar_url_100" {
		t.Errorf("query %q", q)
	}
}

// Without user.info.basic there is no picture, and no error: connect and collect go on.
func TestAvatarScopeNotGranted(t *testing.T) {
	f := newFake(t)
	f.on("GET /v2/user/info/", func(w http.ResponseWriter, h hit) {
		w.WriteHeader(401)
		io.WriteString(w, `{"data":{},"error":{"code":"scope_not_authorized","message":"The user did not authorize the scope required for completing this request.","log_id":"l"}}`)
	})
	c, _ := newClient(t, f, nil)
	if u, err := c.AvatarURL(context.Background()); err != nil || u != "" {
		t.Fatalf("avatar %q %v", u, err)
	}
}
