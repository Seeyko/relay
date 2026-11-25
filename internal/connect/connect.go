// Package connect signs social accounts in with each network's own consent screen ("Connect with Threads",
// "Reconnect"), instead of pasting tokens.
//
// Flow: an owner or admin asks for a connection (dashboard, MCP or REST) and gets a one-time start link
// (10 minutes). GET /connect/{network}/start opens it: from the dashboard of the member who asked it goes on at
// once, else it shows a "Continue" button (a POST), so link previews in chat apps cannot use it. Relay then sends
// the browser to the network with a random state (single use, 10 minutes), bound to that browser by an HttpOnly
// SameSite=Lax cookie, and a PKCE challenge where the network supports it. GET /connect/{network}/callback
// checks the state and the cookie, exchanges the code (and a long-lived token where there is one), then:
//   - Reconnect: the network must have signed in the same remote account; its credentials are updated in
//     place (core.Service.UpdateCredentials), so posts and history stay attached.
//   - Connect: the account is created (or updated in place when it is already connected). When the network
//     gives several (Facebook Pages, Instagram accounts, LinkedIn organizations), a picker page lists them.
//
// Tokens never appear in URLs, pages or logs: they stay in memory until they are sealed in the store.
package connect

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Seeyko/relay/internal/core"
	"github.com/Seeyko/relay/internal/store"
)

// TTL bounds every step: the start link, the network's consent, the picker.
const TTL = 10 * time.Minute

const (
	cookieBinding = "__Host-relay_connect"
	maxPending    = 256
)

//go:embed static
var static embed.FS

var pages = template.Must(template.ParseFS(static, "static/*.html"))

// Request asks for a connection.
type Request struct {
	// Network is a network slug (threads, facebook, instagram, instagram-login, linkedin, youtube, tiktok, x)
	// or a connector name (linkedin-page...). With AccountID it may be empty.
	Network string
	// AccountID reconnects this account instead of connecting a new one.
	AccountID string
	// UserID is the member who asked ("" for the API token).
	UserID string
	// Dashboard is set when the member asked from the dashboard (the start link then goes on at once).
	Dashboard bool
	// Org is the organization (id, slug or name) a new account goes to. Begin resolves it to an id: the only
	// organization the caller sees when empty, the account's own on a reconnection.
	Org string
}

// Started is the link to open in a browser.
type Started struct {
	URL         string    `json:"url"`
	Network     string    `json:"network"`
	Label       string    `json:"label"`
	AccountID   string    `json:"account_id,omitempty"`
	ExpiresAt   time.Time `json:"expires_at"`
	RedirectURI string    `json:"redirect_uri"`
	Note        string    `json:"note"`
}

// Candidate is one account the network signed in: a Page, an organization, a profile.
type Candidate struct {
	RemoteID string
	Name     string
	Handle   string
	// Creds are the credential fields to store (tokens included). Never rendered.
	Creds map[string]string
}

func (c Candidate) label() string {
	switch {
	case c.Name != "" && c.Handle != "" && c.Handle != c.Name:
		return c.Name + " (@" + c.Handle + ")"
	case c.Name != "":
		return c.Name
	case c.Handle != "":
		return "@" + c.Handle
	}
	return c.RemoteID
}

// ErrInvalid is a caller mistake (unknown network, account of another network...).
type ErrInvalid struct{ Msg string }

func (e *ErrInvalid) Error() string { return e.Msg }

func invalid(format string, a ...any) error { return &ErrInvalid{Msg: fmt.Sprintf(format, a...)} }

// Manager runs the flows. Pending flows live in memory: a restart cancels them.
type Manager struct {
	Svc  *core.Service
	HTTP *http.Client
	Log  *slog.Logger

	base     string // e.g. https://relay.example.com
	secure   bool
	networks map[string]*Network
	order    []string

	mu     sync.Mutex
	starts map[string]*start // by start token hash
	flows  map[string]*flow  // by state (OAuth 2) or request token (OAuth 1)
	picks  map[string]*pick  // by pick id
	now    func() time.Time
}

