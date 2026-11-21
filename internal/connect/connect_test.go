package connect

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Seeyko/relay/internal/core"
	"github.com/Seeyko/relay/internal/secret"
	"github.com/Seeyko/relay/internal/store"

	// connectors register themselves
	_ "github.com/Seeyko/relay/internal/providers/linkedin"
	_ "github.com/Seeyko/relay/internal/providers/meta"
	_ "github.com/Seeyko/relay/internal/providers/threads"
	_ "github.com/Seeyko/relay/internal/providers/tiktok"
	_ "github.com/Seeyko/relay/internal/providers/x"
	_ "github.com/Seeyko/relay/internal/providers/youtube"
)

// ---------- harness: Relay + one fake network ----------

type harness struct {
	t     *testing.T
	st    *store.Store
	svc   *core.Service
	m     *Manager
	relay *httptest.Server
	fake  *httptest.Server
	mux   *http.ServeMux
	logs  *syncBuf

	mu      sync.Mutex
	authQ   url.Values // the last authorization request
	signIn  string     // the remote account the fake network signs in: A or B
	clock   time.Time
	session string // dashboard member seen by the start page
	calls   []string
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// users are the remote accounts of every fake network, by sign-in.
var users = map[string]struct{ id, handle, name string }{
	"A": {"111", "alice", "Alice"},
	"B": {"222", "bob", "Bob"},
}

// who is the sign-in a token was issued for: tokens end with -A or -B.
func who(tok string) string {
	if i := strings.LastIndexByte(tok, '-'); i >= 0 {
		return tok[i+1:]
	}
	return ""
}

func newHarness(t *testing.T, env map[string]string) *harness {
	t.Helper()
	h := &harness{t: t, mux: http.NewServeMux(), logs: &syncBuf{}, signIn: "A", clock: time.Now()}
	h.fake = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.calls = append(h.calls, r.Method+" "+r.URL.Path)
		h.mu.Unlock()
		h.mux.ServeHTTP(w, r)
	}))
	t.Cleanup(h.fake.Close)
	var handler http.Handler = http.NotFoundHandler()
	h.relay = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(w, r) }))
	t.Cleanup(h.relay.Close)

	st, err := store.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	box, err := secret.New("test-encryption-key-0123456789")
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(h.logs, nil))
	h.st = st
	h.svc = &core.Service{Store: st, Box: box, MediaDir: t.TempDir(), BaseURL: h.relay.URL, HTTP: &http.Client{Timeout: 10 * time.Second}, Log: log}

	nets := FromEnv(func(k string) string { return env[k] })
	f := h.fake.URL
	for _, n := range nets {
		n.AuthURL = f + "/auth/" + n.Slug
		n.TokenURL = f + "/token/" + n.Slug
		switch n.Slug {
		case "threads", "instagram-login", "linkedin", "x":
			n.API = f
		case "facebook", "instagram":
			n.API = f + "/v26.0"
		}
		n.CredExtra = map[string]string{"base_url": f}
		if n.Slug == "linkedin" || n.Slug == "youtube" {
			n.CredExtra["token_url"] = f + "/token/" + n.Slug
		}
	}
	h.m, err = New(h.relay.URL, h.svc, nets, log)
	if err != nil {
		t.Fatal(err)
	}
	h.m.now = func() time.Time { h.mu.Lock(); defer h.mu.Unlock(); return h.clock }
	mux := http.NewServeMux()
	h.m.Register(mux, func(*http.Request) string { h.mu.Lock(); defer h.mu.Unlock(); return h.session })
	handler = mux

	// The consent screen of every OAuth 2 network: it signs h.signIn in and comes back.
	h.mux.HandleFunc("GET /auth/{net}", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		h.mu.Lock()
		h.authQ = q
		in := h.signIn
		h.mu.Unlock()
		if r.PathValue("net") == "x" {
			back := url.Values{"oauth_token": {q.Get("oauth_token")}, "oauth_verifier": {"verifier-" + in}}
			http.Redirect(w, r, h.relay.URL+"/connect/x/callback?"+back.Encode(), http.StatusFound)
			return
		}
		back := url.Values{"code": {"code-" + in}, "state": {q.Get("state")}}
		http.Redirect(w, r, q.Get("redirect_uri")+"?"+back.Encode(), http.StatusFound)
	})
	return h
}

func (h *harness) advance(d time.Duration) { h.mu.Lock(); h.clock = h.clock.Add(d); h.mu.Unlock() }
func (h *harness) setSignIn(s string)      { h.mu.Lock(); h.signIn = s; h.mu.Unlock() }
func (h *harness) lastAuth() url.Values    { h.mu.Lock(); defer h.mu.Unlock(); return h.authQ }

func browser() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, Timeout: 20 * time.Second}
}

// stopAt is a browser that stops before following a redirect to the Relay callback or picker.
func stopAt(path string) *http.Client {
	c := browser()
	c.CheckRedirect = func(req *http.Request, _ []*http.Request) error {
		if strings.HasSuffix(req.URL.Path, path) {
			return http.ErrUseLastResponse
		}
		return nil
	}
	return c
}

func (h *harness) begin(req Request) *Started {
	h.t.Helper()
	st, err := h.m.Begin(context.Background(), req)
	if err != nil {
		h.t.Fatal(err)
	}
	return st
}

