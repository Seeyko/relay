// Package tiktok is the TikTok connector (Content Posting API + Display API, OAuth 2.0).
package tiktok

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/Seeyko/relay/internal/providers"
)

const DefaultBaseURL = "https://open.tiktokapis.com"

// Upload limits from the Media Transfer Guide.
const (
	chunkSize   = 10 << 20 // every chunk but the last: inside TikTok's 5-64 MB window whatever "MB" means
	singleLimit = 20 << 20 // files up to this size go in one chunk (always <= 64 MB)
	maxVideo    = 4 << 30
	maxImages   = 35
)

var privacyLevels = []string{"PUBLIC_TO_EVERYONE", "MUTUAL_FOLLOW_FRIENDS", "FOLLOWER_OF_CREATOR", "SELF_ONLY"}

var videoMimes = map[string]bool{"video/mp4": true, "video/quicktime": true, "video/webm": true}

var imageMimes = map[string]bool{"image/jpeg": true, "image/jpg": true, "image/webp": true}

func init() {
	providers.Register(providers.Factory{
		Name: "tiktok",
		Description: "TikTok via the Content Posting API (direct post) and Display API. Scopes: user.info.basic, user.info.profile, user.info.stats, " +
			"video.publish, video.list. One video (MP4/MOV/WebM, max 4 GB) or 1-35 JPEG/WebP images (options.media_type=PHOTO; TikTok pulls them " +
			"from RELAY_BASE_URL, which must be a domain or URL prefix verified in your TikTok app). posting_mode=inbox sends the media to the creator's TikTok inbox instead (video.upload scope, works for unaudited apps; the creator publishes in the app within 24 h; privacy and interactions are chosen there). Required option for direct posting: privacy_level " +
			"(PUBLIC_TO_EVERYONE, MUTUAL_FOLLOW_FRIENDS, FOLLOWER_OF_CREATOR or SELF_ONLY; TikTok forbids a default, ask the creator). " +
			"Interactions are off unless enabled: disable_comment, disable_duet, disable_stitch default to true. Other options: title (photo, max 90), " +
			"is_aigc, brand_organic_toggle (your brand), brand_content_toggle (paid partnership, not with SELF_ONLY), auto_add_music, " +
			"photo_cover_index, video_cover_timestamp_ms, duration_sec (checked against the creator's max). Unaudited apps can only post SELF_ONLY. " +
			"Comments are not in TikTok's API for creators: use a browser task (kind tiktok.comments).",
		CredentialFields: []string{"client_key", "client_secret", "access_token", "refresh_token"},
		Validate:         validate,
		Count:            utf16Len,
		New: func(c map[string]string, persist func(map[string]string) error, hc *http.Client) (providers.Client, error) {
			if c["access_token"] == "" && c["refresh_token"] == "" {
				return nil, errors.New("tiktok: access_token or refresh_token is required")
			}
			base := c["base_url"]
			if base == "" {
				base = DefaultBaseURL
			}
			if hc == nil {
				hc = http.DefaultClient
			}
			return &Client{Base: strings.TrimRight(base, "/"), HC: hc, creds: c, persist: persist}, nil
		},
	})
}

// utf16Len counts UTF-16 code units, the unit TikTok uses for its text limits.
func utf16Len(s string) int { return len(utf16.Encode([]rune(s))) }

func isPhoto(req providers.PublishRequest) bool {
	if t, _ := req.Options["media_type"].(string); strings.EqualFold(t, "PHOTO") {
		return true
	}
	for _, m := range req.Media {
		if strings.HasPrefix(m.Mime, "video/") {
			return false
		}
	}
	return len(req.Media) > 0
}

