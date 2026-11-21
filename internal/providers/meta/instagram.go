package meta

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Seeyko/relay/internal/providers"
)

// Limits from the Instagram Platform docs (content publishing and IG User Media reference).
const (
	igMaxCaption   = 2200
	igMaxHashtags  = 30
	igMaxMentions  = 20
	igMaxCarousel  = 20
	igMaxAltText   = 1000
	igMaxImage     = 8 << 20
	igMaxReel      = 300 << 20
	igMaxStoryVid  = 100 << 20
	igTimeLayout   = "2006-01-02T15:04:05-0700"
	igPollDefault  = 15 * time.Second
	igPollDeadline = 10 * time.Minute
)

var (
	igHashtagRe = regexp.MustCompile(`#[\p{L}\p{N}_]+`)
	igMentionRe = regexp.MustCompile(`@[\p{L}\p{N}_.]+`)
)

func init() {
	providers.Register(providers.Factory{
		Name: "instagram",
		Description: "Instagram professional account via the Instagram API with Instagram Login (instagram_business_basic, " +
			"instagram_business_content_publish, instagram_business_manage_comments, instagram_business_manage_insights). " +
			"Image (JPEG), carousel (up to 20 items), Reels (default for video; options share_to_feed, cover_url, thumb_offset, audio_name), " +
			"stories (options.media_type=STORIES); options location_id, collaborators; likes/comments/insights; comment replies; history import. " +
			"Needs RELAY_BASE_URL reachable from the internet: Instagram downloads the media from Relay. Long-lived token is refreshed automatically. " +
			"Accounts linked to a Facebook Page (Instagram API with Facebook Login, as Postiz connects them) also work: set login=facebook, " +
			"user_id=<Instagram business account id> and access_token=<Page access token> (instagram_basic, instagram_content_publish, " +
			"instagram_manage_comments, instagram_manage_insights, pages_read_engagement).",
		CredentialFields: []string{"access_token", "login", "user_id"},
		Validate:         validateInstagram,
		New: func(c map[string]string, persist func(map[string]string) error, hc *http.Client) (providers.Client, error) {
			if c["access_token"] == "" {
				return nil, errors.New("instagram: access_token is required")
			}
			v := c["graph_version"]
			if v == "" {
				v = DefaultVersion
			}
			fbLogin := c["login"] == "facebook"
			if c["login"] != "" && c["login"] != "instagram" && !fbLogin {
				return nil, errors.New("instagram: login must be instagram (default) or facebook")
			}
			if fbLogin && c["user_id"] == "" {
				return nil, errors.New("instagram: login=facebook needs user_id (the Instagram business account id linked to the Page)")
			}
			base := c["base_url"]
			if base == "" {
				base = "https://graph.instagram.com"
				if fbLogin {
					base = "https://graph.facebook.com"
				}
			}
			ig := &Instagram{UserID: c["user_id"], Username: c["username"], g: &graph{Base: base + "/" + v, Token: c["access_token"], HC: hc}, root: base, creds: c, persist: persist, fbLogin: fbLogin}
			return ig, nil
		},
	})
}

func isVideo(m providers.MediaFile) bool { return strings.HasPrefix(m.Mime, "video/") }

