package meta

import (
	"context"
	"encoding/json"
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
		Name: "facebook",
		Description: "Facebook Page via Graph API with a Page access token (pages_manage_posts, pages_read_engagement, pages_manage_engagement). " +
			"Text, link (options.link), photos, one video; reactions/comments/shares; comment replies; history import.",
		CredentialFields: []string{"page_id", "page_access_token"},
		New: func(c map[string]string, _ func(map[string]string) error, hc *http.Client) (providers.Client, error) {
			if c["page_id"] == "" || c["page_access_token"] == "" {
				return nil, errors.New("facebook: page_id and page_access_token are required")
			}
			v := c["graph_version"]
			if v == "" {
				v = DefaultVersion
			}
			base := c["base_url"]
			video := c["video_base_url"]
			if base == "" {
				base, video = "https://graph.facebook.com", "https://graph-video.facebook.com"
			}
			if video == "" {
				video = base
			}
			return &Facebook{PageID: c["page_id"], g: &graph{Base: base + "/" + v, Token: c["page_access_token"], HC: hc}, videoBase: video + "/" + v}, nil
		},
	})
}

type Facebook struct {
	PageID    string
	g         *graph
	videoBase string
}

func (f *Facebook) Identify(ctx context.Context) (*providers.Identity, error) {
	var r struct{ ID, Name, Username string }
	if err := f.g.get(ctx, "/"+f.PageID, url.Values{"fields": {"id,name,username"}}, &r); err != nil {
		return nil, err
	}
	return &providers.Identity{RemoteUserID: r.ID, Handle: r.Username, Name: r.Name}, nil
}

func (f *Facebook) permalink(ctx context.Context, id string) string {
	var r struct {
		PermalinkURL string `json:"permalink_url"`
	}
	if err := f.g.get(ctx, "/"+id, url.Values{"fields": {"permalink_url"}}, &r); err == nil && r.PermalinkURL != "" {
		return r.PermalinkURL
	}
	return "https://www.facebook.com/" + id
}

func (f *Facebook) Publish(ctx context.Context, req providers.PublishRequest) (*providers.PublishResult, error) {
	msg := strings.Join(req.Parts, "\n\n")
	link, _ := req.Options["link"].(string)
	var id string
	switch {
	case len(req.Media) == 1 && strings.HasPrefix(req.Media[0].Mime, "video/"):
		var r idResp
		fields := map[string]string{"description": msg}
		if t, _ := req.Options["title"].(string); t != "" {
			fields["title"] = t
		}
		if err := f.g.postFile(ctx, f.videoBase, "/"+f.PageID+"/videos", fields, "source", req.Media[0], &r); err != nil {
			return nil, err
		}
		id = r.ID
	case len(req.Media) == 1:
		var r idResp
		if err := f.g.postFile(ctx, "", "/"+f.PageID+"/photos", map[string]string{"caption": msg}, "source", req.Media[0], &r); err != nil {
			return nil, err
		}
		id = r.PostID
		if id == "" {
			id = r.ID
		}
	case len(req.Media) > 1:
		form := url.Values{"message": {msg}}
		for i, m := range req.Media {
			if strings.HasPrefix(m.Mime, "video/") {
				return nil, providers.Invalid("facebook: a video must be the only media of a post")
			}
			var r idResp
			if err := f.g.postFile(ctx, "", "/"+f.PageID+"/photos", map[string]string{"published": "false"}, "source", m, &r); err != nil {
				return nil, err
			}
			b, _ := json.Marshal(map[string]string{"media_fbid": r.ID})
			form.Set(fmt.Sprintf("attached_media[%d]", i), string(b))
		}
		var r idResp
		if err := f.g.post(ctx, "/"+f.PageID+"/feed", form, &r); err != nil {
			return nil, err
		}
		id = r.ID
	default:
		form := url.Values{"message": {msg}}
		if link != "" {
			form.Set("link", link)
		}
		var r idResp
		if err := f.g.post(ctx, "/"+f.PageID+"/feed", form, &r); err != nil {
			return nil, err
		}
		id = r.ID
	}
	if id == "" {
		return nil, errors.New("facebook: empty id in response")
	}
	return &providers.PublishResult{RemoteID: id, RemoteIDs: []string{id}, URL: f.permalink(ctx, id)}, nil
}