type start struct {
	net     *Network
	req     Request
	expires time.Time
}

type flow struct {
	net      *Network
	req      Request
	verifier string // PKCE
	secret   string // OAuth 1 request token secret
	binding  string // SHA-256 of the browser cookie
	expires  time.Time
}

type pick struct {
	id      string
	net     *Network
	req     Request
	cands   []Candidate
	binding string
	expires time.Time
}

// New builds the manager for the networks that have app credentials. baseURL must be https (http only for
// localhost). It returns nil when no network is configured.
func New(baseURL string, svc *core.Service, networks []*Network, log *slog.Logger) (*Manager, error) {
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || u.Host == "" {
		return nil, errors.New("connect: RELAY_BASE_URL must be an absolute URL")
	}
	local := u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1"
	if u.Scheme != "https" && !(u.Scheme == "http" && local) {
		return nil, errors.New("connect: RELAY_BASE_URL must use https")
	}
	m := &Manager{
		Svc: svc, HTTP: svc.HTTP, Log: log,
		base: u.Scheme + "://" + u.Host, secure: u.Scheme == "https",
		networks: map[string]*Network{},
		starts:   map[string]*start{}, flows: map[string]*flow{}, picks: map[string]*pick{},
		now: time.Now,
	}
	if m.HTTP == nil {
		m.HTTP = &http.Client{Timeout: time.Minute}
	}
	for _, n := range networks {
		m.networks[n.Slug] = n
		m.order = append(m.order, n.Slug)
	}
	return m, nil
}

// Networks lists the configured networks, in display order.
func (m *Manager) Networks() []*Network {
	out := make([]*Network, 0, len(m.order))
	for _, s := range m.order {
		out = append(out, m.networks[s])
	}
	return out
}

// Network returns a configured network by slug.
func (m *Manager) Network(slug string) *Network { return m.networks[slug] }

// RedirectURI is the callback to register in the network's developer console.
func (m *Manager) RedirectURI(n *Network) string { return m.base + "/connect/" + n.Slug + "/callback" }

// ReconnectNetwork is the network that reconnects acc ("" when none is configured).
func (m *Manager) ReconnectNetwork(ctx context.Context, acc *store.Account) string {
	n, err := m.forAccount(ctx, acc, "")
	if err != nil {
		return ""
	}
	return n.Slug
}

// forAccount picks the network that reconnects acc. Instagram accounts connected with Facebook Login go back
// through Facebook, the others through Instagram Login; either works when only one is configured.
func (m *Manager) forAccount(ctx context.Context, acc *store.Account, want string) (*Network, error) {
	var fits []*Network
	for _, n := range m.Networks() {
		if n.Connector == acc.Provider {
			fits = append(fits, n)
		}
	}
	if len(fits) == 0 {
		return nil, invalid("no sign-in is configured for %s accounts (see the README: app credentials per network)", acc.Provider)
	}
	if want != "" {
		for _, n := range fits {
			if n.Slug == want || n.Connector == want {
				return n, nil
			}
		}
		return nil, invalid("account %s is a %s account: %s cannot reconnect it", acc.ID, acc.Provider, want)
	}
	if len(fits) > 1 && acc.Provider == "instagram" {
		creds := map[string]string{}
		_ = m.Svc.Box.OpenJSON(acc.Credentials, &creds)
		prefer := "instagram-login"
		if creds["login"] == "facebook" {
			prefer = "instagram"
		}
		for _, n := range fits {
			if n.Slug == prefer {
				return n, nil
			}
		}
	}
	return fits[0], nil
}