func read(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func (h *harness) postForm(c *http.Client, path string, form url.Values, origin string) *http.Response {
	h.t.Helper()
	req, _ := http.NewRequest("POST", h.relay.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := c.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	return resp
}

func startToken(t *testing.T, st *Started) string {
	u, err := url.Parse(st.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query().Get("t")
}

// run opens the start link in c, presses Continue and follows the network back to Relay.
func (h *harness) run(c *http.Client, st *Started) (int, string) {
	h.t.Helper()
	resp, err := c.Get(st.URL)
	if err != nil {
		h.t.Fatal(err)
	}
	page := read(h.t, resp)
	if resp.StatusCode != 200 || !strings.Contains(page, "Continue") {
		h.t.Fatalf("start page: %d %s", resp.StatusCode, page)
	}
	resp = h.postForm(c, "/connect/"+st.Network+"/start", url.Values{"t": {startToken(h.t, st)}}, h.relay.URL)
	return resp.StatusCode, read(h.t, resp)
}

func (h *harness) creds(accID string) map[string]string {
	h.t.Helper()
	acc, err := h.st.GetAccount(context.Background(), accID)
	if err != nil {
		h.t.Fatal(err)
	}
	c := map[string]string{}
	if err := h.svc.Box.OpenJSON(acc.Credentials, &c); err != nil {
		h.t.Fatal(err)
	}
	return c
}

func (h *harness) accounts() []*store.Account {
	h.t.Helper()
	accs, err := h.st.ListAccounts(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	return accs
}

// noSecrets checks that no token of the fake networks reached a page or the logs.
func (h *harness) noSecrets(page string) {
	h.t.Helper()
	re := regexp.MustCompile(`(short|long|at|ats|rt|user)-[AB]\b|pt-\d|req-secret|secret-value`)
	if m := re.FindString(page); m != "" {
		h.t.Errorf("page shows a secret %q", m)
	}
	if m := re.FindString(h.logs.String()); m != "" {
		h.t.Errorf("logs show a secret %q:\n%s", m, h.logs.String())
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"error":{"message":%q}}`, msg)
}

// ---------- fake networks ----------

func (h *harness) fakeThreads() {
	h.mux.HandleFunc("POST /token/threads", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f := r.PostForm
		if f.Get("client_id") != "th-id" || f.Get("client_secret") != "secret-value" || f.Get("grant_type") != "authorization_code" ||
			f.Get("redirect_uri") != h.relay.URL+"/connect/threads/callback" || !strings.HasPrefix(f.Get("code"), "code-") {
			fail(w, 400, "bad exchange")
			return
		}
		writeJSON(w, map[string]any{"access_token": "short-" + who(f.Get("code")), "user_id": 111})
	})
	h.mux.HandleFunc("GET /access_token", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("grant_type") != "th_exchange_token" || q.Get("client_secret") != "secret-value" || !strings.HasPrefix(q.Get("access_token"), "short-") {
			fail(w, 400, "bad long-lived exchange")
			return
		}
		writeJSON(w, map[string]any{"access_token": "long-" + who(q.Get("access_token")), "token_type": "bearer", "expires_in": 5183944})
	})
	h.mux.HandleFunc("GET /v1.0/me", func(w http.ResponseWriter, r *http.Request) {
		u, ok := users[who(r.URL.Query().Get("access_token"))]
		if !ok {
			fail(w, 401, "bad token")
			return
		}
		writeJSON(w, map[string]any{"id": u.id, "username": u.handle, "name": u.name})
	})
}

var threadsEnv = map[string]string{"RELAY_THREADS_APP_ID": "th-id", "RELAY_THREADS_APP_SECRET": "secret-value"}

// existing connects an account with old credentials, as connect_account would.
func (h *harness) existing(provider string, creds map[string]string) *store.Account {
	h.t.Helper()
	c := map[string]string{"base_url": h.fake.URL}
	for k, v := range creds {
		c[k] = v
	}
	acc, err := h.svc.ConnectAccount(context.Background(), provider, "", c)
	if err != nil {
		h.t.Fatal(err)
	}
	return acc
}

// ---------- tests ----------

func TestFromEnvOnlyConfiguredNetworks(t *testing.T) {
	nets := FromEnv(func(k string) string {
		return map[string]string{"RELAY_THREADS_APP_ID": "a", "RELAY_THREADS_APP_SECRET": "b", "RELAY_META_APP_ID": "c",
			"RELAY_X_CONSUMER_KEY": "k", "RELAY_X_CONSUMER_SECRET": "s", "RELAY_CONNECT_X_SCOPES": ""}[k]
	})
	var got []string
	for _, n := range nets {
		got = append(got, n.Slug)
	}
	if strings.Join(got, ",") != "threads,x" { // META has no secret: no Facebook button
		t.Fatalf("networks = %v", got)
	}
	over := FromEnv(func(k string) string {
		return map[string]string{"RELAY_LINKEDIN_CLIENT_ID": "a", "RELAY_LINKEDIN_CLIENT_SECRET": "b",
			"RELAY_CONNECT_LINKEDIN_SCOPES": "w_organization_social, r_organization_social"}[k]
	})
	if s := strings.Join(over[0].Scopes, " "); s != "w_organization_social r_organization_social" {
		t.Fatalf("scope override = %q", s)
	}
}

func TestThreadsConnect(t *testing.T) {
	h := newHarness(t, threadsEnv)
	h.fakeThreads()
	st := h.begin(Request{Network: "threads"})
	if st.RedirectURI != h.relay.URL+"/connect/threads/callback" || !strings.HasPrefix(st.URL, h.relay.URL+"/connect/threads/start?t=") {
		t.Fatalf("started = %+v", st)
	}
	code, page := h.run(browser(), st)
	if code != 200 || !strings.Contains(page, "Threads connected") || !strings.Contains(page, "@alice") {
		t.Fatalf("%d %s", code, page)
	}
	q := h.lastAuth()
	if q.Get("client_id") != "th-id" || q.Get("response_type") != "code" || q.Get("redirect_uri") != st.RedirectURI || len(q.Get("state")) < 40 {
		t.Fatalf("authorization request = %v", q)
	}
	if q.Get("scope") != "threads_basic,threads_content_publish,threads_manage_replies,threads_read_replies,threads_manage_insights,threads_delete" {
		t.Fatalf("scope = %q", q.Get("scope"))
	}
	accs := h.accounts()
	if len(accs) != 1 || accs[0].Provider != "threads" || accs[0].RemoteUserID != "111" || accs[0].Handle != "alice" {
		t.Fatalf("accounts = %+v", accs)
	}
	c := h.creds(accs[0].ID)
	if c["access_token"] != "long-A" || c["user_id"] != "111" || c["token_refreshed_at"] == "" || c["token_expires_at"] == "" {
		t.Fatalf("creds = %v", c)
	}
	h.noSecrets(page)
}

func TestThreadsReconnectSameAccount(t *testing.T) {
	h := newHarness(t, threadsEnv)
	h.fakeThreads()
	acc := h.existing("threads", map[string]string{"access_token": "old-A", "token_refreshed_at": time.Now().Format(time.RFC3339)})
	st := h.begin(Request{AccountID: acc.ID})
	if st.Network != "threads" {
		t.Fatalf("network = %q", st.Network)
	}
	code, page := h.run(browser(), st)
	if code != 200 || !strings.Contains(page, "Threads reconnected") {
		t.Fatalf("%d %s", code, page)
	}
	accs := h.accounts()
	if len(accs) != 1 || accs[0].ID != acc.ID {
		t.Fatalf("accounts = %+v", accs)
	}
	if c := h.creds(acc.ID); c["access_token"] != "long-A" || c["base_url"] != h.fake.URL {
		t.Fatalf("creds = %v", c)
	}
	h.noSecrets(page)
}

func TestReconnectWrongAccountRefused(t *testing.T) {
	h := newHarness(t, threadsEnv)
	h.fakeThreads()
	acc := h.existing("threads", map[string]string{"access_token": "old-A", "token_refreshed_at": time.Now().Format(time.RFC3339)})
	h.setSignIn("B")
	code, page := h.run(browser(), h.begin(Request{AccountID: acc.ID}))
	if code != http.StatusConflict || !strings.Contains(page, "You signed in as Bob (@bob), not @alice") {
		t.Fatalf("%d %s", code, page)
	}
	if c := h.creds(acc.ID); c["access_token"] != "old-A" {
		t.Fatalf("credentials changed: %v", c)
	}
	if n := len(h.accounts()); n != 1 {
		t.Fatalf("%d accounts", n)
	}
	h.noSecrets(page)
}

func TestConnectExistingUpdatesInPlace(t *testing.T) {
	h := newHarness(t, threadsEnv)
	h.fakeThreads()
	acc := h.existing("threads", map[string]string{"access_token": "old-A", "token_refreshed_at": time.Now().Format(time.RFC3339)})
	code, page := h.run(browser(), h.begin(Request{Network: "threads"}))
	if code != 200 || !strings.Contains(page, "reconnected") {
		t.Fatalf("%d %s", code, page)
	}
	if accs := h.accounts(); len(accs) != 1 || h.creds(acc.ID)["access_token"] != "long-A" {
		t.Fatalf("not updated in place: %+v", accs)
	}
}

func TestStateSingleUseAndBinding(t *testing.T) {
	h := newHarness(t, threadsEnv)
	h.fakeThreads()

	// The start link is single use; opening it (link previews) does not use it.
	st := h.begin(Request{Network: "threads"})
	c := stopAt("/callback")
	for i := 0; i < 2; i++ {
		resp, _ := c.Get(st.URL)
		if page := read(t, resp); resp.StatusCode != 200 || !strings.Contains(page, "Continue") {
			t.Fatalf("GET %d: %d", i, resp.StatusCode)
		}
	}
	resp := h.postForm(c, "/connect/threads/start", url.Values{"t": {startToken(t, st)}}, h.relay.URL)
	read(t, resp)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("network did not redirect back: %d", resp.StatusCode)
	}
	callback := resp.Header.Get("Location")
	if r := h.postForm(browser(), "/connect/threads/start", url.Values{"t": {startToken(t, st)}}, h.relay.URL); r.StatusCode != http.StatusGone {
		t.Fatalf("start link used twice: %d", r.StatusCode)
	}

	// Another browser (no binding cookie) cannot use the callback: CSRF / login injection.
	other, _ := browser().Get(callback)
	if page := read(t, other); other.StatusCode != http.StatusForbidden || !strings.Contains(page, "another browser") {
		t.Fatalf("callback without binding: %d %s", other.StatusCode, page)
	}
	// The state was spent by that attempt: even the right browser cannot use it now.
	again, _ := c.Get(callback)
	if page := read(t, again); again.StatusCode != http.StatusBadRequest || !strings.Contains(page, "already used") {
		t.Fatalf("replayed state: %d %s", again.StatusCode, page)
	}
	if n := len(h.accounts()); n != 0 {
		t.Fatalf("%d accounts connected", n)
	}

	// The right browser, once; a replay of the same callback is refused.
	st = h.begin(Request{Network: "threads"})
	c = stopAt("/callback")
	resp = h.postForm(c, "/connect/threads/start", url.Values{"t": {startToken(t, st)}}, h.relay.URL)
	read(t, resp)
	callback = resp.Header.Get("Location")
	ok, _ := c.Get(callback)
	if page := read(t, ok); ok.StatusCode != 200 || !strings.Contains(page, "connected") {
		t.Fatalf("callback: %d %s", ok.StatusCode, page)
	}
	replay, _ := c.Get(callback)
	if read(t, replay); replay.StatusCode != http.StatusBadRequest {
		t.Fatalf("replay: %d", replay.StatusCode)
	}

	// A forged state is refused.
	forged, _ := c.Get(h.relay.URL + "/connect/threads/callback?code=code-B&state=forged")
	if read(t, forged); forged.StatusCode != http.StatusBadRequest {
		t.Fatalf("forged state: %d", forged.StatusCode)
	}
	if n := len(h.accounts()); n != 1 {
		t.Fatalf("%d accounts", n)
	}
}

func TestStartNeedsSameOrigin(t *testing.T) {
	h := newHarness(t, threadsEnv)
	h.fakeThreads()
	st := h.begin(Request{Network: "threads"})
	for _, origin := range []string{"", "https://evil.example"} {
		resp := h.postForm(browser(), "/connect/threads/start", url.Values{"t": {startToken(t, st)}}, origin)
		if read(t, resp); resp.StatusCode != http.StatusForbidden {
			t.Fatalf("origin %q: %d", origin, resp.StatusCode)
		}
	}
	// "Origin: null" (privacy settings) passes only with Sec-Fetch-Site: same-origin.
	req, _ := http.NewRequest("POST", h.relay.URL+"/connect/threads/start", strings.NewReader(url.Values{"t": {startToken(t, st)}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "null")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, err := browser().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if read(t, resp); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("null origin, cross-site: %d", resp.StatusCode)
	}
	// The link was not spent by the refused posts.
	if code, _ := h.run(browser(), st); code != 200 {
		t.Fatalf("run: %d", code)
	}
}

func TestExpiry(t *testing.T) {
	h := newHarness(t, threadsEnv)
	h.fakeThreads()
	// An expired start link.
	st := h.begin(Request{Network: "threads"})
	h.advance(TTL + time.Second)
	resp, _ := browser().Get(st.URL)
	if read(t, resp); resp.StatusCode != http.StatusGone {
		t.Fatalf("expired start link: %d", resp.StatusCode)
	}
	// A consent that took longer than 10 minutes.
	st = h.begin(Request{Network: "threads"})
	c := stopAt("/callback")
	resp = h.postForm(c, "/connect/threads/start", url.Values{"t": {startToken(t, st)}}, h.relay.URL)
	read(t, resp)
	h.advance(TTL + time.Second)
	late, _ := c.Get(resp.Header.Get("Location"))
	if page := read(t, late); late.StatusCode != http.StatusBadRequest || !strings.Contains(page, "expired") {
		t.Fatalf("late callback: %d %s", late.StatusCode, page)
	}
	if n := len(h.accounts()); n != 0 {
		t.Fatalf("%d accounts", n)
	}
}

func TestNullOriginSameSite(t *testing.T) {
	h := newHarness(t, threadsEnv)
	h.fakeThreads()
	st := h.begin(Request{Network: "threads"})
	req, _ := http.NewRequest("POST", h.relay.URL+"/connect/threads/start", strings.NewReader(url.Values{"t": {startToken(t, st)}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "null")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	resp, err := browser().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if page := read(t, resp); resp.StatusCode != 200 || !strings.Contains(page, "Threads connected") {
		t.Fatalf("%d %s", resp.StatusCode, page)
	}
}

func TestDashboardStartGoesOnAtOnce(t *testing.T) {
	h := newHarness(t, threadsEnv)
	h.fakeThreads()
	ctx := context.Background()
	owner, err := h.st.CreateUser(ctx, &store.User{Email: "o@example.com", Role: store.RoleOwner, WebAuthnID: "w1"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := h.st.CreateUser(ctx, &store.User{Email: "a@example.com", Role: store.RoleAdmin, WebAuthnID: "w2"})
	if err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	h.session = owner.ID
	h.mu.Unlock()
	// Asked by another member: the owner's session does not skip the confirmation page.
	st := h.begin(Request{Network: "threads", UserID: other.ID, Dashboard: true})
	resp, _ := browser().Get(st.URL)
	if page := read(t, resp); !strings.Contains(page, "Continue") {
		t.Fatalf("expected the confirmation page: %s", page)
	}
	// Asked by the owner from the dashboard: straight to the network.
	st = h.begin(Request{Network: "threads", UserID: owner.ID, Dashboard: true})
	resp, _ = browser().Get(st.URL)
	if page := read(t, resp); resp.StatusCode != 200 || !strings.Contains(page, "Threads connected") {
		t.Fatalf("dashboard start: %d %s", resp.StatusCode, page)
	}
}

func TestDemotedMemberRefusedAtCallback(t *testing.T) {
	h := newHarness(t, threadsEnv)
	h.fakeThreads()
	ctx := context.Background()
	u, err := h.st.CreateUser(ctx, &store.User{Email: "a@example.com", Role: store.RoleAdmin, WebAuthnID: "w1"})
	if err != nil {
		t.Fatal(err)
	}
	st := h.begin(Request{Network: "threads", UserID: u.ID})
	editor := store.RoleEditor
	if _, err := h.st.UpdateUser(ctx, u.ID, store.UserPatch{Role: &editor}); err != nil {
		t.Fatal(err)
	}
	code, page := h.run(browser(), st)
	if code != http.StatusForbidden || !strings.Contains(page, "Only owners and admins") {
		t.Fatalf("%d %s", code, page)
	}
	if n := len(h.accounts()); n != 0 {
		t.Fatalf("%d accounts", n)
	}
}

func TestCancelledConsent(t *testing.T) {
	h := newHarness(t, threadsEnv)
	h.fakeThreads()
	st := h.begin(Request{Network: "threads"})
	c := stopAt("/callback")
	resp := h.postForm(c, "/connect/threads/start", url.Values{"t": {startToken(t, st)}}, h.relay.URL)
	read(t, resp)
	loc, _ := url.Parse(resp.Header.Get("Location"))
	q := url.Values{"error": {"access_denied"}, "error_reason": {"user_denied"}, "state": {loc.Query().Get("state")}}
	back, _ := c.Get(h.relay.URL + "/connect/threads/callback?" + q.Encode())
	if page := read(t, back); !strings.Contains(page, "cancelled") {
		t.Fatalf("%d %s", back.StatusCode, page)
	}
	if n := len(h.accounts()); n != 0 {
		t.Fatalf("%d accounts", n)
	}
}

func TestBeginErrors(t *testing.T) {
	h := newHarness(t, threadsEnv)
	h.fakeThreads()
	ctx := context.Background()
	if _, err := h.m.Begin(ctx, Request{Network: "tiktok"}); err == nil || !strings.Contains(err.Error(), "configured: threads") {
		t.Fatalf("unconfigured network: %v", err)
	}
	if _, err := h.m.Begin(ctx, Request{}); err == nil {
		t.Fatal("no network accepted")
	}
	acc := h.existing("threads", map[string]string{"access_token": "old-A", "token_refreshed_at": time.Now().Format(time.RFC3339)})
	if _, err := h.m.Begin(ctx, Request{AccountID: acc.ID, Network: "x"}); err == nil {
		t.Fatal("a Threads account reconnected with X")
	}
	if _, err := h.m.Begin(ctx, Request{AccountID: "acc_missing"}); err == nil {
		t.Fatal("missing account accepted")
	}
}

func TestNetworkErrorsStayOutOfPagesAndLogs(t *testing.T) {
	h := newHarness(t, threadsEnv)
	// The long-lived exchange fails with an answer that echoes the token.
	h.mux = http.NewServeMux()
	h.mux.HandleFunc("GET /auth/{net}", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		http.Redirect(w, r, q.Get("redirect_uri")+"?code=code-A&state="+q.Get("state"), http.StatusFound)
	})
	h.mux.HandleFunc("POST /token/threads", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"access_token": "short-A"})
	})
	h.mux.HandleFunc("GET /access_token", func(w http.ResponseWriter, r *http.Request) {
		fail(w, 400, "token short-A is invalid")
	})
	code, page := h.run(browser(), h.begin(Request{Network: "threads"}))
	if code != http.StatusBadGateway || !strings.Contains(page, "Threads refused the sign-in") {
		t.Fatalf("%d %s", code, page)
	}
	if strings.Contains(page, "short-A") {
		t.Fatal("page shows the network's answer")
	}
	// The network is down: Go's error quotes the URL, with the token in its query string.
	h.fake.Close()
	_, err := h.m.identify(context.Background(), "threads", map[string]string{"access_token": "long-A", "base_url": h.fake.URL, "token_refreshed_at": time.Now().Format(time.RFC3339)})
	if err == nil || !strings.Contains(err.Error(), "long-A") {
		t.Fatalf("expected an error quoting the token, got %v", err)
	}
	if s := scrub(err); strings.Contains(s, "long-A") || !strings.Contains(s, "access_token=REDACTED") {
		t.Fatalf("scrub = %s", s)
	}
}

// ---------- Facebook and Instagram (Facebook Login) ----------

var metaEnv = map[string]string{"RELAY_META_APP_ID": "meta-id", "RELAY_META_APP_SECRET": "secret-value"}

func (h *harness) fakeMeta() {
	h.mux.HandleFunc("GET /token/{net}", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("client_id") != "meta-id" || q.Get("client_secret") != "secret-value" {
			fail(w, 400, "bad client")
			return
		}
		if q.Get("grant_type") == "fb_exchange_token" {
			writeJSON(w, map[string]any{"access_token": "long-" + who(q.Get("fb_exchange_token")), "expires_in": 5183944})
			return
		}
		if q.Get("redirect_uri") != h.relay.URL+"/connect/"+r.PathValue("net")+"/callback" {
			fail(w, 400, "bad redirect_uri")
			return
		}
		writeJSON(w, map[string]any{"access_token": "user-" + who(q.Get("code"))})
	})
	pages := []map[string]any{
		{"id": "p1", "name": "Alpha Page", "username": "alpha", "access_token": "pt-1", "instagram_business_account": map[string]any{"id": "ig1", "username": "alpha.ig"}},
		{"id": "p2", "name": "Beta Page", "access_token": "pt-2"},
	}
	h.mux.HandleFunc("GET /v26.0/me/accounts", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer long-A" {
			fail(w, 401, "bad user token")
			return
		}
		if r.URL.Query().Get("after") == "" {
			writeJSON(w, map[string]any{"data": pages, "paging": map[string]any{"next": h.fake.URL + "/v26.0/me/accounts?after=2&access_token=long-A"}})
			return
		}
		writeJSON(w, map[string]any{"data": []map[string]any{{"id": "p3", "name": "Gamma Page", "access_token": "pt-3",
			"instagram_business_account": map[string]any{"id": "ig3", "username": "gamma.ig", "name": "Gamma"}}}})
	})
	h.mux.HandleFunc("GET /v26.0/{node}", func(w http.ResponseWriter, r *http.Request) {
		tok, node := r.URL.Query().Get("access_token"), r.PathValue("node")
		known := map[string][2]string{"p1": {"pt-1", "Alpha Page"}, "p2": {"pt-2", "Beta Page"}, "p3": {"pt-3", "Gamma Page"},
			"ig1": {"pt-1", "alpha.ig"}, "ig3": {"pt-3", "gamma.ig"}}
		k, ok := known[node]
		if !ok || (k[0] != tok && !strings.HasPrefix(tok, "old-")) {
			fail(w, 400, "unknown node or token")
			return
		}
		writeJSON(w, map[string]any{"id": node, "name": k[1], "username": k[1]})
	})
}

func TestFacebookPagePicker(t *testing.T) {
	h := newHarness(t, metaEnv)
	h.fakeMeta()
	st := h.begin(Request{Network: "facebook"})
	code, page := h.run(browser(), st)
	_ = code
	q := h.lastAuth()
	if !strings.Contains(q.Get("scope"), "pages_manage_posts") || !strings.Contains(q.Get("scope"), "read_insights") || q.Get("auth_type") != "rerequest" {
		t.Fatalf("authorization request = %v", q)
	}
	// run() used a fresh browser; do it again keeping the browser to post the choice.
	c := browser()
	st = h.begin(Request{Network: "facebook"})
	resp, _ := c.Get(st.URL)
	read(t, resp)
	resp = h.postForm(c, "/connect/facebook/start", url.Values{"t": {startToken(t, st)}}, h.relay.URL)
	page = read(t, resp)
	for _, want := range []string{"Choose a Page", "Alpha Page (@alpha)", "Beta Page", "Gamma Page"} {
		if !strings.Contains(page, want) {
			t.Fatalf("picker lacks %q: %s", want, page)
		}
	}
	h.noSecrets(page)
	id := regexp.MustCompile(`name="pick" value="([^"]+)"`).FindStringSubmatch(page)
	if id == nil {
		t.Fatal("no pick id")
	}
	// Choices are sorted by name: 0 Alpha, 1 Beta, 2 Gamma.
	// Without the browser binding, or from another origin, the choice is refused.
	if r := h.postForm(browser(), "/connect/facebook/pick", url.Values{"pick": {id[1]}, "choice": {"1"}}, h.relay.URL); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("pick without binding: %d", r.StatusCode)
	}
	if r := h.postForm(c, "/connect/facebook/pick", url.Values{"pick": {id[1]}, "choice": {"1"}}, "https://evil.example"); r.StatusCode != http.StatusForbidden {
		t.Fatalf("pick from another origin: %d", r.StatusCode)
	}
	resp = h.postForm(c, "/connect/facebook/pick", url.Values{"pick": {id[1]}, "choice": {"1"}}, h.relay.URL)
	if page := read(t, resp); resp.StatusCode != 200 || !strings.Contains(page, "Facebook connected") {
		t.Fatalf("pick: %d %s", resp.StatusCode, page)
	}
	accs := h.accounts()
	if len(accs) != 1 || accs[0].Provider != "facebook" || accs[0].RemoteUserID != "p2" {
		t.Fatalf("accounts = %+v", accs)
	}
	if cr := h.creds(accs[0].ID); cr["page_id"] != "p2" || cr["page_access_token"] != "pt-2" {
		t.Fatalf("creds = %v", cr)
	}
	// The choice is single use.
	if r := h.postForm(c, "/connect/facebook/pick", url.Values{"pick": {id[1]}, "choice": {"0"}}, h.relay.URL); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("pick replay: %d", r.StatusCode)
	}
	if n := len(h.accounts()); n != 1 {
		t.Fatalf("%d accounts", n)
	}
}

