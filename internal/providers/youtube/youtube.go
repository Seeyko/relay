// Package youtube is the YouTube connector (Data API v3 + Analytics API v2, OAuth 2.0 with refresh tokens).
//
// Docs:
//   - https://developers.google.com/youtube/v3/docs
//   - https://developers.google.com/youtube/v3/guides/using_resumable_upload_protocol
//   - https://developers.google.com/youtube/analytics/reference/reports/query
//   - https://developers.google.com/identity/protocols/oauth2/web-server#offline
package youtube

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Seeyko/relay/internal/providers"
)

const (
	DefaultBaseURL          = "https://www.googleapis.com"
	DefaultUploadBaseURL    = "https://www.googleapis.com"
	DefaultAnalyticsBaseURL = "https://youtubeanalytics.googleapis.com"
	DefaultTokenURL         = "https://oauth2.googleapis.com/token"
)

// Documented metadata limits (video resource reference).
const (
	maxTitleChars     = 100
	maxDescBytes      = 5000
	maxTagsChars      = 500
	shortsMaxDuration = 180 // seconds
	videoBatch        = 50  // ids per videos.list call
	analyticsBatch    = 200 // ids per Analytics query (the API accepts up to 500)
)

func init() {
	providers.Register(providers.Factory{
		Name: "youtube",
		Description: "YouTube via the Data API v3 and Analytics API v2 (OAuth 2.0, offline access). Scopes: youtube.upload, youtube.force-ssl, " +
			"youtube.readonly, yt-analytics.readonly. Publishes exactly one video (resumable upload); the title is options.title or the first line " +
			"of the text, the description is the text. Options: title, privacy_status (public|unlisted|private, default public), category_id, " +
			"tags (list), made_for_kids (default false), notify_subscribers (default true), shorts (vertical video of 3 minutes or less; adds #Shorts). " +
			"Per-video views/likes/comments plus watch time, average view duration and subscribers gained (Analytics, best effort), " +
			"subscriber/video/view counts, comments and replies, history import. Quota: 10,000 units/day; uploads have their own bucket; " +
			"replies cost 50 units. A refresh token of an app in 'Testing' status expires after 7 days.",
		CredentialFields: []string{"client_id", "client_secret", "refresh_token", "access_token"},
		Validate:         validate,
		New: func(c map[string]string, persist func(map[string]string) error, hc *http.Client) (providers.Client, error) {
			if c["access_token"] == "" && (c["refresh_token"] == "" || c["client_id"] == "") {
				return nil, errors.New("youtube: client_id and refresh_token (or an access_token) are required")
			}
			if hc == nil {
				hc = http.DefaultClient
			}
			return &Client{
				Base:          host(c["base_url"], DefaultBaseURL),
				UploadBase:    host(c["upload_base_url"], DefaultUploadBaseURL),
				AnalyticsBase: host(c["analytics_base_url"], DefaultAnalyticsBaseURL),
				TokenURL:      host(c["token_url"], DefaultTokenURL),
				HC:            hc,
				creds:         c,
				persist:       persist,
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

// Client is bound to one channel.
type Client struct {
	Base, UploadBase, AnalyticsBase, TokenURL string
	HC                                        *http.Client
	// RetryWait is the base backoff between upload resume attempts (tests set it low).
	RetryWait time.Duration

	creds   map[string]string
	persist func(map[string]string) error
	mu      sync.Mutex
	avatar  string // channel picture seen by the last Identify or AccountMetrics call
}

// ---------- validation ----------

type meta struct {
	Title, Description string
	Tags               []string
	CategoryID         string
	Privacy            string
	MadeForKids        bool
	Shorts             bool
}

func buildMeta(req providers.PublishRequest) meta {
	o := req.Options
	desc := strings.TrimSpace(strings.Join(req.Parts, "\n\n"))
	m := meta{Description: desc, Privacy: "public"}
	if t, _ := o["title"].(string); strings.TrimSpace(t) != "" {
		m.Title = strings.TrimSpace(t)
	} else {
		m.Title = firstLine(desc, maxTitleChars)
	}
	if p, _ := o["privacy_status"].(string); p != "" {
		m.Privacy = strings.ToLower(p)
	}
	m.CategoryID = stringOpt(o["category_id"])
	m.MadeForKids, _ = o["made_for_kids"].(bool)
	m.Shorts, _ = o["shorts"].(bool)
	m.Tags = stringList(o["tags"])
	if m.Shorts && !strings.Contains(strings.ToLower(m.Title+" "+m.Description), "#shorts") {
		if m.Description == "" {
			m.Description = "#Shorts"
		} else {
			m.Description += "\n\n#Shorts"
		}
	}
	return m
}

func validate(req providers.PublishRequest) []string {
	var errs []string
	videos := 0
	for _, f := range req.Media {
		if strings.HasPrefix(f.Mime, "video/") || f.Mime == "application/octet-stream" {
			videos++
		}
	}
	if len(req.Media) != 1 || videos != 1 {
		errs = append(errs, "YouTube posts take exactly one video")
	}
	if len(req.Parts) > 1 {
		errs = append(errs, "YouTube does not support threads")
	}
	m := buildMeta(req)
	switch {
	case m.Title == "":
		errs = append(errs, "YouTube needs a title (options.title or a first line of text)")
	case providers.CharCount(m.Title) > maxTitleChars:
		errs = append(errs, fmt.Sprintf("YouTube title is limited to %d characters", maxTitleChars))
	}
	if strings.ContainsAny(m.Title, "<>") {
		errs = append(errs, "YouTube title cannot contain < or >")
	}
	if len(m.Description) > maxDescBytes {
		errs = append(errs, fmt.Sprintf("YouTube description is limited to %d bytes (UTF-8)", maxDescBytes))
	}
	if strings.ContainsAny(m.Description, "<>") {
		errs = append(errs, "YouTube description cannot contain < or >")
	}
	if n := tagsLength(m.Tags); n > maxTagsChars {
		errs = append(errs, fmt.Sprintf("YouTube tags total %d characters (limit %d, commas and quotes around tags with spaces count)", n, maxTagsChars))
	}
	switch m.Privacy {
	case "public", "unlisted", "private":
	default:
		errs = append(errs, "YouTube privacy_status must be public, unlisted or private")
	}
	if m.CategoryID != "" {
		if _, err := strconv.Atoi(m.CategoryID); err != nil {
			errs = append(errs, "YouTube category_id must be a numeric category id (see videoCategories.list)")
		}
	}
	return errs
}

// tagsLength counts tags the way YouTube does: commas between tags count, and a tag
// containing a space counts as if it were wrapped in quotes.
func tagsLength(tags []string) int {
	n := 0
	for i, t := range tags {
		if i > 0 {
			n++
		}
		n += providers.CharCount(t)
		if strings.Contains(t, " ") {
			n += 2
		}
	}
	return n
}

func stringOpt(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatInt(int64(x), 10)
	case int:
		return strconv.Itoa(x)
	}
	return ""
}

func stringList(v any) []string {
	var out []string
	add := func(s string) {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	switch x := v.(type) {
	case []string:
		for _, s := range x {
			add(s)
		}
	case []any:
		for _, s := range x {
			if s, ok := s.(string); ok {
				add(s)
			}
		}
	case string:
		for _, s := range strings.Split(x, ",") {
			add(s)
		}
	}
	return out
}

func firstLine(s string, max int) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	r := []rune(strings.TrimSpace(s))
	if len(r) > max {
		r = r[:max]
	}
	return strings.TrimSpace(string(r))
}

// ---------- auth and transport ----------

// token returns a valid access token, refreshing it (about 1h lifetime) when needed.
func (c *Client) token(ctx context.Context, force bool) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	exp, _ := strconv.ParseInt(c.creds["expires_at"], 10, 64)
	if !force && c.creds["access_token"] != "" && (exp == 0 || time.Now().Unix() < exp-120) {
		return c.creds["access_token"], nil
	}
	if c.creds["refresh_token"] == "" || c.creds["client_id"] == "" {
		if c.creds["access_token"] != "" && !force {
			return c.creds["access_token"], nil
		}
		return "", errors.New("youtube: access token expired and no refresh_token/client_id to renew it")
	}
	form := url.Values{
		"client_id":     {c.creds["client_id"]},
		"grant_type":    {"refresh_token"},
		"refresh_token": {c.creds["refresh_token"]},
	}
	if s := c.creds["client_secret"]; s != "" {
		form.Set("client_secret", s)
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
		if errors.As(err, &he) && strings.Contains(he.Body, "invalid_grant") {
			// Revoked or expired refresh token: only a new consent fixes it.
			return "", providers.Invalid("youtube: refresh token rejected (invalid_grant), reconnect the account")
		}
		return "", err
	}
	var r struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return "", err
	}
	if r.AccessToken == "" {
		return "", errors.New("youtube: token refresh returned no access_token")
	}
	c.creds["access_token"] = r.AccessToken
	if r.RefreshToken != "" {
		c.creds["refresh_token"] = r.RefreshToken
	}
	c.creds["expires_at"] = strconv.FormatInt(time.Now().Unix()+r.ExpiresIn, 10)
	if c.persist != nil {
		_ = c.persist(c.creds)
	}
	return r.AccessToken, nil
}

// googleError is the standard Google API error envelope.
type googleError struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Errors  []struct {
			Reason string `json:"reason"`
		} `json:"errors"`
	} `json:"error"`
}

