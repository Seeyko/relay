package x

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// OAuth1 signs requests with OAuth 1.0a user context (HMAC-SHA1).
type OAuth1 struct {
	ConsumerKey    string
	ConsumerSecret string
	Token          string
	TokenSecret    string

	now   func() time.Time
	nonce func() string
}

func percentEncode(s string) string {
	// RFC 3986: unreserved characters stay, everything else is %XX (uppercase).
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '.' || c == '_' || c == '~' {
			b.WriteByte(c)
		} else {
			b.WriteString("%" + strings.ToUpper(hex.EncodeToString([]byte{c})))
		}
	}
	return b.String()
}

// Header computes the Authorization header. extra holds form-encoded body params (only for
// application/x-www-form-urlencoded bodies); JSON and multipart bodies are not signed.
func (o *OAuth1) Header(method, rawURL string, extra url.Values) string {
	return o.HeaderWith(method, rawURL, extra, nil)
}

// HeaderWith is Header with extra oauth_* protocol parameters, signed and sent in the header: oauth_callback
// for a request token, oauth_verifier for an access token (3-legged flow). Without Token (request token step)
// there is no oauth_token parameter and the signing key ends with "&".
func (o *OAuth1) HeaderWith(method, rawURL string, extra url.Values, protocol map[string]string) string {
	now := time.Now
	if o.now != nil {
		now = o.now
	}
	nonce := func() string {
		b := make([]byte, 16)
		_, _ = rand.Read(b)
		return hex.EncodeToString(b)
	}
	if o.nonce != nil {
		nonce = o.nonce
	}
	oauth := map[string]string{
		"oauth_consumer_key":     o.ConsumerKey,
		"oauth_nonce":            nonce(),
		"oauth_signature_method": "HMAC-SHA1",
		"oauth_timestamp":        strconv.FormatInt(now().Unix(), 10),
		"oauth_version":          "1.0",
	}
	if o.Token != "" {
		oauth["oauth_token"] = o.Token
	}
	for k, v := range protocol {
		oauth[k] = v
	}
	u, _ := url.Parse(rawURL)
	type kv struct{ k, v string }
	var params []kv
	for k, v := range oauth {
		params = append(params, kv{percentEncode(k), percentEncode(v)})
	}
	for k, vs := range u.Query() {
		for _, v := range vs {
			params = append(params, kv{percentEncode(k), percentEncode(v)})
		}
	}
	for k, vs := range extra {
		for _, v := range vs {
			params = append(params, kv{percentEncode(k), percentEncode(v)})
		}
	}
	sort.Slice(params, func(i, j int) bool {
		if params[i].k == params[j].k {
			return params[i].v < params[j].v
		}
		return params[i].k < params[j].k
	})
	pairs := make([]string, len(params))
	for i, p := range params {
		pairs[i] = p.k + "=" + p.v
	}
	base := strings.ToUpper(method) + "&" + percentEncode(baseURL(u)) + "&" + percentEncode(strings.Join(pairs, "&"))
	key := percentEncode(o.ConsumerSecret) + "&" + percentEncode(o.TokenSecret)
	mac := hmac.New(sha1.New, []byte(key))
	mac.Write([]byte(base))
	oauth["oauth_signature"] = base64.StdEncoding.EncodeToString(mac.Sum(nil))

	keys := make([]string, 0, len(oauth))
	for k := range oauth {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = percentEncode(k) + `="` + percentEncode(oauth[k]) + `"`
	}
	return "OAuth " + strings.Join(parts, ", ")
}

func baseURL(u *url.URL) string {
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Host)
	if (scheme == "https" && strings.HasSuffix(host, ":443")) || (scheme == "http" && strings.HasSuffix(host, ":80")) {
		host = host[:strings.LastIndex(host, ":")]
	}
	return scheme + "://" + host + u.EscapedPath()
}

func (o *OAuth1) Sign(req *http.Request, form url.Values) {
	req.Header.Set("Authorization", o.Header(req.Method, req.URL.String(), form))
}
