package api_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Seeyko/relay/internal/api"
	"github.com/Seeyko/relay/internal/core"
	"github.com/Seeyko/relay/internal/mcp"
	"github.com/Seeyko/relay/internal/oauth"
	"github.com/Seeyko/relay/internal/secret"
	"github.com/Seeyko/relay/internal/store"
)

// stubKeys stands in for the passkey dashboard: a session is the cookie test_session=<name> for a name in
// sessions, and a "fresh assertion" is {"challenge": <the one handed out>, "ok": true}.
type stubKeys struct {
	mu       sync.Mutex
	sessions map[string]bool
	has      bool
	n        int
}

func (k *stubKeys) SessionID(r *http.Request) (string, bool) {
	c, err := r.Cookie("test_session")
	if err != nil {
		return "", false
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	return "sid-" + c.Value, k.sessions[c.Value]
}

func (k *stubKeys) HasPasskeys(context.Context) bool { return k.has }

func (k *stubKeys) BeginAssertion() (any, any, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.n++
	ch := fmt.Sprintf("challenge-%d", k.n)
	return map[string]any{"publicKey": map[string]any{"challenge": ch, "userVerification": "required"}}, ch, nil
}

func (k *stubKeys) FinishAssertion(_ context.Context, state any, cred []byte) error {
	var c struct {
		Challenge string `json:"challenge"`
		OK        bool   `json:"ok"`
	}
	if err := json.Unmarshal(cred, &c); err != nil {
		return err
	}
	if !c.OK || state != c.Challenge {
		return errors.New("assertion rejected")
	}
	return nil
}

type oenv struct {
	t     *testing.T
	srv   *httptest.Server
	base  string
	st    *store.Store
	as    *oauth.Server
	keys  *stubKeys
	now   time.Time
	ip    int
	mu    sync.Mutex
	plain *http.Client
}

func newOAuthEnv(t *testing.T, withOAuth bool) *oenv {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir + "/relay.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	box, _ := secret.New("k")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewUnstartedServer(nil)
	srv.Start()
	t.Cleanup(srv.Close)
	svc := &core.Service{Store: st, Box: box, MediaDir: dir + "/media", HTTP: http.DefaultClient, Log: log, BaseURL: srv.URL}
	m := mcp.NewServer("relay", "test", mcp.BuildTools(svc))
	e := &oenv{t: t, srv: srv, base: srv.URL, st: st, keys: &stubKeys{sessions: map[string]bool{"owner": true, "other": true}, has: true}, now: time.Now()}
	a := &api.API{Svc: svc, MCP: m, Token: token, DraftToken: "draft-token-0123456789", Log: log}
	if withOAuth {
		as, err := oauth.New(srv.URL, st, e.keys, log)
		if err != nil {
			t.Fatal(err)
		}
		as.Now = func() time.Time { e.mu.Lock(); defer e.mu.Unlock(); return e.now }
		e.as, a.OAuth = as, as
	}
	srv.Config.Handler = a.Handler()
	e.plain = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return e
}

func (e *oenv) advance(d time.Duration) { e.mu.Lock(); e.now = e.now.Add(d); e.mu.Unlock() }

type resp struct {
	code int
	hdr  http.Header
	body []byte
	json map[string]any
}

func (e *oenv) do(req *http.Request) resp {
	e.t.Helper()
	if req.Header.Get("X-Forwarded-For") == "" { // one address per request: rate limits are tested on their own
		e.mu.Lock()
		e.ip++
		req.Header.Set("X-Forwarded-For", fmt.Sprintf("192.0.%d.%d", e.ip/250, e.ip%250))
		e.mu.Unlock()
	}
	res, err := e.plain.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	r := resp{code: res.StatusCode, hdr: res.Header, body: b, json: map[string]any{}}
	_ = json.Unmarshal(b, &r.json)
	return r
}

func (e *oenv) get(path string) resp {
	req, _ := http.NewRequest("GET", e.base+path, nil)
	return e.do(req)
}

func (e *oenv) postJSON(path string, body any, hdr map[string]string) resp {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", e.base+path, strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", e.base)
	for k, v := range hdr {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	return e.do(req)
}

func (e *oenv) postForm(path string, f url.Values, hdr map[string]string) resp {
	req, _ := http.NewRequest("POST", e.base+path, strings.NewReader(f.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	return e.do(req)
}

// mcpCall posts one JSON-RPC request to /mcp with a bearer token.
func (e *oenv) mcpCall(tok, method string, params any) resp {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	req, _ := http.NewRequest("POST", e.base+"/mcp", strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	return e.do(req)
}

func (e *oenv) rest(tok, method, path string) resp {
	req, _ := http.NewRequest(method, e.base+path, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	return e.do(req)
}

func toolNames(t *testing.T, r resp) []string {
	t.Helper()
	res, _ := r.json["result"].(map[string]any)
	tools, _ := res["tools"].([]any)
	var out []string
	for _, x := range tools {
		out = append(out, x.(map[string]any)["name"].(string))
	}
	return out
}

func has(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func pkce() (verifier, challenge string) {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	verifier = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

const appRedirect = "https://app.example/callback"

func (e *oenv) register(name string, redirects ...string) string {
	e.t.Helper()
	r := e.postJSON("/oauth/register", map[string]any{"client_name": name, "redirect_uris": redirects, "token_endpoint_auth_method": "none"}, nil)
	if r.code != 201 {
		e.t.Fatalf("register: %d %s", r.code, r.body)
	}
	if _, ok := r.json["client_secret"]; ok {
		e.t.Fatal("no client secret must be issued")
	}
	return r.json["client_id"].(string)
}

func authorizeQuery(clientID, redirect, challenge string, extra map[string]string) string {
	q := url.Values{"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {redirect},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"}, "state": {"st-42"}}
	for k, v := range extra {
		if v == "" {
			q.Del(k)
		} else {
			q.Set(k, v)
		}
	}
	return "/oauth/authorize?" + q.Encode()
}

var reqRe = regexp.MustCompile(`data-req="([^"]+)"`)

// openConsent loads the consent page and returns the pending request id.
func (e *oenv) openConsent(path string) string {
	e.t.Helper()
	r := e.get(path)
	if r.code != 200 {
		e.t.Fatalf("authorize: %d %s %v", r.code, r.body, r.hdr.Get("Location"))
	}
	m := reqRe.FindSubmatch(r.body)
	if m == nil {
		e.t.Fatalf("no request id in page: %s", r.body)
	}
	return string(m[1])
}

func session(name string) map[string]string {
	return map[string]string{"Cookie": "test_session=" + name}
}

// approve runs the whole consent page as the owner and returns the redirect URL with the code.
func (e *oenv) approve(req, scope string) *url.URL {
	e.t.Helper()
	st := e.postJSON("/oauth/api/state", map[string]any{"req": req}, session("owner"))
	csrf, _ := st.json["csrf"].(string)
	if st.code != 200 || csrf == "" {
		e.t.Fatalf("state: %d %s", st.code, st.body)
	}
	b := e.postJSON("/oauth/api/approve/begin", map[string]any{"req": req, "csrf": csrf}, session("owner"))
	if b.code != 200 {
		e.t.Fatalf("approve/begin: %d %s", b.code, b.body)
	}
	ch := b.json["publicKey"].(map[string]any)["challenge"].(string)
	f := e.postJSON("/oauth/api/approve/finish", map[string]any{"req": req, "csrf": csrf, "scope": scope,
		"credential": map[string]any{"challenge": ch, "ok": true}}, session("owner"))
	if f.code != 200 {
		e.t.Fatalf("approve/finish: %d %s", f.code, f.body)
	}
	u, err := url.Parse(f.json["redirect"].(string))
	if err != nil {
		e.t.Fatal(err)
	}
	return u
}

type conn struct {
	clientID, verifier, access, refresh string
}

// connect registers an app, approves it and exchanges the code.
func (e *oenv) connect(scope string) conn {
	e.t.Helper()
	id := e.register("Test app", appRedirect)
	v, ch := pkce()
	u := e.approve(e.openConsent(authorizeQuery(id, appRedirect, ch, nil)), scope)
	r := e.postForm("/oauth/token", url.Values{"grant_type": {"authorization_code"}, "code": {u.Query().Get("code")},
		"redirect_uri": {appRedirect}, "client_id": {id}, "code_verifier": {v}}, nil)
	if r.code != 200 {
		e.t.Fatalf("token: %d %s", r.code, r.body)
	}
	return conn{clientID: id, verifier: v, access: r.json["access_token"].(string), refresh: r.json["refresh_token"].(string)}
}

func (e *oenv) refresh(c conn, rt string) resp {
	return e.postForm("/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {rt}, "client_id": {c.clientID}}, nil)
}

func wantOAuthErr(t *testing.T, r resp, code int, oerr string) {
	t.Helper()
	if r.code != code || r.json["error"] != oerr {
		t.Fatalf("want %d %s, got %d %s", code, oerr, r.code, r.body)
	}
}

func wantInvalidToken(t *testing.T, e *oenv, tok string) {
	t.Helper()
	r := e.mcpCall(tok, "tools/list", nil)
	if r.code != 401 || !strings.Contains(r.hdr.Get("WWW-Authenticate"), `error="invalid_token"`) {
		t.Fatalf("want 401 invalid_token, got %d %q", r.code, r.hdr.Get("WWW-Authenticate"))
	}
}

// ---------- the whole flow ----------

func TestOAuthEndToEnd(t *testing.T) {
	e := newOAuthEnv(t, true)
	prm := e.base + "/.well-known/oauth-protected-resource"

	// 1. An agent without a token is pointed to the protected resource metadata.
	r := e.mcpCall("", "tools/list", nil)
	if r.code != 401 || r.hdr.Get("WWW-Authenticate") != `Bearer resource_metadata="`+prm+`"` {
		t.Fatalf("unauthenticated /mcp: %d %q", r.code, r.hdr.Get("WWW-Authenticate"))
	}
	wantInvalidToken(t, e, "relay_at_forged")

	// 2. Metadata.
	for _, p := range []string{"/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp"} {
		r = e.get(p)
		if r.code != 200 || r.json["resource"] != e.base+"/mcp" || fmt.Sprint(r.json["authorization_servers"]) != "["+e.base+"]" ||
			fmt.Sprint(r.json["bearer_methods_supported"]) != "[header]" || fmt.Sprint(r.json["scopes_supported"]) != "[relay:admin relay:drafts]" {
			t.Fatalf("%s: %d %s", p, r.code, r.body)
		}
	}
	for _, p := range []string{"/.well-known/oauth-authorization-server", "/.well-known/openid-configuration"} {
		r = e.get(p)
		j := r.json
		if r.code != 200 || j["issuer"] != e.base || j["authorization_endpoint"] != e.base+"/oauth/authorize" ||
			j["token_endpoint"] != e.base+"/oauth/token" || j["registration_endpoint"] != e.base+"/oauth/register" ||
			j["revocation_endpoint"] != e.base+"/oauth/revoke" || fmt.Sprint(j["code_challenge_methods_supported"]) != "[S256]" ||
			fmt.Sprint(j["token_endpoint_auth_methods_supported"]) != "[none]" || fmt.Sprint(j["response_types_supported"]) != "[code]" ||
			fmt.Sprint(j["grant_types_supported"]) != "[authorization_code refresh_token]" {
			t.Fatalf("%s: %d %s", p, r.code, r.body)
		}
	}

	// 3. Registration. The name is shown escaped on the consent page.
	id := e.register(`<img src=x onerror=alert(1)> Claude`, appRedirect)
	v, ch := pkce()
	page := e.get(authorizeQuery(id, appRedirect, ch, map[string]string{"resource": e.base + "/mcp", "scope": "relay:admin"}))
	if page.code != 200 || strings.Contains(string(page.body), "<img") || !strings.Contains(string(page.body), "&lt;img") ||
		!strings.Contains(string(page.body), "app.example") {
		t.Fatalf("consent page: %d %s", page.code, page.body)
	}
	if csp := page.hdr.Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") || !strings.Contains(csp, "script-src 'self'") {
		t.Fatalf("consent page CSP: %q", csp)
	}
	if page.hdr.Get("X-Frame-Options") != "DENY" || page.hdr.Get("Cache-Control") != "no-store" {
		t.Fatalf("consent page headers: %v", page.hdr)
	}
	req := reqRe.FindStringSubmatch(string(page.body))[1]

	// 4. Without a dashboard session the page offers the passkey sign-in, and nothing else works.
	st := e.postJSON("/oauth/api/state", map[string]any{"req": req}, nil)
	if st.code != 200 || st.json["logged_in"] != false || st.json["has_passkeys"] != true || st.json["csrf"] != nil {
		t.Fatalf("state without session: %d %s", st.code, st.body)
	}
	if r := e.postJSON("/oauth/api/approve/begin", map[string]any{"req": req, "csrf": "x"}, nil); r.code != 401 {
		t.Fatalf("approve without session: want 401, got %d", r.code)
	}
	st = e.postJSON("/oauth/api/state", map[string]any{"req": req}, session("owner"))
	csrf := st.json["csrf"].(string)
	if st.json["logged_in"] != true || fmt.Sprint(st.json["scopes"]) != "[relay:admin relay:drafts]" {
		t.Fatalf("state with session: %s", st.body)
	}

	// 5. CSRF: missing, forged, or taken from another session.
	other := e.postJSON("/oauth/api/state", map[string]any{"req": req}, session("other")).json["csrf"].(string)
	for name, tok := range map[string]string{"missing": "", "forged": "AAAA" + csrf[4:], "other session": other} {
		if r := e.postJSON("/oauth/api/approve/begin", map[string]any{"req": req, "csrf": tok}, session("owner")); r.code != 403 {
			t.Fatalf("CSRF %s: want 403, got %d", name, r.code)
		}
	}
	// Cross-origin calls are refused before anything else.
	if r := e.postJSON("/oauth/api/approve/begin", map[string]any{"req": req, "csrf": csrf}, map[string]string{"Origin": "https://evil.example", "Cookie": "test_session=owner"}); r.code != 403 {
		t.Fatalf("cross-origin approve: want 403, got %d", r.code)
	}

	// 6. Approval needs a fresh passkey assertion: a failed one gives no code and burns the challenge.
	b := e.postJSON("/oauth/api/approve/begin", map[string]any{"req": req, "csrf": csrf}, session("owner"))
	chal := b.json["publicKey"].(map[string]any)["challenge"].(string)
	f := e.postJSON("/oauth/api/approve/finish", map[string]any{"req": req, "csrf": csrf, "scope": "relay:admin", "credential": map[string]any{"challenge": chal, "ok": false}}, session("owner"))
	if f.code != 401 || f.json["redirect"] != nil {
		t.Fatalf("failed assertion: want 401 and no redirect, got %d %s", f.code, f.body)
	}
	f = e.postJSON("/oauth/api/approve/finish", map[string]any{"req": req, "csrf": csrf, "scope": "relay:admin", "credential": map[string]any{"challenge": chal, "ok": true}}, session("owner"))
	if f.code != 400 {
		t.Fatalf("finish without a new challenge: want 400, got %d %s", f.code, f.body)
	}
	// A challenge started by another session cannot be finished by this one.
	b = e.postJSON("/oauth/api/approve/begin", map[string]any{"req": req, "csrf": other}, session("other"))
	chal = b.json["publicKey"].(map[string]any)["challenge"].(string)
	f = e.postJSON("/oauth/api/approve/finish", map[string]any{"req": req, "csrf": csrf, "scope": "relay:admin", "credential": map[string]any{"challenge": chal, "ok": true}}, session("owner"))
	if f.code != 400 {
		t.Fatalf("challenge of another session: want 400, got %d %s", f.code, f.body)
	}

	u := e.approve(req, "relay:admin")
	code := u.Query().Get("code")
	if u.Host != "app.example" || u.Path != "/callback" || code == "" || u.Query().Get("state") != "st-42" || u.Query().Get("iss") != e.base {
		t.Fatalf("redirect: %s", u)
	}
	// The request is consumed: it cannot be approved again.
	if r := e.postJSON("/oauth/api/state", map[string]any{"req": req}, session("owner")); r.code != 404 {
		t.Fatalf("consumed request: want 404, got %d", r.code)
	}

	// 7. Code → tokens.
	tr := e.postForm("/oauth/token", url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {appRedirect},
		"client_id": {id}, "code_verifier": {v}, "resource": {e.base + "/mcp"}}, nil)
	if tr.code != 200 || tr.json["token_type"] != "Bearer" || tr.json["expires_in"] != float64(3600) || tr.json["scope"] != "relay:admin" ||
		tr.hdr.Get("Cache-Control") != "no-store" {
		t.Fatalf("token: %d %s %v", tr.code, tr.body, tr.hdr)
	}
	access, refresh := tr.json["access_token"].(string), tr.json["refresh_token"].(string)
	c := conn{clientID: id, verifier: v, access: access, refresh: refresh}

	// 8. The access token opens /mcp (with admin tools) and /v1.
	names := toolNames(t, e.mcpCall(access, "tools/list", nil))
	if !has(names, "publish_now") || !has(names, "list_oauth_grants") || !has(names, "revoke_oauth_grant") {
		t.Fatalf("admin tools missing: %v", names)
	}
	if r := e.rest(access, "GET", "/v1/accounts"); r.code != 200 {
		t.Fatalf("/v1 with an OAuth token: %d %s", r.code, r.body)
	}
	// Only hashes are stored.
	if _, err := e.st.Query(t.Context(), "SELECT token_hash FROM oauth_tokens", 10); err == nil {
		t.Fatal("sql_query must not read the OAuth tables")
	}

	// 9. Refresh rotation: each refresh gives a new pair, the old refresh token dies.
	r1 := e.refresh(c, refresh)
	if r1.code != 200 || r1.json["refresh_token"] == refresh {
		t.Fatalf("refresh: %d %s", r1.code, r1.body)
	}
	r2 := e.refresh(c, r1.json["refresh_token"].(string))
	if r2.code != 200 {
		t.Fatalf("second refresh: %d %s", r2.code, r2.body)
	}
	latestAccess, latestRefresh := r2.json["access_token"].(string), r2.json["refresh_token"].(string)
	if names := toolNames(t, e.mcpCall(latestAccess, "tools/list", nil)); !has(names, "publish_now") {
		t.Fatalf("refreshed token: %v", names)
	}

	// 10. Reuse of a rotated refresh token revokes the whole grant.
	wantOAuthErr(t, e.refresh(c, refresh), 400, "invalid_grant")
	wantInvalidToken(t, e, latestAccess)
	wantInvalidToken(t, e, access)
	wantOAuthErr(t, e.refresh(c, latestRefresh), 400, "invalid_grant")

	// 11. Revocation (RFC 7009) disconnects the app.
	c2 := e.connect("relay:admin")
	if r := e.postForm("/oauth/revoke", url.Values{"token": {c2.refresh}, "client_id": {"relay_c_someone_else"}}, nil); r.code != 401 {
		t.Fatalf("revoke by an unknown client: want 401, got %d", r.code)
	}
	if r := e.postForm("/oauth/revoke", url.Values{"token": {c2.refresh}, "client_id": {c2.clientID}}, nil); r.code != 200 {
		t.Fatalf("revoke: %d %s", r.code, r.body)
	}
	wantInvalidToken(t, e, c2.access)
	if r := e.postForm("/oauth/revoke", url.Values{"token": {"unknown"}, "client_id": {c2.clientID}}, nil); r.code != 200 {
		t.Fatalf("revoking an unknown token is not an error: %d", r.code)
	}

	// 12. Grants management through the admin tools: listed without tokens, revoked at once.
	c3 := e.connect("relay:drafts")
	lr := e.mcpCall(token, "tools/call", map[string]any{"name": "list_oauth_grants", "arguments": map[string]any{}})
	text := lr.json["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	var grants []map[string]any
	if err := json.Unmarshal([]byte(text), &grants); err != nil || len(grants) != 1 || grants[0]["scope"] != "relay:drafts" || grants[0]["client_name"] != "Test app" {
		t.Fatalf("list_oauth_grants: %s", text)
	}
	if strings.Contains(text, c3.access) || strings.Contains(text, "hash") {
		t.Fatalf("grants must never show tokens: %s", text)
	}
	if r := e.rest(token, "DELETE", "/v1/oauth/grants/"+grants[0]["id"].(string)); r.code != 200 {
		t.Fatalf("revoke_oauth_grant: %d %s", r.code, r.body)
	}
	wantInvalidToken(t, e, c3.access)

	// 13. Access tokens expire after an hour.
	c4 := e.connect("relay:admin")
	e.advance(61 * time.Minute)
	wantInvalidToken(t, e, c4.access)
	if r := e.refresh(c4, c4.refresh); r.code != 200 {
		t.Fatalf("refresh after the access token expired: %d %s", r.code, r.body)
	}
}

func TestOAuthCodeReuseRevokes(t *testing.T) {
	e := newOAuthEnv(t, true)
	id := e.register("App", appRedirect)
	v, ch := pkce()
	code := e.approve(e.openConsent(authorizeQuery(id, appRedirect, ch, nil)), "relay:admin").Query().Get("code")
	f := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {appRedirect}, "client_id": {id}, "code_verifier": {v}}
	r := e.postForm("/oauth/token", f, nil)
	if r.code != 200 {
		t.Fatalf("token: %d %s", r.code, r.body)
	}
	access := r.json["access_token"].(string)
	wantOAuthErr(t, e.postForm("/oauth/token", f, nil), 400, "invalid_grant")
	wantInvalidToken(t, e, access)
	wantOAuthErr(t, e.refresh(conn{clientID: id}, r.json["refresh_token"].(string)), 400, "invalid_grant")
}

func TestOAuthTokenErrors(t *testing.T) {
	e := newOAuthEnv(t, true)
	id := e.register("App", appRedirect)
	otherID := e.register("Other", appRedirect)
	newCode := func() (string, string) {
		v, ch := pkce()
		return e.approve(e.openConsent(authorizeQuery(id, appRedirect, ch, nil)), "relay:admin").Query().Get("code"), v
	}
	exchange := func(code, v string, mod map[string]string) resp {
		f := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {appRedirect}, "client_id": {id}, "code_verifier": {v}}
		for k, x := range mod {
			if x == "" {
				f.Del(k)
			} else {
				f.Set(k, x)
			}
		}
		return e.postForm("/oauth/token", f, nil)
	}

	// Wrong verifier: refused, and the code is burned.
	code, v := newCode()
	wrong, _ := pkce()
	wantOAuthErr(t, exchange(code, v, map[string]string{"code_verifier": wrong}), 400, "invalid_grant")
	wantOAuthErr(t, exchange(code, v, nil), 400, "invalid_grant")

	// A verifier equal to the challenge ("plain" PKCE) is refused.
	vv, ch := pkce()
	code = e.approve(e.openConsent(authorizeQuery(id, appRedirect, ch, nil)), "relay:admin").Query().Get("code")
	wantOAuthErr(t, exchange(code, vv, map[string]string{"code_verifier": ch}), 400, "invalid_grant")

	code, v = newCode()
	wantOAuthErr(t, exchange(code, v, map[string]string{"code_verifier": ""}), 400, "invalid_request")

	code, v = newCode()
	wantOAuthErr(t, exchange(code, v, map[string]string{"redirect_uri": "https://app.example/other"}), 400, "invalid_grant")

	code, v = newCode()
	wantOAuthErr(t, exchange(code, v, map[string]string{"client_id": otherID}), 400, "invalid_grant")

	code, v = newCode()
	wantOAuthErr(t, exchange(code, v, map[string]string{"resource": "https://other.example/mcp"}), 400, "invalid_target")

	code, v = newCode()
	e.advance(61 * time.Second)
	wantOAuthErr(t, exchange(code, v, nil), 400, "invalid_grant")

	wantOAuthErr(t, exchange("x", "y", map[string]string{"client_id": "relay_c_unknown"}), 401, "invalid_client")
	wantOAuthErr(t, exchange("x", "y", map[string]string{"grant_type": "password"}), 400, "unsupported_grant_type")
	if r := e.postJSON("/oauth/token", map[string]any{"grant_type": "authorization_code"}, nil); r.json["error"] != "invalid_request" {
		t.Fatalf("JSON body: %d %s", r.code, r.body)
	}

	// client_secret_basic with a made-up secret: the client is identified, the secret ignored, PKCE decides.
	code, v = newCode()
	r := exchange(code, v, map[string]string{"client_id": ""})
	if r.code != 401 {
		t.Fatalf("no client at all: want 401, got %d", r.code)
	}
	code, v = newCode()
	f := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {appRedirect}, "code_verifier": {v}}
	req, _ := http.NewRequest("POST", e.base+"/oauth/token", strings.NewReader(f.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(id, "whatever")
	if r := e.do(req); r.code != 200 {
		t.Fatalf("client_secret_basic: %d %s", r.code, r.body)
	}

	// Refresh: unknown token, other client.
	c := e.connect("relay:admin")
	wantOAuthErr(t, e.refresh(c, "relay_rt_unknown"), 400, "invalid_grant")
	wantOAuthErr(t, e.refresh(conn{clientID: otherID}, c.refresh), 400, "invalid_grant")
	// ...a refused refresh does not burn the token.
	if r := e.refresh(c, c.refresh); r.code != 200 {
		t.Fatalf("refresh after a refused attempt: %d %s", r.code, r.body)
	}
}

func TestOAuthAuthorizeErrors(t *testing.T) {
	e := newOAuthEnv(t, true)
	id := e.register("App", appRedirect, "http://127.0.0.1:1234/cb")
	_, ch := pkce()

	// Errors before the redirect URI is trusted: a page, never a redirect.
	page := map[string]string{
		"unknown client":       authorizeQuery("relay_c_nope", appRedirect, ch, nil),
		"no client":            authorizeQuery("", appRedirect, ch, map[string]string{"client_id": ""}),
		"no redirect":          authorizeQuery(id, "", ch, map[string]string{"redirect_uri": ""}),
		"other host":           authorizeQuery(id, "https://evil.example/callback", ch, nil),
		"other path":           authorizeQuery(id, "https://app.example/callback/../evil", ch, nil),
		"extra query":          authorizeQuery(id, appRedirect+"?next=https://evil.example", ch, nil),
		"user info":            authorizeQuery(id, "https://app.example@evil.example/callback", ch, nil),
		"scheme relative":      authorizeQuery(id, "//evil.example/callback", ch, nil),
		"fragment":             authorizeQuery(id, appRedirect+"#x", ch, nil),
		"http downgrade":       authorizeQuery(id, "http://app.example/callback", ch, nil),
		"loopback other host":  authorizeQuery(id, "http://localhost:5555/cb", ch, nil),
		"loopback other path":  authorizeQuery(id, "http://127.0.0.1:5555/evil", ch, nil),
		"loopback as https":    authorizeQuery(id, "https://127.0.0.1:1234/cb", ch, nil),
		"repeated redirect":    authorizeQuery(id, appRedirect, ch, nil) + "&redirect_uri=" + url.QueryEscape("https://evil.example/"),
		"trailing dot host":    authorizeQuery(id, "https://app.example./callback", ch, nil),
		"case-changed path":    authorizeQuery(id, "https://app.example/Callback", ch, nil),
		"javascript redirect":  authorizeQuery(id, "javascript:alert(1)", ch, nil),
		"backslash in path":    authorizeQuery(id, `https://app.example\@evil.example/callback`, ch, nil),
		"encoded path":         authorizeQuery(id, "https://app.example/%63allback", ch, nil),
		"state too long":       authorizeQuery(id, appRedirect, ch, map[string]string{"state": strings.Repeat("x", 3000)}),
		"missing PKCE, no uri": authorizeQuery(id, "", "", map[string]string{"redirect_uri": "", "code_challenge": ""}),
	}
	for name, p := range page {
		r := e.get(p)
		if r.code != 400 || r.hdr.Get("Location") != "" || !strings.Contains(r.hdr.Get("Content-Type"), "text/html") {
			t.Errorf("%s: want a 400 page without redirect, got %d %q", name, r.code, r.hdr.Get("Location"))
		}
	}

	// The loopback redirect may use any port (RFC 8252).
	e.openConsent(authorizeQuery(id, "http://127.0.0.1:5555/cb", ch, nil))

	// Errors once the redirect URI is trusted: back to the app with state.
	redir := map[string]struct {
		extra map[string]string
		err   string
	}{
		"missing PKCE":     {map[string]string{"code_challenge": "", "code_challenge_method": ""}, "invalid_request"},
		"plain PKCE":       {map[string]string{"code_challenge_method": "plain"}, "invalid_request"},
		"no PKCE method":   {map[string]string{"code_challenge_method": ""}, "invalid_request"},
		"short challenge":  {map[string]string{"code_challenge": "abc"}, "invalid_request"},
		"implicit flow":    {map[string]string{"response_type": "token"}, "unsupported_response_type"},
		"other resource":   {map[string]string{"resource": "https://other.example/mcp"}, "invalid_target"},
		"resource is base": {map[string]string{"resource": e.base}, "invalid_target"},
		"unknown scope":    {map[string]string{"scope": "relay:root"}, "invalid_scope"},
	}
	for name, c := range redir {
		r := e.get(authorizeQuery(id, appRedirect, ch, c.extra))
		loc, _ := url.Parse(r.hdr.Get("Location"))
		if r.code != 302 || loc == nil || loc.Host != "app.example" || loc.Query().Get("error") != c.err || loc.Query().Get("state") != "st-42" || loc.Query().Get("code") != "" {
			t.Errorf("%s: want 302 error=%s, got %d %q", name, c.err, r.code, r.hdr.Get("Location"))
		}
	}

	// Deny: back to the app with access_denied and the state, no session needed.
	req := e.openConsent(authorizeQuery(id, appRedirect, ch, nil))
	d := e.postJSON("/oauth/api/deny", map[string]any{"req": req}, nil)
	loc, _ := url.Parse(fmt.Sprint(d.json["redirect"]))
	if d.code != 200 || loc.Query().Get("error") != "access_denied" || loc.Query().Get("state") != "st-42" || loc.Query().Get("code") != "" {
		t.Fatalf("deny: %d %s", d.code, d.body)
	}
	if r := e.postJSON("/oauth/api/state", map[string]any{"req": req}, session("owner")); r.code != 404 {
		t.Fatalf("denied request must be gone: %d", r.code)
	}

	// Pending requests expire.
	req = e.openConsent(authorizeQuery(id, appRedirect, ch, nil))
	e.advance(11 * time.Minute)
	if r := e.postJSON("/oauth/api/state", map[string]any{"req": req}, session("owner")); r.code != 404 {
		t.Fatalf("expired request: want 404, got %d", r.code)
	}

	// No passkey yet: the page says so (and nothing can be approved).
	e.keys.has = false
	req = e.openConsent(authorizeQuery(id, appRedirect, ch, nil))
	if r := e.postJSON("/oauth/api/state", map[string]any{"req": req}, nil); r.json["has_passkeys"] != false {
		t.Fatalf("no passkey: %s", r.body)
	}
}

func TestOAuthRegisterValidation(t *testing.T) {
	e := newOAuthEnv(t, true)
	bad := [][]string{
		{},
		{"http://app.example/cb"},
		{"ftp://app.example/cb"},
		{"cursor://callback"},
		{"javascript:alert(1)"},
		{"https://app.example/cb#frag"},
		{"https://user:pass@app.example/cb"},
		{"/relative"},
		{"https:///no-host"},
		{"http://10.0.0.1/cb"},
		{"http://127.0.0.2/cb"},
		{"https://ok.example/cb", "http://evil.example/cb"},
	}
	for _, uris := range bad {
		r := e.postJSON("/oauth/register", map[string]any{"redirect_uris": uris}, nil)
		if r.code != 400 || r.json["error"] != "invalid_redirect_uri" {
			t.Errorf("%v: want 400 invalid_redirect_uri, got %d %s", uris, r.code, r.body)
		}
	}
	for _, u := range []string{"http://127.0.0.1:33418/callback", "http://[::1]:8080/cb", "http://localhost/cb", "https://claude.ai/api/mcp/auth_callback"} {
		r := e.postJSON("/oauth/register", map[string]any{"redirect_uris": []string{u}}, nil)
		if r.code != 201 || r.json["token_endpoint_auth_method"] != "none" {
			t.Errorf("%s: want 201, got %d %s", u, r.code, r.body)
		}
	}
	for _, body := range []map[string]any{
		{"redirect_uris": []string{appRedirect}, "grant_types": []string{"implicit"}},
		{"redirect_uris": []string{appRedirect}, "response_types": []string{"token"}},
		{"redirect_uris": []string{appRedirect}, "token_endpoint_auth_method": "private_key_jwt"},
	} {
		if r := e.postJSON("/oauth/register", body, nil); r.code != 400 || r.json["error"] != "invalid_client_metadata" {
			t.Errorf("%v: want invalid_client_metadata, got %d %s", body, r.code, r.body)
		}
	}
	// A client asking for client_secret_post still gets a public client (no secret).
	r := e.postJSON("/oauth/register", map[string]any{"redirect_uris": []string{appRedirect}, "token_endpoint_auth_method": "client_secret_post"}, nil)
	if r.code != 201 || r.json["client_secret"] != nil || r.json["token_endpoint_auth_method"] != "none" {
		t.Fatalf("client_secret_post: %d %s", r.code, r.body)
	}

	// Rate limit per address.
	var last int
	for i := 0; i < 12; i++ {
		last = e.postJSON("/oauth/register", map[string]any{"redirect_uris": []string{appRedirect}}, map[string]string{"X-Forwarded-For": "10.2.2.2"}).code
	}
	if last != 429 {
		t.Fatalf("register rate limit: want 429, got %d", last)
	}

	// Cap: at most MaxClients; registrations that never connected are pruned after a day.
	n, _ := e.st.CountOAuthClients(t.Context())
	for i := n; i < oauth.MaxClients; i++ {
		ip := fmt.Sprintf("10.3.%d.%d", i/250, i%250)
		if r := e.postJSON("/oauth/register", map[string]any{"redirect_uris": []string{appRedirect}}, map[string]string{"X-Forwarded-For": ip}); r.code != 201 {
			t.Fatalf("register #%d: %d %s", i, r.code, r.body)
		}
	}
	if r := e.postJSON("/oauth/register", map[string]any{"redirect_uris": []string{appRedirect}}, map[string]string{"X-Forwarded-For": "10.9.9.9"}); r.code != 503 {
		t.Fatalf("over the cap: want 503, got %d %s", r.code, r.body)
	}
	e.advance(25 * time.Hour)
	if r := e.postJSON("/oauth/register", map[string]any{"redirect_uris": []string{appRedirect}}, map[string]string{"X-Forwarded-For": "10.9.9.9"}); r.code != 201 {
		t.Fatalf("after pruning: want 201, got %d %s", r.code, r.body)
	}
}

func TestOAuthDraftsScope(t *testing.T) {
	e := newOAuthEnv(t, true)
	c := e.connect("relay:drafts")
	names := toolNames(t, e.mcpCall(c.access, "tools/list", nil))
	if has(names, "publish_now") || has(names, "list_oauth_grants") || !has(names, "schedule_post") {
		t.Fatalf("drafts tools: %v", names)
	}
	r := e.mcpCall(c.access, "tools/call", map[string]any{"name": "publish_now", "arguments": map[string]any{"id": "x"}})
	if res, _ := r.json["result"].(map[string]any); res["isError"] != true {
		t.Fatalf("drafts token called an admin tool: %s", r.body)
	}
	r = e.mcpCall(c.access, "tools/call", map[string]any{"name": "revoke_oauth_grant", "arguments": map[string]any{"id": "x"}})
	if res, _ := r.json["result"].(map[string]any); res["isError"] != true {
		t.Fatalf("drafts token revoked a grant: %s", r.body)
	}
	if r := e.rest(c.access, "DELETE", "/v1/accounts/acc_x"); r.code != 403 {
		t.Fatalf("drafts token deleted an account: %d", r.code)
	}
	if r := e.rest(c.access, "POST", "/v1/posts/p/publish"); r.code != 403 {
		t.Fatalf("drafts token published: %d %s", r.code, r.body)
	}
	// A refresh cannot widen the scope.
	wantOAuthErr(t, e.postForm("/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {c.refresh}, "client_id": {c.clientID}, "scope": {"relay:admin"}}, nil), 400, "invalid_scope")

	// An app that asks for drafts only cannot be given full access.
	id := e.register("Drafter", appRedirect)
	_, ch := pkce()
	req := e.openConsent(authorizeQuery(id, appRedirect, ch, map[string]string{"scope": "relay:drafts"}))
	st := e.postJSON("/oauth/api/state", map[string]any{"req": req}, session("owner"))
	if fmt.Sprint(st.json["scopes"]) != "[relay:drafts]" {
		t.Fatalf("scopes offered: %s", st.body)
	}
	csrf := st.json["csrf"].(string)
	b := e.postJSON("/oauth/api/approve/begin", map[string]any{"req": req, "csrf": csrf}, session("owner"))
	chal := b.json["publicKey"].(map[string]any)["challenge"].(string)
	if r := e.postJSON("/oauth/api/approve/finish", map[string]any{"req": req, "csrf": csrf, "scope": "relay:admin", "credential": map[string]any{"challenge": chal, "ok": true}}, session("owner")); r.code != 400 {
		t.Fatalf("escalated scope: want 400, got %d %s", r.code, r.body)
	}

	// An admin grant can hand out a drafts-only access token on refresh.
	a := e.connect("relay:admin")
	r = e.postForm("/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {a.refresh}, "client_id": {a.clientID}, "scope": {"relay:drafts"}}, nil)
	if r.code != 200 || r.json["scope"] != "relay:drafts" {
		t.Fatalf("narrowed refresh: %d %s", r.code, r.body)
	}
	if names := toolNames(t, e.mcpCall(r.json["access_token"].(string), "tools/list", nil)); has(names, "publish_now") {
		t.Fatalf("narrowed token sees admin tools: %v", names)
	}
}

func TestOAuthOff(t *testing.T) {
	e := newOAuthEnv(t, false)
	for _, p := range []string{"/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp",
		"/.well-known/oauth-authorization-server", "/.well-known/openid-configuration", "/oauth/authorize"} {
		if r := e.get(p); r.code != 404 {
			t.Errorf("%s with OAuth off: want 404, got %d", p, r.code)
		}
	}
	if r := e.postForm("/oauth/token", url.Values{"grant_type": {"refresh_token"}}, nil); r.code != 404 {
		t.Errorf("/oauth/token with OAuth off: want 404, got %d", r.code)
	}
	r := e.mcpCall("", "tools/list", nil)
	if r.code != 401 || r.hdr.Get("WWW-Authenticate") != `Bearer realm="relay"` {
		t.Fatalf("401 with OAuth off: %d %q", r.code, r.hdr.Get("WWW-Authenticate"))
	}
	if names := toolNames(t, e.mcpCall(token, "tools/list", nil)); !has(names, "publish_now") {
		t.Fatalf("static admin token: %v", names)
	}
	if names := toolNames(t, e.mcpCall("draft-token-0123456789", "tools/list", nil)); has(names, "publish_now") || !has(names, "schedule_post") {
		t.Fatalf("static drafts token: %v", names)
	}
}

// Static tokens keep working next to OAuth, and an OAuth token is not a static token.
func TestOAuthStaticTokensStillWork(t *testing.T) {
	e := newOAuthEnv(t, true)
	if names := toolNames(t, e.mcpCall(token, "tools/list", nil)); !has(names, "publish_now") {
		t.Fatalf("static admin token: %v", names)
	}
	if names := toolNames(t, e.mcpCall("draft-token-0123456789", "tools/list", nil)); has(names, "publish_now") {
		t.Fatalf("static drafts token: %v", names)
	}
	c := e.connect("relay:admin")
	wantInvalidToken(t, e, c.refresh) // a refresh token is not an access token
	wantInvalidToken(t, e, strings.TrimSuffix(c.access, c.access[len(c.access)-2:])+"xx")
}

// A token stored for another resource (audience) is refused even though it exists and is live.
func TestOAuthAudience(t *testing.T) {
	e := newOAuthEnv(t, true)
	ctx := t.Context()
	for i, res := range []string{"https://other.example/mcp", e.base + "/mcp"} {
		cid := fmt.Sprintf("relay_c_aud%d", i)
		if err := e.st.CreateOAuthClient(ctx, &store.OAuthClient{ID: cid, RedirectURIs: []string{appRedirect}}); err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		h := fmt.Sprintf("code%d", i)
		if err := e.st.CreateOAuthCode(ctx, &store.OAuthCode{Hash: h, ClientID: cid, RedirectURI: appRedirect, Challenge: "c", Scope: oauth.ScopeAdmin, Resource: res, CreatedAt: now, ExpiresAt: now.Add(time.Minute)}); err != nil {
			t.Fatal(err)
		}
		tok := fmt.Sprintf("relay_at_audience%d", i)
		sum := sha256.Sum256([]byte(tok))
		g := &store.OAuthGrant{ID: fmt.Sprintf("ogr_aud%d", i), ExpiresAt: now.Add(time.Hour)}
		if err := e.st.ExchangeOAuthCode(ctx, h, func(*store.OAuthCode) error { return nil }, g, func(g *store.OAuthGrant) []*store.OAuthToken {
			return []*store.OAuthToken{{Hash: fmt.Sprintf("%x", sum), GrantID: g.ID, Kind: store.TokenAccess, Scope: g.Scope, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}}
		}); err != nil {
			t.Fatal(err)
		}
	}
	wantInvalidToken(t, e, "relay_at_audience0")
	if names := toolNames(t, e.mcpCall("relay_at_audience1", "tools/list", nil)); !has(names, "publish_now") {
		t.Fatalf("same token shape for the right resource must work: %v", names)
	}
}