func reasonOf(body string) string {
	var ge googleError
	if json.Unmarshal([]byte(body), &ge) == nil && len(ge.Error.Errors) > 0 {
		return ge.Error.Errors[0].Reason
	}
	return ""
}

// mapError turns documented error reasons into Relay semantics: quota errors are retried later,
// metadata errors are never retried.
func mapError(err error) error {
	var he *providers.HTTPError
	if !errors.As(err, &he) {
		return err
	}
	switch reasonOf(he.Body) {
	case "quotaExceeded", "rateLimitExceeded", "userRateLimitExceeded", "uploadLimitExceeded":
		return &providers.HTTPError{Status: http.StatusTooManyRequests, Body: he.Body}
	case "invalidTitle", "invalidDescription", "invalidTags", "invalidCategoryId", "invalidVideoMetadata",
		"mediaBodyRequired", "invalidFilename", "defaultLanguageNotSet", "invalidRecordingDetails",
		"commentTextRequired", "commentTextTooLong", "invalidCustomEmoji", "parentCommentIsPrivate",
		"parentCommentNotFound", "parentIdMissing", "operationNotSupported":
		return providers.Invalid("youtube: " + strings.TrimSpace(he.Error()))
	}
	return err
}

// do sends an authorized request built by mk (called again after a token refresh) and decodes JSON into out.
func (c *Client) do(ctx context.Context, mk func() (*http.Request, error), out any) error {
	for attempt := 0; attempt < 2; attempt++ {
		tok, err := c.token(ctx, attempt > 0)
		if err != nil {
			return err
		}
		req, err := mk()
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := c.HC.Do(req)
		if err != nil {
			return err
		}
		b, err := providers.ReadBody(resp)
		if err != nil {
			var he *providers.HTTPError
			if attempt == 0 && errors.As(err, &he) && he.Status == http.StatusUnauthorized && c.creds["refresh_token"] != "" {
				continue
			}
			return mapError(err)
		}
		if out != nil && len(b) > 0 {
			return json.Unmarshal(b, out)
		}
		return nil
	}
	return errors.New("youtube: authentication failed")
}

