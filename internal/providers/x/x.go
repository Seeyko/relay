// Package x is the X (Twitter) connector, using API v2 with OAuth 1.0a user context.
//
// Reference: https://docs.x.com/x-api (posts, users, search, media upload v2).
package x

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/Seeyko/relay/internal/providers"
)

const DefaultBaseURL = "https://api.x.com"

// Media limits from https://docs.x.com/x-api/media/quickstart/best-practices.
const (
	maxImageBytes = 5 << 20
	maxGIFBytes   = 15 << 20
	maxVideoBytes = 16 << 30 // initialize accepts up to 16 GB (8 GB for non-Premium accounts)
)

func init() {
	providers.Register(providers.Factory{
		Name:             "x",
		Description:      "X / Twitter via API v2 (OAuth 1.0a user context). Publishing, threads (each part can carry its own media: up to 4 images or 1 video/GIF), images/videos, metrics, replies, history import.",
		MediaPerPart:     true,
		CredentialFields: []string{"consumer_key", "consumer_secret", "access_token", "access_token_secret"},
		Validate:         validate,
		Count:            WeightedLength,
		New: func(c map[string]string, _ func(map[string]string) error, hc *http.Client) (providers.Client, error) {
			for _, k := range []string{"consumer_key", "consumer_secret", "access_token", "access_token_secret"} {
				if c[k] == "" {
					return nil, fmt.Errorf("x: missing credential %q", k)
				}
			}
			base := c["base_url"]
			if base == "" {
				base = DefaultBaseURL
			}
			if hc == nil {
				hc = http.DefaultClient
			}
			return &Client{
				Base:   strings.TrimRight(base, "/"),
				HC:     hc,
				Auth:   &OAuth1{ConsumerKey: c["consumer_key"], ConsumerSecret: c["consumer_secret"], Token: c["access_token"], TokenSecret: c["access_token_secret"]},
				userID: c["user_id"], username: c["username"],
			}, nil
		},
	})
}

// validate applies X's limits. options.max_chars raises the per-post limit (Premium allows 25,000).
func validate(req providers.PublishRequest) []string {
	var errs []string
	limit := 280
	if v, ok := req.Options["max_chars"].(float64); ok && v > 0 {
		limit = int(v)
	}
	if len(req.Parts) > 25 {
		errs = append(errs, "X threads are limited to 25 posts here")
	}
	for i, p := range req.Parts {
		if n := WeightedLength(p); n > limit {
			errs = append(errs, fmt.Sprintf("part %d is %d characters (X limit %d, links count as 23)", i+1, n, limit))
		}
		if strings.TrimSpace(p) == "" && len(req.MediaOf(i)) == 0 {
			errs = append(errs, fmt.Sprintf("part %d is empty", i+1))
		}
	}
	errs = append(errs, validateMedia(req.Media, "")...)
	for i := 1; i < len(req.Parts); i++ {
		errs = append(errs, validateMedia(req.MediaOf(i), fmt.Sprintf("part %d: ", i+1))...)
	}
	return errs
}

// validateMedia checks the media of one post: up to 4 images, or one video or GIF.
func validateMedia(list []providers.MediaFile, prefix string) []string {
	var errs []string
	videos := 0
	for _, m := range list {
		switch cat := mediaCategory(m.Mime); {
		case cat == "tweet_video":
			videos++
			if m.Size > maxVideoBytes {
				errs = append(errs, fmt.Sprintf("%s is larger than X's 16 GB video limit", m.Filename))
			}
		case cat == "tweet_gif":
			videos++
			if m.Size > maxGIFBytes {
				errs = append(errs, fmt.Sprintf("%s is larger than X's 15 MB GIF limit", m.Filename))
			}
		case !strings.HasPrefix(m.Mime, "image/"):
			errs = append(errs, fmt.Sprintf("%s: X accepts images (JPG, PNG, GIF, WEBP) and videos only", m.Filename))
		case m.Size > maxImageBytes:
			errs = append(errs, fmt.Sprintf("%s is larger than X's 5 MB image limit", m.Filename))
		}
	}
	if len(list) > 4 {
		errs = append(errs, "X allows at most 4 media per post")
	}
	if videos > 0 && len(list) > 1 {
		errs = append(errs, "a video or GIF must be the only media of an X post")
	}
	for i := range errs {
		errs[i] = prefix + errs[i]
	}
	return errs
}

