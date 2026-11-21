// Package threads is the Threads (Meta) connector, built on the Threads API (graph.threads.net).
//
// Publishing follows the documented two-step flow: create a media container
// (POST /{user-id}/threads), wait until its status is FINISHED, then publish it
// (POST /{user-id}/threads_publish). Meta downloads media from MediaFile.PublicURL.
package threads

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Seeyko/relay/internal/providers"
)

// Limits from the Threads API docs (Posts guide, media specifications, publishing reference).
const (
	DefaultVersion = "v1.0"
	DefaultBase    = "https://graph.threads.net"

	maxText        = 500
	maxLinks       = 5
	minCarousel    = 2
	maxCarousel    = 20
	maxAltText     = 1000
	maxTopicTag    = 50
	maxImage       = 8 << 20
	maxVideo       = 1 << 30
	timeLayout     = "2006-01-02T15:04:05-0700"
	pollDefault    = 10 * time.Second
	pollDeadline   = 10 * time.Minute
	publishRetries = 3
	// earliestInsights is the earliest `since` the user insights endpoint accepts (2024-04-13).
	earliestInsights = 1712991600
)

var (
	linkRe        = regexp.MustCompile(`https?://\S+`)
	replyControls = map[string]bool{"everyone": true, "accounts_you_follow": true, "mentioned_only": true,
		"parent_post_author_only": true, "followers_only": true}
)

func init() {
	providers.Register(providers.Factory{
		Name: "threads",
		Description: "Threads profile via the Threads API (threads_basic, threads_content_publish, threads_manage_replies, " +
			"threads_manage_insights). Text (500 characters, up to 5 links), image (JPEG/PNG), video (MP4/MOV), carousel (2 to 20 items), " +
			"threads (each part is a reply to the previous one and can carry its own image, video or carousel), options reply_to, quote_post_id, link_attachment (text posts), " +
			"reply_control, topic_tag, location_id, made_with_ai; views/likes/replies/reposts/quotes/shares insights; replies read + answer; " +
			"history import; followers. Needs RELAY_BASE_URL reachable from the internet: Threads downloads the media from Relay. " +
			"Long-lived token (60 days) is refreshed automatically.",
		CredentialFields: []string{"access_token", "user_id"},
		Validate:         validate,
		MediaPerPart:     true,
		New: func(c map[string]string, persist func(map[string]string) error, hc *http.Client) (providers.Client, error) {
			if c["access_token"] == "" {
				return nil, errors.New("threads: access_token is required")
			}
			v := c["api_version"]
			if v == "" {
				v = DefaultVersion
			}
			root := strings.TrimRight(c["base_url"], "/")
			if root == "" {
				root = DefaultBase
			}
			if hc == nil {
				hc = http.DefaultClient
			}
			return &Client{UserID: c["user_id"], Username: c["username"],
				api: &api{Base: root + "/" + v, Token: c["access_token"], HC: hc}, root: root, creds: c, persist: persist}, nil
		},
	})
}

// TextLen counts characters the way Threads does: one per code point, except emojis,
// which count as their UTF-8 byte length (Posts guide). Accented letters count as one.
func TextLen(s string) int {
	n := 0
	for _, r := range s {
		if isEmoji(r) {
			n += utf8.RuneLen(r)
		} else {
			n++
		}
	}
	return n
}

func isEmoji(r rune) bool {
	switch {
	case r >= 0x1F000 && r <= 0x1FAFF, // pictographs, emoticons, transport, flags, symbols
		r >= 0x2600 && r <= 0x27BF,            // misc symbols, dingbats
		r >= 0x2300 && r <= 0x23FF,            // misc technical (watch, hourglass...)
		r >= 0x2B00 && r <= 0x2BFF,            // arrows, stars
		r == 0x200D, r == 0xFE0F, r == 0x20E3: // ZWJ, variation selector, keycap
		return true
	}
	return false
}

func isVideo(m providers.MediaFile) bool { return strings.HasPrefix(m.Mime, "video/") }

func optString(o map[string]any, k string) string {
	switch v := o[k].(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case int, int64, bool:
		return fmt.Sprint(v)
	}
	return ""
}

func optBool(o map[string]any, k string) bool {
	switch v := o[k].(type) {
	case bool:
		return v
	case string:
		b, _ := strconv.ParseBool(v)
		return b
	}
	return false
}