func TestFacebookReconnectPicksTheSamePage(t *testing.T) {
	h := newHarness(t, metaEnv)
	h.fakeMeta()
	acc := h.existing("facebook", map[string]string{"page_id": "p3", "page_access_token": "old-A"})
	code, page := h.run(browser(), h.begin(Request{AccountID: acc.ID}))
	if code != 200 || !strings.Contains(page, "Facebook reconnected") {
		t.Fatalf("%d %s", code, page)
	}
	if c := h.creds(acc.ID); c["page_access_token"] != "pt-3" || c["page_id"] != "p3" {
		t.Fatalf("creds = %v", c)
	}
}

func TestFacebookReconnectPageNotGranted(t *testing.T) {
	h := newHarness(t, metaEnv)
	h.fakeMeta()
	h.mux.HandleFunc("GET /v26.0/p9", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"id": "p9", "name": "Lost Page"})
	})
	acc := h.existing("facebook", map[string]string{"page_id": "p9", "page_access_token": "old-A"})
	code, page := h.run(browser(), h.begin(Request{AccountID: acc.ID}))
	if code != http.StatusConflict || !strings.Contains(page, "not Lost Page") {
		t.Fatalf("%d %s", code, page)
	}
}

func TestInstagramViaFacebook(t *testing.T) {
	h := newHarness(t, metaEnv)
	h.fakeMeta()
	c := browser()
	st := h.begin(Request{Network: "instagram"})
	resp, _ := c.Get(st.URL)
	read(t, resp)
	resp = h.postForm(c, "/connect/instagram/start", url.Values{"t": {startToken(t, st)}}, h.relay.URL)
	page := read(t, resp)
	if !strings.Contains(page, "Choose an Instagram account") || !strings.Contains(page, "alpha.ig") || !strings.Contains(page, "Gamma (@gamma.ig)") || strings.Contains(page, "Beta") {
		t.Fatalf("picker: %s", page)
	}
	q := h.lastAuth()
	if !strings.Contains(q.Get("scope"), "instagram_content_publish") || !strings.Contains(q.Get("scope"), "business_management") {
		t.Fatalf("scope = %q", q.Get("scope"))
	}
	id := regexp.MustCompile(`name="pick" value="([^"]+)"`).FindStringSubmatch(page)
	resp = h.postForm(c, "/connect/instagram/pick", url.Values{"pick": {id[1]}, "choice": {"1"}}, h.relay.URL)
	if page := read(t, resp); resp.StatusCode != 200 {
		t.Fatalf("pick: %d %s", resp.StatusCode, page)
	}
	accs := h.accounts()
	if len(accs) != 1 || accs[0].Provider != "instagram" || accs[0].RemoteUserID != "ig3" {
		t.Fatalf("accounts = %+v", accs)
	}
	if cr := h.creds(accs[0].ID); cr["login"] != "facebook" || cr["user_id"] != "ig3" || cr["access_token"] != "pt-3" {
		t.Fatalf("creds = %v", cr)
	}
}