var linkRe = regexp.MustCompile(`https?://\S+`)

// WeightedLength approximates X's counting (https://docs.x.com/fundamentals/counting-characters,
// twitter-text v3 config): links count 23; code points in the Latin, punctuation and common symbol
// ranges count 1; everything else (CJK, emoji) counts 2. Emoji modifiers, variation selectors and
// zero-width joiners add nothing, so a ZWJ sequence or a flag counts 2 in total.
func WeightedLength(s string) int {
	n := 0
	s = linkRe.ReplaceAllStringFunc(s, func(string) string { n += 23; return "" })
	prevRI, prevZWJ := false, false
	for _, r := range s {
		isRI := r >= 0x1F1E6 && r <= 0x1F1FF // regional indicators: a pair is one flag
		switch {
		case isRI && prevRI:
			isRI = false // second half of a flag
		case prevZWJ:
			// a code point joined by ZWJ belongs to the previous emoji
		case r == 0x200D, r >= 0xFE00 && r <= 0xFE0F, r >= 0x1F3FB && r <= 0x1F3FF, r >= 0xE0020 && r <= 0xE007F, r == 0x20E3:
			// joiners, variation selectors, skin tones, tag sequences, keycap: weight 0
		case r <= 0x10FF, r >= 0x2000 && r <= 0x200C, r >= 0x2010 && r <= 0x201F, r >= 0x2032 && r <= 0x2037:
			n++
		default:
			n += 2
		}
		prevRI, prevZWJ = isRI, r == 0x200D
	}
	return n
}

// AccountMetrics returns follower and post counts (GET /2/users/me, user.fields=public_metrics). The same
// call reads profile_image_url, kept for AvatarURL.
func (c *Client) AccountMetrics(ctx context.Context) (map[string]int64, error) {
	var r struct {
		Data struct {
			ProfileImageURL string `json:"profile_image_url"`
			PublicMetrics   struct {
				Followers int64 `json:"followers_count"`
				Following int64 `json:"following_count"`
				Tweets    int64 `json:"tweet_count"`
				Listed    int64 `json:"listed_count"`
			} `json:"public_metrics"`
		} `json:"data"`
	}
	if err := c.do(ctx, "GET", "/2/users/me?user.fields=public_metrics,profile_image_url", nil, &r); err != nil {
		return nil, err
	}
	c.avatar = largeAvatar(r.Data.ProfileImageURL)
	m := r.Data.PublicMetrics
	return map[string]int64{"followers": m.Followers, "following": m.Following, "posts": m.Tweets, "listed": m.Listed}, nil
}

type Client struct {
	Base     string
	HC       *http.Client
	Auth     *OAuth1
	userID   string
	username string
	avatar   string // profile picture seen by the last Identify or AccountMetrics call
	// PollInterval for video processing status (overridable in tests).
	PollInterval time.Duration
}

// apiError is one entry of the top-level "errors" array X returns, sometimes with a 200.
type apiError struct {
	Title   string `json:"title"`
	Detail  string `json:"detail"`
	Message string `json:"message"`
	Type    string `json:"type"`
	Value   string `json:"value"`
}

func errorsText(es []apiError) string {
	var parts []string
	for _, e := range es {
		for _, s := range []string{e.Detail, e.Message, e.Title} {
			if s != "" {
				parts = append(parts, s)
				break
			}
		}
	}
	return strings.Join(parts, "; ")
}

func (c *Client) send(req *http.Request, out any) error {
	c.Auth.Sign(req, nil) // JSON and multipart bodies are not part of the OAuth 1.0a signature
	resp, err := c.HC.Do(req)
	if err != nil {
		return err
	}
	b, err := providers.ReadBody(resp)
	if err != nil {
		return err
	}
	if out != nil && len(bytes.TrimSpace(b)) > 0 {
		return json.Unmarshal(b, out)
	}
	return nil
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.send(req, out)
}

func (c *Client) Identify(ctx context.Context) (*providers.Identity, error) {
	var r struct {
		Data struct {
			ID, Name, Username string
			ProfileImageURL    string `json:"profile_image_url"`
		} `json:"data"`
	}
	if err := c.do(ctx, "GET", "/2/users/me?user.fields=profile_image_url", nil, &r); err != nil {
		return nil, err
	}
	c.userID, c.username = r.Data.ID, r.Data.Username
	c.avatar = largeAvatar(r.Data.ProfileImageURL)
	return &providers.Identity{RemoteUserID: r.Data.ID, Handle: r.Data.Username, Name: r.Data.Name, AvatarURL: c.avatar}, nil
}

