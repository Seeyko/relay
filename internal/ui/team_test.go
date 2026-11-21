package ui

import (
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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Seeyko/relay/internal/email"
	"github.com/Seeyko/relay/internal/oauth"
	"github.com/Seeyko/relay/internal/store"
	"github.com/Seeyko/relay/internal/team"
)

// ---------- a fake Resend ----------

type sentMail struct {
	From, Subject, HTML, Text string
	To                        []string
}

type fakeResend struct {
	mu   sync.Mutex
	sent []sentMail
	auth []string
}

func (f *fakeResend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var m sentMail
	_ = json.NewDecoder(r.Body).Decode(&m)
	f.mu.Lock()
	f.sent = append(f.sent, m)
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	f.mu.Unlock()
	if r.URL.Path != "/emails" || r.Method != http.MethodPost {
		w.WriteHeader(404)
		return
	}
	_, _ = io.WriteString(w, `{"id":"fake"}`)
}

func (f *fakeResend) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.sent) }

func (f *fakeResend) last(t *testing.T) sentMail {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) == 0 {
		t.Fatal("no email sent")
	}
	return f.sent[len(f.sent)-1]
}

var linkRe = regexp.MustCompile(`token=([A-Za-z0-9_-]{43})`)
var codeRe = regexp.MustCompile(`\b(\d{6})\b`)

func tokenIn(t *testing.T, m sentMail) string {
	t.Helper()
	got := linkRe.FindStringSubmatch(m.Text)
	if got == nil {
		t.Fatalf("no link in %q", m.Text)
	}
	return got[1]
}

// ---------- harness with members, email and OAuth ----------

type teamEnv struct {
	*harness
	mail  *fakeResend
	team  *team.Team
	as    *oauth.Server
	clock *clock
	ip    int
	ipMu  sync.Mutex
}

type clock struct {
	mu sync.Mutex
	d  time.Duration
}

func (c *clock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return time.Now().Add(c.d) }
func (c *clock) add(d time.Duration) { c.mu.Lock(); c.d += d; c.mu.Unlock() }

func newTeamEnv(t *testing.T, withEmail bool) *teamEnv {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	fr := &fakeResend{}
	rsrv := httptest.NewServer(fr)
	t.Cleanup(rsrv.Close)
	var sender email.Sender
	if withEmail {
		rs, err := email.NewResend("re_test_key", "RideMyPark <no-reply@ridemypark.com>")
		if err != nil {
			t.Fatal(err)
		}
		rs.Endpoint = rsrv.URL + "/emails"
		sender = rs
	}
	base := "http://localhost"
	ck := &clock{}
	tm := team.New(st, sender, base, log)
	tm.Now = ck.now
	u, err := New(base, st, testToken, log)
	if err != nil {
		t.Fatal(err)
	}
	u.Apps, u.Team = true, tm
	as, err := oauth.New(base, st, u, log)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	u.Register(mux)
	as.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	jar, _ := cookiejar.New(nil)
	h := &harness{t: t, srv: srv, st: st, client: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, origin: base, ui: u}
	return &teamEnv{harness: h, mail: fr, team: tm, as: as, clock: ck}
}

// browser is another person on the same server (own cookies).
func (e *teamEnv) browser() *harness {
	b := *e.harness
	jar, _ := cookiejar.New(nil)
	b.client = &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &b
}

// call posts JSON from a fresh client address (the per-address limits are tested on their own).
func (e *teamEnv) call(b *harness, method, path string, body any) (int, map[string]any) {
	e.t.Helper()
	e.ipMu.Lock()
	e.ip++
	ip := fmt.Sprintf("198.51.%d.%d", e.ip/250, e.ip%250)
	e.ipMu.Unlock()
	return b.do(method, path, body, map[string]string{"X-Forwarded-For": ip})
}

// owner bootstraps the first owner with a passkey (API token), signed in on e's browser.
func (e *teamEnv) owner() (*softKey, *store.User) {
	e.t.Helper()
	k := newSoftKey("localhost", e.origin)
	e.register(k)
	if code, res := e.login(k); code != 200 {
		e.t.Fatalf("owner login: %d %v", code, res)
	}
	o, err := e.st.FirstOwner(e.t.Context())
	if err != nil {
		e.t.Fatal(err)
	}
	return k, o
}

