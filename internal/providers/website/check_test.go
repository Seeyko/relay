package website_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Seeyko/relay/internal/providers/website"
	"github.com/Seeyko/relay/internal/providers/website/sitetest"
)

func TestCheckConformingSite(t *testing.T) {
	s := sitetest.New("tok", sitetest.All...)
	s.MachineTranslate = true
	s.Extensions = []sitetest.Extension{{Info: map[string]any{"name": "search", "description": "Search.", "method": "GET", "path": "/x/search", "input_schema": map[string]any{"type": "object"}}}}
	srv := s.Start()
	defer srv.Close()
	s.Add(sitetest.Article{Title: "First", Status: "published", BodyHTML: "<p>x</p>"})
	var out bytes.Buffer
	if n := website.Check(context.Background(), srv.Client(), srv.URL, "tok", true, &out); n != 0 {
		t.Fatalf("%d failures:\n%s", n, out.String())
	}
	for _, want := range []string{"PASS  GET /info", "PASS  a wrong token gets 401", "PASS  PATCH with a stale expected_updated_at gets 409",
		"PASS  PUT /posts/{id}/translations/fr", "PASS  a deleted article gets 404", "PASS  extension search"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("report lacks %q:\n%s", want, out.String())
		}
	}
	// Read-only mode never writes.
	before := len(s.Calls())
	out.Reset()
	website.Check(context.Background(), srv.Client(), srv.URL, "tok", false, &out)
	for _, c := range s.Calls()[before:] {
		if c.Method != "GET" {
			t.Fatalf("read-only check sent %s %s", c.Method, c.Path)
		}
	}
	if !strings.Contains(out.String(), "SKIP  write checks") {
		t.Fatalf("read-only report:\n%s", out.String())
	}
}

func TestCheckBrokenSite(t *testing.T) {
	s := sitetest.New("tok", "drafts", "edit")
	s.Extensions = []sitetest.Extension{{Info: map[string]any{"name": "Bad Name", "description": "", "method": "GET", "path": "/search"}}}
	inner := s
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/posts/relay-site-check-unknown-id"):
			w.Write([]byte(`{}`)) // should be 404
		case r.Method == "PATCH":
			r.Header.Set("Content-Type", "application/json")
			w.Write([]byte(`{"id":"1","title":"Relay site-check draft (edited)","status":"draft","updated_at":"2026-01-01T00:00:00Z"}`)) // never 409
		default:
			inner.ServeHTTP(w, r)
		}
	})
	srv := httptest.NewServer(h)
	defer srv.Close()
	s.URL = srv.URL
	var out bytes.Buffer
	n := website.Check(context.Background(), srv.Client(), srv.URL, "tok", true, &out)
	for _, want := range []string{"FAIL  GET /posts/<unknown id> must get 404", "FAIL  extension \"Bad Name\"", "FAIL  PATCH with a stale expected_updated_at must get 409",
		"WARN  draft 1 is left on the site"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("report lacks %q:\n%s", want, out.String())
		}
	}
	if n < 3 {
		t.Fatalf("want at least 3 failures, got %d:\n%s", n, out.String())
	}
}

func TestCheckV1Site(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/relay/v1/info" {
			w.Write([]byte(`{"name":"Old","url":"https://old.example","version":"1","capabilities":["articles"]}`))
			return
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()
	var out bytes.Buffer
	if n := website.Check(context.Background(), srv.Client(), srv.URL, "tok", false, &out); n == 0 || !strings.Contains(out.String(), "speaks v1") {
		t.Fatalf("v1 site should fail with a hint:\n%s", out.String())
	}
}
