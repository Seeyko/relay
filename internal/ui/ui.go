// Package ui serves Relay's dashboard: a week and day calendar of posts, the drafts, connected apps and the
// team, behind passkey or email login.
//
// Security model:
//   - Members sign in with a WebAuthn passkey (user verification required) or with a single-use sign-in link
//     sent by email (see internal/team). A passkey signs in the member it was registered for.
//   - A signed-in member adds passkeys for themselves. Without a session, adding a passkey requires the admin
//     API token and registers it for the first owner (bootstrap).
//   - A session is a random 256-bit cookie (__Host- prefix, Secure, HttpOnly, SameSite=Strict); only its SHA-256
//     hash is stored. Sessions expire after SessionTTL. The member's role is read on every request.
//   - State-changing calls are JSON POSTs checked against the expected Origin.
//   - Strict CSP (no inline script), no framing, no caching of API responses.
//   - Roles: viewers and editors read; owners and admins also manage the team, revoke any app and connect or
//     reconnect social accounts with each network's sign-in (internal/connect). The dashboard never publishes,
//     approves, edits or replies.
package ui

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/Seeyko/relay/internal/connect"
	"github.com/Seeyko/relay/internal/core"
	"github.com/Seeyko/relay/internal/oauth"
	"github.com/Seeyko/relay/internal/store"
	"github.com/Seeyko/relay/internal/team"
)

//go:embed static
var static embed.FS

const (
	SessionTTL     = 12 * time.Hour
	ceremonyTTL    = 3 * time.Minute
	maxCeremonies  = 256
	cookieSession  = "__Host-relay_session"
	cookieCeremony = "__Host-relay_ceremony"
)

type UI struct {
	Store *store.Store
	// Svc (optional) runs the pre-flight checks of the preview with the instance settings (timezone, media
	// URLs). Without it, a bare service over Store is used.
	Svc      *core.Service
	APIToken string // registers a passkey for the first owner without a session (bootstrap)
	// RegistrationClosed refuses new passkeys (RELAY_PASSKEY_REGISTRATION=off), even with the API token.
	RegistrationClosed bool
	// Apps shows the apps connected with OAuth (list and revoke). Set when OAuth is on.
	Apps bool
	// Team (optional) manages members: the Team sheet, invitations and, when email is configured, sign-in links.
	Team *team.Team
	// Connect (optional) signs accounts in with each network ("Connect" / "Reconnect" in the Accounts sheet).
	Connect *connect.Manager
	Log     *slog.Logger

	origin string // e.g. https://relay.example.com
	wa     *webauthn.WebAuthn
	secure bool // cookies get Secure and the __Host- prefix (false only for http://localhost)

	mu         sync.Mutex
	ceremonies map[string]ceremony
	limiter    map[string][]time.Time
	now        func() time.Time
}

type ceremony struct {
	kind    string // "register" or "login"
	session webauthn.SessionData
	name    string
	userID  string // register: the member the passkey is for
	expires time.Time
}

// New builds the dashboard for baseURL. It needs an https origin (http is accepted only for localhost).
func New(baseURL string, st *store.Store, apiToken string, log *slog.Logger) (*UI, error) {
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || u.Host == "" {
		return nil, errors.New("dashboard: RELAY_BASE_URL must be an absolute URL")
	}
	host := u.Hostname()
	local := host == "localhost" || host == "127.0.0.1"
	if u.Scheme != "https" && !(u.Scheme == "http" && local) {
		return nil, errors.New("dashboard: RELAY_BASE_URL must use https (passkeys need a secure origin)")
	}
	origin := u.Scheme + "://" + u.Host
	wa, err := webauthn.New(&webauthn.Config{
		RPID:          host,
		RPDisplayName: "Relay",
		RPOrigins:     []string{origin},
	})
	if err != nil {
		return nil, err
	}
	return &UI{
		Store: st, APIToken: apiToken, Log: log,
		origin: origin, wa: wa, secure: u.Scheme == "https",
		ceremonies: map[string]ceremony{}, limiter: map[string][]time.Time{}, now: time.Now,
	}, nil
}