func TestInstagramReconnectRoute(t *testing.T) {
	env := map[string]string{"RELAY_META_APP_ID": "meta-id", "RELAY_META_APP_SECRET": "secret-value",
		"RELAY_INSTAGRAM_APP_ID": "ig-id", "RELAY_INSTAGRAM_APP_SECRET": "secret-value"}
	h := newHarness(t, env)
	h.fakeMeta()
	fb := h.existing("instagram", map[string]string{"login": "facebook", "user_id": "ig1", "access_token": "old-A"})
	if got := h.m.ReconnectNetwork(context.Background(), fb); got != "instagram" {
		t.Fatalf("Facebook Login account reconnects with %q", got)
	}
	h.mux.HandleFunc("GET /v26.0/me", func(w http.ResponseWriter, r *http.Request) { // Instagram Login identity
		writeJSON(w, map[string]any{"id": "app-scoped", "user_id": "ig7", "username": "seven"})
	})
	ig := h.existing("instagram", map[string]string{"access_token": "old-A", "token_refreshed_at": time.Now().Format(time.RFC3339)})
	if got := h.m.ReconnectNetwork(context.Background(), ig); got != "instagram-login" {
		t.Fatalf("Instagram Login account reconnects with %q", got)
	}
}

// ---------- Instagram Login ----------

