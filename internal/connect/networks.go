package connect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Seeyko/relay/internal/providers"
	"github.com/Seeyko/relay/internal/providers/linkedin"
	"github.com/Seeyko/relay/internal/providers/meta"
	"github.com/Seeyko/relay/internal/providers/x"
)

// Network is one "Connect with ..." sign-in.
type Network struct {
	Slug      string // URL segment: /connect/{slug}/callback
	Label     string
	Connector string // the Relay provider of the accounts it connects
	Scopes    []string
	PKCE      bool
	// Env names the app credential variables (documentation and the Accounts sheet).
	Env string
	// Console is where the redirect URI is registered.
	Console string

	ClientID, ClientSecret string
	// Endpoints. Tests point them at fake servers.
	AuthURL, TokenURL, API string
	// CredExtra is added to the stored credentials (tests point the connectors at fake servers).
	CredExtra map[string]string

	clientParam string // client_id, or client_key (TikTok)
	scopeSep    string
	authExtra   url.Values
	none        string // shown when the sign-in gives no account to connect
	pickTitle   string
	begin       func(ctx context.Context, m *Manager, f *flow, state string) (authURL, key, secret string, err error)
	finish      func(ctx context.Context, m *Manager, f *flow, q url.Values) ([]Candidate, error)
}

// FromEnv returns the networks whose app credentials are set, in display order.
func FromEnv(getenv func(string) string) []*Network {
	var out []*Network
	add := func(n *Network, idVar, secretVar string) {
		n.ClientID, n.ClientSecret = strings.TrimSpace(getenv(idVar)), strings.TrimSpace(getenv(secretVar))
		if n.ClientID == "" || n.ClientSecret == "" {
			return
		}
		n.Env = idVar + ", " + secretVar
		if s := getenv("RELAY_CONNECT_" + strings.ToUpper(strings.ReplaceAll(n.Slug, "-", "_")) + "_SCOPES"); s != "" {
			n.Scopes = splitScopes(s)
		}
		out = append(out, n)
	}
	add(Threads(), "RELAY_THREADS_APP_ID", "RELAY_THREADS_APP_SECRET")
	add(Facebook(), "RELAY_META_APP_ID", "RELAY_META_APP_SECRET")
	add(InstagramFacebook(), "RELAY_META_APP_ID", "RELAY_META_APP_SECRET")
	add(InstagramLogin(), "RELAY_INSTAGRAM_APP_ID", "RELAY_INSTAGRAM_APP_SECRET")
	add(LinkedIn(), "RELAY_LINKEDIN_CLIENT_ID", "RELAY_LINKEDIN_CLIENT_SECRET")
	add(YouTube(), "RELAY_YOUTUBE_CLIENT_ID", "RELAY_YOUTUBE_CLIENT_SECRET")
	add(TikTok(), "RELAY_TIKTOK_CLIENT_KEY", "RELAY_TIKTOK_CLIENT_SECRET")
	add(X(), "RELAY_X_CONSUMER_KEY", "RELAY_X_CONSUMER_SECRET")
	return out
}

func splitScopes(s string) []string {
	var out []string
	for _, p := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' }) {
		out = append(out, p)
	}
	return out
}

// ---------- networks ----------

// Threads: Threads API sign-in, then a 60-day long-lived token (refreshed by the connector).
func Threads() *Network {
	return &Network{
		Slug: "threads", Label: "Threads", Connector: "threads",
		Scopes:  []string{"threads_basic", "threads_content_publish", "threads_manage_replies", "threads_read_replies", "threads_manage_insights", "threads_delete"},
		AuthURL: "https://threads.com/oauth/authorize", TokenURL: "https://graph.threads.net/oauth/access_token", API: "https://graph.threads.net",
		Console: "Meta app > Use cases > Access the Threads API > Settings: Redirect Callback URLs",
		begin:   oauth2Begin, finish: threadsFinish,
	}
}

// Facebook: Facebook Login for Pages. The Page tokens come from a long-lived user token, so they do not expire.
func Facebook() *Network {
	return &Network{
		Slug: "facebook", Label: "Facebook", Connector: "facebook",
		Scopes: []string{"pages_show_list", "pages_manage_posts", "pages_read_engagement", "pages_read_user_content",
			"pages_manage_engagement", "read_insights", "business_management"},
		AuthURL:   "https://www.facebook.com/" + meta.DefaultVersion + "/dialog/oauth",
		TokenURL:  "https://graph.facebook.com/" + meta.DefaultVersion + "/oauth/access_token",
		API:       "https://graph.facebook.com/" + meta.DefaultVersion,
		Console:   "Meta app > Facebook Login for Business > Settings: Valid OAuth Redirect URIs",
		authExtra: url.Values{"auth_type": {"rerequest"}},
		none:      "no Page found. Give Relay access to at least one Page.",
		pickTitle: "Choose a Page",
		begin:     oauth2Begin, finish: metaFinish(false),
	}
}

