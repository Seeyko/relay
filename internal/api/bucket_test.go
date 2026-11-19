package api_test

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/Seeyko/relay/internal/blob"
)

// Media saved while a bucket is configured go to the bucket (not the disk) and are served back
// through /media/, with Range forwarded.
func TestMediaInBucket(t *testing.T) {
	objects := map[string][]byte{}
	s3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
			w.WriteHeader(403)
			return
		}
		switch r.Method {
		case "PUT":
			objects[r.URL.Path], _ = io.ReadAll(r.Body)
		case "GET":
			b, ok := objects[r.URL.Path]
			if !ok {
				w.WriteHeader(404)
				return
			}
			w.Header().Set("Content-Type", "image/png")
			if r.Header.Get("Range") == "bytes=0-3" {
				w.Header().Set("Content-Range", "bytes 0-3/"+strconv.Itoa(len(b)))
				w.WriteHeader(206)
				w.Write(b[:4])
				return
			}
			w.Write(b)
		}
	}))
	defer s3.Close()

	e := newEnv(t)
	e.svc.Blob = &blob.S3{Endpoint: s3.URL, Bucket: "relay-media", Region: "auto", AccessKey: "id", SecretKey: "secret"}

	payload := []byte("\x89PNG fake image bytes")
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "pic.png")
	fw.Write(payload)
	mw.Close()
	req, _ := http.NewRequest("POST", e.srv.URL+"/v1/media", &buf)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("upload: %d", resp.StatusCode)
	}
	if len(objects) != 1 {
		t.Fatalf("want 1 object in bucket, got %v", objects)
	}
	var key string
	for k, v := range objects {
		key = strings.TrimPrefix(k, "/relay-media/")
		if !bytes.Equal(v, payload) {
			t.Fatal("bucket content differs")
		}
	}
	if entries, _ := os.ReadDir(e.svc.MediaDir); len(entries) != 0 {
		t.Fatalf("local copy should be removed, found %d files", len(entries))
	}
	ms, _ := e.svc.Store.ListMedia(t.Context())
	if len(ms) != 1 || !ms[0].InBucket() || ms[0].Key() != key {
		t.Fatalf("bad media row: %+v", ms[0])
	}

	got, err := http.Get(e.srv.URL + "/media/" + key)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(got.Body)
	got.Body.Close()
	if got.StatusCode != 200 || !bytes.Equal(b, payload) || got.Header.Get("Content-Type") != "image/png" {
		t.Fatalf("serve: %d %q %s", got.StatusCode, b, got.Header.Get("Content-Type"))
	}

	rq, _ := http.NewRequest("GET", e.srv.URL+"/media/"+key, nil)
	rq.Header.Set("Range", "bytes=0-3")
	got, err = http.DefaultClient.Do(rq)
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(got.Body)
	got.Body.Close()
	if got.StatusCode != 206 || len(b) != 4 || got.Header.Get("Content-Range") == "" {
		t.Fatalf("range: %d %q", got.StatusCode, b)
	}

	if got, _ := http.Get(e.srv.URL + "/media/med_missing.png"); got.StatusCode != 404 {
		t.Fatalf("missing: want 404, got %d", got.StatusCode)
	}

	// Providers read the file from the bucket too.
	rc, err := e.svc.OpenMedia(t.Context(), ms[0])
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(b, payload) {
		t.Fatal("OpenMedia content differs")
	}
	if u := e.svc.MediaURL(ms[0]); u != "https://relay.example/media/"+key {
		t.Fatalf("media url: %s", u)
	}
	e.svc.MediaPublicURL = "https://media.example.com"
	if u := e.svc.MediaURL(ms[0]); u != "https://media.example.com/"+key {
		t.Fatalf("public media url: %s", u)
	}
}
