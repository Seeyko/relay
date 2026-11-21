package linkedin

import (
	"context"
	"net/http"
	"testing"
)

// The Page logo is logoV2 (a digitalmediaAsset URN) resolved through the Images API; the cropped
// logo first, the original when the cropped one fails.
func TestAvatarLogo(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		switch r.URL.EscapedPath() {
		case "/rest/organizations/42":
			w.Write([]byte(`{"id":42,"localizedName":"RideMyPark","logoV2":{"cropped":"urn:li:digitalmediaAsset:CROP","original":"urn:li:digitalmediaAsset:ORIG","cropInfo":{"x":0,"y":0,"width":0,"height":0}}}`))
		case "/rest/images/urn%3Ali%3Aimage%3ACROP":
			w.WriteHeader(404)
			w.Write([]byte(`{"status":404,"message":"Not found"}`))
		case "/rest/images/urn%3Ali%3Aimage%3AORIG":
			w.Write([]byte(`{"id":"urn:li:image:ORIG","status":"AVAILABLE","downloadUrl":"https://media.licdn.com/logo.png"}`))
		default:
			return false
		}
		return true
	})
	c := f.client(t, nil, nil)
	if u, err := c.AvatarURL(context.Background()); err != nil || u != "https://media.licdn.com/logo.png" {
		t.Fatalf("avatar %q %v", u, err)
	}
}

// A Page without a logo has no picture and no error.
func TestAvatarNoLogo(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		if r.URL.Path != "/rest/organizations/42" {
			return false
		}
		w.Write([]byte(`{"id":42,"localizedName":"RideMyPark"}`))
		return true
	})
	if u, err := f.client(t, nil, nil).AvatarURL(context.Background()); err != nil || u != "" {
		t.Fatalf("avatar %q %v", u, err)
	}
}