// InstagramFacebook: Instagram professional accounts linked to a Page, with Facebook Login (as Postiz does).
func InstagramFacebook() *Network {
	return &Network{
		Slug: "instagram", Label: "Instagram (Facebook)", Connector: "instagram",
		Scopes: []string{"instagram_basic", "instagram_content_publish", "instagram_manage_comments", "instagram_manage_insights",
			"pages_show_list", "pages_read_engagement", "business_management"},
		AuthURL:   "https://www.facebook.com/" + meta.DefaultVersion + "/dialog/oauth",
		TokenURL:  "https://graph.facebook.com/" + meta.DefaultVersion + "/oauth/access_token",
		API:       "https://graph.facebook.com/" + meta.DefaultVersion,
		Console:   "Meta app > Facebook Login for Business > Settings: Valid OAuth Redirect URIs",
		authExtra: url.Values{"auth_type": {"rerequest"}},
		none:      "no Instagram professional account linked to your Pages.",
		pickTitle: "Choose an Instagram account",
		begin:     oauth2Begin, finish: metaFinish(true),
	}
}

// InstagramLogin: Instagram API with Instagram Login (no Facebook Page needed).
func InstagramLogin() *Network {
	return &Network{
		Slug: "instagram-login", Label: "Instagram", Connector: "instagram",
		Scopes: []string{"instagram_business_basic", "instagram_business_content_publish", "instagram_business_manage_comments",
			"instagram_business_manage_insights"},
		AuthURL: "https://www.instagram.com/oauth/authorize", TokenURL: "https://api.instagram.com/oauth/access_token", API: "https://graph.instagram.com",
		Console:   "Meta app > Instagram > API setup with Instagram login > Business login settings: OAuth redirect URIs",
		authExtra: url.Values{"enable_fb_login": {"0"}, "force_reauth": {"true"}},
		begin:     oauth2Begin, finish: instagramLoginFinish,
	}
}

// LinkedIn: company pages with the Community Management API. r_organization_admin belongs to the Advertising
// API, not to Community Management: rw_organization_admin covers the organization lists and statistics.
func LinkedIn() *Network {
	return &Network{
		Slug: "linkedin", Label: "LinkedIn", Connector: "linkedin-page",
		Scopes:  []string{"r_organization_social", "w_organization_social", "rw_organization_admin"},
		AuthURL: "https://www.linkedin.com/oauth/v2/authorization", TokenURL: linkedin.DefaultTokenURL, API: linkedin.DefaultBaseURL,
		Console:   "LinkedIn developer portal > your app > Auth: Authorized redirect URLs",
		scopeSep:  " ",
		none:      "you administer no LinkedIn page.",
		pickTitle: "Choose a page",
		begin:     oauth2Begin, finish: linkedinFinish,
	}
}

// YouTube: Google OAuth, offline access (refresh token) and PKCE.
func YouTube() *Network {
	g := "https://www.googleapis.com/auth/"
	return &Network{
		Slug: "youtube", Label: "YouTube", Connector: "youtube",
		Scopes:  []string{g + "youtube.upload", g + "youtube.readonly", g + "youtube.force-ssl", g + "yt-analytics.readonly"},
		PKCE:    true,
		AuthURL: "https://accounts.google.com/o/oauth2/v2/auth", TokenURL: "https://oauth2.googleapis.com/token",
		Console:   "Google Cloud console > APIs & Services > Credentials > OAuth client (Web application): Authorized redirect URIs",
		scopeSep:  " ",
		authExtra: url.Values{"access_type": {"offline"}, "prompt": {"consent"}, "include_granted_scopes": {"true"}},
		begin:     oauth2Begin, finish: youtubeFinish,
	}
}

// TikTok: Login Kit for Web v2. TikTok documents PKCE for mobile and desktop apps only, so the web flow
// relies on the state and the browser binding.
func TikTok() *Network {
	return &Network{
		Slug: "tiktok", Label: "TikTok", Connector: "tiktok",
		Scopes:  []string{"user.info.basic", "user.info.profile", "user.info.stats", "video.publish", "video.list"},
		AuthURL: "https://www.tiktok.com/v2/auth/authorize/", TokenURL: "https://open.tiktokapis.com/v2/oauth/token/",
		Console:     "TikTok for Developers > your app > Login Kit: Redirect URI (Web)",
		clientParam: "client_key",
		begin:       oauth2Begin, finish: tiktokFinish,
	}
}

