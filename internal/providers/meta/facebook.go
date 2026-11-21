package meta

import (
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

// Limits from the Graph API reference.
const (
	fbMaxText = 63206
	// Comment messages are limited to 8000 characters.
	fbMaxComment = 8000
	// https://developers.facebook.com/docs/graph-api/reference/page/photos/ : 10 MB, jpeg/bmp/png/gif/tiff.
	fbMaxPhoto = 10 << 20
	// Videos above this size use the documented chunked upload (upload_phase=start/transfer/finish)
	// instead of a single multipart request held in memory.
	fbChunkedVideo = 64 << 20
	// /{page-id}/posts and comment edges accept at most 100 items per page.
	fbPageSize = "100"
	// Parallel object reads in Metrics (one GET per post since v26.0 dropped ?ids=).
	fbConcurrency = 5
)

var fbPhotoMimes = map[string]bool{"image/jpeg": true, "image/jpg": true, "image/png": true, "image/gif": true, "image/bmp": true, "image/tiff": true}

func init() {
	providers.Register(providers.Factory{
		Name: "facebook",
		Description: "Facebook Page via Graph API with a Page access token " +
			"(pages_show_list, pages_manage_posts, pages_read_engagement, pages_read_user_content, pages_manage_engagement; read_insights for views/reach/clicks). " +
			"Text, link (options.link), photos (up to 10 MB each, multi-photo posts), one video (options.title), Reels (options.reel=true, MP4 9:16, 3-90 s). " +
			"Thread parts: part 1 is the post, parts 2..n are published right after as comments of the Page on it, in order (text only, 8000 characters each); " +
			"reactions/comments/shares + post insights; comment replies; history import.",
		CredentialFields: []string{"page_id", "page_access_token"},
		Validate:         fbValidate,
		New: func(c map[string]string, _ func(map[string]string) error, hc *http.Client) (providers.Client, error) {
			if c["page_id"] == "" || c["page_access_token"] == "" {
				return nil, errors.New("facebook: page_id and page_access_token are required")
			}
			v := c["graph_version"]
			if v == "" {
				v = DefaultVersion
			}
			base := strings.TrimSuffix(c["base_url"], "/")
			if base == "" {
				base = "https://graph.facebook.com"
			}
			// graph-video.facebook.com is deprecated for uploads; graph.facebook.com handles them.
			// https://developers.facebook.com/documentation/video-api/overview
			video := strings.TrimSuffix(c["video_base_url"], "/")
			if video == "" {
				video = base
			}
			rupload := strings.TrimSuffix(c["rupload_base_url"], "/")
			if rupload == "" {
				rupload = "https://rupload.facebook.com"
			}
			return &Facebook{
				PageID:    c["page_id"],
				g:         &graph{Base: base + "/" + v, Token: c["page_access_token"], HC: hc},
				videoBase: video + "/" + v,
				reelBase:  rupload + "/video-upload/" + v,
			}, nil
		},
	})
}

func fbValidate(req providers.PublishRequest) []string {
	var errs []string
	first := ""
	if len(req.Parts) > 0 {
		first = req.Parts[0]
	}
	if n := providers.CharCount(first); n > fbMaxText {
		errs = append(errs, fmt.Sprintf("Facebook posts are limited to %d characters (got %d)", fbMaxText, n))
	}
	if strings.TrimSpace(first) == "" && len(req.Media) == 0 && req.Options["link"] == nil {
		errs = append(errs, "empty post")
	}
	errs = append(errs, commentProblems(req.Parts, fbMaxComment, "Facebook")...)
	videos := 0
	for _, m := range req.Media {
		switch {
		case strings.HasPrefix(m.Mime, "video/"):
			videos++
		case !fbPhotoMimes[m.Mime]:
			errs = append(errs, fmt.Sprintf("Facebook photos must be JPEG, PNG, GIF, BMP or TIFF (%s is %s)", m.Filename, m.Mime))
		case m.Size > fbMaxPhoto:
			errs = append(errs, fmt.Sprintf("Facebook photos are limited to 10 MB (%s is %d bytes)", m.Filename, m.Size))
		}
	}
	if videos > 0 && len(req.Media) > 1 {
		errs = append(errs, "a video must be the only media of a Facebook post")
	}
	if reel, _ := req.Options["reel"].(bool); reel && (len(req.Media) != 1 || videos != 1) {
		errs = append(errs, "a Facebook Reel needs exactly one video (MP4, 9:16, 3 to 90 seconds)")
	}
	return errs
}

type Facebook struct {
	PageID    string
	g         *graph
	videoBase string
	reelBase  string
}

func (f *Facebook) Identify(ctx context.Context) (*providers.Identity, error) {
	var r struct{ ID, Name, Username string }
	if err := f.g.get(ctx, "/"+f.PageID, url.Values{"fields": {"id,name,username"}}, &r); err != nil {
		return nil, err
	}
	return &providers.Identity{RemoteUserID: r.ID, Handle: r.Username, Name: r.Name}, nil
}

// AccountMetrics reads Page counters. For New Pages Experience Pages fan_count mirrors followers_count.
func (f *Facebook) AccountMetrics(ctx context.Context) (map[string]int64, error) {
	var r struct {
		Fans      int64 `json:"fan_count"`
		Followers int64 `json:"followers_count"`
	}
	if err := f.g.get(ctx, "/"+f.PageID, url.Values{"fields": {"fan_count,followers_count"}}, &r); err != nil {
		return nil, err
	}
	return map[string]int64{"followers": r.Followers, "page_likes": r.Fans}, nil
}

// AvatarURL is the Page profile picture: GET /{page-id}/picture?type=large&redirect=false answers
// {"data": {"url", "is_silhouette"}} instead of redirecting to the image. A silhouette is Facebook's
// placeholder for a Page without a picture: there is none then.
func (f *Facebook) AvatarURL(ctx context.Context) (string, error) {
	var r struct {
		Data struct {
			URL          string `json:"url"`
			IsSilhouette bool   `json:"is_silhouette"`
		} `json:"data"`
	}
	if err := f.g.get(ctx, "/"+f.PageID+"/picture", url.Values{"type": {"large"}, "redirect": {"false"}}, &r); err != nil {
		return "", err
	}
	if r.Data.IsSilhouette {
		return "", nil
	}
	return r.Data.URL, nil
}

func (f *Facebook) permalink(ctx context.Context, id, fallback string) string {
	var r struct {
		PermalinkURL string `json:"permalink_url"`
	}
	if err := f.g.get(ctx, "/"+id, url.Values{"fields": {"permalink_url"}}, &r); err == nil && r.PermalinkURL != "" {
		if strings.HasPrefix(r.PermalinkURL, "/") {
			return "https://www.facebook.com" + r.PermalinkURL
		}
		return r.PermalinkURL
	}
	return fallback
}

// Publish posts part 1 (with the media), then parts 2..n as comments of the Page on that post.
// With options.thread_root (a resumed thread) or options.reply_to, every part is a comment on that object.
func (f *Facebook) Publish(ctx context.Context, req providers.PublishRequest) (*providers.PublishResult, error) {
	if to := commentTarget(req.Options); to != "" {
		if len(req.Media) > 0 {
			return nil, providers.Invalid("facebook: comments are text only")
		}
		return f.comments(ctx, to, req.Parts)
	}
	if req.LaterMedia() > 0 {
		return nil, providers.Invalid("facebook: comments are text only")
	}
	msg := ""
	if len(req.Parts) > 0 {
		msg = req.Parts[0]
	}
	title, _ := req.Options["title"].(string)
	var id, fallback string
	var err error
	switch reel, _ := req.Options["reel"].(bool); {
	case reel:
		if len(req.Media) != 1 || !strings.HasPrefix(req.Media[0].Mime, "video/") {
			return nil, providers.Invalid("facebook: a Reel needs exactly one video")
		}
		id, err = f.publishReel(ctx, req.Media[0], msg, title)
		fallback = "https://www.facebook.com/reel/" + id
	case len(req.Media) == 1 && strings.HasPrefix(req.Media[0].Mime, "video/"):
		id, err = f.publishVideo(ctx, req.Media[0], msg, title)
		fallback = "https://www.facebook.com/" + f.PageID + "/videos/" + id
	case len(req.Media) == 1:
		var r idResp
		err = f.g.postFile(ctx, "", "/"+f.PageID+"/photos", photoFields(req.Media[0], map[string]string{"caption": msg}), "source", req.Media[0], &r)
		id = r.PostID
		if id == "" {
			id = r.ID
		}
	case len(req.Media) > 1:
		id, err = f.publishAlbum(ctx, req.Media, msg)
	default:
		form := url.Values{"message": {msg}}
		if link, _ := req.Options["link"].(string); link != "" {
			form.Set("link", link)
		}
		var r idResp
		err = f.g.post(ctx, "/"+f.PageID+"/feed", form, &r)
		id = r.ID
	}
	if err != nil {
		return nil, err
	}
	if id == "" {
		return nil, errors.New("facebook: empty id in response")
	}
	if fallback == "" {
		fallback = "https://www.facebook.com/" + id
	}
	res := &providers.PublishResult{RemoteID: id, RemoteIDs: []string{id}, URL: f.permalink(ctx, id, fallback)}
	if len(req.Parts) > 1 {
		more, err := f.comments(ctx, id, req.Parts[1:])
		if more != nil {
			res.RemoteIDs = append(res.RemoteIDs, more.RemoteIDs...)
		}
		if err != nil {
			return res, fmt.Errorf("facebook: post published, comment %d/%d failed: %w", len(res.RemoteIDs), len(req.Parts)-1, err)
		}
	}
	return res, nil
}

// comments posts each text as a comment of the Page on the object to (POST /{object-id}/comments).
func (f *Facebook) comments(ctx context.Context, to string, texts []string) (*providers.PublishResult, error) {
	res := &providers.PublishResult{}
	for _, t := range texts {
		if strings.TrimSpace(t) == "" {
			return partial(res), providers.Invalid("facebook: empty comment")
		}
		var r idResp
		if err := f.g.post(ctx, "/"+to+"/comments", url.Values{"message": {t}}, &r); err != nil {
			return partial(res), err
		}
		if r.ID == "" {
			return partial(res), errors.New("facebook: empty comment id in response")
		}
		res.RemoteIDs = append(res.RemoteIDs, r.ID)
	}
	if len(res.RemoteIDs) > 0 {
		res.RemoteID = res.RemoteIDs[0]
	}
	return res, nil
}

func photoFields(m providers.MediaFile, fields map[string]string) map[string]string {
	if m.Alt != "" {
		fields["alt_text_custom"] = m.Alt
	}
	return fields
}

// publishAlbum uploads unpublished photos, then attaches them to one feed post.
func (f *Facebook) publishAlbum(ctx context.Context, media []providers.MediaFile, msg string) (string, error) {
	form := url.Values{"message": {msg}}
	for i, m := range media {
		if strings.HasPrefix(m.Mime, "video/") {
			return "", providers.Invalid("facebook: a video must be the only media of a post")
		}
		var r idResp
		if err := f.g.postFile(ctx, "", "/"+f.PageID+"/photos", photoFields(m, map[string]string{"published": "false"}), "source", m, &r); err != nil {
			return "", err
		}
		b, _ := json.Marshal(map[string]string{"media_fbid": r.ID})
		form.Set(fmt.Sprintf("attached_media[%d]", i), string(b))
	}
	var r idResp
	err := f.g.post(ctx, "/"+f.PageID+"/feed", form, &r)
	return r.ID, err
}

// publishVideo posts a Page video. Large files use the chunked upload phases of POST /{page-id}/videos.
// https://developers.facebook.com/docs/graph-api/reference/page/videos/
func (f *Facebook) publishVideo(ctx context.Context, m providers.MediaFile, msg, title string) (string, error) {
	meta := map[string]string{"description": msg}
	if title != "" {
		meta["title"] = title
	}
	if m.Size <= fbChunkedVideo {
		var r idResp
		err := f.g.postFile(ctx, f.videoBase, "/"+f.PageID+"/videos", meta, "source", m, &r)
		return r.ID, err
	}
	path := "/" + f.PageID + "/videos"
	var start struct {
		SessionID string  `json:"upload_session_id"`
		VideoID   string  `json:"video_id"`
		Start     flexInt `json:"start_offset"`
		End       flexInt `json:"end_offset"`
	}
	if err := f.g.postForm(ctx, f.videoBase, path, url.Values{"upload_phase": {"start"}, "file_size": {strconv.FormatInt(m.Size, 10)}}, &start); err != nil {
		return "", err
	}
	src, err := m.Open()
	if err != nil {
		return "", err
	}
	defer src.Close()
	var pos int64
	for lo, hi := int64(start.Start), int64(start.End); lo < hi; {
		if lo < pos {
			return "", fmt.Errorf("facebook: upload asked to rewind to offset %d", lo)
		}
		if _, err := io.CopyN(io.Discard, src, lo-pos); err != nil {
			return "", err
		}
		pos = lo
		var next struct {
			Start flexInt `json:"start_offset"`
			End   flexInt `json:"end_offset"`
		}
		fields := map[string]string{"upload_phase": "transfer", "upload_session_id": start.SessionID, "start_offset": strconv.FormatInt(lo, 10)}
		if err := f.g.postMultipart(ctx, f.videoBase, path, fields, "video_file_chunk", m.Filename, io.LimitReader(src, hi-lo), &next); err != nil {
			return "", err
		}
		pos = hi
		lo, hi = int64(next.Start), int64(next.End)
	}
	form := url.Values{"upload_phase": {"finish"}, "upload_session_id": {start.SessionID}}
	for k, v := range meta {
		form.Set(k, v)
	}
	var fin struct {
		Success bool `json:"success"`
	}
	if err := f.g.postForm(ctx, f.videoBase, path, form, &fin); err != nil {
		return "", err
	}
	if !fin.Success {
		return "", errors.New("facebook: video upload finish was not successful")
	}
	return start.VideoID, nil
}

// publishReel follows the three-step Reels flow: start, upload to rupload.facebook.com, finish.
// https://developers.facebook.com/docs/video-api/guides/reels-publishing
func (f *Facebook) publishReel(ctx context.Context, m providers.MediaFile, msg, title string) (string, error) {
	path := "/" + f.PageID + "/video_reels"
	var start struct {
		VideoID   string `json:"video_id"`
		UploadURL string `json:"upload_url"`
	}
	if err := f.g.post(ctx, path, url.Values{"upload_phase": {"start"}}, &start); err != nil {
		return "", err
	}
	if start.VideoID == "" {
		return "", errors.New("facebook: empty video_id when starting a Reel upload")
	}
	src, err := m.Open()
	if err != nil {
		return "", err
	}
	defer src.Close()
	up := start.UploadURL
	if up == "" {
		up = f.reelBase + "/" + start.VideoID
	}
	req, err := http.NewRequestWithContext(ctx, "POST", up, src)
	if err != nil {
		return "", err
	}
	req.ContentLength = m.Size
	req.Header.Set("Authorization", "OAuth "+f.g.Token)
	req.Header.Set("offset", "0")
	req.Header.Set("file_size", strconv.FormatInt(m.Size, 10))
	req.Header.Set("Content-Type", "application/octet-stream")
	var ok struct {
		Success bool `json:"success"`
	}
	if err := f.g.send(req, &ok); err != nil {
		return "", err
	}
	if !ok.Success {
		return "", errors.New("facebook: Reel upload was not successful")
	}
	form := url.Values{"video_id": {start.VideoID}, "upload_phase": {"finish"}, "video_state": {"PUBLISHED"}, "description": {msg}}
	if title != "" {
		form.Set("title", title)
	}
	if err := f.g.post(ctx, path, form, &ok); err != nil {
		return "", err
	}
	if !ok.Success {
		return "", errors.New("facebook: Reel publish was not successful")
	}
	return start.VideoID, nil
}

type fbSummary struct {
	Summary struct {
		TotalCount int64 `json:"total_count"`
	} `json:"summary"`
}

type fbInsights struct {
	Data []struct {
		Name   string `json:"name"`
		Values []struct {
			Value json.RawMessage `json:"value"`
		} `json:"values"`
	} `json:"data"`
}

// fbPostInsights are lifetime Page post metrics that are current (not deprecated) in 2026.
// post_impressions* were deprecated in favour of the media view metrics.
// https://developers.facebook.com/docs/graph-api/reference/insights/
var fbPostInsights = map[string]string{"post_media_view": "views", "post_total_media_view_unique": "reach", "post_clicks": "clicks"}

// Metrics reads engagement counters. Page post ids look like {page-id}_{post-id}; ids without an
// underscore are video/Reel objects, which expose likes/comments edges and video_insights instead.
func (f *Facebook) Metrics(ctx context.Context, ids []string) (map[string]map[string]int64, error) {
	var posts, objects []string
	for _, id := range ids {
		if strings.Contains(id, "_") {
			posts = append(posts, id)
		} else {
			objects = append(objects, id)
		}
	}
	out := map[string]map[string]int64{}
	if err := f.batch(ctx, posts, "reactions.limit(0).summary(true),comments.limit(0).summary(true),shares", out, func(v json.RawMessage, m map[string]int64) {
		var r struct {
			Reactions fbSummary `json:"reactions"`
			Comments  fbSummary `json:"comments"`
			Shares    struct {
				Count int64 `json:"count"`
			} `json:"shares"`
		}
		_ = json.Unmarshal(v, &r)
		m["like_count"], m["reply_count"], m["share_count"] = r.Reactions.Summary.TotalCount, r.Comments.Summary.TotalCount, r.Shares.Count
	}); err != nil {
		return out, err
	}
	if err := f.batch(ctx, objects, "likes.limit(0).summary(true),comments.limit(0).summary(true)", out, func(v json.RawMessage, m map[string]int64) {
		var r struct {
			Likes    fbSummary `json:"likes"`
			Comments fbSummary `json:"comments"`
		}
		_ = json.Unmarshal(v, &r)
		m["like_count"], m["reply_count"] = r.Likes.Summary.TotalCount, r.Comments.Summary.TotalCount
	}); err != nil {
		return out, err
	}
	// Insights need read_insights: best effort, never fail the engagement read.
	_ = f.batch(ctx, posts, "insights.metric(post_media_view,post_total_media_view_unique,post_clicks).period(lifetime)", out, func(v json.RawMessage, m map[string]int64) {
		var r struct {
			Insights fbInsights `json:"insights"`
		}
		_ = json.Unmarshal(v, &r)
		addInsights(r.Insights, fbPostInsights, m)
	})
	_ = f.batch(ctx, objects, "video_insights.metric(total_video_views).period(lifetime)", out, func(v json.RawMessage, m map[string]int64) {
		var r struct {
			Insights fbInsights `json:"video_insights"`
		}
		_ = json.Unmarshal(v, &r)
		addInsights(r.Insights, map[string]string{"total_video_views": "views"}, m)
	})
	return out, nil
}

// batch reads each object with GET /{id}?fields=... (Graph v26.0 removed the ?ids= batch read)
// and lets fill decode it. Up to fbConcurrency reads run at once. An object the API refuses
// with a 4xx (deleted post, missing permission on one item) is skipped so one bad id does not
// hide the metrics of every other post; the first other error is returned.
func (f *Facebook) batch(ctx context.Context, ids []string, fields string, out map[string]map[string]int64, fill func(json.RawMessage, map[string]int64)) error {
	type res struct {
		id  string
		v   json.RawMessage
		err error
	}
	jobs := make(chan string)
	results := make(chan res)
	var wg sync.WaitGroup
	for w := 0; w < min(fbConcurrency, len(ids)); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range jobs {
				var v json.RawMessage
				err := f.g.get(ctx, "/"+id, url.Values{"fields": {fields}}, &v)
				results <- res{id, v, err}
			}
		}()
	}
	go func() {
		for _, id := range ids {
			jobs <- id
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()
	var first error
	for r := range results {
		if r.err != nil {
			var he *providers.HTTPError
			if errors.As(r.err, &he) && he.Status >= 400 && he.Status < 500 && he.Status != 401 && he.Status != 429 {
				continue
			}
			if first == nil {
				first = r.err
			}
			continue
		}
		if out[r.id] == nil {
			out[r.id] = map[string]int64{}
		}
		fill(r.v, out[r.id])
	}
	return first
}

func addInsights(in fbInsights, names map[string]string, m map[string]int64) {
	for _, d := range in.Data {
		key, ok := names[d.Name]
		if !ok || len(d.Values) == 0 {
			continue
		}
		var n int64
		if json.Unmarshal(d.Values[len(d.Values)-1].Value, &n) == nil {
			m[key] = n
		}
	}
}

func (f *Facebook) Comments(ctx context.Context, remoteID string) ([]providers.Comment, error) {
	q := url.Values{"fields": {"id,message,created_time,permalink_url,from"}, "filter": {"stream"}, "limit": {fbPageSize}}
	var out []providers.Comment
	for page := 0; page < 10; page++ {
		var r struct {
			Data []struct {
				ID           string `json:"id"`
				Message      string `json:"message"`
				CreatedTime  string `json:"created_time"`
				PermalinkURL string `json:"permalink_url"`
				From         *struct {
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"from"`
			} `json:"data"`
			Paging fbPaging `json:"paging"`
		}
		if err := f.g.get(ctx, "/"+remoteID+"/comments", q, &r); err != nil {
			return out, err
		}
		for _, c := range r.Data {
			author := "unknown"
			if c.From != nil {
				if c.From.ID == f.PageID {
					continue
				}
				author = c.From.Name
			}
			var ca *time.Time
			if t := parseGraphTime(c.CreatedTime); !t.IsZero() {
				ca = &t
			}
			out = append(out, providers.Comment{RemoteID: c.ID, Author: author, Text: c.Message, URL: c.PermalinkURL, CreatedAt: ca})
		}
		if !r.Paging.more(len(r.Data)) {
			break
		}
		q.Set("after", r.Paging.Cursors.After)
	}
	return out, nil
}

// Reply comments on a post or answers a comment (POST /{object-id}/comments, pages_manage_engagement).
func (f *Facebook) Reply(ctx context.Context, to, text string) (*providers.PublishResult, error) {
	var r idResp
	if err := f.g.post(ctx, "/"+to+"/comments", url.Values{"message": {text}}, &r); err != nil {
		return nil, err
	}
	if r.ID == "" {
		return nil, errors.New("facebook: empty comment id in response")
	}
	return &providers.PublishResult{RemoteID: r.ID, RemoteIDs: []string{r.ID}, URL: f.permalink(ctx, r.ID, "https://www.facebook.com/"+to)}, nil
}

type fbPaging struct {
	Cursors struct {
		After string `json:"after"`
	} `json:"cursors"`
	Next string `json:"next"`
}

func (p fbPaging) more(n int) bool { return p.Next != "" && p.Cursors.After != "" && n > 0 }

type fbAttachment struct {
	Type      string  `json:"type"`
	MediaType string  `json:"media_type"`
	Media     fbMedia `json:"media"`
	// Subattachments are the photos/videos of an album or a multi-photo post.
	Subattachments struct {
		Data []fbAttachment `json:"data"`
	} `json:"subattachments"`
}

// fbMedia is StoryAttachment.media: for a photo the display-size image, for a video its thumbnail.
type fbMedia struct {
	Image struct {
		Src string `json:"src"`
	} `json:"image"`
}

func (a fbAttachment) isVideo() bool {
	return strings.Contains(a.Type, "video") || a.MediaType == "video"
}

// fbVisuals adds the images of a Page post: the attachments' media (album photos through
// subattachments), else full_picture.
func fbVisuals(it *providers.HistoryItem, atts []fbAttachment, fullPicture string) {
	for _, a := range atts {
		if len(a.Subattachments.Data) > 0 {
			for _, sa := range a.Subattachments.Data {
				it.AddVisual(sa.Media.Image.Src, sa.isVideo())
			}
			continue
		}
		it.AddVisual(a.Media.Image.Src, a.isVideo())
	}
	if len(it.Visuals) == 0 {
		it.AddVisual(fullPicture, it.Kind == "video")
	}
}

// fbKind classifies a Page post from its attachments (StoryAttachment.type / media_type) and status_type.
func fbKind(statusType string, atts []fbAttachment) string {
	for _, a := range atts {
		switch t := a.Type; {
		case t == "album" || t == "multiple" || strings.HasSuffix(t, "_album"):
			return "carousel"
		case strings.Contains(t, "video") || a.MediaType == "video":
			return "video"
		case t == "photo" || t == "cover_photo" || t == "profile_media" || strings.HasPrefix(t, "animated_image") || a.MediaType == "photo":
			return "image"
		}
	}
	switch statusType {
	case "added_video":
		return "video"
	case "added_photos":
		return "image"
	}
	return "text"
}

// fbHistoryFields reads the posts with their visuals in the same request.
const fbHistoryFields = "id,message,created_time,permalink_url,status_type,full_picture," +
	"attachments{type,media_type,media{image{src}},subattachments{type,media_type,media{image{src}}}}"

func (f *Facebook) History(ctx context.Context, since time.Time, max int) ([]providers.HistoryItem, error) {
	if max <= 0 {
		max = 2000
	}
	q := url.Values{"fields": {fbHistoryFields}, "limit": {fbPageSize}}
	if !since.IsZero() {
		q.Set("since", strconv.FormatInt(since.Unix(), 10))
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
				StatusType   string `json:"status_type"`
				FullPicture  string `json:"full_picture"`
				Attachments  struct {
					Data []fbAttachment `json:"data"`
				} `json:"attachments"`
			} `json:"data"`
			Paging fbPaging `json:"paging"`
		}
		if err := f.g.get(ctx, path, q, &r); err != nil {
			return out, err
		}
		for _, p := range r.Data {
			it := providers.HistoryItem{RemoteID: p.ID, Text: p.Message, URL: p.PermalinkURL, CreatedAt: parseGraphTime(p.CreatedTime), Kind: fbKind(p.StatusType, p.Attachments.Data)}
			fbVisuals(&it, p.Attachments.Data, p.FullPicture)
			out = append(out, it)
		}
		if !r.Paging.more(len(r.Data)) {
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

// parseGraphTime reads Graph datetimes: ISO 8601 with a +0000 offset by default, or a UNIX timestamp.
func parseGraphTime(s string) time.Time {
	for _, layout := range []string{"2006-01-02T15:04:05-0700", time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.Unix(n, 0).UTC()
	}
	return time.Time{}
}
