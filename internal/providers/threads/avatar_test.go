package threads

import (
	"context"
	"net/url"
	"testing"
)

// The picture is threads_profile_picture_url on /me; an app without the field falls back to the old fields.
func TestAvatar(t *testing.T) {
	f := newFake(t, map[string]func(url.Values) string{
		"GET /v1.0/me": func(q url.Values) string {
			return `{"id":"u1","username":"rmp","name":"RideMyPark","threads_profile_picture_url":"https://cdn.threads/p.jpg"}`
		},
	})
	th := newTest(t, f, nil, nil)
	id, err := th.Identify(context.Background())
	if err != nil || id.AvatarURL != "https://cdn.threads/p.jpg" {
		t.Fatalf("identify %+v %v", id, err)
	}
	if u, err := th.AvatarURL(context.Background()); err != nil || u != "https://cdn.threads/p.jpg" || f.count("GET /v1.0/me") != 1 {
		t.Fatalf("avatar %q %v, %d calls", u, err, f.count("GET /v1.0/me"))
	}

	f2 := newFake(t, map[string]func(url.Values) string{
		"GET /v1.0/me": func(q url.Values) string {
			if q.Get("fields") != "id,username" {
				return `400 {"error":{"message":"Tried accessing nonexisting field","code":100}}`
			}
			return `{"id":"u1","username":"rmp"}`
		},
	})
	th2 := newTest(t, f2, nil, nil)
	if u, err := th2.AvatarURL(context.Background()); err != nil || u != "" || f2.count("GET /v1.0/me") != 3 {
		t.Fatalf("fallback avatar %q %v, %d calls", u, err, f2.count("GET /v1.0/me"))
	}
}
