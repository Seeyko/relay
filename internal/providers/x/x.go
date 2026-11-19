// Package x is the X (Twitter) connector, using API v2 with OAuth 1.0a user context.
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

func init() {
	providers.Register(providers.Factory{
		Name:             "x",
		Description:      "X / Twitter via API v2 (OAuth 1.0a user context). Publishing, threads, images/videos, metrics, replies, history import.",
		CredentialFields: []string{"consumer_key", "consumer_secret", "access_token", "access_token_secret"},
		Validate:         validate,
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
		if strings.TrimSpace(p) == "" && !(i == 0 && len(req.Media) > 0) {
			errs = append(errs, fmt.Sprintf("part %d is empty", i+1))
		}
	}
	videos := 0
	for _, m := range req.Media {
		if strings.HasPrefix(m.Mime, "video/") || m.Mime == "image/gif" {
			videos++
		}
	}
	if len(req.Media) > 4 {
		errs = append(errs, "X allows at most 4 media per post")
	}
	if videos > 0 && len(req.Media) > 1 {
		errs = append(errs, "a video or GIF must be the only media of an X post")
	}
	return errs
}

var linkRe = regexp.MustCompile(`https?://\S+`)

// WeightedLength approximates X's counting: links count 23, CJK and emoji count 2.
func WeightedLength(s string) int {
	n := 0
	s = linkRe.ReplaceAllStringFunc(s, func(string) string { n += 23; return "" })
	for _, r := range s {
		switch {
		case r <= 0x10FF, r >= 0x2000 && r <= 0x200D, r >= 0x2010 && r <= 0x201F, r >= 0x2032 && r <= 0x2037:
			n++
		default:
			n += 2
		}
	}
	return n
}

// AccountMetrics returns follower and post counts.
func (c *Client) AccountMetrics(ctx context.Context) (map[string]int64, error) {
	var r struct {
		Data struct {
			PublicMetrics struct {
				Followers int64 `json:"followers_count"`
				Following int64 `json:"following_count"`
				Tweets    int64 `json:"tweet_count"`
				Likes     int64 `json:"like_count"`
			} `json:"public_metrics"`
		} `json:"data"`
	}
	if err := c.do(ctx, "GET", "/2/users/me?user.fields=public_metrics", nil, &r); err != nil {
		return nil, err
	}
	m := r.Data.PublicMetrics
	return map[string]int64{"followers": m.Followers, "following": m.Following, "posts": m.Tweets}, nil
}

type Client struct {
	Base     string
	HC       *http.Client
	Auth     *OAuth1
	userID   string
	username string
	// PollInterval for video processing status (overridable in tests).
	PollInterval time.Duration
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
	c.Auth.Sign(req, nil)
	resp, err := c.HC.Do(req)
	if err != nil {
		return err
	}
	b, err := providers.ReadBody(resp)
	if err != nil {
		return err
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

func (c *Client) Identify(ctx context.Context) (*providers.Identity, error) {
	var r struct {
		Data struct{ ID, Name, Username string } `json:"data"`
	}
	if err := c.do(ctx, "GET", "/2/users/me", nil, &r); err != nil {
		return nil, err
	}
	c.userID, c.username = r.Data.ID, r.Data.Username
	return &providers.Identity{RemoteUserID: r.Data.ID, Handle: r.Data.Username, Name: r.Data.Name}, nil
}

func (c *Client) tweetURL(id string) string {
	u := c.username
	if u == "" {
		u = "i/web"
	}
	return "https://x.com/" + u + "/status/" + id
}

type tweetBody struct {
	Text  string `json:"text"`
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
		Data struct{ ID string } `json:"data"`
	}
	if err := c.do(ctx, "POST", "/2/tweets", body, &r); err != nil {
		return "", err
	}
	if r.Data.ID == "" {
		return "", errors.New("x: empty tweet id in response")
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
	var mediaIDs []string
	for _, m := range req.Media {
		id, err := c.UploadMedia(ctx, m)
		if err != nil {
			return nil, fmt.Errorf("x: media %s: %w", m.Filename, err)
		}
		mediaIDs = append(mediaIDs, id)
	}
	replyTo, _ := req.Options["reply_to"].(string)
	res := &providers.PublishResult{}
	for i, part := range req.Parts {
		var mids []string
		if i == 0 {
			mids = mediaIDs
		}
		id, err := c.createTweet(ctx, part, mids, replyTo)
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
	ID            string           `json:"id"`
	Text          string           `json:"text"`
	AuthorID      string           `json:"author_id"`
	CreatedAt     time.Time        `json:"created_at"`
	PublicMetrics map[string]int64 `json:"public_metrics"`
	NonPublic     map[string]int64 `json:"non_public_metrics"`
}

func (c *Client) Metrics(ctx context.Context, ids []string) (map[string]map[string]int64, error) {
	out := map[string]map[string]int64{}
	for start := 0; start < len(ids); start += 100 {
		end := start + 100
		if end > len(ids) {
			end = len(ids)
		}
		batch := strings.Join(ids[start:end], ",")
		var r struct {
			Data []tweet `json:"data"`
		}
		// non_public_metrics (impressions, profile clicks...) only works on own tweets < 30 days old;
		// fall back to public metrics when X refuses.
		err := c.do(ctx, "GET", "/2/tweets?ids="+url.QueryEscape(batch)+"&tweet.fields="+url.QueryEscape("public_metrics,non_public_metrics"), nil, &r)
		if err != nil {
			var he *providers.HTTPError
			if errors.As(err, &he) && (he.Status == 400 || he.Status == 403) {
				r.Data = nil
				err = c.do(ctx, "GET", "/2/tweets?ids="+url.QueryEscape(batch)+"&tweet.fields=public_metrics", nil, &r)
			}
			if err != nil {
				return out, err
			}
		}
		for _, t := range r.Data {
			m := map[string]int64{}
			for k, v := range t.PublicMetrics {
				m[k] = v
			}
			for k, v := range t.NonPublic {
				m[k] = v
			}
			out[t.ID] = m
		}
	}
	return out, nil
}

func (c *Client) Comments(ctx context.Context, remoteID string) ([]providers.Comment, error) {
	q := url.Values{}
	q.Set("query", "conversation_id:"+remoteID)
	q.Set("max_results", "100")
	q.Set("tweet.fields", "author_id,created_at,in_reply_to_user_id")
	q.Set("expansions", "author_id")
	q.Set("user.fields", "username")
	var r struct {
		Data     []tweet `json:"data"`
		Includes struct {
			Users []struct{ ID, Username string } `json:"users"`
		} `json:"includes"`
	}
	if err := c.do(ctx, "GET", "/2/tweets/search/recent?"+q.Encode(), nil, &r); err != nil {
		return nil, err
	}
	names := map[string]string{}
	for _, u := range r.Includes.Users {
		names[u.ID] = u.Username
	}
	var out []providers.Comment
	for _, t := range r.Data {
		if c.userID != "" && t.AuthorID == c.userID {
			continue // our own thread parts / replies
		}
		ca := t.CreatedAt
		handle := names[t.AuthorID]
		out = append(out, providers.Comment{
			RemoteID: t.ID, Author: "@" + handle, Text: t.Text, CreatedAt: &ca,
			URL: "https://x.com/" + handle + "/status/" + t.ID,
		})
	}
	return out, nil
}

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
		q.Set("tweet.fields", "created_at,public_metrics")
		if !since.IsZero() {
			q.Set("start_time", since.UTC().Format(time.RFC3339))
		}
		if token != "" {
			q.Set("pagination_token", token)
		}
		var r struct {
			Data []tweet `json:"data"`
			Meta struct {
				NextToken string `json:"next_token"`
			} `json:"meta"`
		}
		if err := c.do(ctx, "GET", "/2/users/"+c.userID+"/tweets?"+q.Encode(), nil, &r); err != nil {
			return out, err
		}
		for _, t := range r.Data {
			out = append(out, providers.HistoryItem{RemoteID: t.ID, Text: t.Text, URL: c.tweetURL(t.ID), CreatedAt: t.CreatedAt, Metrics: t.PublicMetrics})
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
			State          string `json:"state"`
			CheckAfterSecs int    `json:"check_after_secs"`
			Error          *struct {
				Message string `json:"message"`
			} `json:"error"`
		} `json:"processing_info"`
	} `json:"data"`
}

func (c *Client) multipart(ctx context.Context, path string, fields map[string]string, file io.Reader, filename string) (*mediaResp, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range fields {
		_ = w.WriteField(k, v)
	}
	if file != nil {
		fw, err := w.CreateFormFile("media", filename)
		if err != nil {
			return nil, err
		}
		if _, err := io.Copy(fw, file); err != nil {
			return nil, err
		}
	}
	w.Close()
	req, err := http.NewRequestWithContext(ctx, "POST", c.Base+path, &buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	c.Auth.Sign(req, nil)
	resp, err := c.HC.Do(req)
	if err != nil {
		return nil, err
	}
	b, err := providers.ReadBody(resp)
	if err != nil {
		return nil, err
	}
	r := &mediaResp{}
	if len(bytes.TrimSpace(b)) > 0 {
		if err := json.Unmarshal(b, r); err != nil {
			return nil, err
		}
	}
	return r, nil
}

const chunkSize = 4 << 20

// UploadMedia uploads an image in one request, or a video/GIF with the chunked flow.
func (c *Client) UploadMedia(ctx context.Context, m providers.MediaFile) (string, error) {
	f, err := m.Open()
	if err != nil {
		return "", err
	}
	defer f.Close()
	cat := mediaCategory(m.Mime)
	if cat == "tweet_image" {
		r, err := c.multipart(ctx, "/2/media/upload", map[string]string{"media_category": cat, "media_type": m.Mime}, f, m.Filename)
		if err != nil {
			return "", err
		}
		return r.Data.ID, nil
	}
	// chunked: initialize -> append* -> finalize -> poll status
	var ini mediaResp
	if err := c.do(ctx, "POST", "/2/media/upload/initialize", map[string]any{"media_type": m.Mime, "total_bytes": m.Size, "media_category": cat}, &ini); err != nil {
		return "", err
	}
	id := ini.Data.ID
	buf := make([]byte, chunkSize)
	for seg := 0; ; seg++ {
		n, rerr := io.ReadFull(f, buf)
		if n > 0 {
			if _, err := c.multipart(ctx, "/2/media/upload/"+id+"/append", map[string]string{"segment_index": fmt.Sprint(seg)}, bytes.NewReader(buf[:n]), m.Filename); err != nil {
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
	var fin mediaResp
	if err := c.do(ctx, "POST", "/2/media/upload/"+id+"/finalize", nil, &fin); err != nil {
		return "", err
	}
	info := fin.Data.ProcessingInfo
	poll := c.PollInterval
	for i := 0; info != nil && i < 60; i++ {
		switch info.State {
		case "succeeded":
			return id, nil
		case "failed":
			msg := "processing failed"
			if info.Error != nil {
				msg = info.Error.Message
			}
			return "", errors.New("x: " + msg)
		}
		wait := time.Duration(info.CheckAfterSecs) * time.Second
		if poll > 0 {
			wait = poll
		}
		if wait <= 0 {
			wait = 2 * time.Second
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(wait):
		}
		var st mediaResp
		if err := c.do(ctx, "GET", "/2/media/upload?command=STATUS&media_id="+id, nil, &st); err != nil {
			return "", err
		}
		info = st.Data.ProcessingInfo
	}
	return id, nil
}