func validate(req providers.PublishRequest) []string {
	var errs []string
	text := strings.Join(req.Parts, "\n\n")
	o := req.Options
	privacy, _ := o["privacy_level"].(string)
	inbox := isInbox(o)
	if m, _ := o["posting_mode"].(string); m != "" && m != "direct" && m != "inbox" {
		errs = append(errs, "TikTok posting_mode must be direct (default) or inbox")
	}
	if inbox {
		// Inbox mode sends the media to the creator's TikTok inbox: privacy and interactions are chosen in the app.
	} else if privacy == "" {
		errs = append(errs, "TikTok requires options.privacy_level chosen by the creator (no default allowed): "+strings.Join(privacyLevels, ", "))
	} else if !slices.Contains(privacyLevels, privacy) {
		errs = append(errs, "TikTok privacy_level must be one of "+strings.Join(privacyLevels, ", "))
	}
	if b, _ := o["brand_content_toggle"].(bool); b && privacy == "SELF_ONLY" {
		errs = append(errs, "TikTok branded content (brand_content_toggle) cannot be posted with privacy SELF_ONLY")
	}
	switch {
	case len(req.Media) == 0:
		errs = append(errs, "TikTok needs a video, or 1 to 35 images with options.media_type=PHOTO")
	case isPhoto(req):
		if len(req.Media) > maxImages {
			errs = append(errs, "TikTok photo posts take at most 35 images")
		}
		for _, m := range req.Media {
			if !imageMimes[strings.ToLower(m.Mime)] {
				errs = append(errs, "TikTok photo posts accept only JPEG or WebP images ("+m.Filename+" is "+m.Mime+")")
			}
		}
		if t, _ := o["title"].(string); utf16Len(t) > 90 {
			errs = append(errs, "TikTok photo title is limited to 90 characters")
		}
		if utf16Len(text) > 4000 {
			errs = append(errs, "TikTok photo description is limited to 4000 characters")
		}
	default:
		if len(req.Media) != 1 {
			errs = append(errs, "TikTok video posts take exactly one video")
		}
		m := req.Media[0]
		if !videoMimes[strings.ToLower(m.Mime)] {
			errs = append(errs, "TikTok videos must be MP4, MOV or WebM (got "+m.Mime+")")
		}
		if m.Size > maxVideo {
			errs = append(errs, "TikTok videos are limited to 4 GB")
		}
		if utf16Len(text) > 2200 {
			errs = append(errs, "TikTok video caption is limited to 2200 characters")
		}
	}
	return errs
}

type Client struct {
	Base    string
	HC      *http.Client
	creds   map[string]string
	persist func(map[string]string) error
	mu      sync.Mutex
	avatar  string // avatar seen by the last Identify call
	// PollInterval for publish status (tests override it).
	PollInterval time.Duration
}

