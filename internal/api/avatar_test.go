package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Seeyko/relay/internal/api"
	"github.com/Seeyko/relay/internal/core"
	"github.com/Seeyko/relay/internal/mcp"
	"github.com/Seeyko/relay/internal/store"
)

// pictures is a fake CDN: each path serves the bytes set for it and counts the downloads.
type pictures struct {
	mu    sync.Mutex
	files map[string][]byte
	types map[string]string
	hits  map[string]int
	srv   *httptest.Server
}

func newPictures(t *testing.T) *pictures {
	p := &pictures{files: map[string][]byte{}, types: map[string]string{}, hits: map[string]int{}}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.hits[r.URL.Path]++
		b, ok := p.files[r.URL.Path]
		if !ok {
			w.WriteHeader(404)
			return
		}
		if ct := p.types[r.URL.Path]; ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		w.Write(b)
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *pictures) set(path, ct string, b []byte) {
	p.mu.Lock()
	p.files[path], p.types[path] = b, ct
	p.mu.Unlock()
}

func (p *pictures) count(path string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.hits[path]
}

var (
	jpegA = []byte("\xff\xd8\xff\xe0 avatar A")
	jpegB = []byte("\xff\xd8\xff\xe0 avatar B")
	logo  = []byte("\x89PNG\r\n\x1a\n the brand logo")
)

// fakeXProfile is an X API whose /2/users/me returns a profile picture on pics.
func fakeXProfile(pics *pictures, path string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/2/users/me" {
			w.WriteHeader(404)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "42", "name": "Ride My Park", "username": "ridemypark",
			"profile_image_url": pics.srv.URL + path + "_normal.jpg", "public_metrics": map[string]int{"followers_count": 10}}})
	}
}