// AvatarURL is the profile picture (GET /2/users/me, user.fields=profile_image_url), 400x400.
func (c *Client) AvatarURL(ctx context.Context) (string, error) {
	if c.avatar != "" {
		return c.avatar, nil
	}
	if _, err := c.Identify(ctx); err != nil {
		return "", err
	}
	return c.avatar, nil
}

// largeAvatar turns the 48x48 profile_image_url (..._normal.jpg) into its 400x400 variant.
func largeAvatar(u string) string {
	if i := strings.LastIndex(u, "_normal."); i >= 0 {
		return u[:i] + "_400x400." + u[i+len("_normal."):]
	}
	return u
}

func statusURL(user, id string) string {
	if user == "" {
		user = "i/web"
	}
	return "https://x.com/" + user + "/status/" + id
}

func (c *Client) tweetURL(id string) string { return statusURL(c.username, id) }

// tweetBody is the POST /2/tweets request (https://docs.x.com/x-api/posts/create-post).
// text is optional when media is attached.
type tweetBody struct {
	Text  string `json:"text,omitempty"`
	Media *struct {
		MediaIDs []string `json:"media_ids"`
	} `json:"media,omitempty"`
	Reply *struct {
		InReplyTo string `json:"in_reply_to_tweet_id"`
	} `json:"reply,omitempty"`
}

func (c *Client) createTweet(ctx context.Context, text string, mediaIDs []string, replyTo string) (string, error) {
	body := tweetBody{Text: text}
	if len(mediaIDs) > 0 {
		body.Media = &struct {
			MediaIDs []string `json:"media_ids"`
		}{mediaIDs}
	}
	if replyTo != "" {
		body.Reply = &struct {
			InReplyTo string `json:"in_reply_to_tweet_id"`
		}{replyTo}
	}
	var r struct {
		Data   struct{ ID string } `json:"data"`
		Errors []apiError          `json:"errors"`
	}
	if err := c.do(ctx, "POST", "/2/tweets", body, &r); err != nil {
		return "", err
	}
	if r.Data.ID == "" {
		return "", fmt.Errorf("x: no post id in response: %s", errorsText(r.Errors))
	}
	return r.Data.ID, nil
}

func (c *Client) Publish(ctx context.Context, req providers.PublishRequest) (*providers.PublishResult, error) {
	if len(req.Parts) == 0 {
		return nil, providers.Invalid("x: empty post")
	}
	if c.username == "" {
		_, _ = c.Identify(ctx) // best effort, for nice URLs
	}
	// Upload every part's media before the first post, so a bad file never leaves half a thread.
	// Media ids stay valid for 24 hours, far longer than a thread takes.
	mediaIDs := make([][]string, len(req.Parts))
	for i := range req.Parts {
		list := req.MediaOf(i)
		if len(list) > 4 {
			return nil, providers.Invalid(fmt.Sprintf("x: part %d has %d media (at most 4)", i+1, len(list)))
		}
		for _, m := range list {
			id, err := c.UploadMedia(ctx, m)
			if err != nil {
				return nil, fmt.Errorf("x: media %s: %w", m.Filename, err)
			}
			mediaIDs[i] = append(mediaIDs[i], id)
		}
	}
	replyTo, _ := req.Options["reply_to"].(string)
	res := &providers.PublishResult{}
	for i, part := range req.Parts {
		id, err := c.createTweet(ctx, part, mediaIDs[i], replyTo)
		if err != nil {
			if i > 0 {
				return res, fmt.Errorf("x: thread stopped at part %d/%d: %w", i+1, len(req.Parts), err)
			}
			return nil, err
		}
		res.RemoteIDs = append(res.RemoteIDs, id)
		replyTo = id
	}
	res.RemoteID = res.RemoteIDs[0]
	res.URL = c.tweetURL(res.RemoteID)
	return res, nil
}

func (c *Client) Reply(ctx context.Context, to, text string) (*providers.PublishResult, error) {
	id, err := c.createTweet(ctx, text, nil, to)
	if err != nil {
		return nil, err
	}
	return &providers.PublishResult{RemoteID: id, RemoteIDs: []string{id}, URL: c.tweetURL(id)}, nil
}