// X: OAuth 1.0a three-legged (request token, authorize, access token).
func X() *Network {
	return &Network{
		Slug: "x", Label: "X", Connector: "x",
		AuthURL: "https://api.x.com/oauth/authorize", TokenURL: "https://api.x.com/oauth/access_token", API: "https://api.x.com",
		Console: "X developer portal > your app > User authentication settings: Callback URI (Read and write)",
		begin:   xBegin, finish: xFinish,
	}
}

// ---------- OAuth 2.0 ----------

func oauth2Begin(_ context.Context, m *Manager, f *flow, state string) (string, string, string, error) {
	n := f.net
	q := url.Values{}
	param := n.clientParam
	if param == "" {
		param = "client_id"
	}
	q.Set(param, n.ClientID)
	q.Set("redirect_uri", m.RedirectURI(n))
	q.Set("response_type", "code")
	sep := n.scopeSep
	if sep == "" {
		sep = ","
	}
	q.Set("scope", strings.Join(n.Scopes, sep))
	q.Set("state", state)
	if n.PKCE {
		q.Set("code_challenge", challenge(f.verifier))
		q.Set("code_challenge_method", "S256")
	}
	for k, v := range n.authExtra {
		q[k] = v
	}
	join := "?"
	if strings.Contains(n.AuthURL, "?") {
		join = "&"
	}
	return n.AuthURL + join + q.Encode(), state, "", nil
}

type tokenResp struct {
	AccessToken           string `json:"access_token"`
	RefreshToken          string `json:"refresh_token"`
	ExpiresIn             int64  `json:"expires_in"`
	RefreshExpiresIn      int64  `json:"refresh_expires_in"`       // TikTok
	RefreshTokenExpiresIn int64  `json:"refresh_token_expires_in"` // LinkedIn
	Scope                 string `json:"scope"`
	OpenID                string `json:"open_id"`
	Error                 string `json:"error"`
	// Instagram Login answers {"data": [{"access_token": ...}]}.
	Data []struct {
		AccessToken string `json:"access_token"`
	} `json:"data"`
}

func (t *tokenResp) token() string {
	if t.AccessToken == "" && len(t.Data) > 0 {
		return t.Data[0].AccessToken
	}
	return t.AccessToken
}

// exchange trades the authorization code for tokens (POST form, or GET for Facebook).
func (m *Manager) exchange(ctx context.Context, f *flow, q url.Values, method string, extra url.Values) (*tokenResp, error) {
	code := strings.TrimSuffix(q.Get("code"), "#_")
	if code == "" {
		return nil, errors.New("no code in the callback")
	}
	n := f.net
	form := url.Values{}
	param := n.clientParam
	if param == "" {
		param = "client_id"
	}
	form.Set(param, n.ClientID)
	form.Set("client_secret", n.ClientSecret)
	form.Set("code", code)
	form.Set("redirect_uri", m.RedirectURI(n))
	if method != "GET" {
		form.Set("grant_type", "authorization_code")
	}
	if f.verifier != "" {
		form.Set("code_verifier", f.verifier)
	}
	for k, v := range extra {
		form[k] = v
	}
	var t tokenResp
	if err := m.call(ctx, method, n.TokenURL, form, nil, &t); err != nil {
		return nil, err
	}
	if t.token() == "" {
		return nil, fmt.Errorf("token endpoint returned no access_token (error %q)", t.Error)
	}
	return &t, nil
}

// call sends a request and decodes a JSON answer. A GET carries form as its query string. Errors never carry
// the URL's query (it may hold a token).
func (m *Manager) call(ctx context.Context, method, u string, form url.Values, hdr map[string]string, out any) error {
	var req *http.Request
	var err error
	if method == "GET" {
		full := u
		if len(form) > 0 {
			sep := "?"
			if strings.Contains(u, "?") {
				sep = "&"
			}
			full += sep + form.Encode()
		}
		req, err = http.NewRequestWithContext(ctx, "GET", full, nil)
	} else {
		req, err = http.NewRequestWithContext(ctx, method, u, strings.NewReader(form.Encode()))
		if err == nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
	}
	if err != nil {
		return errors.New("bad endpoint " + redact(u))
	}
	req.Header.Set("Accept", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := m.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, redact(u), unwrapURL(err))
	}
	b, err := providers.ReadBody(resp)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, redact(u), err)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("%s %s: unexpected answer", method, redact(u))
	}
	return nil
}

func unwrapURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// redact drops the query string of a URL.
func redact(u string) string {
	if i := strings.IndexByte(u, '?'); i >= 0 {
		return u[:i]
	}
	return u
}

func bearer(tok string) map[string]string { return map[string]string{"Authorization": "Bearer " + tok} }

func (n *Network) creds(base map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(n.CredExtra))
	for k, v := range n.CredExtra {
		out[k] = v
	}
	for k, v := range base {
		out[k] = v
	}
	return out
}

// identify asks the connector who the credentials belong to, the way it records accounts.
func (m *Manager) identify(ctx context.Context, connector string, creds map[string]string) (*providers.Identity, error) {
	f, err := providers.Get(connector)
	if err != nil {
		return nil, err
	}
	c := make(map[string]string, len(creds))
	for k, v := range creds {
		c[k] = v
	}
	cl, err := f.New(c, func(map[string]string) error { return nil }, m.HTTP)
	if err != nil {
		return nil, err
	}
	return cl.Identify(ctx)
}

func (m *Manager) single(ctx context.Context, n *Network, creds map[string]string) ([]Candidate, error) {
	creds = n.creds(creds)
	id, err := m.identify(ctx, n.Connector, creds)
	if err != nil {
		return nil, err
	}
	return []Candidate{{RemoteID: id.RemoteUserID, Name: id.Name, Handle: id.Handle, Creds: creds}}, nil
}

func (m *Manager) stamp() string { return m.now().UTC().Format(time.RFC3339) }

func (m *Manager) expiry(secs int64) string {
	return m.now().UTC().Add(time.Duration(secs) * time.Second).Format(time.RFC3339)
}

func (m *Manager) unixIn(secs int64) string { return strconv.FormatInt(m.now().Unix()+secs, 10) }

func threadsFinish(ctx context.Context, m *Manager, f *flow, q url.Values) ([]Candidate, error) {
	n := f.net
	short, err := m.exchange(ctx, f, q, "POST", nil)
	if err != nil {
		return nil, err
	}
	var long tokenResp
	if err := m.call(ctx, "GET", n.API+"/access_token", url.Values{"grant_type": {"th_exchange_token"},
		"client_secret": {n.ClientSecret}, "access_token": {short.token()}}, nil, &long); err != nil {
		return nil, fmt.Errorf("long-lived token: %w", err)
	}
	if long.AccessToken == "" {
		return nil, errors.New("long-lived token: no access_token")
	}
	creds := map[string]string{"access_token": long.AccessToken, "token_refreshed_at": m.stamp()}
	if long.ExpiresIn > 0 {
		creds["token_expires_at"] = m.expiry(long.ExpiresIn)
	}
	cands, err := m.single(ctx, n, creds)
	if err == nil {
		cands[0].Creds["user_id"] = cands[0].RemoteID
	}
	return cands, err
}

func instagramLoginFinish(ctx context.Context, m *Manager, f *flow, q url.Values) ([]Candidate, error) {
	n := f.net
	short, err := m.exchange(ctx, f, q, "POST", nil)
	if err != nil {
		return nil, err
	}
	var long tokenResp
	if err := m.call(ctx, "GET", n.API+"/access_token", url.Values{"grant_type": {"ig_exchange_token"},
		"client_secret": {n.ClientSecret}, "access_token": {short.token()}}, nil, &long); err != nil {
		return nil, fmt.Errorf("long-lived token: %w", err)
	}
	if long.AccessToken == "" {
		return nil, errors.New("long-lived token: no access_token")
	}
	creds := map[string]string{"login": "instagram", "access_token": long.AccessToken, "token_refreshed_at": m.stamp()}
	if long.ExpiresIn > 0 {
		creds["token_expires_at"] = m.expiry(long.ExpiresIn)
	}
	cands, err := m.single(ctx, n, creds)
	if err == nil {
		cands[0].Creds["user_id"] = cands[0].RemoteID
	}
	return cands, err
}

