package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Seeyko/relay/internal/store"
)

// fakeNet answers the identity call of a connector and records every DELETE. Ids listed in gone answer
// like the network does for a post that no longer exists; ids in fail answer a server error.
type fakeNet struct {
	mu      sync.Mutex
	deletes []string // escaped paths, in order
	ident   map[string]string
	gone    map[string]string // escaped path -> status + body for an object that does not exist
	fail    map[string]bool
	okBody  string
}

func (f *fakeNet) server(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.EscapedPath()
		if r.Method == "GET" {
			if b, ok := f.ident[p]; ok {
				w.Write([]byte(b))
				return
			}
			w.Write([]byte(`{}`)) // avatar and other best-effort reads made while connecting
			return
		}
		if r.Method != "DELETE" {
			t.Errorf("unexpected %s %s", r.Method, p)
			w.WriteHeader(400)
			return
		}
		f.mu.Lock()
		f.deletes = append(f.deletes, p)
		f.mu.Unlock()
		if f.fail[p] {
			w.WriteHeader(500)
			w.Write([]byte(`{"error":"boom"}`))
			return
		}
		if g, ok := f.gone[p]; ok {
			code := 404
			if strings.HasPrefix(g, "400 ") {
				code, g = 400, g[4:]
			}
			w.WriteHeader(code)
			w.Write([]byte(g))
			return
		}
		w.Write([]byte(f.okBody))
	}))
}

func TestDeletePostRemote(t *testing.T) {
	graphGone := `400 {"error":{"message":"Unsupported delete request. Object with ID does not exist","code":100,"error_subcode":33}}`
	cases := []struct {
		name     string
		provider string
		creds    func(base string) map[string]string
		ident    map[string]string
		okBody   string
		ids      []string
		goneID   string // path answered "does not exist": still counts as deleted
		want     []string
		check    func(t *testing.T, r *http.Request)
	}{
		{
			name: "x thread, last reply first", provider: "x",
			creds: func(b string) map[string]string {
				return map[string]string{"consumer_key": "a", "consumer_secret": "b", "access_token": "c", "access_token_secret": "d", "base_url": b}
			},
			ident:  map[string]string{"/2/users/me": `{"data":{"id":"42","username":"alice"}}`},
			okBody: `{"data":{"deleted":true}}`,
			ids:    []string{"101", "102", "103"},
			goneID: "/2/tweets/102",
			want:   []string{"/2/tweets/103", "/2/tweets/102", "/2/tweets/101"},
		},
		{
			name: "threads thread, last reply first", provider: "threads",
			creds: func(b string) map[string]string {
				return map[string]string{"access_token": "tok", "base_url": b, "token_refreshed_at": time.Now().Format(time.RFC3339)}
			},
			ident:  map[string]string{"/v1.0/me": `{"id":"th1","username":"rmp"}`},
			okBody: `{"success":true}`,
			ids:    []string{"t1", "t2"},
			goneID: "/v1.0/t2",
			want:   []string{"/v1.0/t2", "/v1.0/t1"},
		},
		{
			name: "facebook post, comments go with it", provider: "facebook",
			creds: func(b string) map[string]string {
				return map[string]string{"page_id": "123", "page_access_token": "tok", "base_url": b}
			},
			ident:  map[string]string{"/v26.0/123": `{"id":"123","name":"Page","username":"page"}`},
			okBody: `{"success":true}`,
			ids:    []string{"123_9", "123_9_c1"},
			want:   []string{"/v26.0/123_9"},
		},
		{
			name: "linkedin post, comments go with it", provider: "linkedin-page",
			creds: func(b string) map[string]string {
				return map[string]string{"organization_id": "42", "access_token": "tok", "base_url": b}
			},
			ident: map[string]string{"/rest/organizations/42": `{"id":42,"localizedName":"RMP","vanityName":"rmp"}`},
			ids:   []string{"urn:li:share:7", "urn:li:comment:(urn:li:activity:8,1)"},
			want:  []string{"/rest/posts/urn%3Ali%3Ashare%3A7"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			ctx := context.Background()
			fn := &fakeNet{ident: c.ident, okBody: c.okBody, gone: map[string]string{}, fail: map[string]bool{}}
			if c.goneID != "" {
				fn.gone[c.goneID] = `{"errors":[{"title":"Not Found Error"}]}`
				if c.provider == "threads" {
					fn.gone[c.goneID] = graphGone
				}
			}
			srv := fn.server(t)
			defer srv.Close()
			acc, err := e.svc.ConnectAccount(ctx, c.provider, "", c.creds(srv.URL))
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			p := &store.Post{AccountID: acc.ID, Status: store.StatusPublished, Parts: make([]string, len(c.ids)), RemoteID: c.ids[0], RemoteIDs: c.ids, PublishedAt: &now}
			if err := e.svc.Store.CreatePost(ctx, p); err != nil {
				t.Fatal(err)
			}

			// Network refuses: the Relay copy stays, so the call can be made again.
			fn.fail[c.want[0]] = true
			code, _, raw := e.req("DELETE", "/v1/posts/"+p.ID+"?remote=true", nil)
			if code == 200 {
				t.Fatalf("want an error when the network refuses, got %s", raw)
			}
			if _, err := e.svc.Store.GetPost(ctx, p.ID); err != nil {
				t.Fatalf("post must stay in Relay after a failed network delete: %v", err)
			}
			fn.fail = map[string]bool{}
			fn.deletes = nil

			text, isErr := e.callTool("delete_post", map[string]any{"id": p.ID, "remote": true})
			if isErr {
				t.Fatalf("delete_post remote: %s", text)
			}
			var res struct {
				Deleted       bool     `json:"deleted"`
				RemoteDeleted []string `json:"remote_deleted"`
			}
			json.Unmarshal([]byte(text), &res)
			if !res.Deleted || len(res.RemoteDeleted) != len(c.ids) {
				t.Fatalf("result: %s", text)
			}
			if strings.Join(fn.deletes, " ") != strings.Join(c.want, " ") {
				t.Fatalf("network deletes:\n got  %v\n want %v", fn.deletes, c.want)
			}
			if _, err := e.svc.Store.GetPost(ctx, p.ID); err == nil {
				t.Fatal("post still in Relay")
			}
		})
	}
}