func (m *Manager) resolve(name string) (*Network, error) {
	if n := m.networks[name]; n != nil {
		return n, nil
	}
	for _, n := range m.Networks() {
		if n.Connector == name {
			return n, nil
		}
	}
	var have []string
	for _, n := range m.Networks() {
		have = append(have, n.Slug)
	}
	if len(have) == 0 {
		return nil, invalid("no network sign-in is configured: set the app credentials of a network (see the README)")
	}
	return nil, invalid("unknown or unconfigured network %q (configured: %s)", name, strings.Join(have, ", "))
}

// Begin checks the request and returns the one-time start link. The caller checks the role (owner/admin).
func (m *Manager) Begin(ctx context.Context, req Request) (*Started, error) {
	var n *Network
	var err error
	if req.AccountID != "" {
		acc, gerr := m.Svc.Store.GetAccount(ctx, req.AccountID)
		if gerr != nil {
			return nil, gerr
		}
		req.Org = acc.OrgID
		n, err = m.forAccount(ctx, acc, req.Network)
	} else if req.Network == "" {
		return nil, invalid("give network (or account_id to reconnect an account)")
	} else {
		o, oerr := m.Svc.Store.TargetOrg(ctx, req.Org)
		if errors.Is(oerr, store.ErrOrgRequired) {
			return nil, invalid("%v", oerr)
		}
		if oerr != nil {
			return nil, invalid("unknown organization %q (see list_organizations)", req.Org)
		}
		req.Org = o.ID
		n, err = m.resolve(req.Network)
	}
	if err != nil {
		return nil, err
	}
	tok := randomToken()
	exp := m.now().Add(TTL)
	m.mu.Lock()
	m.sweep()
	if len(m.starts) >= maxPending {
		m.mu.Unlock()
		return nil, invalid("too many pending connections, retry in a few minutes")
	}
	m.starts[hashToken(tok)] = &start{net: n, req: req, expires: exp}
	m.mu.Unlock()
	note := "Open this link in a browser within 10 minutes (single use), then sign in on " + n.Label + "."
	if req.AccountID != "" {
		note += " Sign in with the same " + n.Label + " account: another one is refused."
	}
	return &Started{
		URL:     m.base + "/connect/" + n.Slug + "/start?t=" + url.QueryEscape(tok),
		Network: n.Slug, Label: n.Label, AccountID: req.AccountID, ExpiresAt: exp.UTC(),
		RedirectURI: m.RedirectURI(n), Note: note,
	}, nil
}

// sweep drops expired entries. Callers hold mu.
func (m *Manager) sweep() {
	now := m.now()
	for k, v := range m.starts {
		if now.After(v.expires) {
			delete(m.starts, k)
		}
	}
	for k, v := range m.flows {
		if now.After(v.expires) {
			delete(m.flows, k)
		}
	}
	for k, v := range m.picks {
		if now.After(v.expires) {
			delete(m.picks, k)
		}
	}
}

// ---------- HTTP ----------

// SessionFunc returns the dashboard member signed in on r ("" when none).
type SessionFunc func(r *http.Request) string

// Register mounts the start, callback and picker pages. session (optional) recognizes the dashboard member, so
// a start link opened from the dashboard goes on without the confirmation page.
func (m *Manager) Register(mux *http.ServeMux, session SessionFunc) {
	mux.Handle("GET /connect/static/connect.css", m.headers(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, static, "static/connect.css")
	})))
	mux.Handle("GET /connect/{network}/start", m.headers(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.startPage(w, r, session)
	})))
	mux.Handle("POST /connect/{network}/start", m.headers(m.form(m.startPost)))
	mux.Handle("GET /connect/{network}/callback", m.headers(http.HandlerFunc(m.callback)))
	mux.Handle("POST /connect/{network}/pick", m.headers(m.form(m.pickPost)))
}