func validateInstagram(req providers.PublishRequest) []string {
	var errs []string
	mediaType := strings.ToUpper(optString(req.Options, "media_type"))
	switch mediaType {
	case "", "IMAGE", "VIDEO", "REELS", "STORIES", "CAROUSEL":
	default:
		errs = append(errs, "Instagram options.media_type must be REELS or STORIES (got "+mediaType+")")
	}
	if len(req.Media) == 0 {
		errs = append(errs, "Instagram needs at least one image or video")
	}
	if len(req.Media) > igMaxCarousel {
		errs = append(errs, fmt.Sprintf("Instagram carousels take at most %d media", igMaxCarousel))
	}
	if mediaType == "STORIES" && len(req.Media) > 1 {
		errs = append(errs, "an Instagram story takes exactly one image or video")
	}
	if mediaType == "REELS" && len(req.Media) == 1 && !isVideo(req.Media[0]) {
		errs = append(errs, "an Instagram Reel needs a video")
	}
	caption := strings.Join(req.Parts, "\n\n")
	if n := providers.CharCount(caption); n > igMaxCaption {
		errs = append(errs, fmt.Sprintf("Instagram captions are limited to %d characters (got %d)", igMaxCaption, n))
	}
	if n := len(igHashtagRe.FindAllString(caption, -1)); n > igMaxHashtags {
		errs = append(errs, fmt.Sprintf("Instagram allows at most %d hashtags (got %d)", igMaxHashtags, n))
	}
	if n := len(igMentionRe.FindAllString(caption, -1)); n > igMaxMentions {
		errs = append(errs, fmt.Sprintf("Instagram allows at most %d @ mentions (got %d)", igMaxMentions, n))
	}
	for _, m := range req.Media {
		if isVideo(m) {
			limit := int64(igMaxReel)
			if mediaType == "STORIES" {
				limit = igMaxStoryVid
			}
			if m.Size > limit {
				errs = append(errs, fmt.Sprintf("Instagram videos are limited to %d MB here (%s is %d MB)", limit>>20, m.Filename, m.Size>>20))
			}
			if m.Mime != "video/mp4" && m.Mime != "video/quicktime" {
				errs = append(errs, "Instagram videos must be MP4 or MOV ("+m.Filename+" is "+m.Mime+")")
			}
			continue
		}
		if m.Mime != "" && m.Mime != "image/jpeg" && m.Mime != "image/jpg" {
			errs = append(errs, "Instagram only accepts JPEG images ("+m.Filename+" is "+m.Mime+")")
		}
		if m.Size > igMaxImage {
			errs = append(errs, fmt.Sprintf("Instagram images are limited to 8 MB (%s is %d MB)", m.Filename, m.Size>>20))
		}
		if n := providers.CharCount(m.Alt); n > igMaxAltText {
			errs = append(errs, fmt.Sprintf("Instagram alt text is limited to %d characters (got %d)", igMaxAltText, n))
		}
	}
	return errs
}

type Instagram struct {
	UserID, Username string
	g                *graph
	root             string
	creds            map[string]string
	persist          func(map[string]string) error
	// fbLogin: the account is reached through its Facebook Page (graph.facebook.com, Page token).
	// There is no /me for the Instagram user and the Page token does not expire, so nothing is refreshed.
	fbLogin bool
	// PollInterval for container processing (tests override it).
	PollInterval time.Duration
}

// maybeRefresh renews the 60-day long-lived token when it is older than 30 days.
// Meta only refreshes tokens that are at least 24 hours old and not expired
// (GET graph.instagram.com/refresh_access_token, unversioned).
func (ig *Instagram) maybeRefresh(ctx context.Context) {
	if ig.persist == nil || ig.fbLogin {
		return
	}
	last, _ := time.Parse(time.RFC3339, ig.creds["token_refreshed_at"])
	if time.Since(last) < 30*24*time.Hour {
		return
	}
	var r struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	q := url.Values{"grant_type": {"ig_refresh_token"}, "access_token": {ig.g.Token}}
	req, err := http.NewRequestWithContext(ctx, "GET", ig.root+"/refresh_access_token?"+q.Encode(), nil)
	if err != nil {
		return
	}
	now := time.Now().UTC()
	if err := ig.g.send(req, &r); err != nil || r.AccessToken == "" {
		if last.IsZero() {
			// Unknown age (likely a fresh token under 24h): try again in about a day, not on every call.
			ig.creds["token_refreshed_at"] = now.Add(-29 * 24 * time.Hour).Format(time.RFC3339)
			_ = ig.persist(ig.creds)
		}
		return
	}
	ig.g.Token = r.AccessToken
	ig.creds["access_token"] = r.AccessToken
	ig.creds["token_refreshed_at"] = now.Format(time.RFC3339)
	if r.ExpiresIn > 0 {
		ig.creds["token_expires_at"] = now.Add(time.Duration(r.ExpiresIn) * time.Second).Format(time.RFC3339)
	}
	_ = ig.persist(ig.creds)
}