// invite invites addr with role from inviter and returns the token of the link sent.
func (e *teamEnv) invite(inviter *harness, addr, role string) string {
	e.t.Helper()
	n := e.mail.count()
	if code, res := e.call(inviter, "POST", "/ui/api/team/invite", map[string]any{"email": addr, "role": role}); code != 200 {
		e.t.Fatalf("invite %s: %d %v", addr, code, res)
	}
	e.team.Wait()
	if e.mail.count() != n+1 {
		e.t.Fatal("invitation email not sent")
	}
	return tokenIn(e.t, e.mail.last(e.t))
}

// join accepts an invitation in a new browser, signed in afterwards.
func (e *teamEnv) join(tok, name string) *harness {
	e.t.Helper()
	b := e.browser()
	if code, res := e.call(b, "POST", "/ui/api/invite/accept", map[string]any{"token": tok, "name": name}); code != 200 {
		e.t.Fatalf("accept: %d %v", code, res)
	}
	return b
}

func meOf(t *testing.T, e *teamEnv, b *harness) map[string]any {
	t.Helper()
	_, me := e.call(b, "GET", "/ui/api/me", nil)
	u, _ := me["user"].(map[string]any)
	return u
}

// ---------- invitations ----------

func TestInviteAcceptExpiryReuse(t *testing.T) {
	e := newTeamEnv(t, true)
	e.owner()

	tok := e.invite(e.harness, "Editor@Example.com", "editor")
	m := e.mail.last(t)
	if len(m.To) != 1 || m.To[0] != "editor@example.com" || m.From != "RideMyPark <no-reply@ridemypark.com>" {
		t.Fatalf("email headers: %+v", m)
	}
	if !strings.Contains(m.HTML, "/ui/accept?token="+tok) || !strings.Contains(m.Text, "http://localhost/ui/accept?token=") {
		t.Fatalf("link missing: %s", m.Text)
	}
	if strings.Contains(m.HTML+m.Text, testToken) || strings.Contains(strings.ToLower(m.HTML), "<img") {
		t.Fatal("an email must never carry the API token nor images (tracking)")
	}
	if e.mail.auth[0] != "Bearer re_test_key" {
		t.Fatalf("resend auth: %q", e.mail.auth[0])
	}

	// Only the SHA-256 of the token is stored.
	if _, err := e.st.PeekAuthToken(t.Context(), store.TokenInvite, tok, time.Now()); err == nil {
		t.Fatal("the clear token must not be what is stored")
	}
	if _, err := e.st.PeekAuthToken(t.Context(), store.TokenInvite, team.HashToken(tok), time.Now()); err != nil {
		t.Fatalf("hashed token: %v", err)
	}

	// The accept page shows the address; the name is required.
	b := e.browser()
	if code, res := e.call(b, "POST", "/ui/api/invite", map[string]any{"token": tok}); code != 200 || res["email"] != "editor@example.com" || res["role"] != "editor" {
		t.Fatalf("check: %d %v", code, res)
	}
	if code, _ := e.call(b, "POST", "/ui/api/invite/accept", map[string]any{"token": tok, "name": "  "}); code != 400 {
		t.Fatalf("empty name: want 400, got %d", code)
	}
	if code, res := e.call(b, "POST", "/ui/api/invite/accept", map[string]any{"token": tok, "name": "Eddie"}); code != 200 {
		t.Fatalf("accept: %d %v", code, res)
	}
	if u := meOf(t, e, b); u["role"] != "editor" || u["name"] != "Eddie" || u["email"] != "editor@example.com" {
		t.Fatalf("signed in as the new member: %v", u)
	}
	// Single use.
	if code, _ := e.call(e.browser(), "POST", "/ui/api/invite/accept", map[string]any{"token": tok, "name": "Again"}); code != 400 {
		t.Fatalf("reused invitation: want 400, got %d", code)
	}
	// Seven days.
	tok2 := e.invite(e.harness, "late@example.com", "viewer")
	e.clock.add(InviteTTLForTest + time.Minute)
	if code, _ := e.call(e.browser(), "POST", "/ui/api/invite/accept", map[string]any{"token": tok2, "name": "Late"}); code != 400 {
		t.Fatalf("expired invitation: want 400, got %d", code)
	}
	e.clock.add(-InviteTTLForTest - time.Minute)

	// Resend replaces the link; revoke kills it.
	tok3 := e.invite(e.harness, "r@example.com", "viewer")
	_, list := e.call(e.harness, "GET", "/ui/api/team", nil)
	var invID string
	for _, i := range list["invites"].([]any) {
		if i.(map[string]any)["email"] == "r@example.com" {
			invID = i.(map[string]any)["id"].(string)
		}
	}
	if code, _ := e.call(e.harness, "POST", "/ui/api/team/invites/"+invID+"/resend", map[string]any{}); code != 200 {
		t.Fatal("resend")
	}
	e.team.Wait()
	tok4 := tokenIn(t, e.mail.last(t))
	if code, _ := e.call(e.browser(), "POST", "/ui/api/invite", map[string]any{"token": tok3}); code != 400 {
		t.Fatal("the old link must stop working after a resend")
	}
	if code, _ := e.call(e.harness, "POST", "/ui/api/team/invites/"+invID+"/revoke", map[string]any{}); code != 204 {
		t.Fatal("revoke")
	}
	if code, _ := e.call(e.browser(), "POST", "/ui/api/invite/accept", map[string]any{"token": tok4, "name": "R"}); code != 400 {
		t.Fatal("a revoked invitation must not work")
	}

	// Inviting a member again is refused; CSRF: cross-origin POSTs are refused.
	if code, _ := e.call(e.harness, "POST", "/ui/api/team/invite", map[string]any{"email": "editor@example.com", "role": "viewer"}); code != 400 {
		t.Fatalf("invite a member: want 400, got %d", code)
	}
	if code, _ := e.do("POST", "/ui/api/team/invite", map[string]any{"email": "x@example.com", "role": "viewer"}, map[string]string{"Origin": "https://evil.example"}); code != 403 {
		t.Fatalf("cross-origin invite: want 403, got %d", code)
	}

	// The audit log has the events, never a token.
	entries, _ := e.st.ListAudit(t.Context(), 100)
	seen := map[string]bool{}
	for _, a := range entries {
		seen[a.Action] = true
		if strings.Contains(a.Detail+a.Target, tok) {
			t.Fatal("token in the audit log")
		}
	}
	for _, want := range []string{"invite", "invite.accept", "invite.resend", "invite.revoke", "login"} {
		if !seen[want] {
			t.Fatalf("audit: %q missing in %v", want, seen)
		}
	}
}

