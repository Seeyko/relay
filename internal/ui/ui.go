// Package ui serves Relay's read-only dashboard: a week and day calendar of posts, behind passkey login.
//
// Security model:
//   - Login is a WebAuthn passkey only (no password). User verification (PIN or biometrics) is required.
//   - Adding a passkey requires the admin API token, so the dashboard grants nothing the token holder doesn't have.
//   - A session is a random 256-bit cookie (__Host- prefix, Secure, HttpOnly, SameSite=Strict); only its SHA-256
//     hash is stored. Sessions expire after SessionTTL.
//   - State-changing calls are JSON POSTs checked against the expected Origin.
//   - Strict CSP (no inline script), no framing, no caching of API responses.
//   - The dashboard is read-only: it cannot publish, edit or reply.
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
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/Seeyko/relay/internal/store"
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
	Store    *store.Store
	APIToken string // required to register a passkey
	Log      *slog.Logger

	RegistrationOff bool

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

	mux.Handle("GET /ui/api/me", ui.headers(http.HandlerFunc(ui.me)))
	mux.Handle("POST /ui/api/register/begin", ui.headers(ui.post(ui.registerBegin)))
	mux.Handle("POST /ui/api/register/finish", ui.headers(ui.post(ui.registerFinish)))
	mux.Handle("POST /ui/api/login/begin", ui.headers(ui.post(ui.loginBegin)))
	mux.Handle("POST /ui/api/login/finish", ui.headers(ui.post(ui.loginFinish)))
	mux.Handle("POST /ui/api/logout", ui.headers(ui.post(ui.logout)))
	mux.Handle("GET /ui/api/calendar", ui.headers(ui.authed(ui.calendar)))
	mux.Handle("GET /ui/api/passkeys", ui.headers(ui.authed(ui.passkeys)))
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
		if strings.HasPrefix(r.URL.Path, "/ui/api/") {
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

func (ui *UI) authed(fn http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !ui.loggedIn(r) {
			jsonErr(w, http.StatusUnauthorized, "login required")
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

func (ui *UI) loggedIn(r *http.Request) bool {
	c, err := r.Cookie(ui.cookieName(cookieSession))
	if err != nil || len(c.Value) < 40 {
		return false
	}
	ok, err := ui.Store.UISessionValid(r.Context(), hashToken(c.Value))
	return err == nil && ok
}

func (ui *UI) me(w http.ResponseWriter, r *http.Request) {
	n := 0
	if pks, err := ui.Store.ListPasskeys(r.Context()); err == nil {
		n = len(pks)
	}
	writeJSON(w, 200, map[string]any{"logged_in": ui.loggedIn(r), "has_passkeys": n > 0, "can_register": !ui.RegistrationOff})
}

func (ui *UI) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(ui.cookieName(cookieSession)); err == nil {
		_ = ui.Store.DeleteUISession(r.Context(), hashToken(c.Value))
	}
	ui.setCookie(w, cookieSession, "", -1)
	w.WriteHeader(http.StatusNoContent)
}

// ---------- WebAuthn ceremonies ----------

// owner is the single dashboard user. Its handle is a fixed opaque value (no personal data).
type owner struct{ creds []webauthn.Credential }

var ownerID = []byte("relay-owner")

func (o *owner) WebAuthnID() []byte                         { return ownerID }
func (o *owner) WebAuthnName() string                       { return "relay" }
func (o *owner) WebAuthnDisplayName() string                { return "Relay owner" }
func (o *owner) WebAuthnCredentials() []webauthn.Credential { return o.creds }

func (ui *UI) loadOwner(ctx context.Context) (*owner, map[string]*store.Passkey, error) {
	pks, err := ui.Store.ListPasskeys(ctx)
	if err != nil {
		return nil, nil, err
	}
	o := &owner{}
	byID := map[string]*store.Passkey{}
	for _, p := range pks {
		var c webauthn.Credential
		if err := json.Unmarshal([]byte(p.Credential), &c); err != nil {
			continue
		}
		o.creds = append(o.creds, c)
		byID[p.ID] = p
	}
	return o, byID, nil
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

func (ui *UI) registerBegin(w http.ResponseWriter, r *http.Request) {
	if ui.RegistrationOff {
		jsonErr(w, http.StatusForbidden, "adding passkeys is disabled (RELAY_PASSKEY_REGISTRATION=off)")
		return
	}
	if !ui.tokenOK(r) {
		ui.Log.Warn("dashboard: passkey registration refused (bad API token)", "ip", clientIP(r))
		jsonErr(w, http.StatusUnauthorized, "the admin API token is required to add a passkey")
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
	o, _, err := ui.loadOwner(r.Context())
	if err != nil {
		jsonErr(w, 500, "storage error")
		return
	}
	creation, sess, err := ui.wa.BeginRegistration(o,
		webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementRequired),
		webauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{
			ResidentKey:      protocol.ResidentKeyRequirementRequired,
			UserVerification: protocol.VerificationRequired,
		}),
		webauthn.WithExclusions(webauthn.Credentials(o.creds).CredentialDescriptors()),
	)
	if err != nil {
		jsonErr(w, 500, "could not start registration")
		return
	}
	if !ui.startCeremony(w, ceremony{kind: "register", session: *sess, name: name}) {
		jsonErr(w, http.StatusTooManyRequests, "too many pending logins, retry in a few minutes")
		return
	}
	writeJSON(w, 200, creation)
}

func (ui *UI) registerFinish(w http.ResponseWriter, r *http.Request) {
	if ui.RegistrationOff {
		jsonErr(w, http.StatusForbidden, "adding passkeys is disabled (RELAY_PASSKEY_REGISTRATION=off)")
		return
	}
	if !ui.tokenOK(r) {
		jsonErr(w, http.StatusUnauthorized, "the admin API token is required to add a passkey")
		return
	}
	cer, ok := ui.takeCeremony(w, r, "register")
	if !ok {
		jsonErr(w, http.StatusBadRequest, "registration expired, start again")
		return
	}
	o, _, err := ui.loadOwner(r.Context())
	if err != nil {
		jsonErr(w, 500, "storage error")
		return
	}
	cred, err := ui.wa.FinishRegistration(o, cer.session, r)
	if err != nil {
		ui.Log.Warn("dashboard: passkey registration failed", "err", err)
		jsonErr(w, http.StatusBadRequest, "passkey rejected")
		return
	}
	raw, _ := json.Marshal(cred)
	pk := &store.Passkey{ID: base64.RawURLEncoding.EncodeToString(cred.ID), Name: cer.name, Credential: string(raw)}
	if err := ui.Store.AddPasskey(r.Context(), pk); err != nil {
		jsonErr(w, 500, "could not save the passkey")
		return
	}
	ui.Log.Info("dashboard: passkey added", "name", pk.Name)
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
	o, byID, err := ui.loadOwner(r.Context())
	if err != nil || len(o.creds) == 0 {
		jsonErr(w, http.StatusUnauthorized, "no passkey registered")
		return
	}
	handler := func(rawID, userHandle []byte) (webauthn.User, error) {
		if subtle.ConstantTimeCompare(userHandle, ownerID) != 1 {
			return nil, errors.New("unknown user")
		}
		return o, nil
	}
	cred, err := ui.wa.FinishDiscoverableLogin(handler, cer.session, r)
	if err != nil {
		ui.Log.Warn("dashboard: login failed", "ip", clientIP(r), "err", err)
		jsonErr(w, http.StatusUnauthorized, "login failed")
		return
	}
	if cred.Authenticator.CloneWarning {
		ui.Log.Warn("dashboard: login refused, passkey sign counter went backwards (possible clone)", "ip", clientIP(r))
		jsonErr(w, http.StatusUnauthorized, "login failed")
		return
	}
	id := base64.RawURLEncoding.EncodeToString(cred.ID)
	if _, ok := byID[id]; !ok {
		jsonErr(w, http.StatusUnauthorized, "login failed")
		return
	}
	raw, _ := json.Marshal(cred)
	if err := ui.Store.TouchPasskey(r.Context(), id, string(raw)); err != nil {
		jsonErr(w, 500, "storage error")
		return
	}
	tok := randomToken()
	if err := ui.Store.CreateUISession(r.Context(), hashToken(tok), id, ui.now().Add(SessionTTL)); err != nil {
		jsonErr(w, 500, "storage error")
		return
	}
	ui.setCookie(w, cookieSession, tok, int(SessionTTL.Seconds()))
	ui.Log.Info("dashboard: login", "passkey", byID[id].Name, "ip", clientIP(r))
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---------- read-only data ----------

type calMedia struct {
	URL  string `json:"url"`
	Mime string `json:"mime"`
}

type calPost struct {
	ID        string     `json:"id"`
	GroupID   string     `json:"group_id"`
	Status    string     `json:"status"`
	At        *time.Time `json:"at"`
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

func (ui *UI) passkeys(w http.ResponseWriter, r *http.Request) {
	pks, err := ui.Store.ListPasskeys(r.Context())
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
