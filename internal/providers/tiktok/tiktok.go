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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Seeyko/relay/internal/providers"
)

const DefaultBaseURL = "https://open.tiktokapis.com"

const (
	minChunk = 5 << 20
	maxChunk = 64 << 20
	chunk    = 10 << 20
)

func init() {
	providers.Register(providers.Factory{
		Name: "tiktok",
		Description: "TikTok via the Content Posting API (direct post) and Display API. Scopes: user.info.basic, user.info.stats, " +
			"video.publish, video.list. Video upload, photo carousels (options.media_type=PHOTO, needs RELAY_BASE_URL on a domain verified in your TikTok app), " +
			"per-video views/likes/comments/shares, follower counts, history import. Options: privacy_level (default: most public allowed), " +
			"disable_comment, disable_duet, disable_stitch, title (photo posts), is_aigc, brand_content_toggle. " +
			"Unaudited apps can only post with privacy SELF_ONLY. Reading/answering comments is not in TikTok's API: use a browser task (kind tiktok.comments).",
		CredentialFields: []string{"client_key", "client_secret", "access_token", "refresh_token"},
		Validate:         validate,
		New: func(c map[string]string, persist func(map[string]string) error, hc *http.Client) (providers.Client, error) {
			if c["access_token"] == "" && c["refresh_token"] == "" {
				return nil, errors.New("tiktok: access_token or refresh_token is required")
			}
			base := c["base_url"]
			if base == "" {
				base = DefaultBaseURL
			}
			return &Client{Base: strings.TrimRight(base, "/"), HC: hc, creds: c, persist: persist}, nil
		},
	})
}