func (ig *Instagram) Identify(ctx context.Context) (*providers.Identity, error) {
	ig.maybeRefresh(ctx)
	var r struct {
		ID       string `json:"id"`
		UserID   string `json:"user_id"`
		Username string `json:"username"`
		Name     string `json:"name"`
	}
	if ig.fbLogin {
		// The IG User node has no user_id field: its id is already the professional account id.
		if err := ig.g.get(ctx, "/"+ig.creds["user_id"], url.Values{"fields": {"id,username,name"}}, &r); err != nil {
			return nil, err
		}
	} else if err := ig.g.get(ctx, "/me", url.Values{"fields": {"id,user_id,username,name"}}, &r); err != nil {
		return nil, err
	}
	// user_id is the professional account ID used by /<IG_ID>/media; id is app-scoped.
	id := r.UserID
	if id == "" {
		id = r.ID
	}
	ig.UserID, ig.Username = id, r.Username
	name := r.Name
	if name == "" {
		name = r.Username
	}
	return &providers.Identity{RemoteUserID: id, Handle: r.Username, Name: name}, nil
}

func (ig *Instagram) AccountMetrics(ctx context.Context) (map[string]int64, error) {
	ig.maybeRefresh(ctx)
	var r struct {
		Followers int64 `json:"followers_count"`
		Follows   int64 `json:"follows_count"`
		Media     int64 `json:"media_count"`
	}
	node := "/me"
	if ig.fbLogin {
		node = "/" + ig.creds["user_id"]
	}
	if err := ig.g.get(ctx, node, url.Values{"fields": {"followers_count,follows_count,media_count"}}, &r); err != nil {
		return nil, err
	}
	return map[string]int64{"followers": r.Followers, "following": r.Follows, "posts": r.Media}, nil
}

func (ig *Instagram) me(ctx context.Context) (string, error) {
	if ig.UserID != "" {
		return ig.UserID, nil
	}
	id, err := ig.Identify(ctx)
	if err != nil {
		return "", err
	}
	return id.RemoteUserID, nil
}

// getFields reads path with fields; on a 400 (a field this login type does not expose) it retries with fallback.
func (ig *Instagram) getFields(ctx context.Context, path string, q url.Values, fields, fallback string, out any) error {
	q.Set("fields", fields)
	err := ig.g.get(ctx, path, q, out)
	var he *providers.HTTPError
	if err != nil && fallback != "" && errors.As(err, &he) && he.Status == 400 {
		q.Set("fields", fallback)
		err = ig.g.get(ctx, path, q, out)
	}
	return err
}

// Transient content publishing errors, from the Instagram Platform error codes reference
// (all sent with HTTP 400). 2207042 is the 24h publishing limit (see /<IG_ID>/content_publishing_limit).
var igTransientCodes = map[int]bool{4: true, 17: true, 32: true, 613: true}
var igTransientSubcodes = map[int]bool{2207042: true, 2207003: true, 2207008: true, 2207020: true, 2207027: true}

// graphError decodes a Graph error body.
func graphError(err error) (code, subcode int, msg string) {
	var he *providers.HTTPError
	if !errors.As(err, &he) {
		return 0, 0, ""
	}
	var b struct {
		Error struct {
			Message string `json:"message"`
			Code    int    `json:"code"`
			Subcode int    `json:"error_subcode"`
			UserMsg string `json:"error_user_msg"`
		} `json:"error"`
	}
	_ = json.Unmarshal([]byte(he.Body), &b)
	msg = b.Error.UserMsg
	if msg == "" {
		msg = b.Error.Message
	}
	return b.Error.Code, b.Error.Subcode, msg
}

// publishErr turns Instagram's HTTP 400 transient errors (posting limit, download timeout,
// container not ready) into plain errors so the scheduler retries them.
func publishErr(err error) error {
	code, sub, msg := graphError(err)
	if igTransientCodes[code] || igTransientSubcodes[sub] {
		return fmt.Errorf("instagram: temporary error (code %d/%d): %s", code, sub, msg)
	}
	return err
}

func (ig *Instagram) container(ctx context.Context, uid string, form url.Values) (string, error) {
	var r idResp
	if err := ig.g.post(ctx, "/"+uid+"/media", form, &r); err != nil {
		return "", publishErr(err)
	}
	return r.ID, nil
}