type tweet struct {
	ID               string           `json:"id"`
	Text             string           `json:"text"`
	AuthorID         string           `json:"author_id"`
	CreatedAt        time.Time        `json:"created_at"`
	PublicMetrics    map[string]int64 `json:"public_metrics"`
	NonPublic        map[string]int64 `json:"non_public_metrics"`
	ReferencedTweets []struct {
		Type string `json:"type"` // replied_to, quoted, retweeted
		ID   string `json:"id"`
	} `json:"referenced_tweets"`
	Attachments struct {
		MediaKeys []string `json:"media_keys"`
	} `json:"attachments"`
}

type media struct {
	MediaKey      string           `json:"media_key"`
	Type          string           `json:"type"`              // photo, video, animated_gif
	URL           string           `json:"url"`               // photos only
	PreviewImage  string           `json:"preview_image_url"` // videos and GIFs
	PublicMetrics map[string]int64 `json:"public_metrics"`
}

type tweetsResp struct {
	Data     []tweet `json:"data"`
	Includes struct {
		Media []media `json:"media"`
		Users []struct {
			ID       string `json:"id"`
			Username string `json:"username"`
		} `json:"users"`
	} `json:"includes"`
	Errors []apiError `json:"errors"`
	Meta   struct {
		NextToken string `json:"next_token"`
	} `json:"meta"`
}

func (r *tweetsResp) mediaByKey() map[string]media {
	m := map[string]media{}
	for _, x := range r.Includes.Media {
		m[x.MediaKey] = x
	}
	return m
}

// lookup calls GET /2/tweets for up to 100 ids (https://docs.x.com/x-api/posts/get-posts-by-ids).
func (c *Client) lookup(ctx context.Context, ids []string, fields string) (*tweetsResp, error) {
	q := url.Values{}
	q.Set("ids", strings.Join(ids, ","))
	q.Set("tweet.fields", fields)
	q.Set("expansions", "attachments.media_keys")
	q.Set("media.fields", "public_metrics")
	r := &tweetsResp{}
	return r, c.do(ctx, "GET", "/2/tweets?"+q.Encode(), nil, r)
}

// Metrics returns public_metrics (retweet, reply, like, quote, bookmark, impression counts),
// plus non_public_metrics (engagements, url_link_clicks, user_profile_clicks) for the
// account's own posts younger than 30 days, plus video_view_count for attached videos.
// See https://docs.x.com/x-api/fundamentals/metrics.
func (c *Client) Metrics(ctx context.Context, ids []string) (map[string]map[string]int64, error) {
	out := map[string]map[string]int64{}
	collect := func(r *tweetsResp) {
		mk := r.mediaByKey()
		for _, t := range r.Data {
			m := map[string]int64{}
			for k, v := range t.PublicMetrics {
				m[k] = v
			}
			for k, v := range t.NonPublic {
				m[k] = v
			}
			for _, key := range t.Attachments.MediaKeys {
				if v, ok := mk[key].PublicMetrics["view_count"]; ok {
					m["video_view_count"] += v
				}
			}
			out[t.ID] = m
		}
	}
	for start := 0; start < len(ids); start += 100 {
		batch := ids[start:min(start+100, len(ids))]
		// non_public_metrics needs user context on own posts < 30 days old: X answers either an
		// error status or 200 with per-post errors. Either way, retry the rest with public metrics.
		r, err := c.lookup(ctx, batch, "public_metrics,non_public_metrics,attachments")
		var he *providers.HTTPError
		if err != nil && !(errors.As(err, &he) && (he.Status == 400 || he.Status == 403)) {
			return out, err
		}
		if err == nil {
			collect(r)
		}
		var missing []string
		for _, id := range batch {
			if _, ok := out[id]; !ok {
				missing = append(missing, id)
			}
		}
		if len(missing) == 0 {
			continue
		}
		r, err = c.lookup(ctx, missing, "public_metrics,attachments")
		if err != nil {
			return out, err
		}
		collect(r)
	}
	return out, nil
}