func TestInstagramLogin(t *testing.T) {
	h := newHarness(t, map[string]string{"RELAY_INSTAGRAM_APP_ID": "ig-id", "RELAY_INSTAGRAM_APP_SECRET": "secret-value"})
	h.mux.HandleFunc("POST /token/instagram-login", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.PostForm.Get("client_id") != "ig-id" || r.PostForm.Get("grant_type") != "authorization_code" {
			fail(w, 400, "bad exchange")
			return
		}
		writeJSON(w, map[string]any{"data": []map[string]any{{"access_token": "short-A", "user_id": 777, "permissions": "x"}}})
	})
	h.mux.HandleFunc("GET /access_token", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("grant_type") != "ig_exchange_token" || r.URL.Query().Get("access_token") != "short-A" {
			fail(w, 400, "bad exchange")
			return
		}
		writeJSON(w, map[string]any{"access_token": "long-A", "expires_in": 5183944})
	})
	h.mux.HandleFunc("GET /v26.0/me", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("access_token") != "long-A" {
			fail(w, 401, "bad token")
			return
		}
		writeJSON(w, map[string]any{"id": "app-scoped", "user_id": "ig7", "username": "seven", "name": "Seven"})
	})
	code, page := h.run(browser(), h.begin(Request{Network: "instagram-login"}))
	if code != 200 || !strings.Contains(page, "Instagram connected") {
		t.Fatalf("%d %s", code, page)
	}
	q := h.lastAuth()
	if q.Get("scope") != "instagram_business_basic,instagram_business_content_publish,instagram_business_manage_comments,instagram_business_manage_insights" ||
		q.Get("force_reauth") != "true" {
		t.Fatalf("authorization request = %v", q)
	}
	accs := h.accounts()
	if len(accs) != 1 || accs[0].RemoteUserID != "ig7" {
		t.Fatalf("accounts = %+v", accs)
	}
	if c := h.creds(accs[0].ID); c["login"] != "instagram" || c["access_token"] != "long-A" || c["user_id"] != "ig7" {
		t.Fatalf("creds = %v", c)
	}
	h.noSecrets(page)
}

