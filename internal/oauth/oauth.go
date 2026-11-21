// Package oauth is Relay's OAuth 2.1 authorization server for agents that connect to /mcp (MCP authorization,
// spec 2025-06-18 and later): protected resource metadata (RFC 9728), authorization server metadata (RFC 8414),
// dynamic client registration (RFC 7591), authorization code + PKCE S256 (RFC 7636), resource indicators
// (RFC 8707), refresh token rotation and revocation (RFC 7009).
//
// Security model:
//   - Relay has one human: the owner who signs in to the dashboard with a passkey. An app gets tokens only
//     after the owner approves it on the consent page, and every approval needs a fresh passkey assertion with
//     user verification (not only the 12 h dashboard session).
//   - Clients are public (no secret): PKCE S256 is required, redirect URIs are matched exactly (loopback
//     redirects ignore the port, RFC 8252) and only https or loopback http URIs can be registered.
//   - Codes live 60 s and are single use: a code presented twice deletes the grant made from it.
//   - Access tokens live 1 h. Refresh tokens rotate on every use; an old refresh token presented again deletes
//     the whole grant. A grant lives at most 90 days.
//   - Only SHA-256 hashes of codes and tokens are stored. Tokens are bound to the resource <base>/mcp.
package oauth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/Seeyko/relay/internal/store"
)

const (
	ScopeAdmin  = "relay:admin"
	ScopeDrafts = "relay:drafts"

	CodeTTL     = 60 * time.Second
	AccessTTL   = time.Hour
	RefreshTTL  = 30 * 24 * time.Hour // sliding: each refresh gives a new 30-day token...
	GrantMaxTTL = 90 * 24 * time.Hour // ...but never past 90 days after the approval
	RequestTTL  = 10 * time.Minute    // time to approve on the consent page
	MaxClients  = 200
	maxPending  = 128

	prefixAccess  = "relay_at_"
	prefixRefresh = "relay_rt_"
	prefixCode    = "relay_ac_"
)

var supportedScopes = []string{ScopeAdmin, ScopeDrafts}

// Passkeys is the dashboard's login, used by the consent page.
type Passkeys interface {
	// SessionID returns a stable opaque id of the valid dashboard session of r.
	SessionID(r *http.Request) (string, bool)
	HasPasskeys(ctx context.Context) bool
	// BeginAssertion starts a WebAuthn assertion with user verification required: options go to
	// navigator.credentials.get, state comes back to FinishAssertion (once).
	BeginAssertion() (options any, state any, err error)
	FinishAssertion(ctx context.Context, state any, credential []byte) error
}

type Server struct {
	Store    *store.Store
	Passkeys Passkeys
	Log      *slog.Logger
	Now      func() time.Time

	base     string // https://relay.example.com (also the issuer)
	resource string // https://relay.example.com/mcp
	secure   bool
	key      []byte // HMAC key of the consent CSRF tokens (per process, like pending requests)

	mu      sync.Mutex
	pending map[string]*pending
	limiter map[string][]time.Time
}

// New builds the authorization server for baseURL (https, or http on localhost for tests).
func New(baseURL string, st *store.Store, pk Passkeys, log *slog.Logger) (*Server, error) {
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || u.Host == "" || u.Path != "" || u.RawQuery != "" {
		return nil, errors.New("oauth: RELAY_BASE_URL must be an absolute URL without a path")
	}
	host := u.Hostname()
	local := host == "localhost" || host == "127.0.0.1"
	if u.Scheme != "https" && !(u.Scheme == "http" && local) {
		return nil, errors.New("oauth: RELAY_BASE_URL must use https")
	}
	if pk == nil {
		return nil, errors.New("oauth: needs the passkey dashboard")
	}
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	base := u.Scheme + "://" + u.Host
	return &Server{
		Store: st, Passkeys: pk, Log: log, Now: time.Now,
		base: base, resource: base + "/mcp", secure: u.Scheme == "https", key: key,
		pending: map[string]*pending{}, limiter: map[string][]time.Time{},
	}, nil
}

