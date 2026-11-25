package api_test

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net/http"
	"testing"
)

// A PNG media is also served as JPEG at /media/<id>.jpg: TikTok photo posts pull only JPEG or WebP.
func TestPNGServedAsJPEG(t *testing.T) {
	e := newEnv(t)
	src := image.NewNRGBA(image.Rect(0, 0, 4, 3))
	src.Set(1, 1, color.NRGBA{R: 255, A: 255}) // the rest is transparent: it becomes white
	var buf bytes.Buffer
	if err := png.Encode(&buf, src); err != nil {
		t.Fatal(err)
	}
	m, err := e.svc.SaveMedia(context.Background(), &buf, "slide.png", "image/png")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := e.svc.JPEGURL(m), "https://relay.example/media/"+m.ID+".jpg"; got != want {
		t.Fatalf("JPEGURL = %q, want %q", got, want)
	}
	resp, err := http.Get(e.srv.URL + "/media/" + m.ID + ".jpg")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/jpeg" {
		t.Fatalf("GET jpeg: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	img, err := jpeg.Decode(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != 4 || b.Dy() != 3 {
		t.Fatalf("size %v", b)
	}
	if r, g, b, _ := img.At(0, 0).RGBA(); r>>8 < 240 || g>>8 < 240 || b>>8 < 240 {
		t.Fatalf("transparent pixel not white: %d %d %d", r>>8, g>>8, b>>8)
	}
	// The PNG itself is unchanged, and a JPEG copy exists only for PNGs.
	if r, _ := http.Get(e.srv.URL + "/media/" + m.Key()); r.Header.Get("Content-Type") != "image/png" {
		t.Fatalf("png: %s", r.Header.Get("Content-Type"))
	}
	if r, _ := http.Get(e.srv.URL + "/media/med_doesnotexist.jpg"); r.StatusCode != 404 {
		t.Fatalf("unknown jpeg: %d", r.StatusCode)
	}
}