// InviteTTLForTest mirrors team.InviteTTL.
const InviteTTLForTest = team.InviteTTL

// ---------- roles ----------

func TestRolesEnforced(t *testing.T) {
	e := newTeamEnv(t, true)
	_, owner := e.owner()
	editor := e.join(e.invite(e.harness, "ed@example.com", "editor"), "Ed")
	viewer := e.join(e.invite(e.harness, "vi@example.com", "viewer"), "Vi")
	admin := e.join(e.invite(e.harness, "ad@example.com", "admin"), "Ad")

	// Everyone reads.
	for _, b := range []*harness{editor, viewer, admin} {
		if code, _ := e.call(b, "GET", "/ui/api/calendar?from=2026-01-01T00:00:00Z&to=2026-01-08T00:00:00Z", nil); code != 200 {
			t.Fatalf("read: %d", code)
		}
		if code, _ := e.call(b, "GET", "/ui/api/drafts", nil); code != 200 {
			t.Fatalf("drafts: %d", code)
		}
	}
	// Only owners and admins manage the team.
	for _, b := range []*harness{editor, viewer} {
		if code, _ := e.call(b, "GET", "/ui/api/team", nil); code != 403 {
			t.Fatalf("team list by a non-admin: want 403, got %d", code)
		}
		if code, _ := e.call(b, "POST", "/ui/api/team/invite", map[string]any{"email": "x@example.com", "role": "admin"}); code != 403 {
			t.Fatalf("invite by a non-admin: want 403, got %d", code)
		}
	}
	ed, _ := e.st.UserByEmail(t.Context(), "ed@example.com")
	if code, _ := e.call(viewer, "POST", "/ui/api/team/members/"+ed.ID, map[string]any{"role": "owner"}); code != 403 {
		t.Fatalf("role change by a viewer: want 403, got %d", code)
	}
	// A member renames themselves, but cannot change their own role.
	if code, _ := e.call(editor, "POST", "/ui/api/team/members/"+ed.ID, map[string]any{"name": "Edward"}); code != 200 {
		t.Fatalf("self rename: %d", code)
	}
	if code, _ := e.call(editor, "POST", "/ui/api/team/members/"+ed.ID, map[string]any{"role": "admin"}); code != 403 {
		t.Fatalf("self promotion: want 403, got %d", code)
	}
	// An admin manages members but not owners, and cannot make one.
	if code, _ := e.call(admin, "POST", "/ui/api/team/members/"+ed.ID, map[string]any{"role": "viewer"}); code != 200 {
		t.Fatalf("admin changes an editor: %d", code)
	}
	if code, _ := e.call(admin, "POST", "/ui/api/team/members/"+ed.ID, map[string]any{"role": "owner"}); code != 403 {
		t.Fatalf("admin makes an owner: want 403, got %d", code)
	}
	if code, _ := e.call(admin, "POST", "/ui/api/team/members/"+owner.ID+"/remove", map[string]any{}); code != 403 {
		t.Fatalf("admin removes the owner: want 403, got %d", code)
	}
	if code, _ := e.call(admin, "POST", "/ui/api/team/invite", map[string]any{"email": "o2@example.com", "role": "owner"}); code != 403 {
		t.Fatalf("admin invites an owner: want 403, got %d", code)
	}
	// The last owner can neither be demoted, disabled nor removed, even by itself.
	if code, _ := e.call(e.harness, "POST", "/ui/api/team/members/"+owner.ID, map[string]any{"role": "admin"}); code != 409 {
		t.Fatalf("demote the last owner: want 409, got %d", code)
	}
	if code, _ := e.call(e.harness, "POST", "/ui/api/team/members/"+owner.ID, map[string]any{"disabled": true}); code != 409 {
		t.Fatalf("disable the last owner: want 409, got %d", code)
	}
	if code, _ := e.call(e.harness, "POST", "/ui/api/team/members/"+owner.ID+"/remove", map[string]any{}); code != 409 {
		t.Fatalf("remove the last owner: want 409, got %d", code)
	}

	// The role is read on every request: a demoted admin loses the Team sheet at once.
	ad, _ := e.st.UserByEmail(t.Context(), "ad@example.com")
	if code, _ := e.call(e.harness, "POST", "/ui/api/team/members/"+ad.ID, map[string]any{"role": "viewer"}); code != 200 {
		t.Fatal("demote admin")
	}
	if code, _ := e.call(admin, "GET", "/ui/api/team", nil); code != 403 {
		t.Fatalf("demoted admin: want 403, got %d", code)
	}
	// Disabling signs out; removing too.
	if code, _ := e.call(e.harness, "POST", "/ui/api/team/members/"+ad.ID, map[string]any{"disabled": true}); code != 200 {
		t.Fatal("disable")
	}
	if code, _ := e.call(admin, "GET", "/ui/api/drafts", nil); code != 401 {
		t.Fatalf("disabled member: want 401, got %d", code)
	}
	vi, _ := e.st.UserByEmail(t.Context(), "vi@example.com")
	if code, _ := e.call(e.harness, "POST", "/ui/api/team/members/"+vi.ID+"/remove", map[string]any{}); code != 204 {
		t.Fatal("remove")
	}
	if code, _ := e.call(viewer, "GET", "/ui/api/drafts", nil); code != 401 {
		t.Fatalf("removed member: want 401, got %d", code)
	}

	_, list := e.call(e.harness, "GET", "/ui/api/team", nil)
	if n := len(list["members"].([]any)); n != 3 {
		t.Fatalf("members after removal: %d", n)
	}
}