func (f *Facebook) Metrics(ctx context.Context, ids []string) (map[string]map[string]int64, error) {
	out := map[string]map[string]int64{}
	for start := 0; start < len(ids); start += 50 {
		end := min(start+50, len(ids))
		var r map[string]struct {
			Reactions struct {
				Summary struct {
					TotalCount int64 `json:"total_count"`
				} `json:"summary"`
			} `json:"reactions"`
			Comments struct {
				Summary struct {
					TotalCount int64 `json:"total_count"`
				} `json:"summary"`
			} `json:"comments"`
			Shares struct {
				Count int64 `json:"count"`
			} `json:"shares"`
		}
		q := url.Values{"ids": {strings.Join(ids[start:end], ",")}, "fields": {"reactions.summary(total_count).limit(0),comments.summary(total_count).limit(0),shares"}}
		if err := f.g.get(ctx, "/", q, &r); err != nil {
			return out, err
		}
		for id, v := range r {
			out[id] = map[string]int64{"like_count": v.Reactions.Summary.TotalCount, "reply_count": v.Comments.Summary.TotalCount, "share_count": v.Shares.Count}
		}
	}
	return out, nil
}

func (f *Facebook) Comments(ctx context.Context, remoteID string) ([]providers.Comment, error) {
	var r struct {
		Data []struct {
			ID           string `json:"id"`
			Message      string `json:"message"`
			CreatedTime  string `json:"created_time"`
			PermalinkURL string `json:"permalink_url"`
			From         *struct {
				ID, Name string
			} `json:"from"`
		} `json:"data"`
	}
	q := url.Values{"fields": {"id,message,created_time,permalink_url,from"}, "filter": {"stream"}, "limit": {"100"}}
	if err := f.g.get(ctx, "/"+remoteID+"/comments", q, &r); err != nil {
		return nil, err
	}
	var out []providers.Comment
	for _, c := range r.Data {
		author := "unknown"
		if c.From != nil {
			if c.From.ID == f.PageID {
				continue
			}
			author = c.From.Name
		}
		var ca *time.Time
		if t, err := time.Parse("2006-01-02T15:04:05-0700", c.CreatedTime); err == nil {
			ca = &t
		}
		out = append(out, providers.Comment{RemoteID: c.ID, Author: author, Text: c.Message, URL: c.PermalinkURL, CreatedAt: ca})
	}
	return out, nil
}

func (f *Facebook) Reply(ctx context.Context, to, text string) (*providers.PublishResult, error) {
	var r idResp
	if err := f.g.post(ctx, "/"+to+"/comments", url.Values{"message": {text}}, &r); err != nil {
		return nil, err
	}
	return &providers.PublishResult{RemoteID: r.ID, RemoteIDs: []string{r.ID}, URL: "https://www.facebook.com/" + r.ID}, nil
}

func (f *Facebook) History(ctx context.Context, since time.Time, max int) ([]providers.HistoryItem, error) {
	if max <= 0 {
		max = 2000
	}
	q := url.Values{"fields": {"id,message,created_time,permalink_url"}, "limit": {"100"}}
	if !since.IsZero() {
		q.Set("since", fmt.Sprint(since.Unix()))
	}
	path := "/" + f.PageID + "/posts"
	var out []providers.HistoryItem
	for len(out) < max {
		var r struct {
			Data []struct {
				ID           string `json:"id"`
				Message      string `json:"message"`
				CreatedTime  string `json:"created_time"`
				PermalinkURL string `json:"permalink_url"`
			} `json:"data"`
			Paging struct {
				Cursors struct{ After string } `json:"cursors"`
				Next    string                 `json:"next"`
			} `json:"paging"`
		}
		if err := f.g.get(ctx, path, q, &r); err != nil {
			return out, err
		}
		for _, p := range r.Data {
			t, _ := time.Parse("2006-01-02T15:04:05-0700", p.CreatedTime)
			out = append(out, providers.HistoryItem{RemoteID: p.ID, Text: p.Message, URL: p.PermalinkURL, CreatedAt: t})
		}
		if r.Paging.Next == "" || r.Paging.Cursors.After == "" || len(r.Data) == 0 {
			break
		}
		q.Set("after", r.Paging.Cursors.After)
	}
	if len(out) > max {
		out = out[:max]
	}
	ids := make([]string, len(out))
	for i, it := range out {
		ids[i] = it.RemoteID
	}
	if m, err := f.Metrics(ctx, ids); err == nil {
		for i := range out {
			out[i].Metrics = m[out[i].RemoteID]
		}
	}
	return out, nil
}