func (e *env) account(id string) *store.Account {
	e.t.Helper()
	a, err := e.svc.Store.GetAccount(context.Background(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	return a
}

func (e *env) mediaBytes(path string) []byte {
	e.t.Helper()
	resp, err := http.Get(e.srv.URL + path)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		e.t.Fatalf("GET %s: %d", path, resp.StatusCode)
	}
	return b
}

// The network's picture is downloaded on connect, then at most once a day with the account counters;
// an unchanged picture is not stored twice, a new one replaces (and deletes) the old copy.
func TestAvatarFromNetwork(t *testing.T) {
	e := newEnv(t)
	pics := newPictures(t)
	pics.set("/me_400x400.jpg", "image/jpeg", jpegA)
	clock := time.Date(2026, 12, 1, 8, 0, 0, 0, time.UTC)
	e.svc.Now = func() time.Time { return clock }
	ctx := context.Background()

	acc := connectFakeX(t, e, fakeXProfile(pics, "/me"))
	if acc.AvatarSource != store.AvatarAuto || acc.AvatarMediaID == "" || !strings.HasPrefix(acc.Avatar, "/media/med_") {
		t.Fatalf("connect did not fetch the picture: %+v", acc)
	}
	if got := e.mediaBytes(acc.Avatar); !bytes.Equal(got, jpegA) {
		t.Fatalf("served avatar %q", got)
	}
	// list_accounts and the REST list expose it.
	code, _, raw := e.req("GET", "/v1/accounts", nil)
	if code != 200 || !strings.Contains(string(raw), `"avatar":"`+acc.Avatar+`"`) || !strings.Contains(string(raw), `"avatar_source":"auto"`) {
		t.Fatalf("list accounts: %d %s", code, raw)
	}
	first := acc.AvatarMediaID

	// Same day: the counters are collected, the picture is not downloaded again.
	clock = clock.Add(7 * time.Hour)
	if _, err := e.svc.SnapshotAccount(ctx, e.account(acc.ID)); err != nil {
		t.Fatal(err)
	}
	if n := pics.count("/me_400x400.jpg"); n != 1 {
		t.Fatalf("downloaded %d times within a day", n)
	}

	// A day later, same bytes: downloaded, but the stored copy is kept.
	clock = clock.Add(24 * time.Hour)
	if _, err := e.svc.SnapshotAccount(ctx, e.account(acc.ID)); err != nil {
		t.Fatal(err)
	}
	a := e.account(acc.ID)
	if pics.count("/me_400x400.jpg") != 2 || a.AvatarMediaID != first || a.AvatarCheckedAt == nil || !a.AvatarCheckedAt.Equal(clock) {
		t.Fatalf("daily refresh of an unchanged picture: %d downloads, %+v", pics.count("/me_400x400.jpg"), a)
	}

	// The owner changed the picture on the network: the new one replaces the old copy.
	pics.set("/me_400x400.jpg", "image/jpeg", jpegB)
	clock = clock.Add(24 * time.Hour)
	if _, err := e.svc.SnapshotAccount(ctx, e.account(acc.ID)); err != nil {
		t.Fatal(err)
	}
	a = e.account(acc.ID)
	if a.AvatarMediaID == first || !bytes.Equal(e.mediaBytes(a.Avatar), jpegB) {
		t.Fatalf("new picture not stored: %+v", a)
	}
	if _, err := e.svc.Store.GetMedia(ctx, first); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("old automatic picture kept: %v", err)
	}

	// The preview carries it for the dashboard.
	text, isErr := e.callTool("schedule_post", map[string]any{"account_ids": []string{acc.ID}, "text": "hi", "scheduled_at": "draft"})
	if isErr {
		t.Fatal(text)
	}
	var ps []store.Post
	json.Unmarshal([]byte(text), &ps)
	pv, err := e.svc.PreviewGroup(ctx, ps[0].ID)
	if err != nil || pv.Posts[0].AccountAvatar != a.Avatar {
		t.Fatalf("preview avatar %q, want %q (%v)", pv.Posts[0].AccountAvatar, a.Avatar, err)
	}
	if raw, _ := json.Marshal(pv.Posts[0]); !strings.Contains(string(raw), `"account_avatar":"`+a.Avatar+`"`) {
		t.Fatalf("preview JSON %s", raw)
	}

	// import_history refreshes it too.
	pics.set("/me_400x400.jpg", "image/jpeg", jpegA)
	_, _ = e.svc.ImportHistory(ctx, acc.ID, time.Time{}, 1, core.ImportOptions{SkipVisuals: true}) // the fake has no timeline: the import errors
	if b := e.mediaBytes(e.account(acc.ID).Avatar); !bytes.Equal(b, jpegA) {
		t.Fatalf("import did not refresh the picture: %q", b)
	}
}

// A picture that cannot be used never fails connect or collect: the account has no picture, the
// check is recorded so the network is not asked again before tomorrow.
func TestAvatarFailuresAreBestEffort(t *testing.T) {
	pics := newPictures(t)
	pics.set("/huge_400x400.jpg", "image/jpeg", bytes.Repeat([]byte{0xff}, 6<<20))
	pics.set("/video_400x400.jpg", "video/mp4", []byte("\x00\x00\x00\x18ftypmp42"))
	for _, path := range []string{"/missing", "/huge", "/video"} {
		t.Run(path, func(t *testing.T) {
			e := newEnv(t)
			acc := connectFakeX(t, e, fakeXProfile(pics, path))
			if acc.AvatarMediaID != "" || acc.Avatar != "" || acc.AvatarCheckedAt == nil {
				t.Fatalf("bad picture stored: %+v", acc)
			}
			if _, err := e.svc.SnapshotAccount(context.Background(), e.account(acc.ID)); err != nil {
				t.Fatalf("collect failed: %v", err)
			}
			ms, _ := e.svc.Store.ListMedia(context.Background())
			if len(ms) != 0 {
				t.Fatalf("media stored: %+v", ms)
			}
		})
	}
	// The network itself failing on the picture lookup.
	e := newEnv(t)
	calls := 0
	acc := connectFakeX(t, e, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 { // Identify succeeds without a picture, the separate lookup fails
			w.Write([]byte(`{"data":{"id":"42","username":"alice"}}`))
			return
		}
		w.WriteHeader(503)
	}))
	if acc.AvatarMediaID != "" || acc.AvatarCheckedAt == nil {
		t.Fatalf("%+v", acc)
	}
}

