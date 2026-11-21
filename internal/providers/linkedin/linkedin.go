// Package linkedin is the LinkedIn Page connector: it posts as an organization (company page)
// through the versioned Marketing APIs (Community Management), with OAuth 2.0 refresh tokens.
//
// Docs (Marketing version 202611):
//   - https://learn.microsoft.com/linkedin/marketing/community-management/shares/posts-api
//   - https://learn.microsoft.com/linkedin/marketing/community-management/shares/images-api
//   - https://learn.microsoft.com/linkedin/marketing/community-management/shares/videos-api
//   - https://learn.microsoft.com/linkedin/marketing/community-management/shares/documents-api
//   - https://learn.microsoft.com/linkedin/marketing/community-management/shares/multiimage-post-api
//   - https://learn.microsoft.com/linkedin/marketing/community-management/shares/comments-api
//   - https://learn.microsoft.com/linkedin/marketing/community-management/shares/little-text-format
//   - https://learn.microsoft.com/linkedin/marketing/community-management/organizations/share-statistics
//   - https://learn.microsoft.com/linkedin/marketing/community-management/organizations/organization-lookup-api
//   - https://learn.microsoft.com/linkedin/shared/authentication/programmatic-refresh-tokens
package linkedin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Seeyko/relay/internal/providers"
)

const (
	DefaultBaseURL  = "https://api.linkedin.com"
	DefaultTokenURL = "https://www.linkedin.com/oauth/v2/accessToken"
	// DefaultVersion is the LinkedIn-Version header (YYYYMM). Versions are supported for about a year;
	// override it with the api_version credential.
	DefaultVersion = "202611"
)

// Documented limits.
const (
	maxCommentary = 3000 // Posts API commentary
	maxComment    = 1250 // comment text
	maxImages     = 20   // MultiImage API
	minVideo      = 75 << 10
	maxVideo      = 500 << 20 // feed videos: 75 KB to 500 MB, MP4, 3 s to 30 min
	maxDocument   = 100 << 20 // documents: 100 MB, 300 pages
	pageSize      = 100       // finder count limit
	statsBatch    = 20        // URNs per organizationalEntityShareStatistics call (keeps URLs short)
)

var imageMimes = map[string]bool{"image/jpeg": true, "image/jpg": true, "image/png": true, "image/gif": true}

var documentMimes = map[string]bool{
	"application/pdf":               true,
	"application/msword":            true,
	"application/vnd.ms-powerpoint": true,
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document":   true,
	"application/vnd.openxmlformats-officedocument.presentationml.presentation": true,
}

func init() {
	providers.Register(providers.Factory{
		Name: "linkedin-page",
		Description: "LinkedIn company page (organization) via the versioned Marketing APIs, Community Management scopes " +
			"(w_organization_social, r_organization_social, rw_organization_admin). Posts as urn:li:organization:{organization_id}: " +
			"text (3000 characters), one image or 2-20 images (JPEG/PNG/GIF), one MP4 video (options.title), one document " +
			"(PDF/PPT/PPTX/DOC/DOCX, options.title), link/article (options.link, options.link_title, options.link_description, " +
			"optional single image as thumbnail). Extra thread parts become comments of the page on the post. " +
			"Lifetime impressions/clicks/likes/comments/shares (organic, last 12 months), follower count, comments and replies " +
			"(through /v2/socialActions), history import. Access tokens last 60 days and are renewed with the refresh token " +
			"(365 days) when client_id/client_secret are given.",
		CredentialFields: []string{"organization_id", "access_token", "refresh_token", "client_id", "client_secret"},
		Validate:         validate,
		New: func(c map[string]string, persist func(map[string]string) error, hc *http.Client) (providers.Client, error) {
			org := strings.TrimPrefix(strings.TrimSpace(c["organization_id"]), "urn:li:organization:")
			if org == "" {
				return nil, errors.New("linkedin-page: organization_id is required")
			}
			if c["access_token"] == "" && (c["refresh_token"] == "" || c["client_id"] == "") {
				return nil, errors.New("linkedin-page: access_token (or refresh_token with client_id and client_secret) is required")
			}
			if hc == nil {
				hc = http.DefaultClient
			}
			creds := make(map[string]string, len(c))
			for k, v := range c {
				creds[k] = v
			}
			v := strings.TrimSpace(c["api_version"])
			if v == "" {
				v = DefaultVersion
			}
			return &Client{
				Base:        host(c["base_url"], DefaultBaseURL),
				TokenURL:    host(c["token_url"], DefaultTokenURL),
				Version:     v,
				OrgID:       org,
				HC:          hc,
				PollEvery:   5 * time.Second,
				PollTimeout: 15 * time.Minute,
				creds:       creds,
				persist:     persist,
			}, nil
		},
	})
}

func host(v, def string) string {
	if v == "" {
		v = def
	}
	return strings.TrimRight(v, "/")
}

// Client is bound to one organization.
type Client struct {
	Base, TokenURL, Version, OrgID string
	HC                             *http.Client
	// PollEvery and PollTimeout bound the wait for uploaded media to become AVAILABLE.
	PollEvery, PollTimeout time.Duration

	creds   map[string]string
	persist func(map[string]string) error
	mu      sync.Mutex
}