// metaFinish: code -> user token -> long-lived user token -> the Pages (/me/accounts) and their tokens.
func metaFinish(instagram bool) func(ctx context.Context, m *Manager, f *flow, q url.Values) ([]Candidate, error) {
	return func(ctx context.Context, m *Manager, f *flow, q url.Values) ([]Candidate, error) {
		n := f.net
		short, err := m.exchange(ctx, f, q, "GET", nil)
		if err != nil {
			return nil, err
		}
		var long tokenResp
		if err := m.call(ctx, "GET", n.TokenURL, url.Values{"grant_type": {"fb_exchange_token"}, "client_id": {n.ClientID},
			"client_secret": {n.ClientSecret}, "fb_exchange_token": {short.token()}}, nil, &long); err != nil {
			return nil, fmt.Errorf("long-lived token: %w", err)
		}
		user := long.AccessToken
		if user == "" {
			return nil, errors.New("long-lived token: no access_token")
		}
		type page struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			Username    string `json:"username"`
			AccessToken string `json:"access_token"`
			IG          *struct {
				ID       string `json:"id"`
				Username string `json:"username"`
				Name     string `json:"name"`
			} `json:"instagram_business_account"`
		}
		var all []page
		next := n.API + "/me/accounts"
		form := url.Values{"fields": {"id,name,username,access_token,instagram_business_account{id,username,name}"}, "limit": {"100"}}
		for i := 0; i < 10 && next != ""; i++ {
			var r struct {
				Data   []page `json:"data"`
				Paging struct {
					Next string `json:"next"`
				} `json:"paging"`
			}
			if err := m.call(ctx, "GET", next, form, bearer(user), &r); err != nil {
				return nil, fmt.Errorf("pages: %w", err)
			}
			all = append(all, r.Data...)
			next, form = r.Paging.Next, nil // next carries its own query
		}
		var out []Candidate
		for _, p := range all {
			if p.AccessToken == "" || p.ID == "" {
				continue
			}
			if !instagram {
				out = append(out, Candidate{RemoteID: p.ID, Name: p.Name, Handle: p.Username,
					Creds: n.creds(map[string]string{"page_id": p.ID, "page_access_token": p.AccessToken})})
				continue
			}
			if p.IG == nil || p.IG.ID == "" {
				continue
			}
			name := p.IG.Name
			if name == "" {
				name = p.Name
			}
			out = append(out, Candidate{RemoteID: p.IG.ID, Name: name, Handle: p.IG.Username,
				Creds: n.creds(map[string]string{"login": "facebook", "user_id": p.IG.ID, "access_token": p.AccessToken})})
		}
		return out, nil
	}
}

// linkedinFinish: the organizations the member administers (organizationAcls), named by the connector.
func linkedinFinish(ctx context.Context, m *Manager, f *flow, q url.Values) ([]Candidate, error) {
	n := f.net
	t, err := m.exchange(ctx, f, q, "POST", nil)
	if err != nil {
		return nil, err
	}
	var r struct {
		Elements []struct {
			Organization       string `json:"organization"`
			OrganizationTarget string `json:"organizationTarget"`
		} `json:"elements"`
	}
	hdr := bearer(t.AccessToken)
	hdr["LinkedIn-Version"] = linkedin.DefaultVersion
	hdr["X-Restli-Protocol-Version"] = "2.0.0"
	if err := m.call(ctx, "GET", n.API+"/rest/organizationAcls", url.Values{"q": {"roleAssignee"}, "role": {"ADMINISTRATOR"},
		"state": {"APPROVED"}, "count": {"100"}}, hdr, &r); err != nil {
		return nil, fmt.Errorf("organizations: %w", err)
	}
	base := map[string]string{"access_token": t.AccessToken, "client_id": n.ClientID, "client_secret": n.ClientSecret}
	if t.ExpiresIn > 0 {
		base["expires_at"] = m.unixIn(t.ExpiresIn)
	}
	if t.RefreshToken != "" {
		base["refresh_token"] = t.RefreshToken
		if t.RefreshTokenExpiresIn > 0 {
			base["refresh_token_expires_at"] = m.unixIn(t.RefreshTokenExpiresIn)
		}
	} else {
		base["refresh_token"] = "" // an old refresh token belongs to the old consent
	}
	seen := map[string]bool{}
	var out []Candidate
	for _, e := range r.Elements {
		urn := e.Organization
		if urn == "" {
			urn = e.OrganizationTarget
		}
		id, ok := strings.CutPrefix(urn, "urn:li:organization:")
		if !ok || id == "" || seen[id] || len(out) >= 50 {
			continue
		}
		seen[id] = true
		creds := map[string]string{"organization_id": id}
		for k, v := range base {
			creds[k] = v
		}
		creds = n.creds(creds)
		c := Candidate{RemoteID: id, Creds: creds}
		if who, err := m.identify(ctx, n.Connector, creds); err == nil {
			c.Name, c.Handle = who.Name, who.Handle
			if who.RemoteUserID != "" {
				c.RemoteID = who.RemoteUserID
			}
		}
		out = append(out, c)
	}
	return out, nil
}