// ---------- sign-in links ----------

func TestMagicLink(t *testing.T) {
	e := newTeamEnv(t, true)
	e.owner()
	e.join(e.invite(e.harness, "m@example.com", "viewer"), "M")
	n := e.mail.count()

	// Same answer for a member and a stranger; only the member gets an email.
	b := e.browser()
	c1, r1 := e.call(b, "POST", "/ui/api/email/login", map[string]any{"email": "M@example.com"})
	c2, r2 := e.call(b, "POST", "/ui/api/email/login", map[string]any{"email": "nobody@example.com"})
	if c1 != 200 || c2 != 200 || fmt.Sprint(r1) != fmt.Sprint(r2) {
		t.Fatalf("answers differ: %d %v / %d %v", c1, r1, c2, r2)
	}
	e.team.Wait()
	if e.mail.count() != n+1 {
		t.Fatalf("want exactly one email, got %d", e.mail.count()-n)
	}
	m := e.mail.last(t)
	if m.To[0] != "m@example.com" || !strings.Contains(m.Text, "/ui/signin?token=") {
		t.Fatalf("sign-in email: %+v", m)
	}
	tok := tokenIn(t, m)

	// The landing page is the dashboard; the token is posted, never consumed by a GET (link scanners).
	resp, err := b.client.Get(e.srv.URL + "/ui/signin?token=" + tok)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Cache-Control") != "no-store" || resp.Header.Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("landing: %d %v", resp.StatusCode, resp.Header)
	}
	if u := meOf(t, e, b); u != nil {
		t.Fatal("a GET must not sign in")
	}
	if code, _ := e.call(b, "POST", "/ui/api/email/signin", map[string]any{"token": tok}); code != 200 {
		t.Fatalf("signin: %d", code)
	}
	if u := meOf(t, e, b); u == nil || u["email"] != "m@example.com" {
		t.Fatalf("signed in: %v", u)
	}
	// Single use.
	if code, _ := e.call(e.browser(), "POST", "/ui/api/email/signin", map[string]any{"token": tok}); code != 400 {
		t.Fatalf("reused link: want 400, got %d", code)
	}
	// 15 minutes.
	e.call(e.browser(), "POST", "/ui/api/email/login", map[string]any{"email": "m@example.com"})
	e.team.Wait()
	tok = tokenIn(t, e.mail.last(t))
	e.clock.add(16 * time.Minute)
	if code, _ := e.call(e.browser(), "POST", "/ui/api/email/signin", map[string]any{"token": tok}); code != 400 {
		t.Fatalf("expired link: want 400, got %d", code)
	}
	// Only the latest link works.
	e.call(e.browser(), "POST", "/ui/api/email/login", map[string]any{"email": "m@example.com"})
	e.team.Wait()
	old := tokenIn(t, e.mail.last(t))
	e.clock.add(16 * time.Minute) // past the per-address window
	e.call(e.browser(), "POST", "/ui/api/email/login", map[string]any{"email": "m@example.com"})
	e.team.Wait()
	if code, _ := e.call(e.browser(), "POST", "/ui/api/email/signin", map[string]any{"token": old}); code != 400 {
		t.Fatalf("superseded link: want 400, got %d", code)
	}
	e.clock.add(16 * time.Minute)

	// Per address: at most 3 links per 15 minutes, with the same answer after that.
	n = e.mail.count()
	for i := 0; i < 5; i++ {
		if code, _ := e.call(e.browser(), "POST", "/ui/api/email/login", map[string]any{"email": "m@example.com"}); code != 200 {
			t.Fatalf("per-address limit must not change the answer: %d", code)
		}
	}
	e.team.Wait()
	if got := e.mail.count() - n; got != 3 {
		t.Fatalf("per-address limit: want 3 emails, got %d", got)
	}
	// Per client: 10 requests per 15 minutes.
	var last int
	for i := 0; i < 12; i++ {
		last, _ = b.do("POST", "/ui/api/email/login", map[string]any{"email": fmt.Sprintf("x%d@example.com", i)}, map[string]string{"X-Forwarded-For": "203.0.113.7"})
	}
	if last != 429 {
		t.Fatalf("per-client limit: want 429, got %d", last)
	}

	// A disabled member gets no link.
	e.clock.add(16 * time.Minute)
	mu, _ := e.st.UserByEmail(t.Context(), "m@example.com")
	yes := true
	if _, err := e.st.UpdateUser(t.Context(), mu.ID, store.UserPatch{Disabled: &yes}); err != nil {
		t.Fatal(err)
	}
	n = e.mail.count()
	e.call(e.browser(), "POST", "/ui/api/email/login", map[string]any{"email": "m@example.com"})
	e.team.Wait()
	if e.mail.count() != n {
		t.Fatal("disabled member got a sign-in link")
	}
	if u := meOf(t, e, b); u != nil {
		t.Fatal("disabling must end the session")
	}
}