func (m *Manager) headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		// same-origin, not no-referrer: with no-referrer, browsers send "Origin: null" on form posts, and the
		// network never sees a Relay URL either way.
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		if m.secure {
			h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		}
		if !strings.HasPrefix(r.URL.Path, "/connect/static/") {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// form accepts same-origin form posts only (CSRF): Origin is Relay's, or, when a browser sends "null"
// (privacy settings), Sec-Fetch-Site says same-origin.
func (m *Manager) form(fn http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o := r.Header.Get("Origin")
		if o != m.base && !(o == "null" && r.Header.Get("Sec-Fetch-Site") == "same-origin") {
			m.page(w, http.StatusForbidden, view{Error: "This form must be sent from Relay itself."})
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
		if err := r.ParseForm(); err != nil {
			m.page(w, http.StatusBadRequest, view{Error: "Bad form."})
			return
		}
		fn(w, r)
	})
}

type view struct {
	Title   string
	Message string
	Error   string
	Network string
	// Start confirmation
	Token string
	// Picker
	PickID string
	Cands  []option
	Action string
	Done   bool
	// Next: the network's sign-in page, opened at once (after the Continue form)
	Next string
}

type option struct {
	Index int
	Label string
}

func (m *Manager) page(w http.ResponseWriter, status int, v view) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := pages.ExecuteTemplate(w, "connect.html", v); err != nil {
		m.Log.Warn("connect: render", "err", scrub(err))
	}
}

func (m *Manager) takeStart(tok, slug string) *start {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := hashToken(tok)
	s := m.starts[k]
	delete(m.starts, k)
	if s == nil || s.net.Slug != slug || m.now().After(s.expires) {
		return nil
	}
	return s
}

func (m *Manager) peekStart(tok, slug string) *start {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.starts[hashToken(tok)]
	if s == nil || s.net.Slug != slug || m.now().After(s.expires) {
		return nil
	}
	return s
}

const expired = "This link expired or was already used. Start again from Relay."

// startPage: from the dashboard of the member who asked, go on at once; else ask for a click (POST), so link
// previews and mail scanners that open links do not use the single-use link.
func (m *Manager) startPage(w http.ResponseWriter, r *http.Request, session SessionFunc) {
	slug, tok := r.PathValue("network"), r.URL.Query().Get("t")
	s := m.peekStart(tok, slug)
	if s == nil {
		m.page(w, http.StatusGone, view{Error: expired})
		return
	}
	if s.req.Dashboard && s.req.UserID != "" && session != nil && session(r) == s.req.UserID {
		if s = m.takeStart(tok, slug); s == nil {
			m.page(w, http.StatusGone, view{Error: expired})
			return
		}
		m.redirectToNetwork(w, r, s)
		return
	}
	title := "Connect " + s.net.Label
	if s.req.AccountID != "" {
		title = "Reconnect " + s.net.Label
		if acc, err := m.Svc.Store.GetAccount(r.Context(), s.req.AccountID); err == nil && acc.Handle != "" {
			title += " · @" + acc.Handle
		}
	}
	m.page(w, 200, view{Title: title, Network: s.net.Slug, Token: tok, Action: "/connect/" + s.net.Slug + "/start",
		Message: "Relay will open " + s.net.Label + " to sign in."})
}

func (m *Manager) startPost(w http.ResponseWriter, r *http.Request) {
	s := m.takeStart(r.PostForm.Get("t"), r.PathValue("network"))
	if s == nil {
		m.page(w, http.StatusGone, view{Error: expired})
		return
	}
	m.redirectToNetwork(w, r, s)
}