func (c *Client) get(ctx context.Context, base, path string, q url.Values, out any) error {
	u := base + path + "?" + q.Encode()
	return c.do(ctx, func() (*http.Request, error) { return http.NewRequestWithContext(ctx, "GET", u, nil) }, out)
}

func (c *Client) postJSON(ctx context.Context, path string, q url.Values, body, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	u := c.Base + path + "?" + q.Encode()
	return c.do(ctx, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, "POST", u, bytes.NewReader(raw))
		if err == nil {
			req.Header.Set("Content-Type", "application/json; charset=UTF-8")
		}
		return req, err
	}, out)
}

// count decodes the unsigned 64-bit counters that the API serializes as JSON strings.
type count int64

func (n *count) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*n = 0
		return nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	*n = count(v)
	return err
}

// ---------- channel ----------

type channel struct {
	ID      string `json:"id"`
	Snippet struct {
		Title     string `json:"title"`
		CustomURL string `json:"customUrl"`
		// Thumbnails is the channel picture: default (88px), medium (240px), high (800px).
		Thumbnails map[string]struct {
			URL string `json:"url"`
		} `json:"thumbnails"`
	} `json:"snippet"`
	ContentDetails struct {
		RelatedPlaylists struct {
			Uploads string `json:"uploads"`
		} `json:"relatedPlaylists"`
	} `json:"contentDetails"`
	Statistics struct {
		ViewCount             count `json:"viewCount"`
		SubscriberCount       count `json:"subscriberCount"`
		HiddenSubscriberCount bool  `json:"hiddenSubscriberCount"`
		VideoCount            count `json:"videoCount"`
	} `json:"statistics"`
}