// A member signed in by email adds a passkey for themselves (no API token), then signs in with it.
func TestMemberAddsPasskey(t *testing.T) {
	e := newTeamEnv(t, true)
	e.owner()
	b := e.join(e.invite(e.harness, "p@example.com", "editor"), "P")
	pu, _ := e.st.UserByEmail(t.Context(), "p@example.com")

	k := newSoftKey("localhost", e.origin)
	code, opts := e.call(b, "POST", "/ui/api/register/begin", map[string]any{"name": "laptop"})
	if code != 200 {
		t.Fatalf("begin: %d %v", code, opts)
	}
	if uid := opts["publicKey"].(map[string]any)["user"].(map[string]any)["id"]; uid != base64.RawURLEncoding.EncodeToString([]byte(pu.ID)) {
		t.Fatalf("the passkey must carry the member's handle, got %v", uid)
	}
	if code, res := e.call(b, "POST", "/ui/api/register/finish", k.create(challengeOf(t, opts))); code != 200 {
		t.Fatalf("finish: %d %v", code, res)
	}
	if pks, _ := e.st.ListPasskeys(t.Context(), pu.ID); len(pks) != 1 {
		t.Fatal("passkey not tied to the member")
	}
	// Signing in with it from a new browser opens the member's session, not the owner's.
	nb := e.browser()
	_, lo := e.call(nb, "POST", "/ui/api/login/begin", map[string]any{})
	if code, res := e.call(nb, "POST", "/ui/api/login/finish", k.get(challengeOf(t, lo), []byte(pu.ID))); code != 200 {
		t.Fatalf("member passkey login: %d %v", code, res)
	}
	if u := meOf(t, e, nb); u["email"] != "p@example.com" || u["passkeys"] != float64(1) {
		t.Fatalf("signed in as: %v", u)
	}
	// The member's passkey with the owner's handle is refused.
	_, lo = e.call(nb, "POST", "/ui/api/login/begin", map[string]any{})
	if code, _ := e.call(nb, "POST", "/ui/api/login/finish", k.get(challengeOf(t, lo), ownerID)); code != 401 {
		t.Fatalf("handle mismatch: want 401, got %d", code)
	}
	// The passkeys list shows the member's own keys only.
	_, pl := e.call(nb, "GET", "/ui/api/passkeys", nil)
	if n := len(pl["passkeys"].([]any)); n != 1 {
		t.Fatalf("own passkeys: %d", n)
	}
	// Without a session and without the API token: refused.
	if code, _ := e.call(e.browser(), "POST", "/ui/api/register/begin", map[string]any{}); code != 401 {
		t.Fatalf("anonymous registration: want 401, got %d", code)
	}
}