// Resource is the protected resource (the MCP endpoint) tokens are issued for.
func (s *Server) Resource() string { return s.resource }

// ResourceMetadataURL goes in the WWW-Authenticate header of a 401 from /mcp.
func (s *Server) ResourceMetadataURL() string {
	return s.base + "/.well-known/oauth-protected-resource"
}

// Register mounts the metadata, the endpoints and the consent page on mux.
func (s *Server) Register(mux *http.ServeMux) {
	for _, p := range []string{"/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp"} {
		mux.Handle("GET "+p, cors(http.HandlerFunc(s.resourceMetadata)))
		mux.Handle("OPTIONS "+p, cors(nil))
	}
	for _, p := range []string{"/.well-known/oauth-authorization-server", "/.well-known/openid-configuration"} {
		mux.Handle("GET "+p, cors(http.HandlerFunc(s.serverMetadata)))
		mux.Handle("OPTIONS "+p, cors(nil))
	}
	for p, h := range map[string]http.HandlerFunc{"/oauth/register": s.register, "/oauth/token": s.token, "/oauth/revoke": s.revoke} {
		mux.Handle("POST "+p, cors(h))
		mux.Handle("OPTIONS "+p, cors(nil))
	}
	s.registerConsent(mux)
}

// Authenticate checks an OAuth access token and returns its scope.
func (s *Server) Authenticate(ctx context.Context, token string) (string, bool) {
	if !strings.HasPrefix(token, prefixAccess) {
		return "", false
	}
	a, err := s.Store.OAuthAccessToken(ctx, hashToken(token), s.Now())
	if err != nil {
		return "", false
	}
	if subtle.ConstantTimeCompare([]byte(a.Resource), []byte(s.resource)) != 1 { // audience (RFC 8707)
		return "", false
	}
	return a.Scope, true
}

// ---------- metadata ----------

func (s *Server) resourceMetadata(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"resource":                 s.resource,
		"resource_name":            "Relay",
		"authorization_servers":    []string{s.base},
		"bearer_methods_supported": []string{"header"},
		"scopes_supported":         supportedScopes,
	})
}

func (s *Server) serverMetadata(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"issuer":                                         s.base,
		"authorization_endpoint":                         s.base + "/oauth/authorize",
		"token_endpoint":                                 s.base + "/oauth/token",
		"registration_endpoint":                          s.base + "/oauth/register",
		"revocation_endpoint":                            s.base + "/oauth/revoke",
		"response_types_supported":                       []string{"code"},
		"response_modes_supported":                       []string{"query"},
		"grant_types_supported":                          []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":               []string{"S256"},
		"token_endpoint_auth_methods_supported":          []string{"none"},
		"revocation_endpoint_auth_methods_supported":     []string{"none"},
		"scopes_supported":                               supportedScopes,
		"authorization_response_iss_parameter_supported": true,
	})
}

// ---------- dynamic client registration (RFC 7591) ----------

