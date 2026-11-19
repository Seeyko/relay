package meta

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Seeyko/relay/internal/providers"
)

func init() {
	providers.Register(providers.Factory{
		Name: "instagram",
		Description: "Instagram professional account via the Instagram API with Instagram Login (instagram_business_basic, " +
			"instagram_business_content_publish, instagram_business_manage_comments, instagram_business_manage_insights). " +
			"Image, carousel, Reels (options.media_type=REELS default for video), stories (options.media_type=STORIES); likes/comments/insights; comment replies. " +
			"Needs RELAY_BASE_URL reachable from the internet: Instagram downloads the media from Relay. Long-lived token is refreshed automatically.",
		CredentialFields: []string{"access_token"},
		Validate: func(req providers.PublishRequest) []string {
			var errs []string
			if len(req.Media) == 0 {
				errs = append(errs, "Instagram needs at least one image or video")
			}
			if len(req.Media) > 10 {
				errs = append(errs, "Instagram carousels take at most 10 media")
			}
			caption := strings.Join(req.Parts, "\n\n")
			if n := providers.CharCount(caption); n > 2200 {
				errs = append(errs, fmt.Sprintf("Instagram captions are limited to 2200 characters (got %d)", n))
			}
			if n := strings.Count(caption, "#"); n > 30 {
				errs = append(errs, fmt.Sprintf("Instagram allows at most 30 hashtags (got %d)", n))
			}
			for _, m := range req.Media {
				if m.Mime != "" && !strings.HasPrefix(m.Mime, "video/") && m.Mime != "image/jpeg" {
					errs = append(errs, "Instagram only accepts JPEG images ("+m.Filename+" is "+m.Mime+")")
				}
			}
			return errs
		},
		New: func(c map[string]string, persist func(map[string]string) error, hc *http.Client) (providers.Client, error) {
			if c["access_token"] == "" {
				return nil, errors.New("instagram: access_token is required")
			}
			v := c["graph_version"]
			if v == "" {
				v = DefaultVersion
			}
			base := c["base_url"]
			if base == "" {
				base = "https://graph.instagram.com"
			}
			ig := &Instagram{UserID: c["user_id"], Username: c["username"], g: &graph{Base: base + "/" + v, Token: c["access_token"], HC: hc}, root: base, creds: c, persist: persist}
			return ig, nil
		},
	})
}

type Instagram struct {
	UserID, Username string
	g                *graph
	root             string
	creds            map[string]string
	persist          func(map[string]string) error
	// PollInterval for container processing (tests override it).
	PollInterval time.Duration
}

// maybeRefresh renews the 60-day long-lived token when it is older than 30 days.
func (ig *Instagram) maybeRefresh(ctx context.Context) {
	if ig.persist == nil {
		return
	}
	last, _ := time.Parse(time.RFC3339, ig.creds["token_refreshed_at"])
	if time.Since(last) < 30*24*time.Hour {
		return
	}
	var r struct {
		AccessToken string `json:"access_token"`
	}
	req, err := http.NewRequestWithContext(ctx, "GET", ig.root+"/refresh_access_token?grant_type=ig_refresh_token&access_token="+url.QueryEscape(ig.g.Token), nil)
	if err != nil {
		return
	}
	if err := ig.g.send(req, &r); err != nil || r.AccessToken == "" {
		return
	}
	ig.g.Token = r.AccessToken
	ig.creds["access_token"] = r.AccessToken
	ig.creds["token_refreshed_at"] = time.Now().UTC().Format(time.RFC3339)
	_ = ig.persist(ig.creds)
}