// Register mounts the dashboard on mux under /ui/.
func (ui *UI) Register(mux *http.ServeMux) {
	sub, _ := fs.Sub(static, "static")
	files := http.FileServerFS(sub)
	mux.Handle("GET /ui/", ui.headers(http.StripPrefix("/ui/", files)))
	mux.Handle("GET /ui", http.RedirectHandler("/ui/", http.StatusMovedPermanently))
	// Landing pages of the links sent by email: the page itself, the token is posted by its script.
	index := ui.headers(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, sub, "index.html")
	}))
	mux.Handle("GET /ui/accept", index)
	mux.Handle("GET /ui/signin", index)

	mux.Handle("GET /ui/api/me", ui.headers(http.HandlerFunc(ui.me)))
	mux.Handle("POST /ui/api/register/begin", ui.headers(ui.post(ui.registerBegin)))
	mux.Handle("POST /ui/api/register/finish", ui.headers(ui.post(ui.registerFinish)))
	mux.Handle("POST /ui/api/login/begin", ui.headers(ui.post(ui.loginBegin)))
	mux.Handle("POST /ui/api/login/finish", ui.headers(ui.post(ui.loginFinish)))
	mux.Handle("POST /ui/api/logout", ui.headers(ui.post(ui.logout)))
	mux.Handle("GET /ui/api/calendar", ui.headers(ui.authed(ui.calendar)))
	mux.Handle("GET /ui/api/drafts", ui.headers(ui.authed(ui.drafts)))
	mux.Handle("GET /ui/api/posts/{id}", ui.headers(ui.authed(ui.preview)))
	mux.Handle("GET /ui/api/passkeys", ui.headers(ui.authed(ui.passkeys)))
	if ui.Apps {
		mux.Handle("GET /ui/api/apps", ui.headers(ui.authed(ui.apps)))
		mux.Handle("POST /ui/api/apps/{id}/revoke", ui.headers(ui.post(ui.authed(ui.revokeApp).ServeHTTP)))
	}
	if ui.Team != nil {
		ui.registerTeam(mux)
	}
	ui.registerAccounts(mux)
}

// ---------- middleware ----------

func (ui *UI) headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data: blob:; media-src 'self'; connect-src 'self'; manifest-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=()")
		if ui.secure {
			h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		}
		if strings.HasPrefix(r.URL.Path, "/ui/api/") || r.URL.Path == "/ui/accept" || r.URL.Path == "/ui/signin" {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// post accepts only same-origin JSON requests, rate-limited per client address.
func (ui *UI) post(fn http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != ui.origin {
			jsonErr(w, http.StatusForbidden, "bad origin")
			return
		}
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			jsonErr(w, http.StatusUnsupportedMediaType, "JSON only")
			return
		}
		if !ui.allow(clientIP(r)) {
			jsonErr(w, http.StatusTooManyRequests, "too many attempts, wait a minute")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		fn(w, r)
	})
}

type userKey struct{}

// userFrom is the signed-in member of a request that went through authed.
func userFrom(r *http.Request) *store.User {
	u, _ := r.Context().Value(userKey{}).(*store.User)
	return u
}

func (ui *UI) authed(fn http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, _ := ui.sessionUser(r)
		if u == nil {
			jsonErr(w, http.StatusUnauthorized, "login required")
			return
		}
		fn(w, r.WithContext(context.WithValue(r.Context(), userKey{}, u)))
	})
}

func isAdmin(u *store.User) bool {
	return u != nil && (u.Role == store.RoleOwner || u.Role == store.RoleAdmin)
}

// admin is authed for owners and admins only.
func (ui *UI) admin(fn http.HandlerFunc) http.Handler {
	return ui.authed(func(w http.ResponseWriter, r *http.Request) {
		if !isAdmin(userFrom(r)) {
			jsonErr(w, http.StatusForbidden, "owners and admins only")
			return
		}
		fn(w, r)
	})
}

