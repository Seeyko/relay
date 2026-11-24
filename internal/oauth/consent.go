package oauth

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Seeyko/relay/internal/store"
)

//go:embed static
var static embed.FS

var pages = template.Must(template.ParseFS(static, "static/*.html"))

// pending is an authorization request waiting on the consent page. Its parameters were validated by
// GET /oauth/authorize and never come back from the browser: the page only holds the request id.
type pending struct {
	id          string
	client      *store.OAuthClient
	redirectURI string // as sent (exact)
	state       string
	challenge   string
	resource    string
	scopes      []string // what the owner may choose, most privileged first
	expires     time.Time

	assertion        any    // WebAuthn state of the fresh assertion in progress
	assertionSession string // dashboard session that started the fresh check
	check            string // the fresh check in progress: "passkey" or "email"
}

func (s *Server) registerConsent(mux *http.ServeMux) {
	sub, _ := fs.Sub(static, "static")
	files := http.FileServerFS(sub)
	mux.Handle("GET /oauth/static/{file}", s.headers(http.StripPrefix("/oauth/static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".html") {
			http.NotFound(w, r)
			return
		}
		files.ServeHTTP(w, r)
	}))))
	mux.Handle("GET /oauth/authorize", s.headers(http.HandlerFunc(s.authorize)))
	mux.Handle("POST /oauth/api/state", s.headers(s.post(s.consentState)))
	mux.Handle("POST /oauth/api/approve/begin", s.headers(s.post(s.approveBegin)))
	mux.Handle("POST /oauth/api/approve/finish", s.headers(s.post(s.approveFinish)))
	mux.Handle("POST /oauth/api/deny", s.headers(s.post(s.deny)))
}

// headers: the dashboard's strict policy, no framing, no caching. No Cross-Origin-Opener-Policy: apps open
// this page in a popup and wait for it to come back to them.
func (s *Server) headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=()")
		if s.secure {
			h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		}
		if !strings.HasPrefix(r.URL.Path, "/oauth/static/") {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// post accepts only same-origin JSON requests, rate-limited per client address.
func (s *Server) post(fn http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != s.base {
			jsonErr(w, http.StatusForbidden, "bad origin")
			return
		}
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			jsonErr(w, http.StatusUnsupportedMediaType, "JSON only")
			return
		}
		if !s.allow("consent", clientIP(r), 30) {
			jsonErr(w, http.StatusTooManyRequests, "too many attempts, wait a minute")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		fn(w, r)
	})
}

type pageData struct {
	Req   string
	Name  string
	Host  string
	Error string
}

func page(w http.ResponseWriter, status int, d pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = pages.ExecuteTemplate(w, "authorize.html", d)
}

// errorPage is for requests whose redirect_uri is not trusted: never redirect them.
func errorPage(w http.ResponseWriter, status int, msg string) { page(w, status, pageData{Error: msg}) }

func (s *Server) authorize(w http.ResponseWriter, r *http.Request) {
	if !s.allow("authorize", clientIP(r), 30) {
		errorPage(w, http.StatusTooManyRequests, "Too many attempts, wait a minute.")
		return
	}
	q := r.URL.Query()
	for _, v := range q {
		if len(v) > 1 {
			errorPage(w, 400, "Invalid request.")
			return
		}
	}
	client, err := s.Store.GetOAuthClient(r.Context(), q.Get("client_id"))
	if err != nil {
		errorPage(w, 400, "Unknown app.")
		return
	}
	redirect := q.Get("redirect_uri")
	if !matchRedirect(client.RedirectURIs, redirect) {
		errorPage(w, 400, "This redirect address is not registered for this app.")
		return
	}
	state := q.Get("state")
	if len(state) > 2048 {
		errorPage(w, 400, "Invalid request.")
		return
	}
	// From here on the redirect URI is trusted: errors go back to the app (RFC 6749 §4.1.2.1).
	fail := func(code, desc string) {
		http.Redirect(w, r, s.redirectTo(redirect, url.Values{"error": {code}, "error_description": {desc}}, state), http.StatusFound)
	}
	switch {
	case q.Get("response_type") != "code":
		fail("unsupported_response_type", "response_type must be code")
		return
	case q.Get("code_challenge") == "":
		fail("invalid_request", "code_challenge is required (PKCE)")
		return
	case q.Get("code_challenge_method") != "S256":
		fail("invalid_request", "code_challenge_method must be S256")
		return
	case !validChallenge(q.Get("code_challenge")):
		fail("invalid_request", "code_challenge must be a base64url SHA-256")
		return
	}
	if res := q.Get("resource"); res != "" && normResource(res) != s.resource {
		fail("invalid_target", "the only resource is "+s.resource)
		return
	}
	scopes, ok := parseScope(q.Get("scope"))
	if !ok {
		fail("invalid_scope", "supported scopes: "+strings.Join(supportedScopes, " "))
		return
	}
	p := &pending{
		id: randomToken(32), client: client, redirectURI: redirect, state: state,
		challenge: q.Get("code_challenge"), resource: s.resource, scopes: scopes, expires: s.Now().Add(RequestTTL),
	}
	s.addPending(p)
	host := ""
	if u, err := url.Parse(redirect); err == nil {
		host = u.Host
	}
	page(w, 200, pageData{Req: p.id, Name: client.Name, Host: host})
}