func (ig *Instagram) Identify(ctx context.Context) (*providers.Identity, error) {
	var r struct {
		ID       string `json:"id"`
		UserID   string `json:"user_id"`
		Username string `json:"username"`
		Name     string `json:"name"`
	}
	if err := ig.g.get(ctx, "/me", url.Values{"fields": {"id,user_id,username,name"}}, &r); err != nil {
		return nil, err
	}
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
	var r struct {
		Followers int64 `json:"followers_count"`
		Follows   int64 `json:"follows_count"`
		Media     int64 `json:"media_count"`
	}
	if err := ig.g.get(ctx, "/me", url.Values{"fields": {"followers_count,follows_count,media_count"}}, &r); err != nil {
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

func (ig *Instagram) container(ctx context.Context, uid string, form url.Values) (string, error) {
	var r idResp
	if err := ig.g.post(ctx, "/"+uid+"/media", form, &r); err != nil {
		return "", err
	}
	return r.ID, nil
}

func (ig *Instagram) waitReady(ctx context.Context, id string) error {
	wait := ig.PollInterval
	if wait <= 0 {
		wait = 5 * time.Second
	}
	for i := 0; i < 120; i++ {
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
		case "ERROR", "EXPIRED":
			return fmt.Errorf("instagram: media processing %s: %s", r.StatusCode, r.Status)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
	return errors.New("instagram: media processing timed out")
}

func mediaForm(m providers.MediaFile, mediaType string) (url.Values, error) {
	if m.PublicURL == "" {
		return nil, providers.Invalid("instagram: RELAY_BASE_URL must be set so Instagram can download the media")
	}
	f := url.Values{}
	if strings.HasPrefix(m.Mime, "video/") {
		if mediaType == "" {
			mediaType = "REELS"
		}
		f.Set("media_type", mediaType)
		f.Set("video_url", m.PublicURL)
	} else {
		if mediaType == "STORIES" {
			f.Set("media_type", "STORIES")
		}
		f.Set("image_url", m.PublicURL)
		if m.Alt != "" {
			f.Set("alt_text", m.Alt)
		}
	}
	return f, nil
}

func (ig *Instagram) Publish(ctx context.Context, req providers.PublishRequest) (*providers.PublishResult, error) {
	ig.maybeRefresh(ctx)
	if len(req.Media) == 0 {
		return nil, providers.Invalid("instagram: a post needs at least one image or video")
	}
	if len(req.Media) > 10 {
		return nil, providers.Invalid("instagram: at most 10 media per carousel")
	}
	uid, err := ig.me(ctx)
	if err != nil {
		return nil, err
	}
	caption := strings.Join(req.Parts, "\n\n")
	mediaType, _ := req.Options["media_type"].(string)
	var creation string
	if len(req.Media) == 1 {
		form, err := mediaForm(req.Media[0], mediaType)
		if err != nil {
			return nil, err
		}
		if mediaType != "STORIES" {
			form.Set("caption", caption)
		}
		if creation, err = ig.container(ctx, uid, form); err != nil {
			return nil, err
		}
	} else {
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
		if creation, err = ig.container(ctx, uid, url.Values{"media_type": {"CAROUSEL"}, "children": {strings.Join(children, ",")}, "caption": {caption}}); err != nil {
			return nil, err
		}
	}
	if err := ig.waitReady(ctx, creation); err != nil {
		return nil, err
	}
	var r idResp
	if err := ig.g.post(ctx, "/"+uid+"/media_publish", url.Values{"creation_id": {creation}}, &r); err != nil {
		return nil, err
	}
	link := ""
	var p struct {
		Permalink string `json:"permalink"`
	}
	if err := ig.g.get(ctx, "/"+r.ID, url.Values{"fields": {"permalink"}}, &p); err == nil {
		link = p.Permalink
	}
	return &providers.PublishResult{RemoteID: r.ID, RemoteIDs: []string{r.ID}, URL: link}, nil
}

func (ig *Instagram) Metrics(ctx context.Context, ids []string) (map[string]map[string]int64, error) {
	ig.maybeRefresh(ctx)
	out := map[string]map[string]int64{}
	for _, id := range ids {
		var r struct {
			LikeCount     int64 `json:"like_count"`
			CommentsCount int64 `json:"comments_count"`
		}
		if err := ig.g.get(ctx, "/"+id, url.Values{"fields": {"like_count,comments_count"}}, &r); err != nil {
			return out, err
		}
		m := map[string]int64{"like_count": r.LikeCount, "reply_count": r.CommentsCount}
		// Insights are best effort: available metrics differ per media type.
		var ins struct {
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
		if err := ig.g.get(ctx, "/"+id+"/insights", url.Values{"metric": {"views,reach,saved,shares"}}, &ins); err == nil {
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

func (ig *Instagram) Comments(ctx context.Context, remoteID string) ([]providers.Comment, error) {
	var r struct {
		Data []struct {
			ID        string `json:"id"`
			Text      string `json:"text"`
			Username  string `json:"username"`
			Timestamp string `json:"timestamp"`
			Replies   *struct {
				Data []struct {
					ID        string `json:"id"`
					Text      string `json:"text"`
					Username  string `json:"username"`
					Timestamp string `json:"timestamp"`
				} `json:"data"`
			} `json:"replies"`
		} `json:"data"`
	}
	if err := ig.g.get(ctx, "/"+remoteID+"/comments", url.Values{"fields": {"id,text,username,timestamp,replies{id,text,username,timestamp}"}, "limit": {"100"}}, &r); err != nil {
		return nil, err
	}
	var out []providers.Comment
	add := func(id, text, user, ts string) {
		if user == ig.Username {
			return
		}
		var ca *time.Time
		if t, err := time.Parse("2006-01-02T15:04:05-0700", ts); err == nil {
			ca = &t
		}
		out = append(out, providers.Comment{RemoteID: id, Author: "@" + user, Text: text, CreatedAt: ca})
	}
	for _, c := range r.Data {
		add(c.ID, c.Text, c.Username, c.Timestamp)
		if c.Replies != nil {
			for _, rp := range c.Replies.Data {
				add(rp.ID, rp.Text, rp.Username, rp.Timestamp)
			}
		}
	}
	return out, nil
}

func (ig *Instagram) Reply(ctx context.Context, to, text string) (*providers.PublishResult, error) {
	var r idResp
	if err := ig.g.post(ctx, "/"+to+"/replies", url.Values{"message": {text}}, &r); err != nil {
		return nil, err
	}
	return &providers.PublishResult{RemoteID: r.ID, RemoteIDs: []string{r.ID}}, nil
}

func (ig *Instagram) History(ctx context.Context, since time.Time, max int) ([]providers.HistoryItem, error) {
	uid, err := ig.me(ctx)
	if err != nil {
		return nil, err
	}
	if max <= 0 {
		max = 5000
	}
	q := url.Values{"fields": {"id,caption,permalink,timestamp,like_count,comments_count"}, "limit": {"100"}}
	if !since.IsZero() {
		q.Set("since", fmt.Sprint(since.Unix()))
	}
	var out []providers.HistoryItem
	for len(out) < max {
		var r struct {
			Data []struct {
				ID            string `json:"id"`
				Caption       string `json:"caption"`
				Permalink     string `json:"permalink"`
				Timestamp     string `json:"timestamp"`
				LikeCount     int64  `json:"like_count"`
				CommentsCount int64  `json:"comments_count"`
			} `json:"data"`
			Paging struct {
				Cursors struct{ After string } `json:"cursors"`
				Next    string                 `json:"next"`
			} `json:"paging"`
		}
		if err := ig.g.get(ctx, "/"+uid+"/media", q, &r); err != nil {
			return out, err
		}
		for _, m := range r.Data {
			t, _ := time.Parse("2006-01-02T15:04:05-0700", m.Timestamp)
			out = append(out, providers.HistoryItem{RemoteID: m.ID, Text: m.Caption, URL: m.Permalink, CreatedAt: t,
				Metrics: map[string]int64{"like_count": m.LikeCount, "reply_count": m.CommentsCount}})
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
