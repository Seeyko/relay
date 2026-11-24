package api_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Seeyko/relay/internal/api"
	"github.com/Seeyko/relay/internal/core"
	"github.com/Seeyko/relay/internal/email"
	"github.com/Seeyko/relay/internal/mcp"
	"github.com/Seeyko/relay/internal/oauth"
	"github.com/Seeyko/relay/internal/secret"
	"github.com/Seeyko/relay/internal/store"
	"github.com/Seeyko/relay/internal/team"
)

type mailbox struct {
	mu   sync.Mutex
	sent []map[string]any
}

func (m *mailbox) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var v map[string]any
	_ = json.NewDecoder(r.Body).Decode(&v)
	m.mu.Lock()
	m.sent = append(m.sent, v)
	m.mu.Unlock()
	_, _ = io.WriteString(w, `{"id":"x"}`)
}

type tenv struct {
	*oenv
	st   *store.Store
	team *team.Team
	mail *mailbox
}

func newTeamAPI(t *testing.T) *tenv {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir + "/relay.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mb := &mailbox{}
	msrv := httptest.NewServer(mb)
	t.Cleanup(msrv.Close)
	rs, _ := email.NewResend("re_key", "Relay <no-reply@example.com>")
	rs.Endpoint = msrv.URL
	srv := httptest.NewUnstartedServer(nil)
	srv.Start()
	t.Cleanup(srv.Close)
	tm := team.New(st, rs, srv.URL, log)
	box, _ := secret.New("k")
	svc := &core.Service{Store: st, Box: box, MediaDir: dir + "/media", HTTP: http.DefaultClient, Log: log, BaseURL: srv.URL}
	m := mcp.NewServer("relay", "test", append(append(mcp.BuildTools(svc), mcp.TeamTools(tm)...), mcp.OrgTools(st)...))
	m.Store = st
	e := &oenv{t: t, srv: srv, base: srv.URL, st: st, keys: &stubKeys{sessions: map[string]bool{}, has: true}, now: time.Now()}
	as, err := oauth.New(srv.URL, st, e.keys, log)
	if err != nil {
		t.Fatal(err)
	}
	e.as = as
	a := &api.API{Svc: svc, MCP: m, Token: token, DraftToken: "draft-token-0123456789", Log: log, OAuth: as}
	srv.Config.Handler = a.Handler()
	e.plain = &http.Client{}
	return &tenv{oenv: e, st: st, team: tm, mail: mb}
}

// grant forges an approved OAuth app of member userID with scope and returns its access token.
func (e *tenv) grant(userID, scope string) string {
	e.t.Helper()
	return e.grantOrgs(userID, scope, nil)
}