// redirectTo adds params, state and iss (RFC 9207) to the registered redirect URI, keeping its own query.
func (s *Server) redirectTo(redirect string, params url.Values, state string) string {
	u, _ := url.Parse(redirect)
	q := u.Query()
	for k, v := range params {
		q[k] = v
	}
	if state != "" {
		q.Set("state", state)
	}
	q.Set("iss", s.base)
	u.RawQuery = q.Encode()
	return u.String()
}

// ---------- pending requests ----------

func (s *Server) addPending(p *pending) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.Now()
	var oldest *pending
	for k, v := range s.pending {
		if !now.Before(v.expires) {
			delete(s.pending, k)
		} else if oldest == nil || v.expires.Before(oldest.expires) {
			oldest = v
		}
	}
	if len(s.pending) >= maxPending && oldest != nil {
		delete(s.pending, oldest.id)
	}
	s.pending[p.id] = p
}

func (s *Server) getPending(id string) *pending {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.pending[id]
	if p == nil || !s.Now().Before(p.expires) {
		delete(s.pending, id)
		return nil
	}
	return p
}

func (s *Server) takePending(id string) *pending {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.pending[id]
	delete(s.pending, id)
	if p == nil || !s.Now().Before(p.expires) {
		return nil
	}
	return p
}

// csrf binds the consent page to the dashboard session and to every parameter of the request.
func (s *Server) csrf(session string, p *pending) string {
	return s.mac("consent", session, p.id, p.client.ID, p.redirectURI, p.state, p.challenge, p.resource, strings.Join(p.scopes, " "))
}

type consentIn struct {
	Req        string          `json:"req"`
	CSRF       string          `json:"csrf"`
	Scope      string          `json:"scope"`
	Credential json.RawMessage `json:"credential"`
	Code       string          `json:"code"`
	// Orgs limits the app to these organization ids (empty: every organization the member sees).
	Orgs []string `json:"orgs"`
}

// consentCheck loads the request and checks the session, the CSRF token and that the member may approve.
func (s *Server) consentCheck(w http.ResponseWriter, r *http.Request) (*pending, string, *Member, consentIn, bool) {
	var in consentIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		jsonErr(w, 400, "invalid JSON")
		return nil, "", nil, in, false
	}
	p := s.getPending(in.Req)
	if p == nil {
		jsonErr(w, http.StatusNotFound, "expired")
		return nil, "", nil, in, false
	}
	sid, m, ok := s.Passkeys.Session(r)
	if !ok {
		jsonErr(w, http.StatusUnauthorized, "login required")
		return nil, "", nil, in, false
	}
	if in.CSRF == "" || subtle.ConstantTimeCompare([]byte(in.CSRF), []byte(s.csrf(sid, p))) != 1 {
		s.Log.Warn("oauth: consent refused (bad CSRF token)", "ip", clientIP(r))
		jsonErr(w, http.StatusForbidden, "bad CSRF token")
		return nil, "", nil, in, false
	}
	if len(ScopesFor(m.Role, p.scopes)) == 0 {
		jsonErr(w, http.StatusForbidden, "your role cannot approve apps")
		return nil, "", nil, in, false
	}
	return p, sid, m, in, true
}

// freshMethod is the check that gates an approval: the member's passkey, else a code sent by email.
func (s *Server) freshMethod(m *Member) string {
	switch {
	case m.HasPasskey:
		return "passkey"
	case m.Email != "" && s.Passkeys.EmailCodes():
		return "email"
	}
	return ""
}

func (s *Server) consentState(w http.ResponseWriter, r *http.Request) {
	var in consentIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		jsonErr(w, 400, "invalid JSON")
		return
	}
	p := s.getPending(in.Req)
	if p == nil {
		jsonErr(w, http.StatusNotFound, "expired")
		return
	}
	pk, em := s.Passkeys.LoginMethods(r.Context())
	out := map[string]any{"logged_in": false, "has_passkeys": pk, "email": em, "scopes": p.scopes}
	if sid, m, ok := s.Passkeys.Session(r); ok {
		out["logged_in"] = true
		out["csrf"] = s.csrf(sid, p)
		out["scopes"] = ScopesFor(m.Role, p.scopes)
		out["can_approve"] = len(ScopesFor(m.Role, p.scopes)) > 0
		out["method"] = s.freshMethod(m)
		if orgs, err := s.memberOrgs(r.Context(), m); err == nil {
			out["orgs"] = orgs
		}
	}
	writeJSON(w, 200, out)
}