func TestEmailOff(t *testing.T) {
	e := newTeamEnv(t, false)
	e.owner()
	_, me := e.call(e.harness, "GET", "/ui/api/me", nil)
	if me["email"] != false || me["team"] != true {
		t.Fatalf("me: %v", me)
	}
	if code, res := e.call(e.browser(), "POST", "/ui/api/email/login", map[string]any{"email": "a@example.com"}); code != 409 || !strings.Contains(fmt.Sprint(res["error"]), "RELAY_RESEND_API_KEY") {
		t.Fatalf("email login while off: %d %v", code, res)
	}
	if code, _ := e.call(e.harness, "POST", "/ui/api/team/invite", map[string]any{"email": "a@example.com", "role": "viewer"}); code != 409 {
		t.Fatalf("invite while off: want 409, got %d", code)
	}
	_, list := e.call(e.harness, "GET", "/ui/api/team", nil)
	if list["email"] != false {
		t.Fatalf("team: %v", list)
	}
}

// ---------- OAuth approvals by members ----------

func pkcePair() (string, string) {
	v := randomToken()
	s := sha256.Sum256([]byte(v))
	return v, base64.RawURLEncoding.EncodeToString(s[:])
}

var reqRe = regexp.MustCompile(`data-req="([^"]+)"`)

const appRedirect = "https://app.example/callback"