func validate(req providers.PublishRequest) []string {
	var errs []string
	if len(req.Parts) == 0 && len(req.Media) == 0 {
		errs = append(errs, "Threads needs text or media")
	}
	for i, p := range req.Parts {
		if n := TextLen(p); n > maxText {
			errs = append(errs, fmt.Sprintf("Threads posts are limited to %d characters (part %d has %d; emojis count as several)", maxText, i+1, n))
		}
		if n := len(linkRe.FindAllString(p, -1)); n > maxLinks {
			errs = append(errs, fmt.Sprintf("Threads posts take at most %d links (part %d has %d)", maxLinks, i+1, n))
		}
		if strings.TrimSpace(p) == "" && len(req.MediaOf(i)) == 0 {
			errs = append(errs, fmt.Sprintf("Threads part %d is empty", i+1))
		}
	}
	errs = append(errs, validateMedia(req.Media, "")...)
	for i := 1; i < len(req.Parts); i++ {
		errs = append(errs, validateMedia(req.MediaOf(i), fmt.Sprintf("part %d: ", i+1))...)
	}
	if optString(req.Options, "link_attachment") != "" && len(req.Media) > 0 {
		errs = append(errs, "Threads options.link_attachment only works on text posts (no media)")
	}
	if rc := optString(req.Options, "reply_control"); rc != "" && !replyControls[rc] {
		errs = append(errs, "Threads options.reply_control must be everyone, accounts_you_follow, mentioned_only, parent_post_author_only or followers_only")
	}
	if t := optString(req.Options, "topic_tag"); t != "" {
		if n := providers.CharCount(t); n > maxTopicTag || strings.ContainsAny(t, ".&") {
			errs = append(errs, "Threads options.topic_tag must be 1 to 50 characters, without periods or ampersands")
		}
	}
	return errs
}

// validateMedia checks the media of one post: one image or video, or a carousel of up to 20 items.
func validateMedia(list []providers.MediaFile, prefix string) []string {
	var errs []string
	if len(list) > maxCarousel {
		errs = append(errs, fmt.Sprintf("Threads carousels take at most %d media (got %d)", maxCarousel, len(list)))
	}
	for _, m := range list {
		if isVideo(m) {
			if m.Mime != "video/mp4" && m.Mime != "video/quicktime" {
				errs = append(errs, "Threads videos must be MP4 or MOV ("+m.Filename+" is "+m.Mime+")")
			}
			if m.Size > maxVideo {
				errs = append(errs, fmt.Sprintf("Threads videos are limited to 1 GB (%s is %d MB)", m.Filename, m.Size>>20))
			}
		} else {
			switch m.Mime {
			case "", "image/jpeg", "image/jpg", "image/png":
			default:
				errs = append(errs, "Threads only accepts JPEG or PNG images and MP4/MOV videos ("+m.Filename+" is "+m.Mime+")")
			}
			if m.Size > maxImage {
				errs = append(errs, fmt.Sprintf("Threads images are limited to 8 MB (%s is %d MB)", m.Filename, m.Size>>20))
			}
		}
		if n := providers.CharCount(m.Alt); n > maxAltText {
			errs = append(errs, fmt.Sprintf("Threads alt text is limited to %d characters (got %d)", maxAltText, n))
		}
	}
	for i := range errs {
		errs[i] = prefix + errs[i]
	}
	return errs
}

// Client is bound to one Threads profile.
type Client struct {
	UserID, Username string
	api              *api
	root             string
	creds            map[string]string
	persist          func(map[string]string) error
	avatar           string // threads_profile_picture_url seen by the last Identify call
	// PollInterval for container processing (tests override it).
	PollInterval time.Duration
}