// grantOrgs is grant for an app limited to some organizations (nil: all the member sees).
func (e *tenv) grantOrgs(userID, scope string, orgs []string) string {
	e.t.Helper()
	ctx := e.t.Context()
	suffix := strings.Join(orgs, "")
	c := &store.OAuthClient{ID: "relay_c_" + userID + scope + suffix, Name: "App", RedirectURIs: []string{appRedirect}}
	if err := e.st.CreateOAuthClient(ctx, c); err != nil {
		e.t.Fatal(err)
	}
	now := time.Now()
	code := "code-" + userID + scope + suffix
	if err := e.st.CreateOAuthCode(ctx, &store.OAuthCode{Hash: code, ClientID: c.ID, RedirectURI: appRedirect, Challenge: "x", Scope: scope,
		Resource: e.as.Resource(), UserID: userID, CreatedAt: now, ExpiresAt: now.Add(time.Minute), Orgs: orgs}); err != nil {
		e.t.Fatal(err)
	}
	tok := "relay_at_" + userID + strings.ReplaceAll(scope, ":", "") + suffix
	h := sha256.Sum256([]byte(tok))
	g := &store.OAuthGrant{ID: "ogr_" + userID + strings.ReplaceAll(scope, ":", "") + suffix, ExpiresAt: now.Add(time.Hour)}
	err := e.st.ExchangeOAuthCode(ctx, code, func(*store.OAuthCode) error { return nil }, g, func(g *store.OAuthGrant) []*store.OAuthToken {
		return []*store.OAuthToken{{Hash: hex.EncodeToString(h[:]), GrantID: g.ID, Kind: store.TokenAccess, Scope: scope, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}}
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return tok
}

func (e *tenv) restJSON(tok, method, path string, body any) resp {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	}
	req, _ := http.NewRequest(method, e.base+path, rd)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	return e.do(req)
}

func TestTeamToolsREST(t *testing.T) {
	e := newTeamAPI(t)
	ctx := t.Context()
	owner, _ := e.st.CreateUser(ctx, &store.User{Email: "owner@example.com", Role: store.RoleOwner})

	r := e.restJSON(token, "POST", "/v1/members", map[string]any{"email": "New@Example.com", "role": "editor", "lang": "fr"})
	if r.code != 200 || r.json["email"] != "new@example.com" || r.json["role"] != "editor" {
		t.Fatalf("invite: %d %s", r.code, r.body)
	}
	e.team.Wait()
	if len(e.mail.sent) != 1 || e.mail.sent[0]["subject"] != "Invitation à Relay" {
		t.Fatalf("french invitation: %v", e.mail.sent)
	}
	if strings.Contains(e.mail.sent[0]["html"].(string)+e.mail.sent[0]["text"].(string), token) {
		t.Fatal("the API token must never be in an email")
	}
	r = e.restJSON(token, "GET", "/v1/members", nil)
	if r.code != 200 || len(r.json["members"].([]any)) != 1 || len(r.json["invites"].([]any)) != 1 || r.json["email_enabled"] != true {
		t.Fatalf("list: %d %s", r.code, r.body)
	}
	if strings.Contains(string(r.body), "token_hash") || strings.Contains(string(r.body), "webauthn") {
		t.Fatal("list leaks login data")
	}
	// The last owner is protected.
	if r := e.restJSON(token, "PATCH", "/v1/members/"+owner.ID, map[string]any{"role": "admin"}); r.code != 409 {
		t.Fatalf("demote last owner: %d %s", r.code, r.body)
	}
	if r := e.restJSON(token, "DELETE", "/v1/members/"+owner.ID, nil); r.code != 409 {
		t.Fatalf("remove last owner: %d %s", r.code, r.body)
	}
	// The drafts key has no team tools.
	if r := e.restJSON("draft-token-0123456789", "GET", "/v1/members", nil); r.code != 403 {
		t.Fatalf("drafter lists members: %d", r.code)
	}
	names := toolNames(t, e.mcpCall("draft-token-0123456789", "tools/list", nil))
	if has(names, "invite_member") || has(names, "list_members") {
		t.Fatalf("drafter sees team tools: %v", names)
	}
	if names := toolNames(t, e.mcpCall(token, "tools/list", nil)); !has(names, "invite_member") || !has(names, "remove_member") || !has(names, "update_member") {
		t.Fatalf("admin tools: %v", names)
	}
	// Revoke the pending invitation through remove_member.
	invID := r.json["invites"].([]any)[0].(map[string]any)["id"].(string)
	if r := e.restJSON(token, "DELETE", "/v1/members/"+invID, nil); r.code != 200 {
		t.Fatalf("revoke invite: %d %s", r.code, r.body)
	}
	if r := e.restJSON(token, "POST", "/v1/members", map[string]any{"email": "nope", "role": "editor"}); r.code != 400 {
		t.Fatalf("bad address: %d", r.code)
	}
}

// The role of the member who approved an app caps what the app can do on the API.
func TestMemberRoleCapsOAuthApps(t *testing.T) {
	e := newTeamAPI(t)
	ctx := t.Context()
	e.st.CreateUser(ctx, &store.User{Email: "owner@example.com", Role: store.RoleOwner})
	ed, _ := e.st.CreateUser(ctx, &store.User{Email: "ed@example.com", Role: store.RoleEditor})
	vi, _ := e.st.CreateUser(ctx, &store.User{Email: "vi@example.com", Role: store.RoleViewer})
	ad, _ := e.st.CreateUser(ctx, &store.User{Email: "ad@example.com", Role: store.RoleAdmin})

	// An editor's app, even holding a full-access grant, is drafts only: it reads and drafts, never publishes.
	edTok := e.grant(ed.ID, oauth.ScopeAdmin)
	if r := e.restJSON(edTok, "GET", "/v1/posts", nil); r.code != 200 {
		t.Fatalf("editor reads: %d %s", r.code, r.body)
	}
	if r := e.restJSON(edTok, "POST", "/v1/posts/post_x/publish", map[string]any{}); r.code != 403 {
		t.Fatalf("editor publishes: want 403, got %d %s", r.code, r.body)
	}
	if r := e.restJSON(edTok, "POST", "/v1/accounts", map[string]any{"provider": "x"}); r.code != 403 {
		t.Fatalf("editor connects an account: want 403, got %d", r.code)
	}
	if r := e.restJSON(edTok, "POST", "/v1/posts/post_x/approve", map[string]any{}); r.code != 403 {
		t.Fatalf("editor approves: want 403, got %d", r.code)
	}
	// A viewer's app gets nothing.
	if r := e.restJSON(e.grant(vi.ID, oauth.ScopeDrafts), "GET", "/v1/posts", nil); r.code != 401 {
		t.Fatalf("viewer app: want 401, got %d", r.code)
	}
	// An admin's app manages the team as that admin: no owner promotion.
	adTok := e.grant(ad.ID, oauth.ScopeAdmin)
	if r := e.restJSON(adTok, "PATCH", "/v1/members/"+ed.ID, map[string]any{"role": "owner"}); r.code != 403 {
		t.Fatalf("admin app makes an owner: want 403, got %d %s", r.code, r.body)
	}
	if r := e.restJSON(adTok, "PATCH", "/v1/members/"+ed.ID, map[string]any{"role": "viewer"}); r.code != 200 {
		t.Fatalf("admin app changes an editor: %d %s", r.code, r.body)
	}
	// Removing the admin disconnects its app.
	if r := e.restJSON(token, "DELETE", "/v1/members/"+ad.ID, nil); r.code != 200 {
		t.Fatalf("remove admin: %d %s", r.code, r.body)
	}
	if r := e.restJSON(adTok, "GET", "/v1/posts", nil); r.code != 401 {
		t.Fatalf("removed admin's app: want 401, got %d", r.code)
	}
}