// ---------- LinkedIn ----------

func TestLinkedInOrganizationPicker(t *testing.T) {
	h := newHarness(t, map[string]string{"RELAY_LINKEDIN_CLIENT_ID": "li-id", "RELAY_LINKEDIN_CLIENT_SECRET": "secret-value"})
	h.mux.HandleFunc("POST /token/linkedin", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.PostForm.Get("client_secret") != "secret-value" || r.PostForm.Get("code") != "code-A" {
			fail(w, 400, "bad exchange")
			return
		}
		writeJSON(w, map[string]any{"access_token": "li-at-A", "expires_in": 5184000, "refresh_token": "li-rt-A", "refresh_token_expires_in": 31536000})
	})
	h.mux.HandleFunc("GET /rest/organizationAcls", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.Header.Get("Authorization") != "Bearer li-at-A" || r.Header.Get("LinkedIn-Version") == "" || r.Header.Get("X-Restli-Protocol-Version") != "2.0.0" ||
			q.Get("q") != "roleAssignee" || q.Get("role") != "ADMINISTRATOR" || q.Get("state") != "APPROVED" {
			fail(w, 400, "bad acl query")
			return
		}
		writeJSON(w, map[string]any{"elements": []map[string]any{
			{"organization": "urn:li:organization:101", "role": "ADMINISTRATOR"},
			{"organizationTarget": "urn:li:organization:202", "role": "ADMINISTRATOR"},
			{"organization": "urn:li:organization:101"},
		}})
	})
	h.mux.HandleFunc("GET /rest/organizations/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer li-at-A" {
			fail(w, 401, "bad token")
			return
		}
		names := map[string][2]string{"101": {"Acme", "acme"}, "202": {"Zeta Corp", "zeta"}}
		n := names[r.PathValue("id")]
		fmt.Fprintf(w, `{"id": %s, "localizedName": %q, "vanityName": %q}`, r.PathValue("id"), n[0], n[1])
	})
	c := browser()
	st := h.begin(Request{Network: "linkedin-page"}) // the connector name works too
	if st.Network != "linkedin" {
		t.Fatalf("network = %q", st.Network)
	}
	resp, _ := c.Get(st.URL)
	read(t, resp)
	resp = h.postForm(c, "/connect/linkedin/start", url.Values{"t": {startToken(t, st)}}, h.relay.URL)
	page := read(t, resp)
	if !strings.Contains(page, "Acme (@acme)") || !strings.Contains(page, "Zeta Corp (@zeta)") || strings.Count(page, `type="radio"`) != 2 {
		t.Fatalf("picker: %s", page)
	}
	if s := h.lastAuth().Get("scope"); s != "r_organization_social w_organization_social rw_organization_admin" {
		t.Fatalf("scope = %q", s)
	}
	id := regexp.MustCompile(`name="pick" value="([^"]+)"`).FindStringSubmatch(page)
	resp = h.postForm(c, "/connect/linkedin/pick", url.Values{"pick": {id[1]}, "choice": {"1"}}, h.relay.URL)
	if page := read(t, resp); resp.StatusCode != 200 || !strings.Contains(page, "LinkedIn connected") {
		t.Fatalf("pick: %d %s", resp.StatusCode, page)
	}
	accs := h.accounts()
	if len(accs) != 1 || accs[0].Provider != "linkedin-page" || accs[0].RemoteUserID != "202" {
		t.Fatalf("accounts = %+v", accs)
	}
	cr := h.creds(accs[0].ID)
	if cr["organization_id"] != "202" || cr["access_token"] != "li-at-A" || cr["refresh_token"] != "li-rt-A" || cr["client_id"] != "li-id" ||
		cr["client_secret"] != "secret-value" || cr["expires_at"] == "" || cr["refresh_token_expires_at"] == "" {
		t.Fatalf("creds = %v", cr)
	}
	h.noSecrets(page)
}