// maybeRefresh renews the 60-day long-lived token when it is older than 30 days.
// Threads only refreshes tokens that are at least 24 hours old and not expired
// (GET /refresh_access_token?grant_type=th_refresh_token, unversioned).
func (c *Client) maybeRefresh(ctx context.Context) {
	if c.persist == nil {
		return
	}
	last, _ := time.Parse(time.RFC3339, c.creds["token_refreshed_at"])
	if time.Since(last) < 30*24*time.Hour {
		return
	}
	var r struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	q := url.Values{"grant_type": {"th_refresh_token"}, "access_token": {c.api.Token}}
	req, err := http.NewRequestWithContext(ctx, "GET", c.root+"/refresh_access_token?"+q.Encode(), nil)
	if err != nil {
		return
	}
	now := time.Now().UTC()
	if err := c.api.send(req, &r); err != nil || r.AccessToken == "" {
		if last.IsZero() {
			// Unknown age (likely a fresh token under 24h): try again in about a day, not on every call.
			c.creds["token_refreshed_at"] = now.Add(-29 * 24 * time.Hour).Format(time.RFC3339)
			_ = c.persist(c.creds)
		}
		return
	}
	c.api.Token = r.AccessToken
	c.creds["access_token"] = r.AccessToken
	c.creds["token_refreshed_at"] = now.Format(time.RFC3339)
	if r.ExpiresIn > 0 {
		c.creds["token_expires_at"] = now.Add(time.Duration(r.ExpiresIn) * time.Second).Format(time.RFC3339)
	}
	_ = c.persist(c.creds)
}

// getFields reads path with fields; on a 400 (a field this app cannot read) it retries with each
// fallback field set in turn.
func (c *Client) getFields(ctx context.Context, path string, q url.Values, out any, fields string, fallbacks ...string) error {
	q.Set("fields", fields)
	err := c.api.get(ctx, path, q, out)
	for _, fb := range fallbacks {
		var he *providers.HTTPError
		if err == nil || fb == "" || !errors.As(err, &he) || he.Status != 400 {
			break
		}
		q.Set("fields", fb)
		err = c.api.get(ctx, path, q, out)
	}
	return err
}

func (c *Client) Identify(ctx context.Context) (*providers.Identity, error) {
	c.maybeRefresh(ctx)
	var r struct {
		ID       string `json:"id"`
		Username string `json:"username"`
		Name     string `json:"name"`
		Picture  string `json:"threads_profile_picture_url"`
	}
	if err := c.getFields(ctx, "/me", url.Values{}, &r, "id,username,name,threads_profile_picture_url", "id,username,name", "id,username"); err != nil {
		return nil, err
	}
	c.UserID, c.Username, c.avatar = r.ID, r.Username, r.Picture
	name := r.Name
	if name == "" {
		name = r.Username
	}
	return &providers.Identity{RemoteUserID: r.ID, Handle: r.Username, Name: name, AvatarURL: r.Picture}, nil
}

// AvatarURL is the profile picture (GET /me?fields=threads_profile_picture_url, threads_basic scope).
// AccountMetrics reads insights, not /me, so the daily refresh costs one small /me call.
func (c *Client) AvatarURL(ctx context.Context) (string, error) {
	if c.avatar != "" {
		return c.avatar, nil
	}
	if _, err := c.Identify(ctx); err != nil {
		return "", err
	}
	return c.avatar, nil
}

func (c *Client) me(ctx context.Context) (string, error) {
	if c.UserID != "" {
		return c.UserID, nil
	}
	id, err := c.Identify(ctx)
	if err != nil {
		return "", err
	}
	return id.RemoteUserID, nil
}