func (c *Client) myChannel(ctx context.Context, parts string) (*channel, error) {
	var r struct {
		Items []channel `json:"items"`
	}
	if err := c.get(ctx, c.Base, "/youtube/v3/channels", url.Values{"part": {parts}, "mine": {"true"}}, &r); err != nil {
		return nil, err
	}
	if len(r.Items) == 0 {
		return nil, providers.Invalid("youtube: this Google account has no YouTube channel")
	}
	ch := &r.Items[0]
	c.mu.Lock()
	if up := ch.ContentDetails.RelatedPlaylists.Uploads; up != "" {
		c.creds["uploads_playlist_id"] = up
	}
	c.mu.Unlock()
	return ch, nil
}

func (c *Client) Identify(ctx context.Context) (*providers.Identity, error) {
	ch, err := c.myChannel(ctx, "id,snippet,contentDetails")
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.creds["channel_id"] = ch.ID
	c.avatar = ch.picture()
	c.mu.Unlock()
	return &providers.Identity{RemoteUserID: ch.ID, Handle: ch.Snippet.CustomURL, Name: ch.Snippet.Title, AvatarURL: ch.picture()}, nil
}

// picture is the largest channel thumbnail.
func (ch *channel) picture() string {
	for _, k := range []string{"high", "medium", "default"} {
		if t, ok := ch.Snippet.Thumbnails[k]; ok && t.URL != "" {
			return t.URL
		}
	}
	return ""
}

// AvatarURL is the channel picture (channels?part=snippet&mine=true, snippet.thumbnails.high).
func (c *Client) AvatarURL(ctx context.Context) (string, error) {
	c.mu.Lock()
	av := c.avatar
	c.mu.Unlock()
	if av != "" {
		return av, nil
	}
	ch, err := c.myChannel(ctx, "snippet")
	if err != nil {
		return "", err
	}
	return ch.picture(), nil
}

// AccountMetrics reads the channel statistics, and its snippet for the picture: the same call and
// quota cost (1 unit), so the daily avatar refresh is free.
func (c *Client) AccountMetrics(ctx context.Context) (map[string]int64, error) {
	ch, err := c.myChannel(ctx, "statistics,snippet")
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.avatar = ch.picture()
	c.mu.Unlock()
	out := map[string]int64{"posts": int64(ch.Statistics.VideoCount), "views_total": int64(ch.Statistics.ViewCount)}
	if !ch.Statistics.HiddenSubscriberCount {
		out["followers"] = int64(ch.Statistics.SubscriberCount)
	}
	return out, nil
}

// ---------- publish ----------

type videoResource struct {
	ID      string `json:"id"`
	Snippet struct {
		PublishedAt time.Time `json:"publishedAt"`
		Title       string    `json:"title"`
		Description string    `json:"description"`
		// Thumbnails is keyed by size: default, medium, high, standard, maxres (not all always present).
		Thumbnails map[string]struct {
			URL string `json:"url"`
		} `json:"thumbnails"`
	} `json:"snippet"`
	ContentDetails struct {
		Duration string `json:"duration"`
	} `json:"contentDetails"`
	Status struct {
		UploadStatus    string `json:"uploadStatus"`
		RejectionReason string `json:"rejectionReason"`
		FailureReason   string `json:"failureReason"`
	} `json:"status"`
	Statistics struct {
		ViewCount     count `json:"viewCount"`
		LikeCount     count `json:"likeCount"`
		FavoriteCount count `json:"favoriteCount"`
		CommentCount  count `json:"commentCount"`
	} `json:"statistics"`
}

func (v videoResource) metrics() map[string]int64 {
	return map[string]int64{
		"view_count":     int64(v.Statistics.ViewCount),
		"like_count":     int64(v.Statistics.LikeCount),
		"comment_count":  int64(v.Statistics.CommentCount),
		"favorite_count": int64(v.Statistics.FavoriteCount),
	}
}

func (c *Client) Publish(ctx context.Context, req providers.PublishRequest) (*providers.PublishResult, error) {
	if errs := validate(req); len(errs) > 0 {
		return nil, providers.Invalid(strings.Join(errs, "; "))
	}
	m := buildMeta(req)
	snippet := map[string]any{"title": m.Title, "description": m.Description}
	if len(m.Tags) > 0 {
		snippet["tags"] = m.Tags
	}
	if m.CategoryID != "" {
		snippet["categoryId"] = m.CategoryID
	}
	body := map[string]any{
		"snippet": snippet,
		"status":  map[string]any{"privacyStatus": m.Privacy, "selfDeclaredMadeForKids": m.MadeForKids},
	}
	q := url.Values{"uploadType": {"resumable"}, "part": {"snippet,status"}}
	if v, ok := req.Options["notify_subscribers"].(bool); ok {
		q.Set("notifySubscribers", strconv.FormatBool(v))
	}
	v, err := c.upload(ctx, req.Media[0], q, body)
	if err != nil {
		return nil, err
	}
	switch v.Status.UploadStatus {
	case "rejected":
		return nil, providers.Invalid("youtube: upload rejected: " + v.Status.RejectionReason)
	case "failed":
		return nil, providers.Invalid("youtube: upload failed: " + v.Status.FailureReason)
	}
	u := "https://www.youtube.com/watch?v=" + v.ID
	if m.Shorts {
		u = "https://www.youtube.com/shorts/" + v.ID
	}
	return &providers.PublishResult{RemoteID: v.ID, RemoteIDs: []string{v.ID}, URL: u}, nil
}