func (ig *Instagram) wait(ctx context.Context) error {
	d := ig.PollInterval
	if d <= 0 {
		d = igPollDefault
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// waitReady polls the container status_code (EXPIRED, ERROR, FINISHED, IN_PROGRESS, PUBLISHED).
func (ig *Instagram) waitReady(ctx context.Context, id string) error {
	deadline := time.Now().Add(igPollDeadline)
	for {
		var r struct {
			StatusCode string `json:"status_code"`
			Status     string `json:"status"`
		}
		if err := ig.g.get(ctx, "/"+id, url.Values{"fields": {"status_code,status"}}, &r); err != nil {
			return err
		}
		switch r.StatusCode {
		case "FINISHED", "PUBLISHED", "":
			return nil
		case "ERROR":
			// Kept retryable: a failed download (2207003) succeeds with a new container.
			return errors.New("instagram: media processing failed: " + r.Status)
		case "EXPIRED":
			return errors.New("instagram: media container expired (not published within 24 hours)")
		}
		if time.Now().After(deadline) {
			return errors.New("instagram: media processing timed out")
		}
		if err := ig.wait(ctx); err != nil {
			return err
		}
	}
}

func optString(o map[string]any, k string) string {
	switch v := o[k].(type) {
	case string:
		return v
	case float64: // JSON numbers; avoid exponent notation
		return strconv.FormatFloat(v, 'f', -1, 64)
	case int, int64, bool:
		return fmt.Sprint(v)
	}
	return ""
}

func mediaForm(m providers.MediaFile, mediaType string) (url.Values, error) {
	if m.PublicURL == "" {
		return nil, providers.Invalid("instagram: RELAY_BASE_URL must be set so Instagram can download the media")
	}
	f := url.Values{}
	if isVideo(m) {
		f.Set("media_type", mediaType)
		f.Set("video_url", m.PublicURL)
		return f, nil
	}
	if mediaType == "STORIES" {
		f.Set("media_type", "STORIES")
	} else if m.Alt != "" {
		f.Set("alt_text", m.Alt) // feed and carousel images only
	}
	f.Set("image_url", m.PublicURL)
	return f, nil
}

// setOptions copies the documented optional container parameters from the post options.
func setOptions(f url.Values, o map[string]any, reel, story bool) {
	set := func(k string) {
		if v := optString(o, k); v != "" {
			f.Set(k, v)
		}
	}
	if !story {
		set("location_id")
		if c, ok := o["collaborators"].([]any); ok && len(c) > 0 {
			b, _ := json.Marshal(c)
			f.Set("collaborators", string(b))
		} else if s, ok := o["collaborators"].(string); ok && s != "" {
			b, _ := json.Marshal(strings.Split(s, ","))
			f.Set("collaborators", string(b))
		}
	}
	if reel {
		for _, k := range []string{"share_to_feed", "cover_url", "thumb_offset", "audio_name"} {
			set(k)
		}
	}
}

func (ig *Instagram) Publish(ctx context.Context, req providers.PublishRequest) (*providers.PublishResult, error) {
	ig.maybeRefresh(ctx)
	if len(req.Media) == 0 {
		return nil, providers.Invalid("instagram: a post needs at least one image or video")
	}
	if len(req.Media) > igMaxCarousel {
		return nil, providers.Invalid(fmt.Sprintf("instagram: at most %d media per carousel", igMaxCarousel))
	}
	uid, err := ig.me(ctx)
	if err != nil {
		return nil, err
	}
	caption := strings.Join(req.Parts, "\n\n")
	mediaType := strings.ToUpper(optString(req.Options, "media_type"))
	var creation string
	if len(req.Media) == 1 {
		m := req.Media[0]
		story := mediaType == "STORIES"
		if !story {
			mediaType = ""
			if isVideo(m) {
				mediaType = "REELS" // single feed videos are published as Reels
			}
		}
		form, err := mediaForm(m, mediaType)
		if err != nil {
			return nil, err
		}
		if !story && caption != "" {
			form.Set("caption", caption)
		}
		setOptions(form, req.Options, mediaType == "REELS", story)
		if creation, err = ig.container(ctx, uid, form); err != nil {
			return nil, err
		}
	} else {
		if mediaType == "STORIES" {
			return nil, providers.Invalid("instagram: a story takes one media")
		}
		var children []string
		for _, m := range req.Media {
			form, err := mediaForm(m, "VIDEO")
			if err != nil {
				return nil, err
			}
			form.Set("is_carousel_item", "true")
			id, err := ig.container(ctx, uid, form)
			if err != nil {
				return nil, err
			}
			if err := ig.waitReady(ctx, id); err != nil {
				return nil, err
			}
			children = append(children, id)
		}
		form := url.Values{"media_type": {"CAROUSEL"}, "children": {strings.Join(children, ",")}}
		if caption != "" {
			form.Set("caption", caption)
		}
		setOptions(form, req.Options, false, false)
		if creation, err = ig.container(ctx, uid, form); err != nil {
			return nil, err
		}
	}
	if err := ig.waitReady(ctx, creation); err != nil {
		return nil, err
	}
	var r idResp
	for attempt := 0; ; attempt++ {
		err := ig.g.post(ctx, "/"+uid+"/media_publish", url.Values{"creation_id": {creation}}, &r)
		if err == nil {
			break
		}
		// 2207027: "media is not ready for publishing, please wait for a moment".
		if _, sub, _ := graphError(err); sub == 2207027 && attempt < 3 {
			if err := ig.wait(ctx); err != nil {
				return nil, err
			}
			continue
		}
		return nil, publishErr(err)
	}
	var p struct {
		Permalink string `json:"permalink"`
	}
	_ = ig.g.get(ctx, "/"+r.ID, url.Values{"fields": {"permalink"}}, &p)
	return &providers.PublishResult{RemoteID: r.ID, RemoteIDs: []string{r.ID}, URL: p.Permalink}, nil
}

// insightMetrics lists the media insights valid for a surface (IG Media Insights reference, v22.0+:
// impressions, plays and video_views are gone; views replaces them).
func insightMetrics(product string) string {
	switch product {
	case "REELS":
		return "views,reach,saved,shares,total_interactions,ig_reels_avg_watch_time,ig_reels_video_view_total_time"
	case "STORY":
		return "views,reach,shares,total_interactions,replies,follows,profile_visits"
	default: // FEED: images, videos and carousels
		return "views,reach,saved,shares,total_interactions,follows,profile_visits"
	}
}

type igInsights struct {
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

func (ig *Instagram) Metrics(ctx context.Context, ids []string) (map[string]map[string]int64, error) {
	ig.maybeRefresh(ctx)
	out := map[string]map[string]int64{}
	for _, id := range ids {
		var r struct {
			LikeCount     int64  `json:"like_count"`
			CommentsCount int64  `json:"comments_count"`
			ProductType   string `json:"media_product_type"`
		}
		if err := ig.getFields(ctx, "/"+id, url.Values{}, "like_count,comments_count,media_product_type", "like_count,comments_count", &r); err != nil {
			return out, err
		}
		m := map[string]int64{"like_count": r.LikeCount, "reply_count": r.CommentsCount}
		// Insights are best effort: one unsupported metric fails the whole call, so fall back to the core pair.
		var ins igInsights
		err := ig.g.get(ctx, "/"+id+"/insights", url.Values{"metric": {insightMetrics(r.ProductType)}}, &ins)
		if err != nil {
			err = ig.g.get(ctx, "/"+id+"/insights", url.Values{"metric": {"views,reach"}}, &ins)
		}
		if err == nil {
			for _, d := range ins.Data {
				switch {
				case d.TotalValue != nil:
					m[d.Name] = d.TotalValue.Value
				case len(d.Values) > 0:
					m[d.Name] = d.Values[0].Value
				}
			}
		}
		out[id] = m
	}
	return out, nil
}

type igComment struct {
	ID        string `json:"id"`
	Text      string `json:"text"`
	Username  string `json:"username"`
	Timestamp string `json:"timestamp"`
	Replies   *struct {
		Data []igComment `json:"data"`
	} `json:"replies"`
}

type igPaging struct {
	Cursors struct {
		After string `json:"after"`
	} `json:"cursors"`
	Next string `json:"next"`
}

// Comments returns top-level comments (50 per page, newest first) and their replies, except the account's own.
func (ig *Instagram) Comments(ctx context.Context, remoteID string) ([]providers.Comment, error) {
	ig.maybeRefresh(ctx)
	if ig.Username == "" {
		if _, err := ig.Identify(ctx); err != nil {
			return nil, err
		}
	}
	var out []providers.Comment
	add := func(c igComment) {
		if c.Username == "" || c.Username == ig.Username {
			return
		}
		var ca *time.Time
		if t, err := time.Parse(igTimeLayout, c.Timestamp); err == nil {
			ca = &t
		}
		out = append(out, providers.Comment{RemoteID: c.ID, Author: "@" + c.Username, Text: c.Text, CreatedAt: ca})
	}
	q := url.Values{"fields": {"id,text,username,timestamp,replies{id,text,username,timestamp}"}, "limit": {"50"}}
	for page := 0; page < 20; page++ {
		var r struct {
			Data   []igComment `json:"data"`
			Paging igPaging    `json:"paging"`
		}
		if err := ig.g.get(ctx, "/"+remoteID+"/comments", q, &r); err != nil {
			return out, err
		}
		for _, c := range r.Data {
			add(c)
			if c.Replies != nil {
				for _, rp := range c.Replies.Data {
					add(rp)
				}
			}
		}
		if r.Paging.Next == "" || r.Paging.Cursors.After == "" || len(r.Data) == 0 {
			break
		}
		q.Set("after", r.Paging.Cursors.After)
	}
	return out, nil
}

// Reply answers a comment. Instagram attaches replies to a reply to its top-level comment.
func (ig *Instagram) Reply(ctx context.Context, to, text string) (*providers.PublishResult, error) {
	ig.maybeRefresh(ctx)
	var r idResp
	if err := ig.g.post(ctx, "/"+to+"/replies", url.Values{"message": {text}}, &r); err != nil {
		return nil, err
	}
	return &providers.PublishResult{RemoteID: r.ID, RemoteIDs: []string{r.ID}}, nil
}

// igKind maps media_type / media_product_type to HistoryItem.Kind.
func igKind(mediaType, product string) string {
	switch {
	case product == "STORY":
		return "story"
	case product == "REELS":
		return "reel"
	case mediaType == "CAROUSEL_ALBUM":
		return "carousel"
	case mediaType == "VIDEO":
		return "video"
	case mediaType == "IMAGE":
		return "image"
	}
	return ""
}

// History lists the account's feed media and Reels (stories are not returned by /<IG_ID>/media).
func (ig *Instagram) History(ctx context.Context, since time.Time, max int) ([]providers.HistoryItem, error) {
	ig.maybeRefresh(ctx)
	uid, err := ig.me(ctx)
	if err != nil {
		return nil, err
	}
	if max <= 0 {
		max = 5000
	}
	const base = "id,caption,media_type,permalink,timestamp,like_count,comments_count"
	fields := base + ",media_product_type"
	q := url.Values{"limit": {"100"}}
	if !since.IsZero() {
		q.Set("since", fmt.Sprint(since.Unix()))
	}
	var out []providers.HistoryItem
	for len(out) < max {
		var r struct {
			Data []struct {
				ID            string `json:"id"`
				Caption       string `json:"caption"`
				MediaType     string `json:"media_type"`
				ProductType   string `json:"media_product_type"`
				Permalink     string `json:"permalink"`
				Timestamp     string `json:"timestamp"`
				LikeCount     int64  `json:"like_count"`
				CommentsCount int64  `json:"comments_count"`
			} `json:"data"`
			Paging igPaging `json:"paging"`
		}
		if err := ig.getFields(ctx, "/"+uid+"/media", q, fields, base, &r); err != nil {
			return out, err
		}
		fields = q.Get("fields") // keep the field set that worked
		for _, m := range r.Data {
			t, _ := time.Parse(igTimeLayout, m.Timestamp)
			out = append(out, providers.HistoryItem{RemoteID: m.ID, Text: m.Caption, URL: m.Permalink, CreatedAt: t,
				Metrics: map[string]int64{"like_count": m.LikeCount, "reply_count": m.CommentsCount},
				Kind:    igKind(m.MediaType, m.ProductType)})
		}
		if r.Paging.Next == "" || r.Paging.Cursors.After == "" || len(r.Data) == 0 {
			break
		}
		q.Set("after", r.Paging.Cursors.After)
	}
	if len(out) > max {
		out = out[:max]
	}
	return out, nil
}