// allow is a sliding-window limit: 20 auth calls per minute per client address.
func (ui *UI) allow(ip string) bool {
	ui.mu.Lock()
	defer ui.mu.Unlock()
	now := ui.now()
	cut := now.Add(-time.Minute)
	if len(ui.limiter) > 4096 {
		ui.limiter = map[string][]time.Time{}
	}
	kept := ui.limiter[ip][:0]
	for _, t := range ui.limiter[ip] {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= 20 {
		ui.limiter[ip] = kept
		return false
	}
	ui.limiter[ip] = append(kept, now)
	return true
}

func clientIP(r *http.Request) string {
	// Behind a reverse proxy (Traefik), the last X-Forwarded-For hop is the one the proxy saw.
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		return strings.TrimSpace(parts[len(parts)-1])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ---------- sessions ----------

func hashToken(tok string) string {
	h := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(h[:])
}

func randomToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func (ui *UI) cookieName(base string) string {
	if ui.secure {
		return base
	}
	return strings.TrimPrefix(base, "__Host-")
}

func (ui *UI) setCookie(w http.ResponseWriter, name, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: ui.cookieName(name), Value: value, Path: "/", MaxAge: maxAge,
		Secure: ui.secure, HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
}

// sessionUser returns the active member of the session cookie of r and the session's hash.
func (ui *UI) sessionUser(r *http.Request) (*store.User, string) {
	c, err := r.Cookie(ui.cookieName(cookieSession))
	if err != nil || len(c.Value) < 40 {
		return nil, ""
	}
	h := hashToken(c.Value)
	u, err := ui.Store.SessionUser(r.Context(), h)
	if err != nil {
		return nil, ""
	}
	return u, h
}

// startSession signs u in: a new session cookie. method is passkey or email.
func (ui *UI) startSession(w http.ResponseWriter, r *http.Request, u *store.User, passkeyID, method string) error {
	tok := randomToken()
	if err := ui.Store.CreateSession(r.Context(), hashToken(tok), u.ID, passkeyID, method, ui.now().Add(SessionTTL)); err != nil {
		return err
	}
	ui.setCookie(w, cookieSession, tok, int(SessionTTL.Seconds()))
	if ui.Team != nil {
		ui.Team.Audit(r.Context(), team.ActorFor(u, clientIP(r)), "login", u.Email, "method="+method)
	} else {
		ui.Log.Info("dashboard: login", "user", u.ID, "method", method, "ip", clientIP(r))
	}
	return nil
}

type meUser struct {
	ID       string `json:"id"`
	Email    string `json:"email"`
	Name     string `json:"name"`
	Role     string `json:"role"`
	Passkeys int    `json:"passkeys"`
}

func (ui *UI) me(w http.ResponseWriter, r *http.Request) {
	n := 0
	if pks, err := ui.Store.ListPasskeys(r.Context()); err == nil {
		n = len(pks)
	}
	out := map[string]any{"logged_in": false, "has_passkeys": n > 0, "registration_open": !ui.RegistrationClosed, "apps": ui.Apps,
		"email": ui.Team.EmailOn(), "team": false}
	if u, _ := ui.sessionUser(r); u != nil {
		out["logged_in"] = true
		out["user"] = meUser{ID: u.ID, Email: u.Email, Name: u.Name, Role: u.Role, Passkeys: u.Passkeys}
		out["team"] = ui.Team != nil && isAdmin(u)
		out["accounts"] = isAdmin(u)
	}
	writeJSON(w, 200, out)
}

func (ui *UI) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(ui.cookieName(cookieSession)); err == nil {
		_ = ui.Store.DeleteSession(r.Context(), hashToken(c.Value))
	}
	ui.setCookie(w, cookieSession, "", -1)
	w.WriteHeader(http.StatusNoContent)
}

// ---------- WebAuthn ceremonies ----------

// waUser is one member as WebAuthn sees it. Its handle is opaque (no personal data): the user id, or
// store.LegacyOwnerHandle for the first owner.
type waUser struct {
	u     *store.User
	creds []webauthn.Credential
	byID  map[string]*store.Passkey
}

// ownerID is the handle of the passkeys registered before team members (and of the first owner).
var ownerID = []byte(store.LegacyOwnerHandle)

func (w *waUser) WebAuthnID() []byte                         { return []byte(w.u.WebAuthnID) }
func (w *waUser) WebAuthnName() string                       { return "relay" }
func (w *waUser) WebAuthnDisplayName() string                { return "Relay" }
func (w *waUser) WebAuthnCredentials() []webauthn.Credential { return w.creds }

func (ui *UI) loadWAUser(ctx context.Context, u *store.User) (*waUser, error) {
	pks, err := ui.Store.ListPasskeys(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	w := &waUser{u: u, byID: map[string]*store.Passkey{}}
	for _, p := range pks {
		var c webauthn.Credential
		if err := json.Unmarshal([]byte(p.Credential), &c); err != nil {
			continue
		}
		w.creds = append(w.creds, c)
		w.byID[p.ID] = p
	}
	return w, nil
}

func (ui *UI) startCeremony(w http.ResponseWriter, c ceremony) bool {
	ui.mu.Lock()
	defer ui.mu.Unlock()
	now := ui.now()
	for k, v := range ui.ceremonies {
		if now.After(v.expires) {
			delete(ui.ceremonies, k)
		}
	}
	if len(ui.ceremonies) >= maxCeremonies {
		return false
	}
	id := randomToken()
	c.expires = now.Add(ceremonyTTL)
	ui.ceremonies[id] = c
	ui.setCookie(w, cookieCeremony, id, int(ceremonyTTL.Seconds()))
	return true
}

// takeCeremony returns and removes the pending ceremony: each challenge is usable once.
func (ui *UI) takeCeremony(w http.ResponseWriter, r *http.Request, kind string) (ceremony, bool) {
	c, err := r.Cookie(ui.cookieName(cookieCeremony))
	ui.setCookie(w, cookieCeremony, "", -1)
	if err != nil {
		return ceremony{}, false
	}
	ui.mu.Lock()
	defer ui.mu.Unlock()
	cer, ok := ui.ceremonies[c.Value]
	delete(ui.ceremonies, c.Value)
	if !ok || cer.kind != kind || ui.now().After(cer.expires) {
		return ceremony{}, false
	}
	return cer, true
}

func (ui *UI) tokenOK(r *http.Request) bool {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ui.APIToken != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(ui.APIToken)) == 1
}

// registrant is who a new passkey is for: the signed-in member, or the first owner with the API token (created
// on a fresh instance).
func (ui *UI) registrant(r *http.Request) (*store.User, error) {
	if u, _ := ui.sessionUser(r); u != nil {
		return u, nil
	}
	if !ui.tokenOK(r) {
		return nil, nil
	}
	u, err := ui.Store.FirstOwner(r.Context())
	if errors.Is(err, store.ErrNotFound) {
		return ui.Store.CreateUser(r.Context(), &store.User{Role: store.RoleOwner, WebAuthnID: store.LegacyOwnerHandle})
	}
	return u, err
}

func (ui *UI) registerBegin(w http.ResponseWriter, r *http.Request) {
	if ui.RegistrationClosed {
		jsonErr(w, http.StatusForbidden, "adding passkeys is disabled (RELAY_PASSKEY_REGISTRATION=off)")
		return
	}
	u, err := ui.registrant(r)
	if err != nil {
		jsonErr(w, 500, "storage error")
		return
	}
	if u == nil {
		ui.Log.Warn("dashboard: passkey registration refused (no session, bad API token)", "ip", clientIP(r))
		jsonErr(w, http.StatusUnauthorized, "sign in, or give the admin API token, to add a passkey")
		return
	}
	var in struct {
		Name string `json:"name"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	name := strings.TrimSpace(in.Name)
	if len(name) > 60 {
		name = name[:60]
	}
	if name == "" {
		name = "passkey"
	}
	wu, err := ui.loadWAUser(r.Context(), u)
	if err != nil {
		jsonErr(w, 500, "storage error")
		return
	}
	creation, sess, err := ui.wa.BeginRegistration(wu,
		webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementRequired),
		webauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{
			ResidentKey:      protocol.ResidentKeyRequirementRequired,
			UserVerification: protocol.VerificationRequired,
		}),
		webauthn.WithExclusions(webauthn.Credentials(wu.creds).CredentialDescriptors()),
	)
	if err != nil {
		jsonErr(w, 500, "could not start registration")
		return
	}
	if !ui.startCeremony(w, ceremony{kind: "register", session: *sess, name: name, userID: u.ID}) {
		jsonErr(w, http.StatusTooManyRequests, "too many pending logins, retry in a few minutes")
		return
	}
	writeJSON(w, 200, creation)
}

func (ui *UI) registerFinish(w http.ResponseWriter, r *http.Request) {
	if ui.RegistrationClosed {
		jsonErr(w, http.StatusForbidden, "adding passkeys is disabled (RELAY_PASSKEY_REGISTRATION=off)")
		return
	}
	u, err := ui.registrant(r)
	if err != nil {
		jsonErr(w, 500, "storage error")
		return
	}
	if u == nil {
		jsonErr(w, http.StatusUnauthorized, "sign in, or give the admin API token, to add a passkey")
		return
	}
	cer, ok := ui.takeCeremony(w, r, "register")
	if !ok || cer.userID != u.ID { // the ceremony was started for this very member
		jsonErr(w, http.StatusBadRequest, "registration expired, start again")
		return
	}
	wu, err := ui.loadWAUser(r.Context(), u)
	if err != nil {
		jsonErr(w, 500, "storage error")
		return
	}
	cred, err := ui.wa.FinishRegistration(wu, cer.session, r)
	if err != nil {
		ui.Log.Warn("dashboard: passkey registration failed", "err", err)
		jsonErr(w, http.StatusBadRequest, "passkey rejected")
		return
	}
	raw, _ := json.Marshal(cred)
	pk := &store.Passkey{ID: base64.RawURLEncoding.EncodeToString(cred.ID), UserID: u.ID, Name: cer.name, Credential: string(raw)}
	if err := ui.Store.AddPasskey(r.Context(), pk); err != nil {
		jsonErr(w, 500, "could not save the passkey")
		return
	}
	if ui.Team != nil {
		ui.Team.Audit(r.Context(), team.ActorFor(u, clientIP(r)), "passkey.add", u.Email, pk.Name)
	} else {
		ui.Log.Info("dashboard: passkey added", "name", pk.Name)
	}
	writeJSON(w, 200, map[string]any{"ok": true, "name": pk.Name})
}

func (ui *UI) loginBegin(w http.ResponseWriter, r *http.Request) {
	assertion, sess, err := ui.wa.BeginDiscoverableLogin(webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		jsonErr(w, 500, "could not start login")
		return
	}
	if !ui.startCeremony(w, ceremony{kind: "login", session: *sess}) {
		jsonErr(w, http.StatusTooManyRequests, "too many pending logins, retry in a few minutes")
		return
	}
	writeJSON(w, 200, assertion)
}

func (ui *UI) loginFinish(w http.ResponseWriter, r *http.Request) {
	cer, ok := ui.takeCeremony(w, r, "login")
	if !ok {
		jsonErr(w, http.StatusBadRequest, "login expired, start again")
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "unreadable body")
		return
	}
	u, pk, err := ui.verifyAssertion(r.Context(), cer.session, body, nil)
	if errors.Is(err, errStorage) {
		jsonErr(w, 500, "storage error")
		return
	}
	if err != nil {
		ui.Log.Warn("dashboard: login failed", "ip", clientIP(r), "err", err)
		jsonErr(w, http.StatusUnauthorized, "login failed")
		return
	}
	if err := ui.startSession(w, r, u, pk.ID, "passkey"); err != nil {
		jsonErr(w, 500, "storage error")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

var errStorage = errors.New("storage error")

// verifyAssertion checks a user-verified assertion against the ceremony session and stores the new sign count.
// With only (a member), the passkey must be one of that member's; otherwise the passkey says who signs in.
func (ui *UI) verifyAssertion(ctx context.Context, sess webauthn.SessionData, body []byte, only *store.User) (*store.User, *store.Passkey, error) {
	parsed, err := protocol.ParseCredentialRequestResponseBytes(body)
	if err != nil {
		return nil, nil, err
	}
	var wu *waUser
	handler := func(rawID, userHandle []byte) (webauthn.User, error) {
		pks, err := ui.Store.ListPasskeys(ctx)
		if err != nil {
			return nil, errStorage
		}
		id := base64.RawURLEncoding.EncodeToString(rawID)
		var owner string
		for _, p := range pks {
			if p.ID == id {
				owner = p.UserID
			}
		}
		if owner == "" {
			return nil, errors.New("unknown passkey")
		}
		u, err := ui.Store.GetUser(ctx, owner)
		if err != nil {
			return nil, errors.New("unknown user")
		}
		if u.Disabled {
			return nil, errors.New("member disabled")
		}
		if only != nil && u.ID != only.ID {
			return nil, errors.New("passkey of another member")
		}
		if subtle.ConstantTimeCompare(userHandle, []byte(u.WebAuthnID)) != 1 {
			return nil, errors.New("unknown user")
		}
		if wu, err = ui.loadWAUser(ctx, u); err != nil {
			return nil, errStorage
		}
		return wu, nil
	}
	cred, err := ui.wa.ValidateDiscoverableLogin(handler, sess, parsed)
	if err != nil {
		if errors.Is(err, errStorage) {
			return nil, nil, errStorage
		}
		return nil, nil, err
	}
	if cred.Authenticator.CloneWarning {
		return nil, nil, errors.New("passkey sign counter went backwards (possible clone)")
	}
	if !cred.Flags.UserVerified {
		return nil, nil, errors.New("user verification missing")
	}
	id := base64.RawURLEncoding.EncodeToString(cred.ID)
	pk, ok := wu.byID[id]
	if !ok {
		return nil, nil, errors.New("unknown passkey")
	}
	raw, _ := json.Marshal(cred)
	if err := ui.Store.TouchPasskey(ctx, id, string(raw)); err != nil {
		return nil, nil, errStorage
	}
	return wu.u, pk, nil
}

// ---------- OAuth consent (implements oauth.Passkeys) ----------

// Session returns the hash of the valid dashboard session cookie of r and its member.
func (ui *UI) Session(r *http.Request) (string, *oauth.Member, bool) {
	u, h := ui.sessionUser(r)
	if u == nil {
		return "", nil, false
	}
	return h, &oauth.Member{ID: u.ID, Email: u.Email, Name: u.Name, Role: u.Role, HasPasskey: u.Passkeys > 0}, true
}

// LoginMethods tells the signed-out consent page how it can sign in.
func (ui *UI) LoginMethods(ctx context.Context) (passkey, email bool) {
	pks, err := ui.Store.ListPasskeys(ctx)
	return err == nil && len(pks) > 0, ui.Team.EmailOn()
}

// BeginAssertion starts a fresh passkey check of the member (user verification required). The state lives
// with the caller.
func (ui *UI) BeginAssertion(ctx context.Context, userID string) (any, any, error) {
	assertion, sess, err := ui.wa.BeginDiscoverableLogin(webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		return nil, nil, err
	}
	return assertion, &assertionState{session: *sess, userID: userID, expires: ui.now().Add(ceremonyTTL)}, nil
}

type assertionState struct {
	session webauthn.SessionData
	userID  string
	expires time.Time
}

// FinishAssertion verifies the answer to BeginAssertion: a passkey of that same member.
func (ui *UI) FinishAssertion(ctx context.Context, userID string, state any, credential []byte) error {
	st, ok := state.(*assertionState)
	if !ok || st == nil {
		return errors.New("no passkey check in progress")
	}
	if ui.now().After(st.expires) {
		return errors.New("passkey check expired")
	}
	if st.userID != userID {
		return errors.New("passkey check of another member")
	}
	u, err := ui.Store.GetUser(ctx, userID)
	if err != nil {
		return err
	}
	_, pk, err := ui.verifyAssertion(ctx, st.session, credential, u)
	if err != nil {
		return err
	}
	ui.Log.Info("dashboard: passkey check for an app approval", "passkey", pk.Name, "user", u.ID)
	return nil
}

// EmailCodes reports whether the fresh check can be a code sent by email.
func (ui *UI) EmailCodes() bool { return ui.Team.EmailOn() }

func (ui *UI) SendCode(ctx context.Context, userID, app, lang, ip string) error {
	if !ui.Team.EmailOn() {
		return team.ErrNoEmail
	}
	u, err := ui.Store.GetUser(ctx, userID)
	if err != nil {
		return err
	}
	return ui.Team.SendCode(ctx, u, app, lang, ip)
}

func (ui *UI) CheckCode(ctx context.Context, userID, code string) error {
	if !ui.Team.EmailOn() {
		return team.ErrNoEmail
	}
	return ui.Team.CheckCode(ctx, userID, code)
}

// ---------- connected apps (OAuth grants) ----------

func (ui *UI) apps(w http.ResponseWriter, r *http.Request) {
	gs, err := ui.Store.ListOAuthGrants(r.Context())
	if err != nil {
		jsonErr(w, 500, "storage error")
		return
	}
	u := userFrom(r)
	type app struct {
		*store.OAuthGrant
		CanRevoke bool `json:"can_revoke"`
	}
	out := make([]app, 0, len(gs))
	for _, g := range gs {
		out = append(out, app{g, isAdmin(u) || g.UserID == u.ID})
	}
	writeJSON(w, 200, map[string]any{"apps": out})
}

// revokeApp: owners and admins revoke any app, other members the apps they approved.
func (ui *UI) revokeApp(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	g, err := ui.Store.GetOAuthGrant(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		jsonErr(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		jsonErr(w, 500, "storage error")
		return
	}
	if !isAdmin(u) && g.UserID != u.ID {
		jsonErr(w, http.StatusForbidden, "owners and admins only")
		return
	}
	if err := ui.Store.DeleteOAuthGrant(r.Context(), g.ID); err != nil {
		jsonErr(w, 500, "storage error")
		return
	}
	if ui.Team != nil {
		ui.Team.Audit(r.Context(), team.ActorFor(u, clientIP(r)), "app.revoke", g.ClientName, g.ID)
	} else {
		ui.Log.Info("dashboard: app revoked", "grant", g.ID, "ip", clientIP(r))
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------- read-only data ----------

type calMedia struct {
	URL  string `json:"url"`
	Mime string `json:"mime"`
}

type calPost struct {
	ID      string     `json:"id"`
	GroupID string     `json:"group_id"`
	Status  string     `json:"status"`
	At      *time.Time `json:"at"`
	// Planned is true for a draft placed at its planned time (it will not publish by itself).
	Planned   bool       `json:"planned,omitempty"`
	Provider  string     `json:"provider"`
	Account   string     `json:"account"`
	Handle    string     `json:"handle"`
	Parts     []string   `json:"parts"`
	Media     []calMedia `json:"media"`
	Title     string     `json:"title,omitempty"`
	Kind      string     `json:"kind,omitempty"`
	RemoteURL string     `json:"remote_url,omitempty"`
	Error     string     `json:"error,omitempty"`
	Source    string     `json:"source"`
	// Stats: normalized counters of a published post (views, likes, comments...).
	Stats map[string]float64 `json:"stats,omitempty"`
}

func (ui *UI) calendar(w http.ResponseWriter, r *http.Request) {
	from, err1 := time.Parse(time.RFC3339, r.URL.Query().Get("from"))
	to, err2 := time.Parse(time.RFC3339, r.URL.Query().Get("to"))
	if err1 != nil || err2 != nil || !to.After(from) || to.Sub(from) > 45*24*time.Hour {
		jsonErr(w, http.StatusBadRequest, "from and to must be RFC 3339, at most 45 days apart")
		return
	}
	ctx := r.Context()
	accs, err := ui.Store.ListAccounts(ctx)
	if err != nil {
		jsonErr(w, 500, "storage error")
		return
	}
	byAcc := map[string]*store.Account{}
	for _, a := range accs {
		byAcc[a.ID] = a
	}
	posts, err := ui.Store.ListPosts(ctx, store.PostFilter{From: &from, To: &to, Limit: 1000})
	if err != nil {
		jsonErr(w, 500, "storage error")
		return
	}
	mediaCache := map[string]*store.Media{}
	out := make([]calPost, 0, len(posts))
	for _, p := range posts {
		cp := calPost{ID: p.ID, GroupID: p.GroupID, Status: p.Status, Parts: p.Parts, RemoteURL: p.RemoteURL, Error: p.Error, Source: p.Source, Media: []calMedia{}}
		switch {
		case p.PublishedAt != nil:
			cp.At = p.PublishedAt
		case p.ScheduledAt != nil:
			cp.At = p.ScheduledAt
		case p.PlannedAt != nil:
			cp.At, cp.Planned = p.PlannedAt, true
		case p.Status == store.StatusDraft:
			continue // a draft without a planned time has no place in the calendar: see the Drafts view
		default:
			t := p.CreatedAt
			cp.At = &t
		}
		if a := byAcc[p.AccountID]; a != nil {
			cp.Provider, cp.Account, cp.Handle = a.Provider, a.Name, a.Handle
		}
		if s, ok := p.Options["title"].(string); ok {
			cp.Title = s
		}
		if s, ok := p.Options["kind"].(string); ok {
			cp.Kind = s
		}
		if p.Status == store.StatusPublished {
			cp.Stats = core.PublicStats(p.Metrics)
		}
		for _, id := range p.Media {
			m, ok := mediaCache[id]
			if !ok {
				m, _ = ui.Store.GetMedia(ctx, id)
				mediaCache[id] = m
			}
			if m != nil {
				cp.Media = append(cp.Media, calMedia{URL: "/media/" + m.Key(), Mime: m.Mime})
			}
		}
		out = append(out, cp)
	}
	writeJSON(w, 200, map[string]any{"posts": out})
}

func (ui *UI) svc() *core.Service {
	if ui.Svc != nil {
		return ui.Svc
	}
	return &core.Service{Store: ui.Store}
}

type draftPost struct {
	ID       string    `json:"id"`
	Provider string    `json:"provider"`
	Account  string    `json:"account"`
	Handle   string    `json:"handle"`
	Text     string    `json:"text"`
	Parts    int       `json:"parts"`
	Media    *calMedia `json:"media,omitempty"` // first media, as a thumbnail
	Count    int       `json:"media_count"`
	Problems int       `json:"problems"`
}

type draftGroup struct {
	GroupID   string      `json:"group_id"`
	PlannedAt *time.Time  `json:"planned_at,omitempty"`
	CreatedAt time.Time   `json:"created_at"`
	UpdatedAt time.Time   `json:"updated_at"`
	Title     string      `json:"title,omitempty"`
	Posts     []draftPost `json:"posts"`
}

// drafts lists every draft, dated or not, one card per group (one schedule_post call), newest first.
func (ui *UI) drafts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	posts, err := ui.Store.ListPosts(ctx, store.PostFilter{Status: store.StatusDraft, Limit: 1000})
	if err != nil {
		jsonErr(w, 500, "storage error")
		return
	}
	accs, err := ui.Store.ListAccounts(ctx)
	if err != nil {
		jsonErr(w, 500, "storage error")
		return
	}
	byAcc := map[string]*store.Account{}
	for _, a := range accs {
		byAcc[a.ID] = a
	}
	svc := ui.svc()
	groups := map[string]*draftGroup{}
	var order []*draftGroup
	for _, p := range posts {
		g := groups[p.GroupID]
		if g == nil {
			g = &draftGroup{GroupID: p.GroupID, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt, Posts: []draftPost{}}
			groups[p.GroupID] = g
			order = append(order, g)
		}
		if p.CreatedAt.After(g.CreatedAt) {
			g.CreatedAt = p.CreatedAt
		}
		if p.UpdatedAt.After(g.UpdatedAt) {
			g.UpdatedAt = p.UpdatedAt
		}
		if p.PlannedAt != nil && (g.PlannedAt == nil || p.PlannedAt.Before(*g.PlannedAt)) {
			g.PlannedAt = p.PlannedAt
		}
		if t, ok := p.Options["title"].(string); ok && g.Title == "" {
			g.Title = t
		}
		a := byAcc[p.AccountID]
		pv := svc.PreviewPost(ctx, p, a)
		dp := draftPost{ID: p.ID, Parts: len(p.Parts), Count: len(p.Media), Problems: len(pv.Problems)}
		if len(p.Parts) > 0 {
			dp.Text = p.Parts[0]
		}
		if a != nil {
			dp.Provider, dp.Account, dp.Handle = a.Provider, a.Name, a.Handle
		}
		for _, m := range pv.Media {
			if !m.Missing {
				dp.Media = &calMedia{URL: m.Path, Mime: m.Mime}
				break
			}
		}
		g.Posts = append(g.Posts, dp)
	}
	sort.SliceStable(order, func(i, j int) bool { return order[i].CreatedAt.After(order[j].CreatedAt) })
	if order == nil {
		order = []*draftGroup{}
	}
	writeJSON(w, 200, map[string]any{"groups": order})
}

// preview returns the full preview of the group of one post: every account, text, media, options, problems.
func (ui *UI) preview(w http.ResponseWriter, r *http.Request) {
	pv, err := ui.svc().PreviewGroup(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		jsonErr(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		jsonErr(w, 500, "storage error")
		return
	}
	writeJSON(w, 200, pv)
}

func (ui *UI) passkeys(w http.ResponseWriter, r *http.Request) {
	pks, err := ui.Store.ListPasskeys(r.Context(), userFrom(r).ID)
	if err != nil {
		jsonErr(w, 500, "storage error")
		return
	}
	writeJSON(w, 200, map[string]any{"passkeys": pks})
}

// ---------- helpers ----------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func jsonErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
