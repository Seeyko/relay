package blob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The "GET Object" example from the AWS Signature V4 documentation:
// https://docs.aws.amazon.com/AmazonS3/latest/API/sig-v4-header-based-auth.html
func TestSignMatchesAWSExample(t *testing.T) {
	s := &S3{Region: "us-east-1", AccessKey: "AKIAIOSFODNN7EXAMPLE", SecretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		now: func() time.Time { return time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC) }}
	req, _ := http.NewRequest("GET", "https://examplebucket.s3.amazonaws.com/test.txt", nil)
	req.Header.Set("Range", "bytes=0-9")
	s.sign(req, emptySHA)
	want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, SignedHeaders=host;range;x-amz-content-sha256;x-amz-date, Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	if got := req.Header.Get("Authorization"); got != want {
		t.Fatalf("signature mismatch\n got: %s\nwant: %s", got, want)
	}
}

func TestPutGetDelete(t *testing.T) {
	objects := map[string][]byte{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=key/") {
			w.WriteHeader(403)
			return
		}
		switch r.Method {
		case "PUT":
			b, _ := io.ReadAll(r.Body)
			sum := sha256.Sum256(b)
			if hex.EncodeToString(sum[:]) != r.Header.Get("X-Amz-Content-Sha256") {
				w.WriteHeader(400)
				return
			}
			objects[r.URL.Path] = b
		case "GET":
			b, ok := objects[r.URL.Path]
			if !ok {
				w.WriteHeader(404)
				return
			}
			w.Write(b)
		case "DELETE":
			delete(objects, r.URL.Path)
			w.WriteHeader(204)
		}
	}))
	defer srv.Close()
	s := &S3{Endpoint: srv.URL, Bucket: "media", AccessKey: "key", SecretKey: "secret"}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	data := []byte("hello")
	sum := sha256.Sum256(data)
	ctx := context.Background()
	if err := s.Put(ctx, "med_1.jpg", strings.NewReader("hello"), 5, hex.EncodeToString(sum[:]), "image/jpeg"); err != nil {
		t.Fatal(err)
	}
	if _, ok := objects["/media/med_1.jpg"]; !ok {
		t.Fatalf("object not stored path-style: %v", objects)
	}
	resp, err := s.Get(ctx, "med_1.jpg", "")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "hello" {
		t.Fatalf("got %q", b)
	}
	if err := s.Delete(ctx, "med_1.jpg"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, "med_1.jpg", ""); err == nil {
		t.Fatal("want 404 after delete")
	}
}