func (c *Client) orgURN() string { return "urn:li:organization:" + c.OrgID }

// ---------- validation ----------

type mediaKind int

const (
	kindImage mediaKind = iota
	kindVideo
	kindDocument
	kindUnknown
)

func kindOf(m providers.MediaFile) mediaKind {
	switch {
	case imageMimes[m.Mime]:
		return kindImage
	case strings.HasPrefix(m.Mime, "video/"):
		return kindVideo
	case documentMimes[m.Mime]:
		return kindDocument
	}
	return kindUnknown
}

func validate(req providers.PublishRequest) []string {
	var errs []string
	first := ""
	if len(req.Parts) > 0 {
		first = req.Parts[0]
	}
	link, _ := req.Options["link"].(string)
	if strings.TrimSpace(first) == "" && len(req.Media) == 0 && link == "" {
		errs = append(errs, "empty post")
	}
	if n := providers.CharCount(first); n > maxCommentary {
		errs = append(errs, fmt.Sprintf("LinkedIn posts are limited to %d characters (got %d)", maxCommentary, n))
	}
	for i, p := range req.Parts {
		if i == 0 {
			continue
		}
		if strings.TrimSpace(p) == "" {
			errs = append(errs, fmt.Sprintf("part %d is empty (extra parts are posted as comments)", i+1))
		}
		if n := providers.CharCount(p); n > maxComment {
			errs = append(errs, fmt.Sprintf("part %d becomes a LinkedIn comment, limited to %d characters (got %d)", i+1, maxComment, n))
		}
	}
	var images, videos, docs int
	for _, m := range req.Media {
		switch kindOf(m) {
		case kindImage:
			images++
		case kindVideo:
			videos++
			if m.Mime != "video/mp4" {
				errs = append(errs, fmt.Sprintf("LinkedIn videos must be MP4 (%s is %s)", m.Filename, m.Mime))
			}
			if m.Size > 0 && (m.Size < minVideo || m.Size > maxVideo) {
				errs = append(errs, fmt.Sprintf("LinkedIn videos must be between 75 KB and 500 MB (%s is %d bytes)", m.Filename, m.Size))
			}
		case kindDocument:
			docs++
			if m.Size > maxDocument {
				errs = append(errs, fmt.Sprintf("LinkedIn documents are limited to 100 MB (%s is %d bytes)", m.Filename, m.Size))
			}
		default:
			errs = append(errs, fmt.Sprintf("LinkedIn accepts JPEG/PNG/GIF images, MP4 videos and PDF/PPT/PPTX/DOC/DOCX documents (%s is %s)", m.Filename, m.Mime))
		}
	}
	if (videos > 0 || docs > 0) && len(req.Media) > 1 {
		errs = append(errs, "a LinkedIn video or document must be the only media of the post")
	}
	if images > maxImages {
		errs = append(errs, fmt.Sprintf("LinkedIn multi-image posts take at most %d images (got %d)", maxImages, images))
	}
	if link != "" {
		if u, err := url.Parse(link); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, "options.link must be an http(s) URL")
		}
		if videos > 0 || docs > 0 || images > 1 {
			errs = append(errs, "a LinkedIn link post takes at most one image (its thumbnail)")
		}
	}
	return errs
}

// checkShape rejects at publish time the media combinations no LinkedIn post can carry
// (the full pre-flight validate can be bypassed with force, these cannot).
func checkShape(req providers.PublishRequest) error {
	link, _ := req.Options["link"].(string)
	for _, m := range req.Media {
		if k := kindOf(m); k == kindUnknown || (k != kindImage && len(req.Media) > 1) {
			return providers.Invalid("linkedin-page: a post takes images, or one video, or one document")
		}
	}
	switch {
	case len(req.Media) > maxImages:
		return providers.Invalid(fmt.Sprintf("linkedin-page: at most %d images per post", maxImages))
	case link != "" && (len(req.Media) > 1 || len(req.Media) == 1 && kindOf(req.Media[0]) != kindImage):
		return providers.Invalid("linkedin-page: a link post takes at most one image (its thumbnail)")
	case strings.TrimSpace(strings.Join(req.Parts, "")) == "" && len(req.Media) == 0 && link == "":
		return providers.Invalid("linkedin-page: empty post")
	}
	return nil
}

// ---------- little text ----------

// mentionRe matches an annotation that must reach LinkedIn unescaped: @[Name](urn:li:organization:123) or a person URN.
var mentionRe = regexp.MustCompile(`@\[[^\]]+\]\(urn:li:(?:organization|person):[^)]+\)`)

// hashtagRe matches a hashtag, which LinkedIn turns into a hashtag link when it is not escaped.
var hashtagRe = regexp.MustCompile(`^#[\p{L}\p{N}]+`)

// escapeLittle escapes the reserved characters of the "little" text format used by post commentary,
// like Postiz does, except mention annotations and hashtags (#word), which stay live.
func escapeLittle(s string) string {
	var b strings.Builder
	last := 0
	for _, loc := range mentionRe.FindAllStringIndex(s, -1) {
		escapeChunk(&b, s[last:loc[0]])
		b.WriteString(s[loc[0]:loc[1]])
		last = loc[1]
	}
	escapeChunk(&b, s[last:])
	return b.String()
}