func (c *Client) wait(ctx context.Context) error {
	d := c.PollInterval
	if d <= 0 {
		d = pollDefault
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// waitReady polls GET /{container-id}?fields=status,error_message
// (EXPIRED, ERROR, FINISHED, IN_PROGRESS, PUBLISHED).
func (c *Client) waitReady(ctx context.Context, id string) error {
	deadline := time.Now().Add(pollDeadline)
	for {
		var r struct {
			Status       string `json:"status"`
			ErrorMessage string `json:"error_message"`
		}
		if err := c.api.get(ctx, "/"+id, url.Values{"fields": {"status,error_message"}}, &r); err != nil {
			return err
		}
		switch r.Status {
		case "FINISHED", "PUBLISHED", "":
			return nil
		case "ERROR":
			if r.ErrorMessage == "FAILED_DOWNLOADING_VIDEO" || r.ErrorMessage == "UNKNOWN" || r.ErrorMessage == "" {
				// A failed download can succeed with a new container: keep it retryable.
				return errors.New("threads: media processing failed: " + r.ErrorMessage)
			}
			// INVALID_DURATION, INVALID_ASPEC_RATIO... the same file will always fail.
			return providers.Invalid("threads: media rejected: " + r.ErrorMessage)
		case "EXPIRED":
			return errors.New("threads: media container expired (not published within 24 hours)")
		}
		if time.Now().After(deadline) {
			return errors.New("threads: media processing timed out")
		}
		if err := c.wait(ctx); err != nil {
			return err
		}
	}
}

func (c *Client) container(ctx context.Context, uid string, form url.Values) (string, error) {
	var r idResp
	if err := c.api.post(ctx, "/"+uid+"/threads", form, &r); err != nil {
		return "", err
	}
	if r.ID == "" {
		return "", errors.New("threads: container creation returned no id")
	}
	return r.ID, nil
}

func mediaForm(m providers.MediaFile) (url.Values, error) {
	if m.PublicURL == "" {
		return nil, providers.Invalid("threads: RELAY_BASE_URL must be set so Threads can download the media")
	}
	f := url.Values{}
	if isVideo(m) {
		f.Set("media_type", "VIDEO")
		f.Set("video_url", m.PublicURL)
	} else {
		f.Set("media_type", "IMAGE")
		f.Set("image_url", m.PublicURL)
	}
	if m.Alt != "" {
		f.Set("alt_text", m.Alt)
	}
	return f, nil
}

// postOptions are the container parameters copied from the post options onto every part.
var postOptions = []string{"reply_control", "topic_tag", "location_id"}

// createPart builds the (ready) container of one post: text, single media or carousel.
func (c *Client) createPart(ctx context.Context, uid, text string, media []providers.MediaFile, replyTo string, o map[string]any, first bool) (string, error) {
	var form url.Values
	switch {
	case len(media) == 0:
		form = url.Values{"media_type": {"TEXT"}}
		if first {
			if l := optString(o, "link_attachment"); l != "" {
				form.Set("link_attachment", l)
			}
		}
	case len(media) == 1:
		f, err := mediaForm(media[0])
		if err != nil {
			return "", err
		}
		form = f
	default:
		if len(media) > maxCarousel {
			return "", providers.Invalid(fmt.Sprintf("threads: at most %d media per carousel", maxCarousel))
		}
		var children []string
		for _, m := range media {
			f, err := mediaForm(m)
			if err != nil {
				return "", err
			}
			f.Set("is_carousel_item", "true") // is_made_with_ai is rejected here: it goes on the parent
			id, err := c.container(ctx, uid, f)
			if err != nil {
				return "", err
			}
			children = append(children, id)
		}
		for _, id := range children {
			if err := c.waitReady(ctx, id); err != nil {
				return "", err
			}
		}
		form = url.Values{"media_type": {"CAROUSEL"}, "children": {strings.Join(children, ",")}}
	}
	if text != "" {
		form.Set("text", text)
	}
	if replyTo != "" {
		form.Set("reply_to_id", replyTo)
	}
	if first {
		if q := optString(o, "quote_post_id"); q != "" {
			form.Set("quote_post_id", q)
		}
	}
	for _, k := range postOptions {
		if v := optString(o, k); v != "" {
			form.Set(k, v)
		}
	}
	if optBool(o, "made_with_ai") {
		form.Set("is_made_with_ai", "true")
	}
	id, err := c.container(ctx, uid, form)
	if err != nil {
		return "", err
	}
	if err := c.waitReady(ctx, id); err != nil {
		return "", err
	}
	return id, nil
}

// publish runs threads_publish, retrying while Threads does not see the container yet (4279009).
func (c *Client) publish(ctx context.Context, uid, creation string) (string, error) {
	for attempt := 0; ; attempt++ {
		var r idResp
		err := c.api.post(ctx, "/"+uid+"/threads_publish", url.Values{"creation_id": {creation}}, &r)
		if err == nil {
			return r.ID, nil
		}
		if code, sub, _ := graphError(err); code == 4279009 || sub == 4279009 {
			if attempt < publishRetries {
				if err := c.wait(ctx); err != nil {
					return "", err
				}
				continue
			}
			return "", fmt.Errorf("threads: media container not found yet, try again: %w", err)
		}
		return "", err
	}
}

func (c *Client) permalink(ctx context.Context, id string) string {
	var p struct {
		Permalink string `json:"permalink"`
	}
	// The post is live: never fail over a cosmetic field.
	if err := c.api.get(ctx, "/"+id, url.Values{"fields": {"permalink"}}, &p); err != nil || p.Permalink == "" {
		if c.Username != "" {
			return "https://www.threads.net/@" + c.Username
		}
	}
	return p.Permalink
}

// Publish posts one Threads post or a chain: each part after the first replies to the previous one.
// Each part carries its own media (req.MediaOf). options.reply_to makes the first part a reply to an
// existing post; with options.thread_root (a resumed thread) the first part is not the thread's first post,
// so the first-post-only options (link_attachment, quote_post_id) are not sent again.
func (c *Client) Publish(ctx context.Context, req providers.PublishRequest) (*providers.PublishResult, error) {
	c.maybeRefresh(ctx)
	parts := req.Parts
	if len(parts) == 0 {
		if len(req.Media) == 0 {
			return nil, providers.Invalid("threads: empty post")
		}
		parts = []string{""}
	}
	for i, p := range parts {
		if TextLen(p) > maxText {
			return nil, providers.Invalid(fmt.Sprintf("threads: part %d is over %d characters", i+1, maxText))
		}
	}
	uid, err := c.me(ctx)
	if err != nil {
		return nil, err
	}
	replyTo := optString(req.Options, "reply_to")
	resumed := optString(req.Options, "thread_root") != ""
	res := &providers.PublishResult{}
	for i, text := range parts {
		id, err := c.createPart(ctx, uid, text, req.MediaOf(i), replyTo, req.Options, i == 0 && !resumed)
		if err == nil {
			id, err = c.publish(ctx, uid, id)
		}
		if err != nil {
			if i > 0 {
				res.RemoteID = res.RemoteIDs[0]
				return res, fmt.Errorf("threads: thread stopped at part %d/%d: %w", i+1, len(parts), err)
			}
			return nil, err
		}
		res.RemoteIDs = append(res.RemoteIDs, id)
		replyTo = id
	}
	res.RemoteID = res.RemoteIDs[0]
	res.URL = c.permalink(ctx, res.RemoteID)
	return res, nil
}

// Reply answers a post or a reply with a text reply.
func (c *Client) Reply(ctx context.Context, to, text string) (*providers.PublishResult, error) {
	c.maybeRefresh(ctx)
	if TextLen(text) > maxText {
		return nil, providers.Invalid(fmt.Sprintf("threads: replies are limited to %d characters", maxText))
	}
	uid, err := c.me(ctx)
	if err != nil {
		return nil, err
	}
	id, err := c.createPart(ctx, uid, text, nil, to, nil, false)
	if err != nil {
		return nil, err
	}
	if id, err = c.publish(ctx, uid, id); err != nil {
		return nil, err
	}
	return &providers.PublishResult{RemoteID: id, RemoteIDs: []string{id}, URL: c.permalink(ctx, id)}, nil
}

// metricKeys maps Threads insight names to Relay's raw keys (see core.Normalize).
var metricKeys = map[string]string{"views": "views", "likes": "like_count", "replies": "reply_count",
	"reposts": "repost_count", "quotes": "quote_count", "shares": "shares"}

const (
	mediaMetrics     = "views,likes,replies,reposts,quotes,shares"
	mediaMetricsCore = "views,likes,replies,reposts,quotes" // shares is still "in development"
)

type insights struct {
	Data []struct {
		Name   string `json:"name"`
		Values []struct {
			Value int64 `json:"value"`
		} `json:"values"`
		TotalValue *struct {
			Value int64 `json:"value"`
		} `json:"total_value"`
	} `json:"data"`
}

// postInsights reads the lifetime insights of one post, without shares when Threads refuses it.
func (c *Client) postInsights(ctx context.Context, id string) (map[string]int64, error) {
	var ins insights
	err := c.api.get(ctx, "/"+id+"/insights", url.Values{"metric": {mediaMetrics}}, &ins)
	var he *providers.HTTPError
	if err != nil && errors.As(err, &he) && he.Status == 400 {
		ins = insights{}
		err = c.api.get(ctx, "/"+id+"/insights", url.Values{"metric": {mediaMetricsCore}}, &ins)
	}
	if err != nil {
		return nil, err
	}
	m := map[string]int64{}
	for _, d := range ins.Data {
		k, ok := metricKeys[d.Name]
		if !ok {
			continue
		}
		switch {
		case len(d.Values) > 0:
			m[k] = d.Values[0].Value
		case d.TotalValue != nil:
			m[k] = d.TotalValue.Value
		}
	}
	return m, nil
}

// Metrics reads lifetime media insights. Threads does not count nested replies.
func (c *Client) Metrics(ctx context.Context, ids []string) (map[string]map[string]int64, error) {
	c.maybeRefresh(ctx)
	out := map[string]map[string]int64{}
	for _, id := range ids {
		m, err := c.postInsights(ctx, id)
		if err != nil {
			return out, err
		}
		out[id] = m
	}
	return out, nil
}

// AccountMetrics reports followers, plus lifetime views and likes when the insights range allows it.
func (c *Client) AccountMetrics(ctx context.Context) (map[string]int64, error) {
	c.maybeRefresh(ctx)
	uid, err := c.me(ctx)
	if err != nil {
		return nil, err
	}
	var ins insights
	if err := c.api.get(ctx, "/"+uid+"/threads_insights", url.Values{"metric": {"followers_count"}}, &ins); err != nil {
		return nil, err
	}
	out := map[string]int64{}
	for _, d := range ins.Data {
		if d.Name == "followers_count" && d.TotalValue != nil {
			out["followers"] = d.TotalValue.Value
		}
	}
	// Best effort: views is a daily series, likes a total, over the widest range Threads accepts.
	q := url.Values{"metric": {"views,likes"}, "since": {strconv.Itoa(earliestInsights)},
		"until": {strconv.FormatInt(time.Now().Unix(), 10)}}
	var tot insights
	if err := c.api.get(ctx, "/"+uid+"/threads_insights", q, &tot); err == nil {
		for _, d := range tot.Data {
			var v int64
			if d.TotalValue != nil {
				v = d.TotalValue.Value
			} else {
				for _, x := range d.Values {
					v += x.Value
				}
			}
			switch d.Name {
			case "views":
				out["views_total"] = v
			case "likes":
				out["likes_total"] = v
			}
		}
	}
	return out, nil
}

type paging struct {
	Cursors struct {
		After string `json:"after"`
	} `json:"cursors"`
	Next string `json:"next"`
}

type reply struct {
	ID        string `json:"id"`
	Text      string `json:"text"`
	Username  string `json:"username"`
	Permalink string `json:"permalink"`
	Timestamp string `json:"timestamp"`
	Mine      bool   `json:"is_reply_owned_by_me"`
}

const replyFields = "id,text,username,permalink,timestamp,is_reply_owned_by_me"

// Comments returns every reply under a post (all depths, oldest first), except the account's own.
// /conversation is meant for root posts; for a reply it falls back to its direct /replies.
func (c *Client) Comments(ctx context.Context, remoteID string) ([]providers.Comment, error) {
	c.maybeRefresh(ctx)
	if c.Username == "" {
		_, _ = c.Identify(ctx) // best effort; is_reply_owned_by_me already filters our replies
	}
	edge := "/conversation"
	q := url.Values{"fields": {replyFields}, "reverse": {"false"}, "limit": {"100"}}
	var out []providers.Comment
	for page := 0; page < 50; page++ {
		var r struct {
			Data   []reply `json:"data"`
			Paging paging  `json:"paging"`
		}
		err := c.api.get(ctx, "/"+remoteID+edge, q, &r)
		var he *providers.HTTPError
		if err != nil && page == 0 && edge == "/conversation" && errors.As(err, &he) && he.Status == 400 {
			edge = "/replies"
			err = c.api.get(ctx, "/"+remoteID+edge, q, &r)
		}
		if err != nil {
			return out, err
		}
		for _, rp := range r.Data {
			if rp.Mine || (c.Username != "" && rp.Username == c.Username) {
				continue
			}
			var ca *time.Time
			if t, err := time.Parse(timeLayout, rp.Timestamp); err == nil {
				ca = &t
			}
			author := ""
			if rp.Username != "" {
				author = "@" + rp.Username
			}
			out = append(out, providers.Comment{RemoteID: rp.ID, Author: author, Text: rp.Text, URL: rp.Permalink, CreatedAt: ca})
		}
		next := r.Paging.Cursors.After
		if next == "" || next == q.Get("after") || len(r.Data) == 0 {
			break
		}
		q.Set("after", next)
	}
	return out, nil
}

// kind maps media_type (TEXT_POST, IMAGE, VIDEO, CAROUSEL_ALBUM, AUDIO, REPOST_FACADE) to HistoryItem.Kind.
func kind(mediaType string, quote bool) string {
	if mediaType == "REPOST_FACADE" {
		return "repost"
	}
	if quote {
		return "quote"
	}
	switch mediaType {
	case "TEXT_POST":
		return "text"
	case "IMAGE":
		return "image"
	case "VIDEO":
		return "video"
	case "CAROUSEL_ALBUM":
		return "carousel"
	}
	return ""
}

// visual is the image part of a Threads media (or of a carousel child).
type visual struct {
	MediaType    string `json:"media_type"`
	MediaURL     string `json:"media_url"`
	ThumbnailURL string `json:"thumbnail_url"`
}

func (v visual) addTo(it *providers.HistoryItem) {
	switch v.MediaType {
	case "VIDEO":
		it.AddVisual(v.ThumbnailURL, true)
	case "IMAGE", "CAROUSEL_ALBUM":
		it.AddVisual(v.MediaURL, false)
	}
}

// History lists the profile's posts (GET /{user-id}/threads; replies live under /replies and are not imported).
// The list endpoint has no counters, so each post's insights are read too (one call per post, best effort).
func (c *Client) History(ctx context.Context, since time.Time, max int) ([]providers.HistoryItem, error) {
	c.maybeRefresh(ctx)
	uid, err := c.me(ctx)
	if err != nil {
		return nil, err
	}
	if max <= 0 {
		max = 5000
	}
	q := url.Values{"limit": {"100"}}
	if !since.IsZero() {
		q.Set("since", strconv.FormatInt(since.Unix(), 10))
	}
	// Visuals come in the same request: media_url (an IMAGE; for a VIDEO it is the video file),
	// thumbnail_url (the cover of a VIDEO) and the children of a CAROUSEL_ALBUM.
	const base = "id,text,media_type,permalink,timestamp"
	const visuals = base + ",media_url,thumbnail_url,children{media_type,media_url,thumbnail_url}"
	chain := []string{visuals + ",is_quote_post", visuals, base}
	var out []providers.HistoryItem
	withMetrics := true
	for len(out) < max {
		var r struct {
			Data []struct {
				ID        string `json:"id"`
				Text      string `json:"text"`
				Permalink string `json:"permalink"`
				Timestamp string `json:"timestamp"`
				Quote     bool   `json:"is_quote_post"`
				visual
				Children struct {
					Data []visual `json:"data"`
				} `json:"children"`
			} `json:"data"`
			Paging paging `json:"paging"`
		}
		if err := c.getFields(ctx, "/"+uid+"/threads", q, &r, chain[0], chain[1:]...); err != nil {
			return out, err
		}
		for i, f := range chain { // keep the field set that worked
			if f == q.Get("fields") {
				chain = chain[i:]
				break
			}
		}
		for _, m := range r.Data {
			if len(out) >= max {
				break
			}
			t, _ := time.Parse(timeLayout, m.Timestamp)
			it := providers.HistoryItem{RemoteID: m.ID, Text: m.Text, URL: m.Permalink, CreatedAt: t, Kind: kind(m.MediaType, m.Quote)}
			if m.MediaType != "REPOST_FACADE" { // a repost shows someone else's post
				if len(m.Children.Data) > 0 {
					for _, ch := range m.Children.Data {
						ch.addTo(&it)
					}
				} else {
					m.visual.addTo(&it)
				}
			}
			if withMetrics && m.MediaType != "REPOST_FACADE" { // reposts return no insights
				met, err := c.postInsights(ctx, m.ID)
				var he *providers.HTTPError
				if err != nil && errors.As(err, &he) && he.Status == http.StatusTooManyRequests {
					withMetrics = false // rate limited: keep importing posts, leave counters to refresh_metrics
				}
				it.Metrics = met
			}
			out = append(out, it)
		}
		next := r.Paging.Cursors.After
		if next == "" || next == q.Get("after") || len(r.Data) == 0 {
			break
		}
		q.Set("after", next)
	}
	if len(out) > max {
		out = out[:max]
	}
	return out, nil
}

type idResp struct {
	ID string `json:"id"`
}

// Delete removes the posts of a thread, last reply first (DELETE /{threads-media-id}, needs the
// threads_delete permission).
// https://developers.facebook.com/docs/threads/posts/delete-posts
func (c *Client) Delete(ctx context.Context, remoteIDs []string) error {
	for i := len(remoteIDs) - 1; i >= 0; i-- {
		id := remoteIDs[i]
		var r struct {
			Success bool `json:"success"`
		}
		if err := c.api.del(ctx, "/"+url.PathEscape(id), &r); err != nil {
			if gone(err) {
				continue
			}
			return fmt.Errorf("threads: delete post %s: %w", id, err)
		}
		if !r.Success {
			return fmt.Errorf("threads: post %s not deleted", id)
		}
	}
	return nil
}