// consent registers an app and opens its consent page; it returns the request id and the PKCE verifier.
func (e *teamEnv) consent(b *harness, scope string) (string, string, string) {
	e.t.Helper()
	_, reg := e.call(b, "POST", "/oauth/register", map[string]any{"client_name": "Claude", "redirect_uris": []string{appRedirect}})
	id, _ := reg["client_id"].(string)
	v, ch := pkcePair()
	q := url.Values{"response_type": {"code"}, "client_id": {id}, "redirect_uri": {appRedirect}, "code_challenge": {ch}, "code_challenge_method": {"S256"}, "state": {"s"}}
	if scope != "" {
		q.Set("scope", scope)
	}
	resp, err := b.client.Get(e.srv.URL + "/oauth/authorize?" + q.Encode())
	if err != nil {
		e.t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	m := reqRe.FindSubmatch(body)
	if m == nil {
		e.t.Fatalf("no consent request: %d %s", resp.StatusCode, body)
	}
	return string(m[1]), id, v
}

func (e *teamEnv) state(b *harness, req string) map[string]any {
	e.t.Helper()
	_, st := e.call(b, "POST", "/oauth/api/state", map[string]any{"req": req})
	return st
}

// sentCode returns the 6-digit code of the last email.
func (e *teamEnv) sentCode() string {
	e.t.Helper()
	e.team.Wait()
	m := e.mail.last(e.t)
	c := codeRe.FindStringSubmatch(m.Text)
	if c == nil {
		e.t.Fatalf("no code in %q", m.Text)
	}
	return c[1]
}

func TestOAuthEmailCodeCheck(t *testing.T) {
	e := newTeamEnv(t, true)
	e.owner()
	editor := e.join(e.invite(e.harness, "ed@example.com", "editor"), "Ed")
	viewer := e.join(e.invite(e.harness, "vi@example.com", "viewer"), "Vi")
	ed, _ := e.st.UserByEmail(t.Context(), "ed@example.com")

	// An editor without a passkey: drafts only, confirmed by an email code.
	req, clientID, verifier := e.consent(editor, "")
	st := e.state(editor, req)
	if st["method"] != "email" || fmt.Sprint(st["scopes"]) != "[relay:drafts]" || st["can_approve"] != true {
		t.Fatalf("editor state: %v", st)
	}
	csrf := st["csrf"].(string)
	if code, res := e.call(editor, "POST", "/oauth/api/approve/begin", map[string]any{"req": req, "csrf": csrf}); code != 200 || res["method"] != "email" {
		t.Fatalf("begin: %d %v", code, res)
	}
	code6 := e.sentCode()
	if m := e.mail.last(t); m.To[0] != "ed@example.com" || !strings.Contains(m.Text, "Claude") {
		t.Fatalf("code email: %+v", m)
	}
	// Full access is beyond the editor's role.
	if code, _ := e.call(editor, "POST", "/oauth/api/approve/finish", map[string]any{"req": req, "csrf": csrf, "scope": "relay:admin", "code": code6}); code != 400 {
		t.Fatalf("editor asks full access: want 400, got %d", code)
	}
	wrong := fmt.Sprintf("%06d", (atoi(code6)+1)%1000000)
	if code, _ := e.call(editor, "POST", "/oauth/api/approve/finish", map[string]any{"req": req, "csrf": csrf, "scope": "relay:drafts", "code": wrong}); code != 401 {
		t.Fatalf("wrong code: want 401, got %d", code)
	}
	code, res := e.call(editor, "POST", "/oauth/api/approve/finish", map[string]any{"req": req, "csrf": csrf, "scope": "relay:drafts", "code": code6})
	if code != 200 {
		t.Fatalf("right code: %d %v", code, res)
	}
	u, _ := url.Parse(res["redirect"].(string))
	tr := e.token(u.Query().Get("code"), clientID, verifier)
	acc, ok := e.as.Authenticate(t.Context(), tr)
	if !ok || acc.Scope != oauth.ScopeDrafts || acc.UserID != ed.ID {
		t.Fatalf("editor token: %+v %v", acc, ok)
	}
	// The Apps sheet says who approved.
	_, apps := e.call(e.harness, "GET", "/ui/api/apps", nil)
	a0 := apps["apps"].([]any)[0].(map[string]any)
	if a0["approved_by"] != "Ed" || a0["user_id"] != ed.ID {
		t.Fatalf("approved_by: %v", a0)
	}
	// The code was single use.
	req2, _, _ := e.consent(editor, "")
	st2 := e.state(editor, req2)
	e.call(editor, "POST", "/oauth/api/approve/begin", map[string]any{"req": req2, "csrf": st2["csrf"]})
	fresh := e.sentCode()
	if fresh != code6 { // the old code is not the live one (and was burned anyway)
		if code, _ := e.call(editor, "POST", "/oauth/api/approve/finish", map[string]any{"req": req2, "csrf": st2["csrf"], "scope": "relay:drafts", "code": code6}); code != 401 {
			t.Fatalf("old code: want 401, got %d", code)
		}
	}
	// 5 attempts, then the code is dead even when right.
	req3, _, _ := e.consent(editor, "")
	st3 := e.state(editor, req3)
	e.call(editor, "POST", "/oauth/api/approve/begin", map[string]any{"req": req3, "csrf": st3["csrf"]})
	good := e.sentCode()
	bad := fmt.Sprintf("%06d", (atoi(good)+7)%1000000)
	for i := 0; i < 5; i++ {
		e.call(editor, "POST", "/oauth/api/approve/finish", map[string]any{"req": req3, "csrf": st3["csrf"], "scope": "relay:drafts", "code": bad})
	}
	if code, _ := e.call(editor, "POST", "/oauth/api/approve/finish", map[string]any{"req": req3, "csrf": st3["csrf"], "scope": "relay:drafts", "code": good}); code != 401 {
		t.Fatalf("after 5 wrong codes: want 401, got %d", code)
	}
	// 10 minutes.
	req4, _, _ := e.consent(editor, "")
	st4 := e.state(editor, req4)
	e.call(editor, "POST", "/oauth/api/approve/begin", map[string]any{"req": req4, "csrf": st4["csrf"]})
	good = e.sentCode()
	e.clock.add(11 * time.Minute)
	if code, _ := e.call(editor, "POST", "/oauth/api/approve/finish", map[string]any{"req": req4, "csrf": st4["csrf"], "scope": "relay:drafts", "code": good}); code != 401 {
		t.Fatalf("expired code: want 401, got %d", code)
	}
	e.clock.add(-11 * time.Minute)

	// A viewer cannot approve.
	reqV, _, _ := e.consent(viewer, "")
	stV := e.state(viewer, reqV)
	if stV["can_approve"] != false {
		t.Fatalf("viewer state: %v", stV)
	}
	if code, _ := e.call(viewer, "POST", "/oauth/api/approve/begin", map[string]any{"req": reqV, "csrf": stV["csrf"]}); code != 403 {
		t.Fatalf("viewer begin: want 403, got %d", code)
	}

	// A demotion caps the apps already approved; a removal disconnects them.
	vu, _ := e.st.UserByEmail(t.Context(), "vi@example.com")
	viewerRole := "viewer"
	if _, err := e.st.UpdateUser(t.Context(), ed.ID, store.UserPatch{Role: &viewerRole}); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.as.Authenticate(t.Context(), tr); ok {
		t.Fatal("an app of a member demoted to viewer must stop working")
	}
	_ = vu
	if err := e.st.DeleteUser(t.Context(), ed.ID); err != nil {
		t.Fatal(err)
	}
	if gs, _ := e.st.ListOAuthGrants(t.Context()); len(gs) != 0 {
		t.Fatalf("grants of a removed member must go: %v", gs)
	}
}

// An admin approving with a passkey: the passkey must be the admin's own.
func TestOAuthPasskeyIsTheMembers(t *testing.T) {
	e := newTeamEnv(t, true)
	ownerKey, _ := e.owner()
	admin := e.join(e.invite(e.harness, "ad@example.com", "admin"), "Ad")
	au, _ := e.st.UserByEmail(t.Context(), "ad@example.com")
	ak := newSoftKey("localhost", e.origin)
	_, opts := e.call(admin, "POST", "/ui/api/register/begin", map[string]any{})
	if code, _ := e.call(admin, "POST", "/ui/api/register/finish", ak.create(challengeOf(t, opts))); code != 200 {
		t.Fatal("admin passkey")
	}

	req, clientID, verifier := e.consent(admin, "relay:admin")
	st := e.state(admin, req)
	if st["method"] != "passkey" || fmt.Sprint(st["scopes"]) != "[relay:admin relay:drafts]" {
		t.Fatalf("admin state: %v", st)
	}
	begin := func() string {
		_, o := e.call(admin, "POST", "/oauth/api/approve/begin", map[string]any{"req": req, "csrf": st["csrf"]})
		return challengeOf(t, o)
	}
	// The owner's passkey (someone else's) cannot confirm the admin's approval.
	if code, _ := e.call(admin, "POST", "/oauth/api/approve/finish", map[string]any{"req": req, "csrf": st["csrf"], "scope": "relay:admin", "credential": ownerKey.get(begin(), ownerID)}); code != 401 {
		t.Fatalf("another member's passkey: want 401, got %d", code)
	}
	code, res := e.call(admin, "POST", "/oauth/api/approve/finish", map[string]any{"req": req, "csrf": st["csrf"], "scope": "relay:admin", "credential": ak.get(begin(), []byte(au.ID))})
	if code != 200 {
		t.Fatalf("own passkey: %d %v", code, res)
	}
	u, _ := url.Parse(res["redirect"].(string))
	tr := e.token(u.Query().Get("code"), clientID, verifier)
	if acc, ok := e.as.Authenticate(t.Context(), tr); !ok || acc.Scope != oauth.ScopeAdmin {
		t.Fatalf("admin token: %+v", acc)
	}
	// Demoted to editor: the same token is now drafts only.
	editorRole := "editor"
	if _, err := e.st.UpdateUser(t.Context(), au.ID, store.UserPatch{Role: &editorRole}); err != nil {
		t.Fatal(err)
	}
	if acc, ok := e.as.Authenticate(t.Context(), tr); !ok || acc.Scope != oauth.ScopeDrafts {
		t.Fatalf("demoted admin's token must be capped to drafts: %+v %v", acc, ok)
	}
	// Disabled: nothing.
	yes := true
	if _, err := e.st.UpdateUser(t.Context(), au.ID, store.UserPatch{Disabled: &yes}); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.as.Authenticate(t.Context(), tr); ok {
		t.Fatal("disabled member's app must stop working")
	}
}

func (e *teamEnv) token(code, clientID, verifier string) string {
	e.t.Helper()
	f := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {appRedirect}, "client_id": {clientID}, "code_verifier": {verifier}}
	resp, err := http.PostForm(e.srv.URL+"/oauth/token", f)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	tok, _ := out["access_token"].(string)
	if tok == "" {
		e.t.Fatalf("token: %d %v", resp.StatusCode, out)
	}
	return tok
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}