func validate(req providers.PublishRequest) []string {
	var errs []string
	text := strings.Join(req.Parts, "\n\n")
	photo, _ := req.Options["media_type"].(string)
	videos, images := 0, 0
	for _, m := range req.Media {
		if strings.HasPrefix(m.Mime, "video/") {
			videos++
		} else {
			images++
		}
	}
	switch {
	case len(req.Media) == 0:
		errs = append(errs, "TikTok needs a video, or 1 to 35 images with options.media_type=PHOTO")
	case strings.EqualFold(photo, "PHOTO") || (videos == 0 && images > 0):
		if videos > 0 {
			errs = append(errs, "TikTok photo posts cannot contain videos")
		}
		if images > 35 {
			errs = append(errs, "TikTok photo posts take at most 35 images")
		}
		if providers.CharCount(text) > 4000 {
			errs = append(errs, "TikTok photo description is limited to 4000 characters")
		}
	default:
		if videos != 1 || images > 0 {
			errs = append(errs, "TikTok video posts take exactly one video")
		}
		if providers.CharCount(text) > 2200 {
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
	// PollInterval for publish status (tests override it).
	PollInterval time.Duration
}

// token returns a valid access token, refreshing it (24h lifetime) when needed.
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
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		OpenID       string `json:"open_id"`
		Error        string `json:"error"`
		ErrorDesc    string `json:"error_description"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return "", err
	}
	if r.AccessToken == "" {
		return "", fmt.Errorf("tiktok: token refresh failed: %s %s", r.Error, r.ErrorDesc)
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

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	LogID   string `json:"log_id"`
}

// call sends a JSON request and decodes `data`. TikTok wraps errors in {"error":{"code":...}}.
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
		if body != nil {
			req.Header.Set("Content-Type", "application/json; charset=UTF-8")
		}
		resp, err := c.HC.Do(req)
		if err != nil {
			return err
		}
		b, rerr := providers.ReadBody(resp)
		var env struct {
			Data  json.RawMessage `json:"data"`
			Error apiError        `json:"error"`
		}
		var he *providers.HTTPError
		if rerr != nil {
			if errors.As(rerr, &he) {
				_ = json.Unmarshal([]byte(he.Body), &env)
				if attempt == 0 && (he.Status == 401 || env.Error.Code == "access_token_invalid") {
					continue // refresh and retry once
				}
			}
			return rerr
		}
		if err := json.Unmarshal(b, &env); err != nil {
			return err
		}
		if env.Error.Code != "" && env.Error.Code != "ok" {
			if attempt == 0 && env.Error.Code == "access_token_invalid" {
				continue
			}
			return &providers.HTTPError{Status: 400, Body: env.Error.Code + ": " + env.Error.Message}
		}
		if data != nil && len(env.Data) > 0 {
			return json.Unmarshal(env.Data, data)
		}
		return nil
	}
	return errors.New("tiktok: authentication failed")
}

func (c *Client) Identify(ctx context.Context) (*providers.Identity, error) {
	var r struct {
		User struct {
			OpenID      string `json:"open_id"`
			DisplayName string `json:"display_name"`
			Username    string `json:"username"`
		} `json:"user"`
	}
	if err := c.call(ctx, "GET", "/v2/user/info/?fields=open_id,display_name,username", nil, &r); err != nil {
		return nil, err
	}
	c.creds["username"] = r.User.Username
	return &providers.Identity{RemoteUserID: r.User.OpenID, Handle: r.User.Username, Name: r.User.DisplayName}, nil
}

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
	PrivacyLevelOptions []string `json:"privacy_level_options"`
	CommentDisabled     bool     `json:"comment_disabled"`
	DuetDisabled        bool     `json:"duet_disabled"`
	StitchDisabled      bool     `json:"stitch_disabled"`
	MaxVideoDuration    int      `json:"max_video_post_duration_sec"`
}

func pickPrivacy(want string, allowed []string) (string, error) {
	if want != "" {
		for _, a := range allowed {
			if a == want {
				return want, nil
			}
		}
		return "", providers.Invalid(fmt.Sprintf("tiktok: privacy_level %s not allowed for this account (allowed: %s)", want, strings.Join(allowed, ", ")))
	}
	for _, pref := range []string{"PUBLIC_TO_EVERYONE", "MUTUAL_FOLLOW_FRIENDS", "FOLLOWER_OF_CREATOR", "SELF_ONLY"} {
		for _, a := range allowed {
			if a == pref {
				return a, nil
			}
		}
	}
	if len(allowed) > 0 {
		return allowed[0], nil
	}
	return "SELF_ONLY", nil
}

func boolOpt(o map[string]any, k string, def bool) bool {
	if v, ok := o[k].(bool); ok {
		return v
	}
	return def
}

func (c *Client) Publish(ctx context.Context, req providers.PublishRequest) (*providers.PublishResult, error) {
	if errs := validate(req); len(errs) > 0 {
		return nil, providers.Invalid(strings.Join(errs, "; "))
	}
	o := req.Options
	if o == nil {
		o = map[string]any{}
	}
	var ci creatorInfo
	if err := c.call(ctx, "POST", "/v2/post/publish/creator_info/query/", map[string]any{}, &ci); err != nil {
		return nil, err
	}
	want, _ := o["privacy_level"].(string)
	privacy, err := pickPrivacy(want, ci.PrivacyLevelOptions)
	if err != nil {
		return nil, err
	}
	text := strings.Join(req.Parts, "\n\n")
	postInfo := map[string]any{
		"privacy_level":   privacy,
		"disable_comment": boolOpt(o, "disable_comment", ci.CommentDisabled),
	}
	for _, k := range []string{"is_aigc", "brand_content_toggle", "brand_organic_toggle"} {
		if v, ok := o[k].(bool); ok {
			postInfo[k] = v
		}
	}
	var publishID string
	if strings.HasPrefix(req.Media[0].Mime, "video/") {
		postInfo["title"] = text // for videos the "title" is the caption
		postInfo["disable_duet"] = boolOpt(o, "disable_duet", ci.DuetDisabled)
		postInfo["disable_stitch"] = boolOpt(o, "disable_stitch", ci.StitchDisabled)
		if ms, ok := o["video_cover_timestamp_ms"].(float64); ok {
			postInfo["video_cover_timestamp_ms"] = int64(ms)
		}
		publishID, err = c.uploadVideo(ctx, req.Media[0], postInfo)
	} else {
		publishID, err = c.postPhotos(ctx, req.Media, text, o, postInfo)
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
		// Private (SELF_ONLY) posts get no public id: keep the publish id so the post stays traceable.
		res.RemoteID, res.RemoteIDs = "publish:"+publishID, []string{"publish:" + publishID}
	} else if u := c.creds["username"]; u != "" {
		res.URL = "https://www.tiktok.com/@" + u + "/video/" + id
	}
	return res, nil
}

// chunkPlan follows TikTok's rules: chunks of 5-64 MB, the last one may hold the remainder (up to 128 MB);
// files under 64 MB go in one chunk.
func chunkPlan(size int64) (chunkSize int64, count int64) {
	if size <= maxChunk {
		return size, 1
	}
	count = size / chunk
	return chunk, count
}

func (c *Client) uploadVideo(ctx context.Context, m providers.MediaFile, postInfo map[string]any) (string, error) {
	cs, n := chunkPlan(m.Size)
	var init struct {
		PublishID string `json:"publish_id"`
		UploadURL string `json:"upload_url"`
	}
	body := map[string]any{
		"post_info":   postInfo,
		"source_info": map[string]any{"source": "FILE_UPLOAD", "video_size": m.Size, "chunk_size": cs, "total_chunk_count": n},
	}
	if err := c.call(ctx, "POST", "/v2/post/publish/video/init/", body, &init); err != nil {
		return "", err
	}
	f, err := m.Open()
	if err != nil {
		return "", err
	}
	defer f.Close()
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
		mime := m.Mime
		if mime == "" {
			mime = "video/mp4"
		}
		req.Header.Set("Content-Type", mime)
		req.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", off, off+length-1, m.Size))
		req.ContentLength = length
		resp, err := c.HC.Do(req)
		if err != nil {
			return "", err
		}
		if _, err := providers.ReadBody(resp); err != nil {
			return "", fmt.Errorf("tiktok: upload chunk %d/%d: %w", i+1, n, err)
		}
		off += length
	}
	return init.PublishID, nil
}

func (c *Client) postPhotos(ctx context.Context, media []providers.MediaFile, text string, o map[string]any, postInfo map[string]any) (string, error) {
	var urls []string
	for _, m := range media {
		if m.PublicURL == "" {
			return "", providers.Invalid("tiktok: photo posts need RELAY_BASE_URL (TikTok pulls images from a verified domain)")
		}
		urls = append(urls, m.PublicURL)
	}
	title, _ := o["title"].(string)
	if title == "" {
		title = firstLine(text, 90)
	}
	postInfo["title"] = title
	postInfo["description"] = text
	postInfo["auto_add_music"] = boolOpt(o, "auto_add_music", true)
	cover := 0
	if v, ok := o["photo_cover_index"].(float64); ok {
		cover = int(v)
	}
	var r struct {
		PublishID string `json:"publish_id"`
	}
	body := map[string]any{
		"post_info":   postInfo,
		"source_info": map[string]any{"source": "PULL_FROM_URL", "photo_images": urls, "photo_cover_index": cover},
		"post_mode":   "DIRECT_POST",
		"media_type":  "PHOTO",
	}
	if err := c.call(ctx, "POST", "/v2/post/publish/content/init/", body, &r); err != nil {
		return "", err
	}
	return r.PublishID, nil
}

func firstLine(s string, max int) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	r := []rune(s)
	if len(r) > max {
		r = r[:max]
	}
	return string(r)
}

func (c *Client) waitPublished(ctx context.Context, publishID string) (string, error) {
	wait := c.PollInterval
	if wait <= 0 {
		wait = 5 * time.Second
	}
	for i := 0; i < 180; i++ {
		var st struct {
			Status     string   `json:"status"`
			FailReason string   `json:"fail_reason"`
			PostIDs    []string `json:"publicaly_available_post_id"` // sic, TikTok's spelling
		}
		if err := c.call(ctx, "POST", "/v2/post/publish/status/fetch/", map[string]string{"publish_id": publishID}, &st); err != nil {
			return "", err
		}
		switch st.Status {
		case "PUBLISH_COMPLETE":
			if len(st.PostIDs) > 0 {
				return st.PostIDs[0], nil
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
	return "", errors.New("tiktok: publishing still processing after 15 minutes (publish_id " + publishID + ")")
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
}

const videoFields = "id,title,video_description,create_time,share_url,view_count,like_count,comment_count,share_count"

func (v video) metrics() map[string]int64 {
	return map[string]int64{"view_count": v.ViewCount, "like_count": v.LikeCount, "comment_count": v.CommentCount, "share_count": v.ShareCount}
}

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

func (c *Client) Comments(ctx context.Context, remoteID string) ([]providers.Comment, error) {
	return nil, providers.ErrUnsupported
}

func (c *Client) Reply(ctx context.Context, to, text string) (*providers.PublishResult, error) {
	return nil, providers.ErrUnsupported
}

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
		if err := c.call(ctx, "POST", "/v2/video/list/?fields="+videoFields, body, &r); err != nil {
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
			out = append(out, providers.HistoryItem{RemoteID: v.ID, Text: text, URL: v.ShareURL, CreatedAt: t, Metrics: v.metrics()})
		}
		if stop || !r.HasMore || len(r.Videos) == 0 {
			break
		}
		cursor = r.Cursor
	}
	if len(out) > max {
		out = out[:max]
	}
	return out, nil
}