type registration struct {
	RedirectURIs            []string `json:"redirect_uris"`
	ClientName              string   `json:"client_name"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
}

func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	if !s.allow("register", clientIP(r), 10) {
		oauthErr(w, http.StatusTooManyRequests, "temporarily_unavailable", "too many registrations, wait a minute")
		return
	}
	var in registration
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in); err != nil {
		oauthErr(w, 400, "invalid_client_metadata", "body must be a JSON object")
		return
	}
	if len(in.RedirectURIs) == 0 || len(in.RedirectURIs) > 10 {
		oauthErr(w, 400, "invalid_redirect_uri", "1 to 10 redirect_uris are required")
		return
	}
	for _, u := range in.RedirectURIs {
		if !validRedirect(u) {
			oauthErr(w, 400, "invalid_redirect_uri", "redirect URIs must be https, or http on 127.0.0.1, [::1] or localhost, without a fragment")
			return
		}
	}
	for _, g := range in.GrantTypes {
		if g != "authorization_code" && g != "refresh_token" {
			oauthErr(w, 400, "invalid_client_metadata", "grant_types: only authorization_code and refresh_token")
			return
		}
	}
	for _, t := range in.ResponseTypes {
		if t != "code" {
			oauthErr(w, 400, "invalid_client_metadata", "response_types: only code")
			return
		}
	}
	switch in.TokenEndpointAuthMethod {
	case "", "none", "client_secret_post", "client_secret_basic":
		// Every client is public: no secret is issued and the response says "none" (RFC 7591 §3.2.1 lets
		// the server replace requested values). A client that still sends a secret is accepted, the secret
		// is ignored: PKCE is the proof of possession.
	default:
		oauthErr(w, 400, "invalid_client_metadata", "token_endpoint_auth_method: only none")
		return
	}
	ctx := r.Context()
	n, err := s.Store.CountOAuthClients(ctx)
	if err == nil && n >= MaxClients {
		_, _ = s.Store.PruneOAuthClients(ctx, s.Now().Add(-24*time.Hour))
		n, err = s.Store.CountOAuthClients(ctx)
	}
	if err != nil {
		oauthErr(w, 500, "server_error", "storage error")
		return
	}
	if n >= MaxClients {
		oauthErr(w, http.StatusServiceUnavailable, "temporarily_unavailable", "too many registered apps")
		return
	}
	c := &store.OAuthClient{ID: "relay_c_" + randomToken(16), Name: cleanName(in.ClientName), RedirectURIs: in.RedirectURIs}
	if err := s.Store.CreateOAuthClient(ctx, c); err != nil {
		oauthErr(w, 500, "server_error", "storage error")
		return
	}
	s.Log.Info("oauth: client registered", "client", c.ID, "name", c.Name, "ip", clientIP(r))
	writeJSON(w, http.StatusCreated, map[string]any{
		"client_id":                  c.ID,
		"client_id_issued_at":        c.CreatedAt.Unix(),
		"client_name":                c.Name,
		"redirect_uris":              c.RedirectURIs,
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
		"scope":                      strings.Join(supportedScopes, " "),
	})
}

// cleanName keeps a printable name of at most 80 characters (it is shown on the consent page, escaped).
func cleanName(s string) string {
	var b strings.Builder
	n := 0
	for _, r := range strings.TrimSpace(s) {
		if !unicode.IsPrint(r) {
			continue
		}
		if n == 80 {
			break
		}
		b.WriteRune(r)
		n++
	}
	return strings.TrimSpace(b.String())
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback() && (ip.Equal(net.IPv4(127, 0, 0, 1)) || ip.Equal(net.IPv6loopback))
}

// validRedirect: https with a host, or http on a loopback address; no fragment, no user info.
func validRedirect(raw string) bool {
	if len(raw) > 2000 || strings.ContainsAny(raw, "#\\ \t\r\n") {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Host == "" || u.Opaque != "" {
		return false
	}
	switch u.Scheme {
	case "https":
		return true
	case "http":
		return isLoopback(u.Hostname())
	}
	return false
}

// matchRedirect returns whether requested is one of the registered URIs: exact string match, except that a
// loopback http URI may use any port (RFC 8252 §7.3).
func matchRedirect(registered []string, requested string) bool {
	if requested == "" || !validRedirect(requested) {
		return false
	}
	for _, reg := range registered {
		if reg == requested {
			return true
		}
	}
	q, _ := url.Parse(requested)
	if q.Scheme != "http" {
		return false
	}
	for _, reg := range registered {
		u, err := url.Parse(reg)
		if err != nil || u.Scheme != "http" || !isLoopback(u.Hostname()) {
			continue
		}
		if u.Hostname() == q.Hostname() && u.EscapedPath() == q.EscapedPath() && u.RawQuery == q.RawQuery {
			return true
		}
	}
	return false
}

// ---------- token endpoint ----------

type oerr struct {
	status      int
	code, desc  string
	basicClient bool
}

func (e *oerr) Error() string { return e.code + ": " + e.desc }

func grantErr(desc string) *oerr { return &oerr{status: 400, code: "invalid_grant", desc: desc} }

// form parses a form POST, refusing repeated parameters (RFC 6749 §3.2).
func form(w http.ResponseWriter, r *http.Request) (url.Values, *oerr) {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
		return nil, &oerr{status: 400, code: "invalid_request", desc: "use application/x-www-form-urlencoded"}
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		return nil, &oerr{status: 400, code: "invalid_request", desc: "unreadable form"}
	}
	for k, v := range r.PostForm {
		if len(v) > 1 {
			return nil, &oerr{status: 400, code: "invalid_request", desc: "repeated parameter " + k}
		}
	}
	return r.PostForm, nil
}

// clientOf identifies the (public) client from client_id or HTTP Basic. A client secret, if any, is ignored.
func (s *Server) clientOf(r *http.Request, f url.Values) (*store.OAuthClient, *oerr) {
	id := f.Get("client_id")
	basic := false
	if u, _, ok := r.BasicAuth(); ok {
		basic = true
		u, _ = url.QueryUnescape(u) // RFC 6749 §2.3.1: form-encoded
		if id != "" && id != u {
			return nil, &oerr{status: 400, code: "invalid_request", desc: "client_id differs from the Authorization header"}
		}
		id = u
	}
	if id == "" {
		return nil, &oerr{status: 401, code: "invalid_client", desc: "client_id is required", basicClient: basic}
	}
	c, err := s.Store.GetOAuthClient(r.Context(), id)
	if err != nil {
		return nil, &oerr{status: 401, code: "invalid_client", desc: "unknown client", basicClient: basic}
	}
	return c, nil
}

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	if !s.allow("token", clientIP(r), 60) {
		oauthErr(w, http.StatusTooManyRequests, "invalid_request", "too many requests, wait a minute")
		return
	}
	f, e := form(w, r)
	if e == nil {
		var c *store.OAuthClient
		if c, e = s.clientOf(r, f); e == nil {
			switch f.Get("grant_type") {
			case "authorization_code":
				e = s.exchangeCode(w, r, c, f)
			case "refresh_token":
				e = s.refresh(w, r, c, f)
			case "":
				e = &oerr{status: 400, code: "invalid_request", desc: "grant_type is required"}
			default:
				e = &oerr{status: 400, code: "unsupported_grant_type", desc: "authorization_code or refresh_token"}
			}
		}
	}
	if e != nil {
		if e.basicClient {
			w.Header().Set("WWW-Authenticate", `Basic realm="relay"`)
		}
		oauthErr(w, e.status, e.code, e.desc)
	}
}

func (s *Server) newTokens(grantID, accessScope, refreshScope string, now, refreshMax time.Time) (string, string, []*store.OAuthToken) {
	at, rt := prefixAccess+randomToken(32), prefixRefresh+randomToken(32)
	rexp := now.Add(RefreshTTL)
	if rexp.After(refreshMax) {
		rexp = refreshMax
	}
	return at, rt, []*store.OAuthToken{
		{Hash: hashToken(at), GrantID: grantID, Kind: store.TokenAccess, Scope: accessScope, CreatedAt: now, ExpiresAt: now.Add(AccessTTL)},
		{Hash: hashToken(rt), GrantID: grantID, Kind: store.TokenRefresh, Scope: refreshScope, CreatedAt: now, ExpiresAt: rexp},
	}
}

func tokenResponse(w http.ResponseWriter, at, rt, scope string) {
	writeJSON(w, 200, map[string]any{
		"access_token":  at,
		"token_type":    "Bearer",
		"expires_in":    int(AccessTTL.Seconds()),
		"refresh_token": rt,
		"scope":         scope,
	})
}

func (s *Server) exchangeCode(w http.ResponseWriter, r *http.Request, c *store.OAuthClient, f url.Values) *oerr {
	code, verifier, redirect := f.Get("code"), f.Get("code_verifier"), f.Get("redirect_uri")
	if code == "" || verifier == "" || redirect == "" {
		return &oerr{status: 400, code: "invalid_request", desc: "code, code_verifier and redirect_uri are required"}
	}
	resource := f.Get("resource")
	now := s.Now()
	var at, rt string
	g := &store.OAuthGrant{ID: store.NewID("ogr"), ExpiresAt: now.Add(GrantMaxTTL)}
	err := s.Store.ExchangeOAuthCode(r.Context(), hashToken(code), func(oc *store.OAuthCode) error {
		switch {
		case !now.Before(oc.ExpiresAt):
			return grantErr("code expired")
		case oc.ClientID != c.ID:
			return grantErr("code issued to another client")
		case oc.RedirectURI != redirect:
			return grantErr("redirect_uri does not match the authorization request")
		case resource != "" && normResource(resource) != oc.Resource:
			return &oerr{status: 400, code: "invalid_target", desc: "unknown resource"}
		case !pkceOK(verifier, oc.Challenge):
			return grantErr("PKCE verification failed")
		}
		return nil
	}, g, func(g *store.OAuthGrant) []*store.OAuthToken {
		var toks []*store.OAuthToken
		at, rt, toks = s.newTokens(g.ID, g.Scope, g.Scope, now, g.ExpiresAt)
		return toks
	})
	var oe *oerr
	switch {
	case errors.As(err, &oe):
		return oe
	case errors.Is(err, store.ErrNotFound):
		return grantErr("unknown code")
	case errors.Is(err, store.ErrOAuthReused):
		s.Log.Warn("oauth: authorization code reused, grant revoked", "client", c.ID, "ip", clientIP(r))
		return grantErr("code already used")
	case err != nil:
		return &oerr{status: 500, code: "server_error", desc: "storage error"}
	}
	s.Log.Info("oauth: app connected", "client", c.ID, "name", c.Name, "scope", g.Scope, "grant", g.ID)
	tokenResponse(w, at, rt, g.Scope)
	return nil
}

func (s *Server) refresh(w http.ResponseWriter, r *http.Request, c *store.OAuthClient, f url.Values) *oerr {
	old := f.Get("refresh_token")
	if old == "" {
		return &oerr{status: 400, code: "invalid_request", desc: "refresh_token is required"}
	}
	resource, want := f.Get("resource"), f.Get("scope")
	now := s.Now()
	var at, rt, scope string
	_, err := s.Store.RotateOAuthRefresh(r.Context(), hashToken(old), func(g *store.OAuthGrant, t *store.OAuthToken) error {
		switch {
		case !now.Before(t.ExpiresAt) || !now.Before(g.ExpiresAt):
			return grantErr("refresh token expired")
		case g.ClientID != c.ID:
			return grantErr("refresh token issued to another client")
		case resource != "" && normResource(resource) != g.Resource:
			return &oerr{status: 400, code: "invalid_target", desc: "unknown resource"}
		}
		scope = g.Scope
		if want != "" {
			choices, ok := parseScope(want)
			if !ok || !allowed(scopesOf(g.Scope), choices[0]) {
				return &oerr{status: 400, code: "invalid_scope", desc: "scope exceeds the approved scope"}
			}
			scope = choices[0]
		}
		return nil
	}, func(g *store.OAuthGrant) []*store.OAuthToken {
		var toks []*store.OAuthToken
		at, rt, toks = s.newTokens(g.ID, scope, g.Scope, now, g.ExpiresAt)
		return toks
	})
	var oe *oerr
	switch {
	case errors.As(err, &oe):
		return oe
	case errors.Is(err, store.ErrNotFound):
		return grantErr("unknown refresh token")
	case errors.Is(err, store.ErrOAuthReused):
		s.Log.Warn("oauth: refresh token reused, grant revoked", "client", c.ID, "ip", clientIP(r))
		return grantErr("refresh token already used")
	case err != nil:
		return &oerr{status: 500, code: "server_error", desc: "storage error"}
	}
	tokenResponse(w, at, rt, scope)
	return nil
}

// ---------- revocation (RFC 7009) ----------

func (s *Server) revoke(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.allow("token", clientIP(r), 60) {
		oauthErr(w, http.StatusTooManyRequests, "invalid_request", "too many requests, wait a minute")
		return
	}
	f, e := form(w, r)
	var c *store.OAuthClient
	if e == nil {
		c, e = s.clientOf(r, f)
	}
	if e == nil && f.Get("token") == "" {
		e = &oerr{status: 400, code: "invalid_request", desc: "token is required"}
	}
	if e != nil {
		oauthErr(w, e.status, e.code, e.desc)
		return
	}
	// Revoking either token disconnects the app: the whole grant goes.
	err := s.Store.RevokeOAuthToken(r.Context(), hashToken(f.Get("token")), c.ID)
	switch {
	case errors.Is(err, store.ErrOAuthClient):
		oauthErr(w, 400, "unauthorized_client", "token issued to another client")
	case err != nil:
		oauthErr(w, 500, "server_error", "storage error")
	default:
		w.WriteHeader(200)
	}
}

// ---------- scopes, PKCE, resource ----------

// parseScope returns the access the owner may choose from, most privileged first. An empty scope asks for
// full access. offline_access is ignored (refresh tokens are always issued).
func parseScope(raw string) ([]string, bool) {
	admin, drafts := false, false
	for _, sc := range strings.Fields(raw) {
		switch sc {
		case ScopeAdmin:
			admin = true
		case ScopeDrafts:
			drafts = true
		case "offline_access":
		default:
			return nil, false
		}
	}
	if admin || !drafts {
		return []string{ScopeAdmin, ScopeDrafts}, true // full access includes drafts
	}
	return []string{ScopeDrafts}, true
}

func scopesOf(granted string) []string {
	if granted == ScopeAdmin {
		return []string{ScopeAdmin, ScopeDrafts}
	}
	return []string{ScopeDrafts}
}

func allowed(choices []string, sc string) bool {
	for _, c := range choices {
		if c == sc {
			return true
		}
	}
	return false
}

func normResource(r string) string { return strings.TrimRight(r, "/") }

// validChallenge: an S256 challenge is the 43-character base64url SHA-256 of the verifier.
func validChallenge(c string) bool {
	if len(c) != 43 {
		return false
	}
	_, err := base64.RawURLEncoding.DecodeString(c)
	return err == nil
}

func pkceOK(verifier, challenge string) bool {
	if len(verifier) < 43 || len(verifier) > 128 {
		return false
	}
	for _, r := range verifier {
		if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || strings.ContainsRune("-._~", r)) {
			return false
		}
	}
	sum := sha256.Sum256([]byte(verifier))
	return subtle.ConstantTimeCompare([]byte(base64.RawURLEncoding.EncodeToString(sum[:])), []byte(challenge)) == 1
}

// ---------- helpers ----------

func hashToken(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

func randomToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func (s *Server) mac(parts ...string) string {
	m := hmac.New(sha256.New, s.key)
	for _, p := range parts {
		m.Write([]byte(p))
		m.Write([]byte{0})
	}
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// cors lets browser-based MCP clients read the metadata and call the token endpoints (no cookies involved).
func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, MCP-Protocol-Version")
		h.Set("X-Content-Type-Options", "nosniff")
		if next == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// allow is a sliding-window limit: n calls per minute per client address and bucket.
func (s *Server) allow(bucket, ip string, n int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.Now()
	cut := now.Add(-time.Minute)
	if len(s.limiter) > 4096 {
		s.limiter = map[string][]time.Time{}
	}
	k := bucket + "|" + ip
	kept := s.limiter[k][:0]
	for _, t := range s.limiter[k] {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= n {
		s.limiter[k] = kept
		return false
	}
	s.limiter[k] = append(kept, now)
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

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func oauthErr(w http.ResponseWriter, status int, code, desc string) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, status, map[string]string{"error": code, "error_description": desc})
}