// upload runs the resumable upload protocol: an initial POST returns a session URI in Location,
// the file is PUT there, and after a network error or 5xx the client asks how many bytes arrived
// (Content-Range: bytes */TOTAL, answered by 308 + Range) and resumes from there.
func (c *Client) upload(ctx context.Context, f providers.MediaFile, q url.Values, body any) (*videoResource, error) {
	mime := f.Mime
	if !strings.HasPrefix(mime, "video/") {
		mime = "application/octet-stream"
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	var session string
	initURL := c.UploadBase + "/upload/youtube/v3/videos?" + q.Encode()
	err = c.doRaw(ctx, func() (*http.Request, error) {
		r, err := http.NewRequestWithContext(ctx, "POST", initURL, bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		r.Header.Set("Content-Type", "application/json; charset=UTF-8")
		r.Header.Set("X-Upload-Content-Length", strconv.FormatInt(f.Size, 10))
		r.Header.Set("X-Upload-Content-Type", mime)
		return r, nil
	}, func(resp *http.Response) error {
		if _, err := providers.ReadBody(resp); err != nil {
			return err
		}
		session = resp.Header.Get("Location")
		if session == "" {
			return errors.New("youtube: upload session has no Location header")
		}
		return nil
	})
	if err != nil {
		return nil, mapError(err)
	}

	wait := c.RetryWait
	if wait <= 0 {
		wait = 2 * time.Second
	}
	var offset int64
	for attempt := 0; ; attempt++ {
		v, done, err := c.putFrom(ctx, session, f, mime, offset)
		if done {
			return v, err
		}
		if attempt >= 5 {
			return nil, fmt.Errorf("youtube: upload interrupted too many times: %w", err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait << attempt):
		}
		var complete *videoResource
		offset, complete, err = c.uploadStatus(ctx, session, f.Size)
		if err != nil {
			return nil, err
		}
		if complete != nil {
			return complete, nil
		}
	}
}

// putFrom sends the file from offset to the end. done=false means the upload can be resumed.
func (c *Client) putFrom(ctx context.Context, session string, f providers.MediaFile, mime string, offset int64) (*videoResource, bool, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, true, err
	}
	defer rc.Close()
	if offset > 0 {
		if s, ok := rc.(io.Seeker); ok {
			_, err = s.Seek(offset, io.SeekStart)
		} else {
			_, err = io.CopyN(io.Discard, rc, offset)
		}
		if err != nil {
			return nil, true, fmt.Errorf("youtube: seek video: %w", err)
		}
	}
	var v videoResource
	var retry bool
	err = c.doRaw(ctx, func() (*http.Request, error) {
		// Wrapping the reader hides it from net/http so a 308 is never followed as a redirect.
		r, err := http.NewRequestWithContext(ctx, "PUT", session, struct{ io.Reader }{rc})
		if err != nil {
			return nil, err
		}
		r.ContentLength = f.Size - offset
		r.Header.Set("Content-Type", mime)
		if offset > 0 {
			r.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, f.Size-1, f.Size))
		}
		return r, nil
	}, func(resp *http.Response) error {
		if resp.StatusCode == http.StatusPermanentRedirect {
			resp.Body.Close()
			retry = true
			return errors.New("youtube: upload incomplete")
		}
		b, err := providers.ReadBody(resp)
		if err != nil {
			return err
		}
		return json.Unmarshal(b, &v)
	})
	if err == nil {
		return &v, true, nil
	}
	var he *providers.HTTPError
	if errors.As(err, &he) {
		switch he.Status {
		case 500, 502, 503, 504:
			return nil, false, err
		}
		return nil, true, mapError(err)
	}
	if retry || ctx.Err() == nil {
		return nil, false, err // network error: resumable
	}
	return nil, true, err
}