// Comments lists replies in the post's conversation from the last 7 days
// (GET /2/tweets/search/recent, https://docs.x.com/x-api/posts/search-recent-posts).
func (c *Client) Comments(ctx context.Context, remoteID string) ([]providers.Comment, error) {
	var out []providers.Comment
	token := ""
	for page := 0; page < 5; page++ {
		q := url.Values{}
		q.Set("query", "conversation_id:"+remoteID)
		q.Set("max_results", "100")
		q.Set("tweet.fields", "author_id,created_at,conversation_id,in_reply_to_user_id")
		q.Set("expansions", "author_id")
		q.Set("user.fields", "username")
		if token != "" {
			q.Set("next_token", token)
		}
		var r tweetsResp
		if err := c.do(ctx, "GET", "/2/tweets/search/recent?"+q.Encode(), nil, &r); err != nil {
			return out, err
		}
		names := map[string]string{}
		for _, u := range r.Includes.Users {
			names[u.ID] = u.Username
		}
		for _, t := range r.Data {
			if c.userID != "" && t.AuthorID == c.userID {
				continue // our own thread parts / replies
			}
			ca := t.CreatedAt
			handle := names[t.AuthorID]
			author := handle
			if author != "" {
				author = "@" + author
			}
			out = append(out, providers.Comment{
				RemoteID: t.ID, Author: author, Text: t.Text, CreatedAt: &ca, URL: statusURL(handle, t.ID),
			})
		}
		if r.Meta.NextToken == "" {
			break
		}
		token = r.Meta.NextToken
	}
	return out, nil
}

// kind classifies a post from referenced_tweets and its attached media types.
func kind(t tweet, mk map[string]media) string {
	// Precedence: a repost wins over a quote, which wins over a reply.
	for _, ref := range [][2]string{{"retweeted", "repost"}, {"quoted", "quote"}, {"replied_to", "reply"}} {
		for _, rt := range t.ReferencedTweets {
			if rt.Type == ref[0] {
				return ref[1]
			}
		}
	}
	k := "text"
	for _, key := range t.Attachments.MediaKeys {
		switch mk[key].Type {
		case "video", "animated_gif":
			return "video"
		case "photo":
			k = "image"
		}
	}
	return k
}

// displaySize asks pbs.twimg.com for the "medium" rendition (at most 1200 px) instead of the original.
// See https://developer.x.com/en/docs/x-api/v1/data-dictionary/object-model/entities (media sizes).
func displaySize(u string) string {
	if u == "" || strings.Contains(u, "?") || !strings.HasPrefix(u, "https://pbs.twimg.com/media/") {
		return u
	}
	return u + "?name=medium"
}

// History pages GET /2/users/:id/tweets (https://docs.x.com/x-api/users/get-posts).
// X serves at most the 3,200 most recent posts of a timeline.
func (c *Client) History(ctx context.Context, since time.Time, max int) ([]providers.HistoryItem, error) {
	if c.userID == "" {
		if _, err := c.Identify(ctx); err != nil {
			return nil, err
		}
	}
	if max <= 0 {
		max = 3200
	}
	var out []providers.HistoryItem
	token := ""
	for len(out) < max {
		q := url.Values{}
		q.Set("max_results", "100")
		q.Set("tweet.fields", "created_at,public_metrics,referenced_tweets,attachments")
		q.Set("expansions", "attachments.media_keys")
		q.Set("media.fields", "type,public_metrics,url,preview_image_url")
		if !since.IsZero() {
			q.Set("start_time", since.UTC().Format(time.RFC3339))
		}
		if token != "" {
			q.Set("pagination_token", token)
		}
		var r tweetsResp
		if err := c.do(ctx, "GET", "/2/users/"+url.PathEscape(c.userID)+"/tweets?"+q.Encode(), nil, &r); err != nil {
			return out, err
		}
		mk := r.mediaByKey()
		for _, t := range r.Data {
			m := map[string]int64{}
			for k, v := range t.PublicMetrics {
				m[k] = v
			}
			for _, key := range t.Attachments.MediaKeys {
				if v, ok := mk[key].PublicMetrics["view_count"]; ok {
					m["video_view_count"] += v
				}
			}
			it := providers.HistoryItem{
				RemoteID: t.ID, Text: t.Text, URL: c.tweetURL(t.ID), CreatedAt: t.CreatedAt, Metrics: m, Kind: kind(t, mk),
			}
			for _, key := range t.Attachments.MediaKeys {
				switch md := mk[key]; md.Type {
				case "photo":
					it.AddVisual(displaySize(md.URL), false)
				case "video", "animated_gif":
					it.AddVisual(md.PreviewImage, true)
				}
			}
			out = append(out, it)
		}
		if r.Meta.NextToken == "" {
			break
		}
		token = r.Meta.NextToken
	}
	if len(out) > max {
		out = out[:max]
	}
	return out, nil
}