// A manual picture (one account or all) wins over the network's until it is cleared.
func TestManualAvatar(t *testing.T) {
	e := newEnv(t)
	pics := newPictures(t)
	pics.set("/a_400x400.jpg", "image/jpeg", jpegA)
	pics.set("/logo.png", "", logo) // no Content-Type: the bytes are sniffed
	pics.set("/clip.mp4", "video/mp4", []byte("\x00\x00\x00\x18ftypmp42"))
	clock := time.Date(2026, 12, 1, 8, 0, 0, 0, time.UTC)
	e.svc.Now = func() time.Time { return clock }
	ctx := context.Background()
	one := connectFakeX(t, e, fakeXProfile(pics, "/a"))
	two := connectFakeX(t, e, fakeXProfile(pics, "/a"))
	autoOne := one.AvatarMediaID

	accounts := func(raw []byte) []store.Account {
		t.Helper()
		var as []store.Account
		if err := json.Unmarshal(raw, &as); err != nil {
			t.Fatalf("want accounts: %s", raw)
		}
		return as
	}

	// One account, from a URL.
	code, _, raw := e.req("PUT", "/v1/accounts/"+one.ID+"/avatar", map[string]any{"media_url": pics.srv.URL + "/logo.png"})
	as := accounts(raw)
	if code != 200 || len(as) != 1 || as[0].AvatarSource != store.AvatarManual || !bytes.Equal(e.mediaBytes(as[0].Avatar), logo) {
		t.Fatalf("set one: %d %s", code, raw)
	}
	manual := as[0].AvatarMediaID
	if _, err := e.svc.Store.GetMedia(ctx, autoOne); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the replaced automatic picture should be deleted: %v", err)
	}
	if a := e.account(two.ID); a.AvatarSource != store.AvatarAuto {
		t.Fatalf("other account changed: %+v", a)
	}

	// The daily refresh does not touch a manual picture, even when the network's one changed.
	pics.set("/a_400x400.jpg", "image/jpeg", jpegB)
	clock = clock.Add(25 * time.Hour)
	for _, id := range []string{one.ID, two.ID} {
		if _, err := e.svc.SnapshotAccount(ctx, e.account(id)); err != nil {
			t.Fatal(err)
		}
	}
	if a := e.account(one.ID); a.AvatarMediaID != manual || a.AvatarSource != store.AvatarManual {
		t.Fatalf("manual picture replaced: %+v", a)
	}
	if a := e.account(two.ID); !bytes.Equal(e.mediaBytes(a.Avatar), jpegB) {
		t.Fatalf("automatic picture not refreshed: %+v", a)
	}

	// All accounts, from a media id of the library (MCP tool).
	text, isErr := e.callTool("set_account_avatar", map[string]any{"account_id": "all", "media": manual})
	as = accounts([]byte(text))
	if isErr || len(as) != 2 || as[0].AvatarMediaID != manual || as[1].AvatarMediaID != manual || as[1].AvatarSource != store.AvatarManual {
		t.Fatalf("set all: %s", text)
	}
	code, _, raw = e.req("GET", "/v1/accounts", nil)
	if code != 200 || strings.Count(string(raw), `"avatar_source":"manual"`) != 2 {
		t.Fatalf("list: %s", raw)
	}

	// Clearing one account brings the network's picture back right away; the manual media stays.
	code, _, raw = e.req("PUT", "/v1/accounts/"+one.ID+"/avatar", map[string]any{"clear": true})
	as = accounts(raw)
	if code != 200 || as[0].AvatarSource != store.AvatarAuto || !bytes.Equal(e.mediaBytes(as[0].Avatar), jpegB) {
		t.Fatalf("clear one: %d %s", code, raw)
	}
	if a := e.account(two.ID); a.AvatarMediaID != manual {
		t.Fatalf("clear touched the other account: %+v", a)
	}
	// Clearing all.
	code, _, raw = e.req("PUT", "/v1/accounts/avatar", map[string]any{"clear": true})
	as = accounts(raw)
	if code != 200 || len(as) != 2 || as[0].AvatarSource != store.AvatarAuto || as[1].AvatarSource != store.AvatarAuto {
		t.Fatalf("clear all: %d %s", code, raw)
	}
	if _, err := e.svc.Store.GetMedia(ctx, manual); err != nil {
		t.Fatalf("the manual media must stay in the library: %v", err)
	}

	// Mistakes are 400s.
	_, _, vraw := e.req("POST", "/v1/media", map[string]any{"url": pics.srv.URL + "/clip.mp4"})
	var up struct{ Media store.Media }
	json.Unmarshal(vraw, &up)
	for name, body := range map[string]map[string]any{
		"nothing":    {},
		"both":       {"media": manual, "media_url": pics.srv.URL + "/logo.png"},
		"clear+url":  {"clear": true, "media_url": pics.srv.URL + "/logo.png"},
		"unknown":    {"media": "med_nope"},
		"video id":   {"media": up.Media.ID},
		"video url":  {"media_url": pics.srv.URL + "/clip.mp4"},
		"broken url": {"media_url": pics.srv.URL + "/missing.png"},
	} {
		if code, _, raw := e.req("PUT", "/v1/accounts/"+one.ID+"/avatar", body); code != 400 {
			t.Errorf("%s: want 400, got %d %s", name, code, raw)
		}
	}
	if code, _, raw := e.req("PUT", "/v1/accounts/acc_nope/avatar", map[string]any{"media": manual}); code != 400 {
		t.Errorf("unknown account: want 400, got %d %s", code, raw)
	}
}