// uploadStatus asks the session how many bytes it holds. It returns the next offset, or the video when complete.
func (c *Client) uploadStatus(ctx context.Context, session string, total int64) (int64, *videoResource, error) {
	var next int64
	var v *videoResource
	err := c.doRaw(ctx, func() (*http.Request, error) {
		r, err := http.NewRequestWithContext(ctx, "PUT", session, http.NoBody)
		if err != nil {
			return nil, err
		}
		r.ContentLength = 0
		r.Header.Set("Content-Range", fmt.Sprintf("bytes */%d", total))
		return r, nil
	}, func(resp *http.Response) error {
		if resp.StatusCode == http.StatusPermanentRedirect {
			resp.Body.Close()
			// Range: bytes=0-999999 (inclusive); absent when nothing was received.
			if rg := resp.Header.Get("Range"); rg != "" {
				if i := strings.LastIndexByte(rg, '-'); i >= 0 {
					end, err := strconv.ParseInt(rg[i+1:], 10, 64)
					if err != nil {
						return fmt.Errorf("youtube: bad Range header %q", rg)
					}
					next = end + 1
				}
			}
			return nil
		}
		b, err := providers.ReadBody(resp)
		if err != nil {
			return err
		}
		v = &videoResource{}
		return json.Unmarshal(b, v)
	})
	if err != nil {
		var he *providers.HTTPError
		if errors.As(err, &he) && he.Status == http.StatusNotFound {
			return 0, nil, errors.New("youtube: upload session expired, the upload will restart")
		}
		return 0, nil, mapError(err)
	}
	return next, v, nil
}