func (m *Manager) redirectToNetwork(w http.ResponseWriter, r *http.Request, s *start) {
	f := &flow{net: s.net, req: s.req, expires: m.now().Add(TTL)}
	if s.net.PKCE {
		f.verifier = randomToken() + randomToken()[:11] // 54 characters (43-128)
	}
	state := randomToken()
	authURL, key, secret, err := s.net.begin(r.Context(), m, f, state)
	if err != nil {
		m.Log.Warn("connect: could not start", "network", s.net.Slug, "err", scrub(err))
		m.page(w, http.StatusBadGateway, view{Error: s.net.Label + " did not answer. Try again later.", Network: s.net.Slug})
		return
	}
	f.secret = secret
	binding := randomToken()
	f.binding = hashToken(binding)
	m.mu.Lock()
	m.sweep()
	if len(m.flows) >= maxPending {
		m.mu.Unlock()
		m.page(w, http.StatusTooManyRequests, view{Error: "Too many pending sign-ins, retry in a few minutes."})
		return
	}
	m.flows[key] = f
	m.mu.Unlock()
	m.setBinding(w, binding)
	if r.Method != http.MethodPost {
		http.Redirect(w, r, authURL, http.StatusSeeOther)
		return
	}
	// After the Continue form, a redirect would count as the form's target: browsers apply the page's
	// form-action 'self' to it (and to the network's own redirects) and silently stay on the page, so a second
	// click finds the link used. A page that moves on by itself is a plain navigation.
	m.page(w, http.StatusOK, view{Title: "Opening " + s.net.Label, Network: s.net.Slug, Next: authURL,
		Message: "Relay is opening " + s.net.Label + " to sign in."})
}

func (m *Manager) cookieName() string {
	if m.secure {
		return cookieBinding
	}
	return strings.TrimPrefix(cookieBinding, "__Host-")
}

// setBinding ties the flow to this browser. SameSite=Lax: the network's redirect back is a cross-site
// top-level GET, which Strict cookies (the dashboard session) do not survive.
func (m *Manager) setBinding(w http.ResponseWriter, v string) {
	maxAge := int(TTL.Seconds())
	if v == "" {
		maxAge = -1
	}
	http.SetCookie(w, &http.Cookie{Name: m.cookieName(), Value: v, Path: "/", MaxAge: maxAge,
		Secure: m.secure, HttpOnly: true, SameSite: http.SameSiteLaxMode})
}

func (m *Manager) bound(r *http.Request, want string) bool {
	c, err := r.Cookie(m.cookieName())
	if err != nil || c.Value == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(hashToken(c.Value)), []byte(want)) == 1
}

func (m *Manager) callback(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("network")
	q := r.URL.Query()
	key := q.Get("state")
	if key == "" {
		key = q.Get("oauth_token") // OAuth 1.0a
	}
	if key == "" {
		key = q.Get("denied") // OAuth 1.0a, refused
	}
	m.mu.Lock()
	f := m.flows[key]
	delete(m.flows, key) // single use, whatever happens next
	m.mu.Unlock()
	if key == "" || f == nil || f.net.Slug != slug || m.now().After(f.expires) {
		m.page(w, http.StatusBadRequest, view{Error: "This sign-in expired or was already used. Start again from Relay."})
		return
	}
	if !m.bound(r, f.binding) {
		m.Log.Warn("connect: callback without the browser binding", "network", slug)
		m.page(w, http.StatusForbidden, view{Error: "This sign-in was started in another browser. Start again from Relay."})
		return
	}
	m.setBinding(w, "")
	if q.Get("error") != "" || q.Get("denied") != "" {
		m.page(w, http.StatusOK, view{Error: f.net.Label + " sign-in was cancelled.", Network: slug})
		return
	}
	if err := m.stillAllowed(r.Context(), f.req); err != nil {
		m.page(w, http.StatusForbidden, view{Error: err.Error()})
		return
	}
	ctx := r.Context()
	cands, err := f.net.finish(ctx, m, f, q)
	if err != nil {
		m.Log.Warn("connect: sign-in failed", "network", slug, "err", scrub(err))
		m.page(w, http.StatusBadGateway, view{Error: userMessage(f.net, err), Network: slug})
		return
	}
	if f.req.AccountID != "" {
		m.finishReconnect(w, r, f.net, f.req, cands)
		return
	}
	if len(cands) == 0 {
		m.page(w, http.StatusOK, view{Error: f.net.Label + ": " + f.net.none, Network: slug})
		return
	}
	if len(cands) == 1 {
		m.finishConnect(w, r, f.net, f.req, cands[0])
		return
	}
	sort.SliceStable(cands, func(i, j int) bool { return strings.ToLower(cands[i].label()) < strings.ToLower(cands[j].label()) })
	p := &pick{id: randomToken(), net: f.net, req: f.req, cands: cands, expires: m.now().Add(TTL)}
	binding := randomToken()
	p.binding = hashToken(binding)
	m.mu.Lock()
	m.picks[p.id] = p
	m.mu.Unlock()
	m.setBinding(w, binding)
	v := view{Title: f.net.pickTitle, Network: slug, PickID: p.id, Action: "/connect/" + slug + "/pick"}
	for i, c := range cands {
		v.Cands = append(v.Cands, option{Index: i, Label: c.label()})
	}
	m.page(w, http.StatusOK, v)
}