// approveBegin starts the fresh check that gates the approval: a passkey assertion of the member, or a code
// sent to the member's address when it has no passkey.
func (s *Server) approveBegin(w http.ResponseWriter, r *http.Request) {
	p, sid, m, _, ok := s.consentCheck(w, r)
	if !ok {
		return
	}
	switch s.freshMethod(m) {
	case "passkey":
		opts, st, err := s.Passkeys.BeginAssertion(r.Context(), m.ID)
		if err != nil {
			jsonErr(w, 500, "could not start the passkey check")
			return
		}
		s.mu.Lock()
		p.assertion, p.assertionSession, p.check = st, sid, "passkey"
		s.mu.Unlock()
		writeJSON(w, 200, opts)
	case "email":
		name := p.client.Name
		if name == "" {
			name = p.client.ID
		}
		if err := s.Passkeys.SendCode(r.Context(), m.ID, name, r.Header.Get("Accept-Language"), clientIP(r)); err != nil {
			s.Log.Warn("oauth: approval code not sent", "client", p.client.ID, "err", err)
			jsonErr(w, http.StatusTooManyRequests, "could not send the code, retry in a few minutes")
			return
		}
		s.mu.Lock()
		p.assertion, p.assertionSession, p.check = nil, sid, "email"
		s.mu.Unlock()
		writeJSON(w, 200, map[string]string{"method": "email"})
	default:
		jsonErr(w, http.StatusConflict, "add a passkey in the dashboard first")
	}
}

// approveFinish verifies the fresh assertion, then issues the code and sends the app's address back.
func (s *Server) approveFinish(w http.ResponseWriter, r *http.Request) {
	p, sid, m, in, ok := s.consentCheck(w, r)
	if !ok {
		return
	}
	if !allowed(ScopesFor(m.Role, p.scopes), in.Scope) {
		jsonErr(w, 400, "scope not allowed")
		return
	}
	if len(in.Orgs) > 0 {
		mine, err := s.memberOrgs(r.Context(), m)
		if err != nil {
			jsonErr(w, 500, "storage error")
			return
		}
		ids := make([]string, len(mine))
		for i, o := range mine {
			ids[i] = o.ID
		}
		if !store.Subset(in.Orgs, ids) {
			jsonErr(w, 400, "organization not allowed")
			return
		}
	}
	s.mu.Lock()
	st, stSession, check := p.assertion, p.assertionSession, p.check
	if check == "passkey" {
		p.assertion, p.assertionSession, p.check = nil, "", "" // each assertion challenge is usable once
	}
	s.mu.Unlock()
	if stSession != sid || check == "" {
		jsonErr(w, http.StatusBadRequest, "check expired, start again")
		return
	}
	switch check {
	case "passkey":
		if st == nil {
			jsonErr(w, http.StatusBadRequest, "check expired, start again")
			return
		}
		if err := s.Passkeys.FinishAssertion(r.Context(), m.ID, st, in.Credential); err != nil {
			s.Log.Warn("oauth: approval refused (passkey check failed)", "client", p.client.ID, "ip", clientIP(r), "err", err)
			jsonErr(w, http.StatusUnauthorized, "passkey check failed")
			return
		}
	case "email":
		// The code itself is single use and allows 5 attempts (the store counts them).
		if err := s.Passkeys.CheckCode(r.Context(), m.ID, in.Code); err != nil {
			s.Log.Warn("oauth: approval refused (email code)", "client", p.client.ID, "ip", clientIP(r), "err", err)
			jsonErr(w, http.StatusUnauthorized, "wrong or expired code")
			return
		}
	}
	if s.takePending(p.id) == nil { // approved twice at the same time: only one code
		jsonErr(w, http.StatusNotFound, "expired")
		return
	}
	code := prefixCode + randomToken(32)
	now := s.Now()
	if err := s.Store.CreateOAuthCode(r.Context(), &store.OAuthCode{
		Hash: hashToken(code), ClientID: p.client.ID, RedirectURI: p.redirectURI, Challenge: p.challenge,
		Scope: in.Scope, Resource: p.resource, UserID: m.ID, CreatedAt: now, ExpiresAt: now.Add(CodeTTL), Orgs: in.Orgs,
	}); err != nil {
		jsonErr(w, 500, "storage error")
		return
	}
	s.Log.Info("oauth: app approved", "client", p.client.ID, "name", p.client.Name, "scope", in.Scope, "user", m.ID, "check", check, "ip", clientIP(r))
	writeJSON(w, 200, map[string]string{"redirect": s.redirectTo(p.redirectURI, url.Values{"code": {code}}, p.state)})
}

// deny needs no session: anyone holding the page may say no.
func (s *Server) deny(w http.ResponseWriter, r *http.Request) {
	var in consentIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		jsonErr(w, 400, "invalid JSON")
		return
	}
	p := s.takePending(in.Req)
	if p == nil {
		jsonErr(w, http.StatusNotFound, "expired")
		return
	}
	writeJSON(w, 200, map[string]string{"redirect": s.redirectTo(p.redirectURI, url.Values{"error": {"access_denied"}}, p.state)})
}

func jsonErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// memberOrgs lists the organizations the member sees (an app can be limited to some of them).
func (s *Server) memberOrgs(ctx context.Context, m *Member) ([]*store.Org, error) {
	ids, full, err := s.Store.UserOrgs(ctx, m.ID)
	if err != nil {
		return nil, err
	}
	if !full {
		ctx = store.WithOrgs(ctx, ids)
	}
	return s.Store.ListOrgs(ctx)
}