func TestDeletePostLocalOnly(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	fn := &fakeNet{ident: map[string]string{"/2/users/me": `{"data":{"id":"42","username":"alice"}}`}}
	srv := fn.server(t)
	defer srv.Close()
	acc, err := e.svc.ConnectAccount(ctx, "x", "", map[string]string{"consumer_key": "a", "consumer_secret": "b", "access_token": "c", "access_token_secret": "d", "base_url": srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	p := &store.Post{AccountID: acc.ID, Status: store.StatusPublished, Parts: []string{"x"}, RemoteID: "101", RemoteIDs: []string{"101"}}
	if err := e.svc.Store.CreatePost(ctx, p); err != nil {
		t.Fatal(err)
	}
	if text, isErr := e.callTool("delete_post", map[string]any{"id": p.ID}); isErr || !strings.Contains(text, `"deleted": true`) {
		t.Fatalf("delete_post: %s", text)
	}
	if len(fn.deletes) != 0 {
		t.Fatalf("without remote, nothing is deleted on the network: %v", fn.deletes)
	}
}

func TestDeletePostRemoteUnsupported(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	ws := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"name":"Site","url":"https://site.example","version":"1"}`))
	}))
	defer ws.Close()
	acc, err := e.svc.ConnectAccount(ctx, "website", "", map[string]string{"base_url": ws.URL, "token": "s"})
	if err != nil {
		t.Fatal(err)
	}
	p := &store.Post{AccountID: acc.ID, Status: store.StatusPublished, Parts: []string{"x"}, RemoteID: "r1", RemoteIDs: []string{"r1"}}
	if err := e.svc.Store.CreatePost(ctx, p); err != nil {
		t.Fatal(err)
	}
	code, _, raw := e.req("DELETE", "/v1/posts/"+p.ID+"?remote=true", nil)
	if code != 400 || !strings.Contains(string(raw), "cannot delete published posts") {
		t.Fatalf("want 400 unsupported, got %d %s", code, raw)
	}
	if _, err := e.svc.Store.GetPost(ctx, p.ID); err != nil {
		t.Fatalf("post must stay: %v", err)
	}
}