func (m *Manager) pickPost(w http.ResponseWriter, r *http.Request) {
	id := r.PostForm.Get("pick")
	m.mu.Lock()
	p := m.picks[id]
	if p != nil && m.bound(r, p.binding) {
		delete(m.picks, id) // single use
	} else {
		p = nil
	}
	m.mu.Unlock()
	if p == nil || p.net.Slug != r.PathValue("network") || m.now().After(p.expires) {
		m.page(w, http.StatusBadRequest, view{Error: "This choice expired or was already used. Start again from Relay."})
		return
	}
	m.setBinding(w, "")
	i, err := strconv.Atoi(r.PostForm.Get("choice"))
	if err != nil || i < 0 || i >= len(p.cands) {
		m.page(w, http.StatusBadRequest, view{Error: "Choose one account. Start again from Relay."})
		return
	}
	if err := m.stillAllowed(r.Context(), p.req); err != nil {
		m.page(w, http.StatusForbidden, view{Error: err.Error()})
		return
	}
	m.finishConnect(w, r, p.net, p.req, p.cands[i])
}

// stillAllowed: the member who asked must still be an active owner or admin when the network answers.
func (m *Manager) stillAllowed(ctx context.Context, req Request) error {
	if req.UserID == "" {
		return nil
	}
	u, err := m.Svc.Store.GetUser(ctx, req.UserID)
	if err != nil || u.Disabled || (u.Role != store.RoleOwner && u.Role != store.RoleAdmin) {
		return &userError{"Only owners and admins can connect accounts."}
	}
	return nil
}

// orgCtx limits the callback to the organization the sign-in was started for.
func (m *Manager) orgCtx(r *http.Request, req Request) context.Context {
	if req.Org == "" {
		return r.Context()
	}
	return store.WithOrgs(r.Context(), []string{req.Org})
}

func (m *Manager) finishReconnect(w http.ResponseWriter, r *http.Request, n *Network, req Request, cands []Candidate) {
	ctx := m.orgCtx(r, req)
	acc, err := m.Svc.Store.GetAccount(ctx, req.AccountID)
	if err != nil {
		m.page(w, http.StatusNotFound, view{Error: "This account was removed from Relay."})
		return
	}
	var match *Candidate
	for i := range cands {
		if acc.RemoteUserID != "" && cands[i].RemoteID == acc.RemoteUserID {
			match = &cands[i]
			break
		}
	}
	if match == nil {
		got := "another account"
		if len(cands) == 1 {
			got = cands[0].label()
		}
		who := acc.Name
		if acc.Handle != "" {
			who = "@" + acc.Handle
		}
		m.Log.Warn("connect: reconnect refused, other remote account", "account", acc.ID, "network", n.Slug)
		m.page(w, http.StatusConflict, view{Network: n.Slug,
			Error: fmt.Sprintf("You signed in as %s, not %s. Nothing changed. Sign out of %s, then start again with %s.", got, who, n.Label, who)})
		return
	}
	updated, err := m.Svc.UpdateCredentials(ctx, acc.ID, match.Creds)
	if err != nil {
		m.Log.Warn("connect: reconnect failed", "account", acc.ID, "network", n.Slug, "err", scrub(err))
		m.page(w, http.StatusBadGateway, view{Error: userMessage(n, err), Network: n.Slug})
		return
	}
	m.Log.Info("connect: account reconnected", "account", updated.ID, "provider", updated.Provider, "by", actor(req))
	m.page(w, http.StatusOK, view{Done: true, Network: n.Slug, Title: n.Label + " reconnected", Message: accountLabel(updated)})
}