// ---------- YouTube (Google, PKCE) ----------

func (h *harness) fakeYouTube(refresh bool) {
	h.mux.HandleFunc("POST /token/youtube", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f := r.PostForm
		sum := sha256.Sum256([]byte(f.Get("code_verifier")))
		if f.Get("code_verifier") == "" || base64.RawURLEncoding.EncodeToString(sum[:]) != h.lastAuth().Get("code_challenge") {
			fail(w, 400, "PKCE verification failed")
			return
		}
		out := map[string]any{"access_token": "yt-at-A", "expires_in": 3599}
		if refresh {
			out["refresh_token"] = "yt-rt-A"
		}
		writeJSON(w, out)
	})
	h.mux.HandleFunc("GET /youtube/v3/channels", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer yt-at-A" {
			fail(w, 401, "bad token")
			return
		}
		writeJSON(w, map[string]any{"items": []map[string]any{{"id": "UC1", "snippet": map[string]any{"title": "Channel", "customUrl": "@chan"}}}})
	})
}

func TestYouTubePKCEAndOffline(t *testing.T) {
	h := newHarness(t, map[string]string{"RELAY_YOUTUBE_CLIENT_ID": "yt-id", "RELAY_YOUTUBE_CLIENT_SECRET": "secret-value"})
	h.fakeYouTube(true)
	code, page := h.run(browser(), h.begin(Request{Network: "youtube"}))
	if code != 200 || !strings.Contains(page, "YouTube connected") {
		t.Fatalf("%d %s", code, page)
	}
	q := h.lastAuth()
	if q.Get("code_challenge_method") != "S256" || len(q.Get("code_challenge")) != 43 || q.Get("access_type") != "offline" || q.Get("prompt") != "consent" {
		t.Fatalf("authorization request = %v", q)
	}
	if !strings.Contains(q.Get("scope"), "https://www.googleapis.com/auth/youtube.upload https://www.googleapis.com/auth/youtube.readonly") ||
		!strings.Contains(q.Get("scope"), "yt-analytics.readonly") {
		t.Fatalf("scope = %q", q.Get("scope"))
	}
	accs := h.accounts()
	if len(accs) != 1 || accs[0].RemoteUserID != "UC1" {
		t.Fatalf("accounts = %+v", accs)
	}
	if c := h.creds(accs[0].ID); c["refresh_token"] != "yt-rt-A" || c["client_id"] != "yt-id" || c["client_secret"] != "secret-value" {
		t.Fatalf("creds = %v", c)
	}
	h.noSecrets(page)
}

func TestYouTubeWithoutRefreshToken(t *testing.T) {
	h := newHarness(t, map[string]string{"RELAY_YOUTUBE_CLIENT_ID": "yt-id", "RELAY_YOUTUBE_CLIENT_SECRET": "secret-value"})
	h.fakeYouTube(false)
	code, page := h.run(browser(), h.begin(Request{Network: "youtube"}))
	if code != http.StatusBadGateway || !strings.Contains(page, "no refresh token") {
		t.Fatalf("%d %s", code, page)
	}
	if n := len(h.accounts()); n != 0 {
		t.Fatalf("%d accounts", n)
	}
}

// ---------- TikTok ----------

func TestTikTok(t *testing.T) {
	h := newHarness(t, map[string]string{"RELAY_TIKTOK_CLIENT_KEY": "tk-key", "RELAY_TIKTOK_CLIENT_SECRET": "secret-value"})
	h.mux.HandleFunc("POST /token/tiktok", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f := r.PostForm
		if f.Get("client_key") != "tk-key" || f.Get("client_secret") != "secret-value" || f.Get("grant_type") != "authorization_code" || f.Get("code") != "code-A" {
			fail(w, 400, "bad exchange")
			return
		}
		writeJSON(w, map[string]any{"access_token": "tt-at-A", "expires_in": 86400, "open_id": "oid-1", "refresh_token": "tt-rt-A",
			"refresh_expires_in": 15552000, "scope": "user.info.basic,video.publish", "token_type": "Bearer"})
	})
	h.mux.HandleFunc("GET /v2/user/info/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tt-at-A" {
			fail(w, 401, "bad token")
			return
		}
		writeJSON(w, map[string]any{"data": map[string]any{"user": map[string]any{"open_id": "oid-1", "display_name": "Tik", "username": "tik"}},
			"error": map[string]any{"code": "ok"}})
	})
	code, page := h.run(browser(), h.begin(Request{Network: "tiktok"}))
	if code != 200 || !strings.Contains(page, "TikTok connected") {
		t.Fatalf("%d %s", code, page)
	}
	q := h.lastAuth()
	if q.Get("client_key") != "tk-key" || q.Get("client_id") != "" || q.Get("scope") != "user.info.basic,user.info.profile,user.info.stats,video.publish,video.upload,video.list" {
		t.Fatalf("authorization request = %v", q)
	}
	accs := h.accounts()
	if len(accs) != 1 || accs[0].RemoteUserID != "oid-1" {
		t.Fatalf("accounts = %+v", accs)
	}
	if c := h.creds(accs[0].ID); c["client_key"] != "tk-key" || c["refresh_token"] != "tt-rt-A" || c["expires_at"] == "" {
		t.Fatalf("creds = %v", c)
	}
	h.noSecrets(page)
}