// doRaw sends an authorized request and hands the raw response to handle, refreshing the token once on 401.
func (c *Client) doRaw(ctx context.Context, mk func() (*http.Request, error), handle func(*http.Response) error) error {
	for attempt := 0; attempt < 2; attempt++ {
		tok, err := c.token(ctx, attempt > 0)
		if err != nil {
			return err
		}
		req, err := mk()
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := c.HC.Do(req)
		if err != nil {
			return err
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 && c.creds["refresh_token"] != "" && req.GetBody != nil {
			resp.Body.Close()
			continue
		}
		return handle(resp)
	}
	return errors.New("youtube: authentication failed")
}

// ---------- metrics ----------

func (c *Client) videos(ctx context.Context, ids []string, parts string) ([]videoResource, error) {
	var out []videoResource
	for start := 0; start < len(ids); start += videoBatch {
		end := min(start+videoBatch, len(ids))
		var r struct {
			Items []videoResource `json:"items"`
		}
		q := url.Values{"part": {parts}, "id": {strings.Join(ids[start:end], ",")}}
		if err := c.get(ctx, c.Base, "/youtube/v3/videos", q, &r); err != nil {
			return out, err
		}
		out = append(out, r.Items...)
	}
	return out, nil
}

func (c *Client) Metrics(ctx context.Context, ids []string) (map[string]map[string]int64, error) {
	out := map[string]map[string]int64{}
	vs, err := c.videos(ctx, ids, "statistics")
	for _, v := range vs {
		out[v.ID] = v.metrics()
	}
	if err != nil {
		return out, err
	}
	c.analytics(ctx, ids, out) // best effort: needs yt-analytics.readonly, data lags 1-2 days
	return out, nil
}

// analytics adds lifetime watch-time metrics from the YouTube Analytics API. Errors are ignored.
func (c *Client) analytics(ctx context.Context, ids []string, out map[string]map[string]int64) {
	names := map[string]string{
		"estimatedMinutesWatched": "estimated_minutes_watched",
		"averageViewDuration":     "average_view_duration",
		"subscribersGained":       "subscribers_gained",
		"shares":                  "share_count",
	}
	for start := 0; start < len(ids); start += analyticsBatch {
		end := min(start+analyticsBatch, len(ids))
		q := url.Values{
			"ids":        {"channel==MINE"},
			"startDate":  {"2005-02-14"},
			"endDate":    {time.Now().UTC().Format("2006-01-02")},
			"metrics":    {"views,estimatedMinutesWatched,averageViewDuration,subscribersGained,shares"},
			"dimensions": {"video"},
			"filters":    {"video==" + strings.Join(ids[start:end], ",")},
			"sort":       {"-views"},
			"maxResults": {strconv.Itoa(end - start)},
		}
		var r struct {
			ColumnHeaders []struct {
				Name string `json:"name"`
			} `json:"columnHeaders"`
			Rows [][]any `json:"rows"`
		}
		if err := c.get(ctx, c.AnalyticsBase, "/v2/reports", q, &r); err != nil {
			return
		}
		for _, row := range r.Rows {
			if len(row) != len(r.ColumnHeaders) || len(row) == 0 {
				continue
			}
			id, _ := row[0].(string)
			m := out[id]
			if m == nil {
				continue
			}
			for i, h := range r.ColumnHeaders {
				if key, ok := names[h.Name]; ok {
					if f, ok := row[i].(float64); ok {
						m[key] = int64(f + 0.5)
					}
				}
			}
		}
	}
}

// ---------- comments ----------

type comment struct {
	ID      string `json:"id"`
	Snippet struct {
		AuthorDisplayName string    `json:"authorDisplayName"`
		TextDisplay       string    `json:"textDisplay"`
		TextOriginal      string    `json:"textOriginal"`
		PublishedAt       time.Time `json:"publishedAt"`
	} `json:"snippet"`
}

func (cm comment) toProvider(videoID string) providers.Comment {
	t := cm.Snippet.PublishedAt
	text := cm.Snippet.TextDisplay // plain text (textFormat=plainText); textOriginal is only shown to its author
	if text == "" {
		text = cm.Snippet.TextOriginal
	}
	return providers.Comment{
		RemoteID: cm.ID, Author: cm.Snippet.AuthorDisplayName, Text: text, CreatedAt: &t,
		URL: "https://www.youtube.com/watch?v=" + videoID + "&lc=" + cm.ID,
	}
}

// Comments returns top-level comments and the replies embedded in each thread (up to 500 threads).
func (c *Client) Comments(ctx context.Context, videoID string) ([]providers.Comment, error) {
	var out []providers.Comment
	page := ""
	for i := 0; i < 5; i++ {
		q := url.Values{
			"part": {"snippet,replies"}, "videoId": {videoID}, "maxResults": {"100"},
			"order": {"time"}, "textFormat": {"plainText"},
		}
		if page != "" {
			q.Set("pageToken", page)
		}
		var r struct {
			NextPageToken string `json:"nextPageToken"`
			Items         []struct {
				Snippet struct {
					TopLevelComment comment `json:"topLevelComment"`
				} `json:"snippet"`
				Replies struct {
					Comments []comment `json:"comments"`
				} `json:"replies"`
			} `json:"items"`
		}
		if err := c.get(ctx, c.Base, "/youtube/v3/commentThreads", q, &r); err != nil {
			var he *providers.HTTPError
			if errors.As(err, &he) && reasonOf(he.Body) == "commentsDisabled" {
				return out, nil
			}
			return out, err
		}
		for _, th := range r.Items {
			out = append(out, th.Snippet.TopLevelComment.toProvider(videoID))
			for _, rep := range th.Replies.Comments {
				out = append(out, rep.toProvider(videoID))
			}
		}
		if r.NextPageToken == "" {
			break
		}
		page = r.NextPageToken
	}
	return out, nil
}

// Reply answers a comment (comments.insert with parentId, 50 quota units). Reply ids have the form
// PARENT.SUFFIX, and YouTube only accepts top-level parents, so the parent part is used.
// An 11-character id is a video id: a top-level comment is posted on it (commentThreads.insert).
func (c *Client) Reply(ctx context.Context, to, text string) (*providers.PublishResult, error) {
	if strings.TrimSpace(text) == "" {
		return nil, providers.Invalid("youtube: comment text is empty")
	}
	q := url.Values{"part": {"snippet"}}
	if len(to) == 11 && !strings.Contains(to, ".") {
		chID := c.creds["channel_id"]
		if chID == "" {
			id, err := c.Identify(ctx)
			if err != nil {
				return nil, err
			}
			chID = id.RemoteUserID
		}
		body := map[string]any{"snippet": map[string]any{
			"channelId": chID, "videoId": to,
			"topLevelComment": map[string]any{"snippet": map[string]any{"textOriginal": text}},
		}}
		var r struct {
			ID string `json:"id"`
		}
		if err := c.postJSON(ctx, "/youtube/v3/commentThreads", q, body, &r); err != nil {
			return nil, err
		}
		return &providers.PublishResult{RemoteID: r.ID, RemoteIDs: []string{r.ID}, URL: "https://www.youtube.com/watch?v=" + to + "&lc=" + r.ID}, nil
	}
	parent, _, _ := strings.Cut(to, ".")
	body := map[string]any{"snippet": map[string]any{"parentId": parent, "textOriginal": text}}
	var r comment
	if err := c.postJSON(ctx, "/youtube/v3/comments", q, body, &r); err != nil {
		return nil, err
	}
	return &providers.PublishResult{RemoteID: r.ID, RemoteIDs: []string{r.ID}}, nil
}

// ---------- history ----------

// History walks the channel's uploads playlist (newest first), 50 items per page, and fetches
// statistics and durations for each page in one videos.list call: 2 quota units per 50 videos.
func (c *Client) History(ctx context.Context, since time.Time, max int) ([]providers.HistoryItem, error) {
	if max <= 0 {
		max = 5000
	}
	uploads := c.creds["uploads_playlist_id"]
	if uploads == "" {
		ch, err := c.myChannel(ctx, "contentDetails")
		if err != nil {
			return nil, err
		}
		uploads = ch.ContentDetails.RelatedPlaylists.Uploads
	}
	var out []providers.HistoryItem
	page := ""
	for len(out) < max {
		q := url.Values{"part": {"contentDetails"}, "playlistId": {uploads}, "maxResults": {"50"}}
		if page != "" {
			q.Set("pageToken", page)
		}
		var r struct {
			NextPageToken string `json:"nextPageToken"`
			Items         []struct {
				ContentDetails struct {
					VideoID          string    `json:"videoId"`
					VideoPublishedAt time.Time `json:"videoPublishedAt"`
				} `json:"contentDetails"`
			} `json:"items"`
		}
		if err := c.get(ctx, c.Base, "/youtube/v3/playlistItems", q, &r); err != nil {
			return out, err
		}
		var ids []string
		for _, it := range r.Items {
			if since.IsZero() || it.ContentDetails.VideoPublishedAt.IsZero() || !it.ContentDetails.VideoPublishedAt.Before(since) {
				ids = append(ids, it.ContentDetails.VideoID)
			}
		}
		vs, err := c.videos(ctx, ids, "snippet,contentDetails,statistics")
		if err != nil {
			return out, err
		}
		for _, v := range vs {
			if !since.IsZero() && v.Snippet.PublishedAt.Before(since) {
				continue
			}
			text := v.Snippet.Description
			if !strings.HasPrefix(text, v.Snippet.Title) {
				text = strings.TrimSpace(v.Snippet.Title + "\n\n" + text)
			}
			it := providers.HistoryItem{
				RemoteID: v.ID, Text: text, URL: "https://www.youtube.com/watch?v=" + v.ID,
				CreatedAt: v.Snippet.PublishedAt.UTC(), Metrics: v.metrics(), Kind: kind(v),
			}
			it.AddVisual(v.thumbnail(), true)
			out = append(out, it)
		}
		// The uploads playlist is newest first: a page with nothing recent ends the walk.
		if r.NextPageToken == "" || len(r.Items) == 0 || (!since.IsZero() && len(ids) == 0) {
			break
		}
		page = r.NextPageToken
	}
	if len(out) > max {
		out = out[:max]
	}
	return out, nil
}

// thumbnail is the largest thumbnail of the snippet (maxres 1280x720 when the channel uploaded a
// custom one or the video is HD, then standard 640x480, high 480x360, medium 320x180, default).
func (v videoResource) thumbnail() string {
	for _, size := range []string{"maxres", "standard", "high", "medium", "default"} {
		if t := v.Snippet.Thumbnails[size].URL; t != "" {
			return t
		}
	}
	return ""
}

// kind classifies a video. The Data API has no Shorts flag: a video longer than 3 minutes cannot be
// a Short, and a short one tagged #Shorts is treated as one. Other short videos stay unclassified.
func kind(v videoResource) string {
	d := parseDuration(v.ContentDetails.Duration)
	switch {
	case d > shortsMaxDuration:
		return "video"
	case d > 0 && strings.Contains(strings.ToLower(v.Snippet.Title+" "+v.Snippet.Description), "#shorts"):
		return "short"
	}
	return ""
}

// parseDuration reads ISO 8601 durations like PT1H2M3S or P1DT2H in seconds (0 when unknown).
func parseDuration(s string) int64 {
	if !strings.HasPrefix(s, "P") {
		return 0
	}
	var total, n int64
	inTime := false
	for _, r := range s[1:] {
		switch {
		case r >= '0' && r <= '9':
			n = n*10 + int64(r-'0')
		case r == 'T':
			inTime = true
		default:
			mult := map[rune]int64{'W': 604800, 'D': 86400, 'H': 3600, 'S': 1}[r]
			if r == 'M' {
				mult = 60
				if !inTime {
					mult = 30 * 86400
				}
			}
			total += n * mult
			n = 0
		}
	}
	return total
}