func (m *Manager) finishConnect(w http.ResponseWriter, r *http.Request, n *Network, req Request, c Candidate) {
	ctx := m.orgCtx(r, req)
	// Already connected: update it in place rather than creating a duplicate. The same remote account cannot
	// belong to two organizations.
	if accs, err := m.Svc.Store.ListAccounts(store.Unscoped(ctx)); err == nil {
		for _, a := range accs {
			if a.Provider == n.Connector && a.RemoteUserID != "" && a.RemoteUserID == c.RemoteID {
				if req.Org != "" && a.OrgID != req.Org {
					m.page(w, http.StatusConflict, view{Network: n.Slug, Error: accountLabel(a) + " is already connected in another organization. Nothing changed."})
					return
				}
				updated, err := m.Svc.UpdateCredentials(ctx, a.ID, c.Creds)
				if err != nil {
					m.Log.Warn("connect: update of an already connected account failed", "account", a.ID, "err", scrub(err))
					m.page(w, http.StatusBadGateway, view{Error: userMessage(n, err), Network: n.Slug})
					return
				}
				m.Log.Info("connect: account reconnected", "account", a.ID, "provider", a.Provider, "by", actor(req))
				m.page(w, http.StatusOK, view{Done: true, Network: n.Slug, Title: n.Label + " reconnected", Message: accountLabel(updated)})
				return
			}
		}
	}
	acc, err := m.Svc.ConnectAccount(ctx, n.Connector, "", c.Creds)
	if err != nil {
		m.Log.Warn("connect: connect failed", "network", n.Slug, "err", scrub(err))
		m.page(w, http.StatusBadGateway, view{Error: userMessage(n, err), Network: n.Slug})
		return
	}
	m.Log.Info("connect: account connected", "account", acc.ID, "provider", acc.Provider, "by", actor(req))
	m.page(w, http.StatusOK, view{Done: true, Network: n.Slug, Title: n.Label + " connected", Message: accountLabel(acc)})
}

func accountLabel(a *store.Account) string {
	if a.Handle != "" {
		return "@" + a.Handle
	}
	return a.Name
}

func actor(req Request) string {
	if req.UserID == "" {
		return "api-token"
	}
	return req.UserID
}

// userMessage is safe to show: our own messages, never a network's raw answer (it may echo parameters).
func userMessage(n *Network, err error) string {
	var inv *core.ErrInvalid
	if errors.As(err, &inv) {
		return inv.Msg
	}
	var ue *userError
	if errors.As(err, &ue) {
		return ue.msg
	}
	return n.Label + " refused the sign-in. Try again; if it keeps failing, check the app settings (redirect URI, permissions)."
}

var secretParam = regexp.MustCompile(`(?i)((?:access_token|refresh_token|fb_exchange_token|client_secret|code|code_verifier|oauth_token|oauth_verifier|token)=)[^&\s"']+`)

// scrub removes secrets from an error before it is logged: connectors put tokens in query strings, and Go's
// HTTP errors quote the URL.
func scrub(err error) string { return secretParam.ReplaceAllString(err.Error(), "${1}REDACTED") }

// userError carries a message meant for the person signing in.
type userError struct{ msg string }

func (e *userError) Error() string { return e.msg }

// ---------- helpers ----------

func randomToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func hashToken(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

// challenge is the PKCE S256 code challenge of a verifier.
func challenge(verifier string) string {
	h := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(h[:])
}