// ---------- media upload (API v2) ----------
// https://docs.x.com/x-api/media/upload-media (simple) and
// https://docs.x.com/x-api/media/quickstart/media-upload-chunked (initialize/append/finalize/status).

func mediaCategory(mime string) string {
	switch {
	case strings.HasPrefix(mime, "video/"):
		return "tweet_video"
	case mime == "image/gif":
		return "tweet_gif"
	default:
		return "tweet_image"
	}
}

type mediaResp struct {
	Data struct {
		ID             string `json:"id"`
		ProcessingInfo *struct {
			State          string `json:"state"` // pending, in_progress, failed, succeeded
			CheckAfterSecs int    `json:"check_after_secs"`
			Error          *struct {
				Message string `json:"message"`
			} `json:"error"`
		} `json:"processing_info"`
	} `json:"data"`
	Errors []apiError `json:"errors"`
}

func (c *Client) multipart(ctx context.Context, path string, fields [][2]string, file io.Reader, filename string) (*mediaResp, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for _, f := range fields {
		_ = w.WriteField(f[0], f[1])
	}
	fw, err := w.CreateFormFile("media", filename) // Content-Type: application/octet-stream
	if err != nil {
		return nil, err
	}
	if _, err := io.Copy(fw, file); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.Base+path, &buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	r := &mediaResp{}
	return r, c.send(req, r)
}

// chunkSize stays under the 5 MB per-segment recommendation (8 MB hard maximum).
const chunkSize = 4 << 20

// UploadMedia uploads an image in one request, or a video/GIF with the chunked flow.
func (c *Client) UploadMedia(ctx context.Context, m providers.MediaFile) (string, error) {
	cat := mediaCategory(m.Mime)
	if cat != "tweet_image" && m.Size <= 0 {
		return "", errors.New("x: chunked upload needs the file size (total_bytes)")
	}
	f, err := m.Open()
	if err != nil {
		return "", err
	}
	defer f.Close()
	if cat == "tweet_image" {
		// The schema allows only media, media_category and additional_owners.
		r, err := c.multipart(ctx, "/2/media/upload", [][2]string{{"media_category", cat}}, f, m.Filename)
		if err != nil {
			return "", err
		}
		if r.Data.ID == "" {
			return "", fmt.Errorf("x: no media id in response: %s", errorsText(r.Errors))
		}
		return r.Data.ID, nil
	}
	var ini mediaResp
	if err := c.do(ctx, "POST", "/2/media/upload/initialize", map[string]any{"media_type": m.Mime, "total_bytes": m.Size, "media_category": cat}, &ini); err != nil {
		return "", err
	}
	id := ini.Data.ID
	if id == "" {
		return "", fmt.Errorf("x: no upload id in response: %s", errorsText(ini.Errors))
	}
	buf := make([]byte, chunkSize)
	for seg := 0; ; seg++ {
		n, rerr := io.ReadFull(f, buf)
		if n > 0 {
			if _, err := c.multipart(ctx, "/2/media/upload/"+id+"/append", [][2]string{{"segment_index", fmt.Sprint(seg)}}, bytes.NewReader(buf[:n]), m.Filename); err != nil {
				return "", err
			}
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			break
		}
		if rerr != nil {
			return "", rerr
		}
	}
	var st mediaResp
	if err := c.do(ctx, "POST", "/2/media/upload/"+id+"/finalize", nil, &st); err != nil {
		return "", err
	}
	for i := 0; ; i++ {
		info := st.Data.ProcessingInfo
		if info == nil || info.State == "succeeded" {
			return id, nil
		}
		if info.State == "failed" {
			msg := errorsText(st.Errors)
			if info.Error != nil && info.Error.Message != "" {
				msg = info.Error.Message
			}
			return "", providers.Invalid("x: media processing failed: " + msg)
		}
		if i >= 120 {
			return "", errors.New("x: media still processing, giving up")
		}
		wait := time.Duration(info.CheckAfterSecs) * time.Second
		if c.PollInterval > 0 {
			wait = c.PollInterval
		}
		if wait <= 0 {
			wait = 2 * time.Second
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(wait):
		}
		st = mediaResp{}
		if err := c.do(ctx, "GET", "/2/media/upload?command=STATUS&media_id="+url.QueryEscape(id), nil, &st); err != nil {
			return "", err
		}
	}
}