// A drafts-only key can neither see nor use set_account_avatar.
func TestAvatarRefusedToDrafter(t *testing.T) {
	e := newEnv(t)
	pics := newPictures(t)
	pics.set("/a_400x400.jpg", "image/jpeg", jpegA)
	acc := connectFakeX(t, e, fakeXProfile(pics, "/a"))
	a := &api.API{Svc: e.svc, MCP: mcp.NewServer("relay", "test", mcp.BuildTools(e.svc)), Token: token, DraftToken: "drafter-token-0123456", Log: e.svc.Log}
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	call := func(method, path string, body any) (int, string) {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest(method, srv.URL+path, bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer drafter-token-0123456")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(raw)
	}
	body := map[string]any{"media_url": pics.srv.URL + "/a_400x400.jpg"}
	if code, raw := call("PUT", "/v1/accounts/"+acc.ID+"/avatar", body); code != 403 {
		t.Fatalf("drafter, one account: want 403, got %d %s", code, raw)
	}
	if code, raw := call("PUT", "/v1/accounts/avatar", map[string]any{"clear": true}); code != 403 {
		t.Fatalf("drafter, all accounts: want 403, got %d %s", code, raw)
	}
	if code, raw := call("POST", "/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"}); code != 200 || strings.Contains(raw, `"name":"set_account_avatar"`) {
		t.Fatalf("drafter sees the tool: %d %s", code, raw)
	}
	if _, raw := call("POST", "/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "set_account_avatar", "arguments": map[string]any{"account_id": "all", "clear": true}}}); !strings.Contains(raw, "isError") && !strings.Contains(raw, "error") {
		t.Fatalf("drafter called the tool: %s", raw)
	}
	if a := e.account(acc.ID); a.AvatarSource != store.AvatarAuto {
		t.Fatalf("drafter changed the picture: %+v", a)
	}
}