// ---------- X (OAuth 1.0a, three-legged) ----------

func pct(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || strings.IndexByte("-._~", c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// checkOAuth1 verifies an OAuth 1.0a HMAC-SHA1 signature (RFC 5849 §3.4), independently of the x package.
func checkOAuth1(r *http.Request, consumerSecret, tokenSecret string) (map[string]string, error) {
	h := strings.TrimPrefix(r.Header.Get("Authorization"), "OAuth ")
	params := map[string]string{}
	for _, part := range strings.Split(h, ", ") {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			return nil, fmt.Errorf("bad header %q", h)
		}
		uv, err := url.PathUnescape(strings.Trim(v, `"`))
		if err != nil {
			return nil, err
		}
		params[k] = uv
	}
	sig := params["oauth_signature"]
	var pairs []string
	for k, v := range params {
		if k != "oauth_signature" {
			pairs = append(pairs, pct(k)+"="+pct(v))
		}
	}
	for k, vs := range r.URL.Query() {
		for _, v := range vs {
			pairs = append(pairs, pct(k)+"="+pct(v))
		}
	}
	sort.Strings(pairs)
	base := r.Method + "&" + pct("http://"+r.Host+r.URL.Path) + "&" + pct(strings.Join(pairs, "&"))
	mac := hmac.New(sha1.New, []byte(pct(consumerSecret)+"&"+pct(tokenSecret)))
	mac.Write([]byte(base))
	if want := base64.StdEncoding.EncodeToString(mac.Sum(nil)); sig != want {
		return nil, fmt.Errorf("bad signature")
	}
	return params, nil
}

func (h *harness) fakeX() {
	h.mux.HandleFunc("POST /oauth/request_token", func(w http.ResponseWriter, r *http.Request) {
		p, err := checkOAuth1(r, "secret-value", "")
		if err != nil || p["oauth_consumer_key"] != "x-key" || p["oauth_callback"] != h.relay.URL+"/connect/x/callback" || p["oauth_token"] != "" {
			http.Error(w, fmt.Sprint("request token refused: ", err, p), 401)
			return
		}
		io.WriteString(w, "oauth_token=req-token&oauth_token_secret=req-secret&oauth_callback_confirmed=true")
	})
	h.mux.HandleFunc("POST /token/x", func(w http.ResponseWriter, r *http.Request) {
		p, err := checkOAuth1(r, "secret-value", "req-secret")
		if err != nil || p["oauth_token"] != "req-token" || !strings.HasPrefix(p["oauth_verifier"], "verifier-") {
			http.Error(w, fmt.Sprint("access token refused: ", err), 401)
			return
		}
		in := who(p["oauth_verifier"])
		u := users[in]
		fmt.Fprintf(w, "oauth_token=xat-%s&oauth_token_secret=xats-%s&user_id=%s&screen_name=%s", in, in, u.id, u.handle)
	})
	h.mux.HandleFunc("GET /2/users/me", func(w http.ResponseWriter, r *http.Request) {
		in := ""
		for _, s := range []string{"A", "B"} {
			if strings.Contains(r.Header.Get("Authorization"), `oauth_token="xat-`+s+`"`) {
				in = s
			}
		}
		if in == "" {
			http.Error(w, "unknown token", 401)
			return
		}
		if _, err := checkOAuth1(r, "secret-value", "xats-"+in); err != nil {
			http.Error(w, "bad signature", 401)
			return
		}
		u := users[in]
		writeJSON(w, map[string]any{"data": map[string]any{"id": u.id, "name": u.name, "username": u.handle}})
	})
}

var xEnv = map[string]string{"RELAY_X_CONSUMER_KEY": "x-key", "RELAY_X_CONSUMER_SECRET": "secret-value"}

func TestXThreeLegged(t *testing.T) {
	h := newHarness(t, xEnv)
	h.fakeX()
	code, page := h.run(browser(), h.begin(Request{Network: "x"}))
	if code != 200 || !strings.Contains(page, "X connected") || !strings.Contains(page, "@alice") {
		t.Fatalf("%d %s", code, page)
	}
	if q := h.lastAuth(); q.Get("oauth_token") != "req-token" || q.Get("force_login") != "" {
		t.Fatalf("authorize = %v", q)
	}
	accs := h.accounts()
	if len(accs) != 1 || accs[0].RemoteUserID != "111" {
		t.Fatalf("accounts = %+v", accs)
	}
	c := h.creds(accs[0].ID)
	if c["access_token"] != "xat-A" || c["access_token_secret"] != "xats-A" || c["consumer_key"] != "x-key" || c["consumer_secret"] != "secret-value" {
		t.Fatalf("creds = %v", c)
	}
	h.noSecrets(page)
}

func TestXReconnect(t *testing.T) {
	h := newHarness(t, xEnv)
	h.fakeX()
	acc := h.existing("x", map[string]string{"consumer_key": "x-key", "consumer_secret": "secret-value", "access_token": "xat-A", "access_token_secret": "xats-A"})
	h.setSignIn("B")
	code, page := h.run(browser(), h.begin(Request{AccountID: acc.ID}))
	if code != http.StatusConflict {
		t.Fatalf("other account: %d %s", code, page)
	}
	if q := h.lastAuth(); q.Get("force_login") != "true" {
		t.Fatalf("authorize = %v", q)
	}
	h.setSignIn("A")
	if code, page := h.run(browser(), h.begin(Request{AccountID: acc.ID})); code != 200 || !strings.Contains(page, "X reconnected") {
		t.Fatalf("%d %s", code, page)
	}
}

func TestXDeniedConsent(t *testing.T) {
	h := newHarness(t, xEnv)
	h.fakeX()
	st := h.begin(Request{Network: "x"})
	c := stopAt("/callback")
	resp := h.postForm(c, "/connect/x/start", url.Values{"t": {startToken(t, st)}}, h.relay.URL)
	read(t, resp)
	back, _ := c.Get(h.relay.URL + "/connect/x/callback?denied=req-token")
	if page := read(t, back); !strings.Contains(page, "cancelled") {
		t.Fatalf("%d %s", back.StatusCode, page)
	}
}