// token returns a valid access token, refreshing it (24h lifetime) when needed.
// TikTok rotates the refresh token on every refresh, so the new one is always persisted.
func (c *Client) token(ctx context.Context, force bool) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	exp, _ := strconv.ParseInt(c.creds["expires_at"], 10, 64)
	if !force && c.creds["access_token"] != "" && (exp == 0 || time.Now().Unix() < exp-300) {
		return c.creds["access_token"], nil
	}
	if c.creds["refresh_token"] == "" || c.creds["client_key"] == "" || c.creds["client_secret"] == "" {
		if c.creds["access_token"] != "" && !force {
			return c.creds["access_token"], nil
		}
		return "", errors.New("tiktok: access token expired and no refresh_token/client_key/client_secret to renew it")
	}
	form := url.Values{
		"client_key": {c.creds["client_key"]}, "client_secret": {c.creds["client_secret"]},
		"grant_type": {"refresh_token"}, "refresh_token": {c.creds["refresh_token"]},
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.Base+"/v2/oauth/token/", strings.NewReader(form.Encode()))
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
		return "", err
	}
	var r struct {
		AccessToken      string `json:"access_token"`
		RefreshToken     string `json:"refresh_token"`
		ExpiresIn        int64  `json:"expires_in"`
		RefreshExpiresIn int64  `json:"refresh_expires_in"`
		OpenID           string `json:"open_id"`
		Scope            string `json:"scope"`
		Error            string `json:"error"`
		ErrorDesc        string `json:"error_description"`
		LogID            string `json:"log_id"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return "", err
	}
	if r.AccessToken == "" {
		// OAuth errors (invalid_grant...) need a new authorization: retrying will not help.
		return "", providers.Invalid(fmt.Sprintf("tiktok: token refresh failed: %s %s (log_id %s)", r.Error, r.ErrorDesc, r.LogID))
	}
	now := time.Now().Unix()
	c.creds["access_token"] = r.AccessToken
	if r.RefreshToken != "" {
		c.creds["refresh_token"] = r.RefreshToken
	}
	c.creds["expires_at"] = strconv.FormatInt(now+r.ExpiresIn, 10)
	if r.RefreshExpiresIn > 0 {
		c.creds["refresh_expires_at"] = strconv.FormatInt(now+r.RefreshExpiresIn, 10)
	}
	if r.Scope != "" {
		c.creds["scope"] = r.Scope
	}
	if c.persist != nil {
		_ = c.persist(c.creds)
	}
	return r.AccessToken, nil
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	LogID   string `json:"log_id"`
}

// statusFor maps TikTok error codes to HTTP statuses so providers.Retryable classifies them.
// Some errors (spam_risk_*) come back with HTTP 200 on creator_info.
func statusFor(code string, status int) int {
	switch {
	case code == "rate_limit_exceeded":
		return 429
	case code == "internal_error":
		return 500
	case code == "access_token_invalid" || code == "scope_not_authorized":
		return 401
	case strings.HasPrefix(code, "spam_risk") || code == "reached_active_user_cap" ||
		code == "unaudited_client_can_only_post_to_private_accounts" || code == "url_ownership_unverified" ||
		code == "privacy_level_option_mismatch":
		return 403
	case status >= 400:
		return status
	}
	return 400
}

// call sends a request (JSON body for POST) and decodes `data`. TikTok wraps errors in {"error":{"code":...}}.
func (c *Client) call(ctx context.Context, method, path string, body any, data any) error {
	for attempt := 0; attempt < 2; attempt++ {
		tok, err := c.token(ctx, attempt > 0)
		if err != nil {
			return err
		}
		var rd io.Reader
		if body != nil {
			raw, err := json.Marshal(body)
			if err != nil {
				return err
			}
			rd = bytes.NewReader(raw)
		}
		req, err := http.NewRequestWithContext(ctx, method, c.Base+path, rd)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		if method == "POST" {
			req.Header.Set("Content-Type", "application/json; charset=UTF-8")
		}
		resp, err := c.HC.Do(req)
		if err != nil {
			return err
		}
		status := resp.StatusCode
		b, rerr := providers.ReadBody(resp)
		var he *providers.HTTPError
		if rerr != nil && !errors.As(rerr, &he) {
			return rerr
		}
		if he != nil {
			b = []byte(he.Body)
		}
		var env struct {
			Data  json.RawMessage `json:"data"`
			Error apiError        `json:"error"`
		}
		jerr := json.Unmarshal(b, &env)
		code := env.Error.Code
		if attempt == 0 && (status == 401 || code == "access_token_invalid") && c.creds["refresh_token"] != "" {
			continue // refresh and retry once
		}
		if he != nil {
			if code != "" {
				return &providers.HTTPError{Status: status, Body: fmt.Sprintf("%s: %s (log_id %s)", code, env.Error.Message, env.Error.LogID)}
			}
			return he
		}
		if jerr != nil {
			return jerr
		}
		if code != "" && code != "ok" {
			return &providers.HTTPError{Status: statusFor(code, status), Body: fmt.Sprintf("%s: %s (log_id %s)", code, env.Error.Message, env.Error.LogID)}
		}
		if data != nil && len(env.Data) > 0 {
			return json.Unmarshal(env.Data, data)
		}
		return nil
	}
	return errors.New("tiktok: authentication failed")
}

// userAvatar holds the avatar fields of /v2/user/info/ (user.info.basic scope).
type userAvatar struct {
	Large  string `json:"avatar_large_url"`
	URL100 string `json:"avatar_url_100"`
	URL    string `json:"avatar_url"`
}

func (a userAvatar) best() string {
	for _, u := range []string{a.Large, a.URL100, a.URL} {
		if u != "" {
			return u
		}
	}
	return ""
}

const avatarFields = "avatar_large_url,avatar_url_100"

func (c *Client) Identify(ctx context.Context) (*providers.Identity, error) {
	var r struct {
		User struct {
			OpenID      string `json:"open_id"`
			DisplayName string `json:"display_name"`
			Username    string `json:"username"`
			userAvatar
		} `json:"user"`
	}
	// username needs user.info.profile; fall back to the basic fields when it was not granted.
	err := c.call(ctx, "GET", "/v2/user/info/?fields=open_id,display_name,username,"+avatarFields, nil, &r)
	var he *providers.HTTPError
	if errors.As(err, &he) && strings.Contains(he.Body, "scope_not_authorized") {
		err = c.call(ctx, "GET", "/v2/user/info/?fields=open_id,display_name,"+avatarFields, nil, &r)
	}
	if err != nil {
		return nil, err
	}
	if r.User.Username != "" {
		c.creds["username"] = r.User.Username
	}
	c.avatar = r.User.best()
	return &providers.Identity{RemoteUserID: r.User.OpenID, Handle: r.User.Username, Name: r.User.DisplayName, AvatarURL: c.avatar}, nil
}

// AvatarURL is the profile picture (avatar_large_url, else avatar_url_100; user.info.basic scope).
// A token without the scope has no picture: "" and no error.
func (c *Client) AvatarURL(ctx context.Context) (string, error) {
	if c.avatar != "" {
		return c.avatar, nil
	}
	var r struct {
		User userAvatar `json:"user"`
	}
	err := c.call(ctx, "GET", "/v2/user/info/?fields="+avatarFields, nil, &r)
	var he *providers.HTTPError
	if errors.As(err, &he) && strings.Contains(he.Body, "scope_not_authorized") {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	c.avatar = r.User.best()
	return c.avatar, nil
}

// AccountMetrics needs the user.info.stats scope.
func (c *Client) AccountMetrics(ctx context.Context) (map[string]int64, error) {
	var r struct {
		User struct {
			FollowerCount  int64 `json:"follower_count"`
			FollowingCount int64 `json:"following_count"`
			LikesCount     int64 `json:"likes_count"`
			VideoCount     int64 `json:"video_count"`
		} `json:"user"`
	}
	if err := c.call(ctx, "GET", "/v2/user/info/?fields=follower_count,following_count,likes_count,video_count", nil, &r); err != nil {
		return nil, err
	}
	return map[string]int64{"followers": r.User.FollowerCount, "following": r.User.FollowingCount, "likes_total": r.User.LikesCount, "posts": r.User.VideoCount}, nil
}

type creatorInfo struct {
	Username            string   `json:"creator_username"`
	Nickname            string   `json:"creator_nickname"`
	PrivacyLevelOptions []string `json:"privacy_level_options"`
	CommentDisabled     bool     `json:"comment_disabled"`
	DuetDisabled        bool     `json:"duet_disabled"`
	StitchDisabled      bool     `json:"stitch_disabled"`
	MaxVideoDuration    int      `json:"max_video_post_duration_sec"`
}

// interaction returns the disable_* flag: off-by-default interactions (TikTok's UX rule) and always
// disabled when the creator turned them off in the app.
func interaction(o map[string]any, k string, creatorDisabled bool) bool {
	if creatorDisabled {
		return true
	}
	if v, ok := o[k].(bool); ok {
		return v
	}
	return true
}

func boolOpt(o map[string]any, k string) bool {
	v, _ := o[k].(bool)
	return v
}

// isInbox: posting_mode=inbox sends the media to the creator's TikTok inbox (Content Posting API
// "upload" / MEDIA_UPLOAD); the creator finishes and publishes the post in the TikTok app within 24 hours.
// It needs the video.upload scope and, unlike direct posting, works for unaudited apps on public accounts.
func isInbox(o map[string]any) bool { m, _ := o["posting_mode"].(string); return m == "inbox" }

func (c *Client) Publish(ctx context.Context, req providers.PublishRequest) (*providers.PublishResult, error) {
	if errs := validate(req); len(errs) > 0 {
		return nil, providers.Invalid(strings.Join(errs, "; "))
	}
	o := req.Options
	photo := isPhoto(req)
	if isInbox(o) {
		return c.publishInbox(ctx, req, photo)
	}
	// TikTok requires fresh creator info before every post.
	var ci creatorInfo
	if err := c.call(ctx, "POST", "/v2/post/publish/creator_info/query/", nil, &ci); err != nil {
		return nil, err
	}
	privacy, _ := o["privacy_level"].(string)
	if !slices.Contains(ci.PrivacyLevelOptions, privacy) {
		return nil, providers.Invalid(fmt.Sprintf("tiktok: privacy_level %s not allowed for @%s (allowed: %s)", privacy, ci.Username, strings.Join(ci.PrivacyLevelOptions, ", ")))
	}
	if d, ok := o["duration_sec"].(float64); ok && !photo && ci.MaxVideoDuration > 0 && int(d) > ci.MaxVideoDuration {
		return nil, providers.Invalid(fmt.Sprintf("tiktok: video is %ds, @%s can post at most %ds", int(d), ci.Username, ci.MaxVideoDuration))
	}
	if ci.Username != "" && c.creds["username"] == "" {
		c.creds["username"] = ci.Username
	}
	text := strings.Join(req.Parts, "\n\n")
	postInfo := map[string]any{
		"privacy_level":        privacy,
		"disable_comment":      interaction(o, "disable_comment", ci.CommentDisabled),
		"brand_content_toggle": boolOpt(o, "brand_content_toggle"),
		"brand_organic_toggle": boolOpt(o, "brand_organic_toggle"),
	}
	var publishID string
	var err error
	if photo {
		publishID, err = c.postPhotos(ctx, req.Media, text, o, postInfo)
	} else {
		if text != "" {
			postInfo["title"] = text // for videos the "title" is the caption
		}
		postInfo["disable_duet"] = interaction(o, "disable_duet", ci.DuetDisabled)
		postInfo["disable_stitch"] = interaction(o, "disable_stitch", ci.StitchDisabled)
		if v, ok := o["is_aigc"].(bool); ok {
			postInfo["is_aigc"] = v
		}
		if ms, ok := o["video_cover_timestamp_ms"].(float64); ok {
			postInfo["video_cover_timestamp_ms"] = int64(ms)
		}
		publishID, err = c.uploadVideo(ctx, req.Media[0], postInfo)
	}
	if err != nil {
		return nil, err
	}
	id, err := c.waitPublished(ctx, publishID)
	if err != nil {
		return nil, err
	}
	res := &providers.PublishResult{RemoteID: id, RemoteIDs: []string{id}}
	if id == "" {
		// Private (SELF_ONLY) or not-yet-moderated posts get no public id: keep the publish id so the post stays traceable.
		res.RemoteID, res.RemoteIDs = "publish:"+publishID, []string{"publish:" + publishID}
	} else if u := c.creds["username"]; u != "" {
		kind := "video"
		if photo {
			kind = "photo"
		}
		res.URL = "https://www.tiktok.com/@" + u + "/" + kind + "/" + id
	}
	return res, nil
}

func (c *Client) publishInbox(ctx context.Context, req providers.PublishRequest, photo bool) (*providers.PublishResult, error) {
	text := strings.Join(req.Parts, "\n\n")
	var publishID string
	var err error
	if photo {
		// MEDIA_UPLOAD keeps only title and description; the rest is chosen in the app.
		publishID, err = c.postPhotos(ctx, req.Media, text, req.Options, map[string]any{}, "MEDIA_UPLOAD")
	} else {
		publishID, err = c.uploadVideoTo(ctx, "/v2/post/publish/inbox/video/init/", req.Media[0], nil)
	}
	if err != nil {
		return nil, err
	}
	if _, err := c.waitPublished(ctx, publishID); err != nil {
		return nil, err
	}
	id := "inbox:" + publishID
	return &providers.PublishResult{RemoteID: id, RemoteIDs: []string{id}}, nil
}

// chunkPlan follows the Media Transfer Guide: total_chunk_count = floor(video_size / chunk_size),
// every chunk is 5-64 MB except the last one, which absorbs the remainder (up to 128 MB);
// files under 5 MB must go in a single chunk. Small files (<= 20 MiB) are sent whole; larger ones
// use 10 MiB chunks, so the last chunk stays under 20 MiB and a 4 GB file needs 409 chunks (max 1000).
func chunkPlan(size int64) (chunk int64, count int64) {
	if size <= singleLimit {
		return size, 1
	}
	return chunkSize, size / chunkSize
}

func (c *Client) uploadVideo(ctx context.Context, m providers.MediaFile, postInfo map[string]any) (string, error) {
	return c.uploadVideoTo(ctx, "/v2/post/publish/video/init/", m, postInfo)
}

// uploadVideoTo initialises a file upload on path (direct post or inbox) and sends the chunks.
func (c *Client) uploadVideoTo(ctx context.Context, path string, m providers.MediaFile, postInfo map[string]any) (string, error) {
	cs, n := chunkPlan(m.Size)
	var init struct {
		PublishID string `json:"publish_id"`
		UploadURL string `json:"upload_url"`
	}
	body := map[string]any{
		"source_info": map[string]any{"source": "FILE_UPLOAD", "video_size": m.Size, "chunk_size": cs, "total_chunk_count": n},
	}
	if postInfo != nil {
		body["post_info"] = postInfo
	}
	if err := c.call(ctx, "POST", path, body, &init); err != nil {
		return "", err
	}
	if init.UploadURL == "" {
		return "", errors.New("tiktok: video init returned no upload_url")
	}
	f, err := m.Open()
	if err != nil {
		return "", err
	}
	defer f.Close()
	mime := strings.ToLower(m.Mime)
	var off int64
	for i := int64(0); i < n; i++ {
		length := cs
		if i == n-1 {
			length = m.Size - off // last chunk takes the remainder
		}
		buf := make([]byte, length)
		if _, err := io.ReadFull(f, buf); err != nil {
			return "", fmt.Errorf("tiktok: read video: %w", err)
		}
		req, err := http.NewRequestWithContext(ctx, "PUT", init.UploadURL, bytes.NewReader(buf))
		if err != nil {
			return "", err
		}
		req.Header.Set("Content-Type", mime)
		req.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", off, off+length-1, m.Size))
		req.ContentLength = length
		resp, err := c.HC.Do(req)
		if err != nil {
			return "", err
		}
		// 206: chunk received, more expected; 201: upload complete.
		if _, err := providers.ReadBody(resp); err != nil {
			return "", fmt.Errorf("tiktok: upload chunk %d/%d: %w", i+1, n, err)
		}
		off += length
	}
	return init.PublishID, nil
}

func (c *Client) postPhotos(ctx context.Context, media []providers.MediaFile, text string, o map[string]any, postInfo map[string]any, mode ...string) (string, error) {
	postMode := "DIRECT_POST"
	if len(mode) > 0 {
		postMode = mode[0]
	}
	var urls []string
	for _, m := range media {
		if !strings.HasPrefix(m.PublicURL, "https://") {
			return "", providers.Invalid("tiktok: photo posts need an https RELAY_BASE_URL on a domain verified in your TikTok app (TikTok pulls the images)")
		}
		urls = append(urls, m.PublicURL)
	}
	title, _ := o["title"].(string)
	if title == "" {
		title = firstLine(text, 90)
	}
	if title != "" {
		postInfo["title"] = title
	}
	if text != "" {
		postInfo["description"] = text
	}
	if postMode == "DIRECT_POST" {
		postInfo["auto_add_music"] = boolOpt(o, "auto_add_music")
	}
	cover := 0
	if v, ok := o["photo_cover_index"].(float64); ok {
		cover = int(v)
	}
	if cover < 0 || cover >= len(urls) {
		return "", providers.Invalid("tiktok: photo_cover_index out of range")
	}
	var r struct {
		PublishID string `json:"publish_id"`
	}
	body := map[string]any{
		"post_info":   postInfo,
		"source_info": map[string]any{"source": "PULL_FROM_URL", "photo_images": urls, "photo_cover_index": cover},
		"post_mode":   postMode,
		"media_type":  "PHOTO",
	}
	if v, ok := o["is_aigc"].(bool); ok {
		body["is_aigc"] = v // top-level for photos, unlike videos
	}
	if err := c.call(ctx, "POST", "/v2/post/publish/content/init/", body, &r); err != nil {
		return "", err
	}
	return r.PublishID, nil
}

// firstLine returns the first line of s cut to max UTF-16 code units.
func firstLine(s string, max int) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	var b strings.Builder
	n := 0
	for _, r := range s {
		w := utf16.RuneLen(r)
		if n+w > max {
			break
		}
		n += w
		b.WriteRune(r)
	}
	return b.String()
}

func (c *Client) waitPublished(ctx context.Context, publishID string) (string, error) {
	wait := c.PollInterval
	if wait <= 0 {
		wait = 5 * time.Second // status/fetch allows 30 requests per minute
	}
	for i := 0; i < 180; i++ {
		var st struct {
			Status     string        `json:"status"`
			FailReason string        `json:"fail_reason"`
			PostIDs    []json.Number `json:"publicaly_available_post_id"` // sic, TikTok's spelling; list<int64>
		}
		if err := c.call(ctx, "POST", "/v2/post/publish/status/fetch/", map[string]string{"publish_id": publishID}, &st); err != nil {
			return "", err
		}
		switch st.Status {
		case "PUBLISH_COMPLETE":
			if len(st.PostIDs) > 0 {
				return st.PostIDs[0].String(), nil
			}
			return "", nil
		case "FAILED":
			return "", providers.Invalid("tiktok: publish failed: " + st.FailReason)
		case "SEND_TO_USER_INBOX":
			return "", nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(wait):
		}
	}
	return "", errors.New("tiktok: publishing still processing after 180 status checks (publish_id " + publishID + ")")
}

type video struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	Description  string `json:"video_description"`
	CreateTime   int64  `json:"create_time"`
	ShareURL     string `json:"share_url"`
	ViewCount    int64  `json:"view_count"`
	LikeCount    int64  `json:"like_count"`
	CommentCount int64  `json:"comment_count"`
	ShareCount   int64  `json:"share_count"`
	// CoverImageURL is a signed link to the video cover; TikTok says it expires within 6 hours.
	CoverImageURL string `json:"cover_image_url"`
}

const videoFields = "id,title,video_description,create_time,share_url,view_count,like_count,comment_count,share_count"

// historyFields adds the cover, so the import gets the visual in the same request.
const historyFields = videoFields + ",cover_image_url"

func (v video) metrics() map[string]int64 {
	return map[string]int64{"view_count": v.ViewCount, "like_count": v.LikeCount, "comment_count": v.CommentCount, "share_count": v.ShareCount}
}

// Metrics queries /v2/video/query/ (20 ids per request, video.list scope). Publish-only ids are skipped.
func (c *Client) Metrics(ctx context.Context, ids []string) (map[string]map[string]int64, error) {
	out := map[string]map[string]int64{}
	var real []string
	for _, id := range ids {
		if !strings.HasPrefix(id, "publish:") {
			real = append(real, id)
		}
	}
	for start := 0; start < len(real); start += 20 {
		end := min(start+20, len(real))
		var r struct {
			Videos []video `json:"videos"`
		}
		if err := c.call(ctx, "POST", "/v2/video/query/?fields="+videoFields, map[string]any{"filters": map[string]any{"video_ids": real[start:end]}}, &r); err != nil {
			return out, err
		}
		for _, v := range r.Videos {
			out[v.ID] = v.metrics()
		}
	}
	return out, nil
}

// Comments: TikTok has no comments API for creators (the Research API is for academic use only).
func (c *Client) Comments(ctx context.Context, remoteID string) ([]providers.Comment, error) {
	return nil, providers.ErrUnsupported
}

func (c *Client) Reply(ctx context.Context, to, text string) (*providers.PublishResult, error) {
	return nil, providers.ErrUnsupported
}

// History pages /v2/video/list/ (newest first, 20 per page, cursor in ms).
func (c *Client) History(ctx context.Context, since time.Time, max int) ([]providers.HistoryItem, error) {
	if max <= 0 {
		max = 5000
	}
	var out []providers.HistoryItem
	var cursor int64
	for len(out) < max {
		body := map[string]any{"max_count": 20}
		if cursor != 0 {
			body["cursor"] = cursor
		}
		var r struct {
			Videos  []video `json:"videos"`
			Cursor  int64   `json:"cursor"`
			HasMore bool    `json:"has_more"`
		}
		if err := c.call(ctx, "POST", "/v2/video/list/?fields="+historyFields, body, &r); err != nil {
			return out, err
		}
		stop := false
		for _, v := range r.Videos {
			t := time.Unix(v.CreateTime, 0).UTC()
			if !since.IsZero() && t.Before(since) {
				stop = true
				break
			}
			text := v.Description
			if text == "" {
				text = v.Title
			}
			it := providers.HistoryItem{RemoteID: v.ID, Text: text, URL: v.ShareURL, CreatedAt: t, Metrics: v.metrics(), Kind: "video"}
			it.AddVisual(v.CoverImageURL, true)
			out = append(out, it)
		}
		if stop || !r.HasMore || len(r.Videos) == 0 || r.Cursor == cursor {
			break
		}
		cursor = r.Cursor
	}
	if len(out) > max {
		out = out[:max]
	}
	return out, nil
}