func youtubeFinish(ctx context.Context, m *Manager, f *flow, q url.Values) ([]Candidate, error) {
	n := f.net
	t, err := m.exchange(ctx, f, q, "POST", nil)
	if err != nil {
		return nil, err
	}
	if t.RefreshToken == "" {
		return nil, &userError{"Google sent no refresh token. Remove Relay from myaccount.google.com/permissions, then try again."}
	}
	creds := map[string]string{"client_id": n.ClientID, "client_secret": n.ClientSecret, "refresh_token": t.RefreshToken,
		"access_token": t.AccessToken, "expires_at": m.unixIn(t.ExpiresIn)}
	return m.single(ctx, n, creds)
}

func tiktokFinish(ctx context.Context, m *Manager, f *flow, q url.Values) ([]Candidate, error) {
	n := f.net
	t, err := m.exchange(ctx, f, q, "POST", nil)
	if err != nil {
		return nil, err
	}
	creds := map[string]string{"client_key": n.ClientID, "client_secret": n.ClientSecret, "access_token": t.AccessToken,
		"refresh_token": t.RefreshToken, "expires_at": m.unixIn(t.ExpiresIn), "scope": t.Scope}
	if t.RefreshExpiresIn > 0 {
		creds["refresh_expires_at"] = m.unixIn(t.RefreshExpiresIn)
	}
	return m.single(ctx, n, creds)
}

// ---------- OAuth 1.0a (X) ----------

func (m *Manager) oauth1(ctx context.Context, u string, o *x.OAuth1, protocol map[string]string) (url.Values, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", u, nil)
	if err != nil {
		return nil, errors.New("bad endpoint " + redact(u))
	}
	req.Header.Set("Authorization", o.HeaderWith("POST", u, nil, protocol))
	resp, err := m.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("POST %s: %w", redact(u), unwrapURL(err))
	}
	b, err := providers.ReadBody(resp)
	if err != nil {
		return nil, fmt.Errorf("POST %s: %w", redact(u), err)
	}
	v, err := url.ParseQuery(string(b))
	if err != nil {
		return nil, fmt.Errorf("POST %s: unexpected answer", redact(u))
	}
	return v, nil
}

func xBegin(ctx context.Context, m *Manager, f *flow, _ string) (string, string, string, error) {
	n := f.net
	v, err := m.oauth1(ctx, n.API+"/oauth/request_token", &x.OAuth1{ConsumerKey: n.ClientID, ConsumerSecret: n.ClientSecret},
		map[string]string{"oauth_callback": m.RedirectURI(n)})
	if err != nil {
		return "", "", "", err
	}
	tok, secret := v.Get("oauth_token"), v.Get("oauth_token_secret")
	if tok == "" || secret == "" || v.Get("oauth_callback_confirmed") != "true" {
		return "", "", "", errors.New("request token: callback not confirmed")
	}
	q := url.Values{"oauth_token": {tok}}
	if f.req.AccountID != "" {
		q.Set("force_login", "true") // let the person switch to the right account
	}
	return n.AuthURL + "?" + q.Encode(), tok, secret, nil
}

func xFinish(ctx context.Context, m *Manager, f *flow, q url.Values) ([]Candidate, error) {
	n := f.net
	verifier := q.Get("oauth_verifier")
	if verifier == "" {
		return nil, errors.New("no oauth_verifier in the callback")
	}
	v, err := m.oauth1(ctx, n.TokenURL, &x.OAuth1{ConsumerKey: n.ClientID, ConsumerSecret: n.ClientSecret,
		Token: q.Get("oauth_token"), TokenSecret: f.secret}, map[string]string{"oauth_verifier": verifier})
	if err != nil {
		return nil, err
	}
	at, as := v.Get("oauth_token"), v.Get("oauth_token_secret")
	if at == "" || as == "" {
		return nil, errors.New("access token: missing token")
	}
	creds := n.creds(map[string]string{"consumer_key": n.ClientID, "consumer_secret": n.ClientSecret, "access_token": at, "access_token_secret": as})
	return []Candidate{{RemoteID: v.Get("user_id"), Handle: v.Get("screen_name"), Creds: creds}}, nil
}