func escapeChunk(b *strings.Builder, s string) {
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if ch == '#' && (i == 0 || !isWordByte(s[i-1])) {
			if tag := hashtagRe.FindString(s[i:]); tag != "" {
				b.WriteString(tag)
				i += len(tag) - 1
				continue
			}
		}
		if strings.IndexByte(`\|{}@[]()<>#*_~`, ch) >= 0 {
			b.WriteByte('\\')
		}
		b.WriteByte(ch)
	}
}

func isWordByte(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

var (
	hashtagTplRe = regexp.MustCompile(`\{hashtag\|\\?#\|([^}]*)\}`)
	mentionTxtRe = regexp.MustCompile(`@\[([^\]]+)\]\(urn:li:[^)]+\)`)
	escapedRe    = regexp.MustCompile(`\\([\\|{}@\[\]()<>#*_~])`)
)

// unescapeLittle turns commentary read back from LinkedIn into plain text.
func unescapeLittle(s string) string {
	s = hashtagTplRe.ReplaceAllString(s, "#$1")
	s = mentionTxtRe.ReplaceAllString(s, "$1")
	return escapedRe.ReplaceAllString(s, "$1")
}

// ---------- auth and transport ----------

// token returns a valid access token, refreshing it when it expired (60-day lifetime) or when force is set.
func (c *Client) token(ctx context.Context, force bool) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	exp, _ := strconv.ParseInt(c.creds["expires_at"], 10, 64)
	canRefresh := c.creds["refresh_token"] != "" && c.creds["client_id"] != ""
	if !force && c.creds["access_token"] != "" && (exp == 0 || time.Now().Unix() < exp-300 || !canRefresh) {
		return c.creds["access_token"], nil
	}
	if !canRefresh {
		return "", providers.Invalid("linkedin-page: access token rejected and no refresh_token/client_id to renew it, reconnect the account")
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {c.creds["refresh_token"]},
		"client_id":     {c.creds["client_id"]},
		"client_secret": {c.creds["client_secret"]},
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.HC.Do(req)
	if err != nil {
		return "", err
	}
	b, err := providers.ReadBody(resp)
	if err != nil {
		var he *providers.HTTPError
		if errors.As(err, &he) && he.Status < 500 && he.Status != http.StatusTooManyRequests {
			// Expired or revoked refresh token: only a new consent fixes it.
			return "", providers.Invalid("linkedin-page: refresh token rejected, reconnect the account: " + he.Error())
		}
		return "", err
	}
	var r struct {
		AccessToken           string `json:"access_token"`
		ExpiresIn             int64  `json:"expires_in"`
		RefreshToken          string `json:"refresh_token"`
		RefreshTokenExpiresIn int64  `json:"refresh_token_expires_in"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return "", err
	}
	if r.AccessToken == "" {
		return "", errors.New("linkedin-page: token refresh returned no access_token")
	}
	now := time.Now().Unix()
	c.creds["access_token"] = r.AccessToken
	if r.ExpiresIn > 0 {
		c.creds["expires_at"] = strconv.FormatInt(now+r.ExpiresIn, 10)
	}
	if r.RefreshToken != "" {
		c.creds["refresh_token"] = r.RefreshToken
	}
	if r.RefreshTokenExpiresIn > 0 {
		c.creds["refresh_token_expires_at"] = strconv.FormatInt(now+r.RefreshTokenExpiresIn, 10)
	}
	if c.persist != nil {
		_ = c.persist(c.creds)
	}
	return r.AccessToken, nil
}

// liError is the Rest.li error envelope.
type liError struct {
	Status           int    `json:"status"`
	ServiceErrorCode int    `json:"serviceErrorCode"`
	Code             string `json:"code"`
	Message          string `json:"message"`
}

// mapError adjusts statuses so providers.Retryable gives the right answer.
func mapError(err error) error {
	var he *providers.HTTPError
	if !errors.As(err, &he) {
		return err
	}
	var le liError
	_ = json.Unmarshal([]byte(he.Body), &le)
	switch {
	case he.Status == http.StatusConflict,
		// Transient errors Postiz retries on.
		strings.Contains(he.Body, "Unable to obtain activity"),
		strings.Contains(he.Body, "Service Unavailable"):
		return &providers.HTTPError{Status: http.StatusServiceUnavailable, Body: he.Body}
	case le.Code == "TOO_MANY_REQUESTS":
		return &providers.HTTPError{Status: http.StatusTooManyRequests, Body: he.Body}
	}
	return err
}

type response struct {
	Body   []byte
	Header http.Header
}

// do sends an authorized request built by mk (rebuilt after a token refresh).
func (c *Client) do(ctx context.Context, mk func() (*http.Request, error)) (*response, error) {
	for attempt := 0; attempt < 2; attempt++ {
		tok, err := c.token(ctx, attempt > 0)
		if err != nil {
			return nil, err
		}
		req, err := mk()
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := c.HC.Do(req)
		if err != nil {
			return nil, err
		}
		h := resp.Header
		b, err := providers.ReadBody(resp)
		if err != nil {
			var he *providers.HTTPError
			if attempt == 0 && errors.As(err, &he) && he.Status == http.StatusUnauthorized && c.creds["refresh_token"] != "" {
				continue
			}
			return nil, mapError(err)
		}
		return &response{Body: b, Header: h}, nil
	}
	return nil, errors.New("linkedin-page: authentication failed")
}

// call is a JSON API call. path starts with /rest/ (versioned API, gets LinkedIn-Version) or /v2/.
// rawQuery is sent as is: Rest.li 2.0 needs List(...) with unescaped parentheses and commas.
func (c *Client) call(ctx context.Context, method, path, rawQuery string, body, out any, extra map[string]string) (*response, error) {
	var raw []byte
	if body != nil {
		var err error
		if raw, err = json.Marshal(body); err != nil {
			return nil, err
		}
	}
	u := c.Base + path
	if rawQuery != "" {
		u += "?" + rawQuery
	}
	resp, err := c.do(ctx, func() (*http.Request, error) {
		var r io.Reader
		if raw != nil {
			r = bytes.NewReader(raw)
		}
		req, err := http.NewRequestWithContext(ctx, method, u, r)
		if err != nil {
			return nil, err
		}
		req.Header.Set("X-Restli-Protocol-Version", "2.0.0")
		if strings.HasPrefix(path, "/rest/") {
			req.Header.Set("LinkedIn-Version", c.Version)
		}
		if raw != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		for k, v := range extra {
			req.Header.Set(k, v)
		}
		return req, nil
	})
	if err != nil {
		return nil, err
	}
	if out != nil && len(resp.Body) > 0 {
		if err := json.Unmarshal(resp.Body, out); err != nil {
			return resp, fmt.Errorf("linkedin-page: decode %s: %w", path, err)
		}
	}
	return resp, nil
}

// enc encodes a URN for a URL path segment or a query value (Rest.li 2.0 wants ':' ',' '(' ')' encoded inside values).
func enc(urn string) string { return url.QueryEscape(urn) }

// ---------- identity and account ----------

func (c *Client) Identify(ctx context.Context) (*providers.Identity, error) {
	var r struct {
		ID            json.Number `json:"id"`
		LocalizedName string      `json:"localizedName"`
		VanityName    string      `json:"vanityName"`
	}
	if _, err := c.call(ctx, "GET", "/rest/organizations/"+url.PathEscape(c.OrgID), "", nil, &r, nil); err != nil {
		return nil, err
	}
	id := r.ID.String()
	if id == "" {
		id = c.OrgID
	}
	return &providers.Identity{RemoteUserID: id, Handle: r.VanityName, Name: r.LocalizedName}, nil
}

// AccountMetrics reads the follower count (networkSizes, COMPANY_FOLLOWED_BY_MEMBER since 202305).
func (c *Client) AccountMetrics(ctx context.Context) (map[string]int64, error) {
	var r struct {
		FirstDegreeSize int64 `json:"firstDegreeSize"`
	}
	if _, err := c.call(ctx, "GET", "/rest/networkSizes/"+enc(c.orgURN()), "edgeType=COMPANY_FOLLOWED_BY_MEMBER", nil, &r, nil); err != nil {
		return nil, err
	}
	return map[string]int64{"followers": r.FirstDegreeSize}, nil
}

// ---------- media ----------

func endpointOf(k mediaKind) string {
	switch k {
	case kindVideo:
		return "videos"
	case kindDocument:
		return "documents"
	}
	return "images"
}

// upload sends one file through the Images, Videos or Documents API and waits until it is AVAILABLE.
func (c *Client) upload(ctx context.Context, m providers.MediaFile) (string, mediaKind, error) {
	k := kindOf(m)
	if k == kindUnknown {
		return "", k, providers.Invalid(fmt.Sprintf("linkedin-page: unsupported media type %s (%s)", m.Mime, m.Filename))
	}
	ep := endpointOf(k)
	init := map[string]any{"owner": c.orgURN()}
	if k == kindVideo {
		init["fileSizeBytes"] = m.Size
		init["uploadCaptions"] = false
		init["uploadThumbnail"] = false
	}
	var r struct {
		Value struct {
			UploadURL          string `json:"uploadUrl"`
			Image              string `json:"image"`
			Video              string `json:"video"`
			Document           string `json:"document"`
			UploadToken        string `json:"uploadToken"`
			UploadInstructions []struct {
				UploadURL string `json:"uploadUrl"`
				FirstByte int64  `json:"firstByte"`
				LastByte  int64  `json:"lastByte"`
			} `json:"uploadInstructions"`
		} `json:"value"`
	}
	if _, err := c.call(ctx, "POST", "/rest/"+ep, "action=initializeUpload", map[string]any{"initializeUploadRequest": init}, &r, nil); err != nil {
		return "", k, fmt.Errorf("linkedin-page: initialize %s upload of %s: %w", ep, m.Filename, err)
	}
	v := r.Value
	urn := v.Image + v.Video + v.Document
	if urn == "" {
		return "", k, fmt.Errorf("linkedin-page: initialize %s upload returned no URN", ep)
	}
	if k == kindVideo {
		if err := c.uploadVideoParts(ctx, m, urn, v.UploadToken, v.UploadInstructions); err != nil {
			return "", k, err
		}
	} else {
		if v.UploadURL == "" {
			return "", k, fmt.Errorf("linkedin-page: initialize %s upload returned no uploadUrl", ep)
		}
		ct := m.Mime
		if _, err := c.do(ctx, func() (*http.Request, error) {
			f, err := m.Open()
			if err != nil {
				return nil, err
			}
			req, err := http.NewRequestWithContext(ctx, "PUT", v.UploadURL, f)
			if err != nil {
				f.Close()
				return nil, err
			}
			if m.Size > 0 {
				req.ContentLength = m.Size
			}
			req.Header.Set("Content-Type", ct)
			return req, nil
		}); err != nil {
			return "", k, fmt.Errorf("linkedin-page: upload %s: %w", m.Filename, err)
		}
	}
	return urn, k, c.waitAvailable(ctx, ep, urn)
}

// uploadVideoParts PUTs each byte range of the upload instructions, then finalizes with the ETags in order.
func (c *Client) uploadVideoParts(ctx context.Context, m providers.MediaFile, urn, uploadToken string, parts []struct {
	UploadURL string `json:"uploadUrl"`
	FirstByte int64  `json:"firstByte"`
	LastByte  int64  `json:"lastByte"`
}) error {
	if len(parts) == 0 {
		return errors.New("linkedin-page: video upload returned no upload instructions")
	}
	src, err := m.Open()
	if err != nil {
		return err
	}
	defer src.Close()
	var pos int64
	etags := make([]string, 0, len(parts))
	for i, p := range parts {
		if p.FirstByte < pos || p.LastByte < p.FirstByte {
			return fmt.Errorf("linkedin-page: video part %d has an invalid byte range %d-%d", i, p.FirstByte, p.LastByte)
		}
		if _, err := io.CopyN(io.Discard, src, p.FirstByte-pos); err != nil {
			return err
		}
		chunk := make([]byte, p.LastByte-p.FirstByte+1)
		if _, err := io.ReadFull(src, chunk); err != nil {
			return fmt.Errorf("linkedin-page: read video part %d: %w", i, err)
		}
		pos = p.LastByte + 1
		resp, err := c.do(ctx, func() (*http.Request, error) {
			req, err := http.NewRequestWithContext(ctx, "PUT", p.UploadURL, bytes.NewReader(chunk))
			if err == nil {
				req.Header.Set("Content-Type", "application/octet-stream")
			}
			return req, err
		})
		if err != nil {
			return fmt.Errorf("linkedin-page: upload video part %d/%d: %w", i+1, len(parts), err)
		}
		etag := strings.Trim(resp.Header.Get("ETag"), `"`)
		if etag == "" {
			return fmt.Errorf("linkedin-page: video part %d/%d returned no ETag", i+1, len(parts))
		}
		etags = append(etags, etag)
	}
	body := map[string]any{"finalizeUploadRequest": map[string]any{"video": urn, "uploadToken": uploadToken, "uploadedPartIds": etags}}
	if _, err := c.call(ctx, "POST", "/rest/videos", "action=finalizeUpload", body, nil, nil); err != nil {
		return fmt.Errorf("linkedin-page: finalize video upload: %w", err)
	}
	return nil
}

// waitAvailable polls GET /rest/{images|videos|documents}/{urn} until status is AVAILABLE.
func (c *Client) waitAvailable(ctx context.Context, ep, urn string) error {
	deadline := time.Now().Add(c.PollTimeout)
	for {
		var r struct {
			Status                  string `json:"status"`
			ProcessingFailureReason string `json:"processingFailureReason"`
		}
		if _, err := c.call(ctx, "GET", "/rest/"+ep+"/"+enc(urn), "", nil, &r, nil); err != nil {
			return fmt.Errorf("linkedin-page: read %s status: %w", ep, err)
		}
		switch r.Status {
		case "AVAILABLE":
			return nil
		case "PROCESSING_FAILED":
			return providers.Invalid(fmt.Sprintf("linkedin-page: %s processing failed: %s", strings.TrimSuffix(ep, "s"), r.ProcessingFailureReason))
		}
		if time.Now().After(deadline) {
			// Retryable: the post was not created, only an orphan upload is left behind.
			return &providers.HTTPError{Status: http.StatusServiceUnavailable, Body: fmt.Sprintf("linkedin-page: %s %s still %s after %s", ep, urn, r.Status, c.PollTimeout)}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(c.PollEvery):
		}
	}
}

// ---------- publish ----------

func postURL(urn string) string { return "https://www.linkedin.com/feed/update/" + urn }

func (c *Client) Publish(ctx context.Context, req providers.PublishRequest) (*providers.PublishResult, error) {
	// Resuming a thread (or replying under an existing post): every part is a comment of the page.
	if to, _ := req.Options["reply_to"].(string); to != "" {
		return c.comments(ctx, to, req.Parts)
	}
	if err := checkShape(req); err != nil {
		return nil, err
	}
	first := ""
	if len(req.Parts) > 0 {
		first = req.Parts[0]
	}
	post := map[string]any{
		"author":     c.orgURN(),
		"commentary": escapeLittle(first),
		"visibility": "PUBLIC",
		"distribution": map[string]any{
			"feedDistribution":               "MAIN_FEED",
			"targetEntities":                 []any{},
			"thirdPartyDistributionChannels": []any{},
		},
		"lifecycleState":            "PUBLISHED",
		"isReshareDisabledByAuthor": false,
	}
	title, _ := req.Options["title"].(string)
	type item struct {
		urn string
		alt string
	}
	var items []item
	var kind mediaKind
	for _, m := range req.Media {
		urn, k, err := c.upload(ctx, m)
		if err != nil {
			return nil, err
		}
		kind = k
		items = append(items, item{urn, m.Alt})
	}
	if link, _ := req.Options["link"].(string); link != "" {
		art := map[string]any{"source": link}
		lt, _ := req.Options["link_title"].(string)
		if lt == "" {
			lt = title
		}
		if lt == "" {
			lt = link
		}
		art["title"] = lt
		if d, _ := req.Options["link_description"].(string); d != "" {
			art["description"] = d
		}
		if len(items) == 1 {
			art["thumbnail"] = items[0].urn
		}
		post["content"] = map[string]any{"article": art}
	} else if len(items) == 1 {
		media := map[string]any{"id": items[0].urn}
		switch {
		case kind == kindImage && items[0].alt != "":
			media["altText"] = items[0].alt
		case kind == kindDocument:
			t := title
			if t == "" {
				t = strings.TrimSuffix(req.Media[0].Filename, fileExt(req.Media[0].Filename))
			}
			media["title"] = t
		case kind == kindVideo && title != "":
			media["title"] = title
		}
		post["content"] = map[string]any{"media": media}
	} else if len(items) > 1 {
		imgs := make([]map[string]any, len(items))
		for i, it := range items {
			imgs[i] = map[string]any{"id": it.urn}
			if it.alt != "" {
				imgs[i]["altText"] = it.alt
			}
		}
		post["content"] = map[string]any{"multiImage": map[string]any{"images": imgs}}
	}
	resp, err := c.call(ctx, "POST", "/rest/posts", "", post, nil, nil)
	if err != nil {
		return nil, err
	}
	urn := resp.Header.Get("x-restli-id")
	if urn == "" {
		var r struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(resp.Body, &r)
		urn = r.ID
	}
	if urn == "" {
		return nil, errors.New("linkedin-page: post created without an x-restli-id")
	}
	res := &providers.PublishResult{RemoteID: urn, RemoteIDs: []string{urn}, URL: postURL(urn)}
	if len(req.Parts) > 1 {
		more, err := c.comments(ctx, urn, req.Parts[1:])
		if more != nil {
			res.RemoteIDs = append(res.RemoteIDs, more.RemoteIDs...)
		}
		if err != nil {
			return res, fmt.Errorf("linkedin-page: thread stopped at part %d/%d: %w", len(res.RemoteIDs)+1, len(req.Parts), err)
		}
	}
	return res, nil
}

func fileExt(name string) string {
	if i := strings.LastIndexByte(name, '.'); i > 0 {
		return name[i:]
	}
	return ""
}

// comments posts each text as a first-level comment of the page under the post `to`
// (a comment URN means "the post that comment belongs to", which is how a stopped thread resumes).
func (c *Client) comments(ctx context.Context, to string, texts []string) (*providers.PublishResult, error) {
	target := to
	if act := activityOf(to); act != "" {
		target = act
	}
	res := &providers.PublishResult{}
	for _, t := range texts {
		urn, err := c.comment(ctx, target, "", t)
		if err != nil {
			if len(res.RemoteIDs) == 0 {
				return nil, err
			}
			return res, err
		}
		res.RemoteIDs = append(res.RemoteIDs, urn)
	}
	if len(res.RemoteIDs) > 0 {
		res.RemoteID = res.RemoteIDs[0]
		res.URL = commentURL(target, res.RemoteID)
	}
	return res, nil
}

var commentURNRe = regexp.MustCompile(`^urn:li:comment:\((urn:li:[a-zA-Z]+:[^,]+),([^)]+)\)$`)

// activityOf returns the thread URN embedded in a comment URN, or "".
func activityOf(urn string) string {
	if m := commentURNRe.FindStringSubmatch(urn); m != nil {
		return m[1]
	}
	return ""
}

func commentURL(post, comment string) string {
	return postURL(post) + "?commentUrn=" + url.QueryEscape(comment)
}

// comment creates a comment as the organization. The /rest/socialActions family is partner-only for
// most apps (403), while /v2/socialActions accepts the same Community Management token.
// parent, when set, makes it a nested reply to that comment URN.
func (c *Client) comment(ctx context.Context, object, parent, text string) (string, error) {
	path := object
	body := map[string]any{"actor": c.orgURN(), "object": object, "message": map[string]any{"text": text}}
	if parent != "" {
		path = parent
		body["parentComment"] = parent
	}
	var r struct {
		URN        string      `json:"$URN"`
		CommentURN string      `json:"commentUrn"`
		ID         json.Number `json:"id"`
		Object     string      `json:"object"`
	}
	resp, err := c.call(ctx, "POST", "/v2/socialActions/"+enc(path)+"/comments", "", body, &r, nil)
	if err != nil {
		return "", err
	}
	switch {
	case r.CommentURN != "":
		return r.CommentURN, nil
	case r.URN != "":
		return r.URN, nil
	}
	id := r.ID.String()
	if id == "" {
		id = resp.Header.Get("x-restli-id")
	}
	if id == "" {
		return "", errors.New("linkedin-page: comment created without an id")
	}
	if strings.HasPrefix(id, "urn:li:comment:") {
		return id, nil
	}
	thread := r.Object
	if thread == "" {
		thread = object
	}
	return "urn:li:comment:(" + thread + "," + id + ")", nil
}

// Reply comments on a post (share/ugcPost URN) or answers a comment (comment URN) as the page.
func (c *Client) Reply(ctx context.Context, to, text string) (*providers.PublishResult, error) {
	if strings.TrimSpace(text) == "" {
		return nil, providers.Invalid("linkedin-page: empty reply")
	}
	if n := providers.CharCount(text); n > maxComment {
		return nil, providers.Invalid(fmt.Sprintf("linkedin-page: comments are limited to %d characters (got %d)", maxComment, n))
	}
	object, parent := to, ""
	if act := activityOf(to); act != "" {
		object, parent = act, to
	}
	urn, err := c.comment(ctx, object, parent, text)
	if err != nil {
		return nil, err
	}
	return &providers.PublishResult{RemoteID: urn, RemoteIDs: []string{urn}, URL: commentURL(object, urn)}, nil
}

// ---------- comments ----------

type liComment struct {
	URN        string      `json:"$URN"`
	CommentURN string      `json:"commentUrn"`
	ID         json.Number `json:"id"`
	Actor      string      `json:"actor"`
	Object     string      `json:"object"`
	Message    struct {
		Text string `json:"text"`
	} `json:"message"`
	Created struct {
		Time int64 `json:"time"`
	} `json:"created"`
}

func (cm liComment) urn() string {
	switch {
	case cm.CommentURN != "":
		return cm.CommentURN
	case cm.URN != "":
		return cm.URN
	case cm.Object != "" && cm.ID.String() != "":
		return "urn:li:comment:(" + cm.Object + "," + cm.ID.String() + ")"
	}
	return cm.ID.String()
}

// Comments lists the first-level comments of a post through /v2/socialActions/{urn}/comments,
// skipping the page's own comments. Authors are member/organization URNs: resolving member names
// needs profile permissions this app does not have.
func (c *Client) Comments(ctx context.Context, remoteID string) ([]providers.Comment, error) {
	var out []providers.Comment
	for start := 0; start < 10*pageSize; start += pageSize {
		var r struct {
			Elements []liComment `json:"elements"`
			Paging   struct {
				Total int `json:"total"`
			} `json:"paging"`
		}
		q := fmt.Sprintf("start=%d&count=%d", start, pageSize)
		if _, err := c.call(ctx, "GET", "/v2/socialActions/"+enc(remoteID)+"/comments", q, nil, &r, nil); err != nil {
			return out, err
		}
		for _, cm := range r.Elements {
			if cm.Actor == c.orgURN() {
				continue
			}
			var at *time.Time
			if cm.Created.Time > 0 {
				t := time.UnixMilli(cm.Created.Time).UTC()
				at = &t
			}
			author := cm.Actor
			if author == "" {
				author = "unknown"
			}
			urn := cm.urn()
			out = append(out, providers.Comment{RemoteID: urn, Author: author, Text: cm.Message.Text, URL: commentURL(remoteID, urn), CreatedAt: at})
		}
		if len(r.Elements) < pageSize || (r.Paging.Total > 0 && start+pageSize >= r.Paging.Total) {
			break
		}
	}
	return out, nil
}

// ---------- metrics ----------

// Metrics reads lifetime organic statistics of shares and ugcPosts (organizationalEntityShareStatistics,
// rw_organization_admin). Posts absent from the answer have no activity yet: their counters are 0.
func (c *Client) Metrics(ctx context.Context, ids []string) (map[string]map[string]int64, error) {
	var shares, ugc []string
	for _, id := range ids {
		switch {
		case strings.HasPrefix(id, "urn:li:share:"):
			shares = append(shares, id)
		case strings.HasPrefix(id, "urn:li:ugcPost:"):
			ugc = append(ugc, id)
		}
	}
	out := map[string]map[string]int64{}
	for _, group := range []struct {
		param string
		urns  []string
	}{{"shares", shares}, {"ugcPosts", ugc}} {
		for i := 0; i < len(group.urns); i += statsBatch {
			batch := group.urns[i:min(i+statsBatch, len(group.urns))]
			if err := c.stats(ctx, group.param, batch, out); err != nil {
				return out, err
			}
		}
	}
	return out, nil
}

func (c *Client) stats(ctx context.Context, param string, urns []string, out map[string]map[string]int64) error {
	encd := make([]string, len(urns))
	for i, u := range urns {
		encd[i] = enc(u)
		out[u] = map[string]int64{"impression_count": 0, "unique_impressions": 0, "clicks": 0, "like_count": 0, "comment_count": 0, "share_count": 0}
	}
	q := "q=organizationalEntity&organizationalEntity=" + enc(c.orgURN()) + "&" + param + "=List(" + strings.Join(encd, ",") + ")"
	var r struct {
		Elements []struct {
			Share   string `json:"share"`
			UGCPost string `json:"ugcPost"`
			Stats   struct {
				ImpressionCount        int64   `json:"impressionCount"`
				UniqueImpressionsCount int64   `json:"uniqueImpressionsCount"`
				ClickCount             int64   `json:"clickCount"`
				LikeCount              int64   `json:"likeCount"`
				CommentCount           int64   `json:"commentCount"`
				ShareCount             int64   `json:"shareCount"`
				Engagement             float64 `json:"engagement"`
			} `json:"totalShareStatistics"`
		} `json:"elements"`
	}
	if _, err := c.call(ctx, "GET", "/rest/organizationalEntityShareStatistics", q, nil, &r, nil); err != nil {
		return err
	}
	for _, e := range r.Elements {
		id := e.Share + e.UGCPost
		if id == "" {
			continue
		}
		s := e.Stats
		out[id] = map[string]int64{
			"impression_count":   s.ImpressionCount,
			"unique_impressions": s.UniqueImpressionsCount,
			"clicks":             s.ClickCount,
			"like_count":         s.LikeCount,
			"comment_count":      s.CommentCount,
			"share_count":        s.ShareCount,
			// LinkedIn's own rate ((clicks+likes+comments+shares)/impressions), in parts per million.
			"engagement_ppm": int64(s.Engagement*1e6 + 0.5),
		}
	}
	return nil
}

// ---------- history ----------

type liPost struct {
	ID             string `json:"id"`
	Commentary     string `json:"commentary"`
	CreatedAt      int64  `json:"createdAt"`
	PublishedAt    int64  `json:"publishedAt"`
	LifecycleState string `json:"lifecycleState"`
	Distribution   struct {
		FeedDistribution string `json:"feedDistribution"`
	} `json:"distribution"`
	Content struct {
		Media *struct {
			ID string `json:"id"`
		} `json:"media"`
		MultiImage *json.RawMessage `json:"multiImage"`
		Article    *json.RawMessage `json:"article"`
		Poll       *json.RawMessage `json:"poll"`
	} `json:"content"`
	ReshareContext *struct {
		Parent string `json:"parent"`
	} `json:"reshareContext"`
}

func (p liPost) kind() string {
	switch {
	case p.ReshareContext != nil && p.ReshareContext.Parent != "":
		if strings.TrimSpace(p.Commentary) == "" {
			return "repost"
		}
		return "quote"
	case p.Content.MultiImage != nil:
		return "carousel"
	case p.Content.Article != nil:
		return "article"
	case p.Content.Poll != nil:
		return "poll"
	case p.Content.Media != nil:
		switch id := p.Content.Media.ID; {
		case strings.HasPrefix(id, "urn:li:video:"):
			return "video"
		case strings.HasPrefix(id, "urn:li:document:"):
			return "document"
		case strings.HasPrefix(id, "urn:li:image:"):
			return "image"
		}
	}
	return "text"
}

// History lists the page's organic posts, newest first (GET /rest/posts?q=author, sortBy=CREATED).
func (c *Client) History(ctx context.Context, since time.Time, max int) ([]providers.HistoryItem, error) {
	if max <= 0 {
		max = 2000
	}
	var out []providers.HistoryItem
	done := false
	for start := 0; !done && len(out) < max; start += pageSize {
		var r struct {
			Elements []liPost `json:"elements"`
		}
		q := fmt.Sprintf("q=author&author=%s&sortBy=CREATED&start=%d&count=%d", enc(c.orgURN()), start, pageSize)
		if _, err := c.call(ctx, "GET", "/rest/posts", q, nil, &r, map[string]string{"X-RestLi-Method": "FINDER"}); err != nil {
			return out, err
		}
		for _, p := range r.Elements {
			ts := p.PublishedAt
			if ts == 0 {
				ts = p.CreatedAt
			}
			at := time.UnixMilli(ts).UTC()
			if !since.IsZero() && at.Before(since) {
				done = true
				break
			}
			// Dark posts (feedDistribution NONE) are ad creatives, not page posts.
			if p.LifecycleState != "" && p.LifecycleState != "PUBLISHED" || p.Distribution.FeedDistribution == "NONE" {
				continue
			}
			out = append(out, providers.HistoryItem{RemoteID: p.ID, Text: unescapeLittle(p.Commentary), URL: postURL(p.ID), CreatedAt: at, Kind: p.kind()})
			if len(out) >= max {
				break
			}
		}
		// The finder can return a short page before the end, so only an empty page ends the walk.
		if len(r.Elements) == 0 {
			break
		}
	}
	ids := make([]string, len(out))
	for i, it := range out {
		ids[i] = it.RemoteID
	}
	if m, err := c.Metrics(ctx, ids); err == nil {
		for i := range out {
			out[i].Metrics = m[out[i].RemoteID]
		}
	}
	return out, nil
}
