package core

import (
	"context"
	"errors"
	"io"
	"sort"
	"strings"

	"github.com/Seeyko/relay/internal/providers"
	"github.com/Seeyko/relay/internal/store"
)

// The preview is what the owner reviews before approving a post: the complete text per account, every
// media, the options, the pre-flight problems and display hints per network (limits, "more" cut).
// The limits and cuts below are display hints only; the provider's Validate stays the source of truth.

// PreviewText is one block of text as the network shows it.
type PreviewText struct {
	// Label is post, caption, title, description, comment or body.
	Label string `json:"label"`
	Text  string `json:"text"`
	Count int    `json:"count"`
	Limit int    `json:"limit,omitempty"`
	Unit  string `json:"unit"` // chars or bytes
	// Fold is roughly how many characters the network shows before its "more" cut (0: no cut).
	Fold int `json:"fold,omitempty"`
}

// PreviewMedia is one attached file, in publishing order.
type PreviewMedia struct {
	ID       string `json:"id"`
	Filename string `json:"filename,omitempty"`
	Mime     string `json:"mime,omitempty"`
	Size     int64  `json:"size,omitempty"`
	// Path is /media/<file> on this Relay instance. It works for a private bucket too (Relay proxies it).
	Path    string `json:"path,omitempty"`
	Missing bool   `json:"missing,omitempty"`
}

// PostPreview is one post (one account) of a group.
type PostPreview struct {
	Post    *store.Post    `json:"post"`
	Account *store.Account `json:"account,omitempty"`
	// Layout says how the network lays the post out: thread (X, Threads), caption (Instagram, Facebook,
	// TikTok), video (YouTube), post_comments (LinkedIn: extra parts become comments), article (website).
	Layout   string         `json:"layout"`
	Title    *PreviewText   `json:"title,omitempty"`
	Texts    []PreviewText  `json:"texts"`
	Media    []PreviewMedia `json:"media"`
	Problems []string       `json:"problems"`
}

// GroupPreview is every post created by one schedule_post call (one per account).
type GroupPreview struct {
	GroupID  string        `json:"group_id"`
	Timezone string        `json:"timezone"`
	Posts    []PostPreview `json:"posts"`
}

// PreviewGroup previews the group of the post id: the same content across all its accounts.
func (s *Service) PreviewGroup(ctx context.Context, postID string) (*GroupPreview, error) {
	p, err := s.Store.GetPost(ctx, postID)
	if err != nil {
		return nil, err
	}
	posts, err := s.Store.ListPosts(ctx, store.PostFilter{GroupID: p.GroupID, Limit: 200})
	if err != nil {
		return nil, err
	}
	accs := map[string]*store.Account{}
	out := &GroupPreview{GroupID: p.GroupID, Timezone: s.timezone(), Posts: make([]PostPreview, 0, len(posts))}
	for _, gp := range posts {
		a, ok := accs[gp.AccountID]
		if !ok {
			a, err = s.Store.GetAccount(ctx, gp.AccountID)
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				return nil, err
			}
			accs[gp.AccountID] = a
		}
		out.Posts = append(out.Posts, s.PreviewPost(ctx, gp, a))
	}
	sort.SliceStable(out.Posts, func(i, j int) bool {
		pi, pj := out.Posts[i], out.Posts[j]
		if pi.Post.ID == postID || pj.Post.ID == postID {
			return pi.Post.ID == postID // the requested post first
		}
		return providerOf(pi) < providerOf(pj)
	})
	return out, nil
}

func providerOf(p PostPreview) string {
	if p.Account == nil {
		return ""
	}
	return p.Account.Provider + "/" + p.Account.Name
}

func (s *Service) timezone() string {
	if s.Location == nil || s.Location.String() == "Local" {
		return "UTC"
	}
	return s.Location.String()
}

// PreviewPost builds the preview of one post. acc may be nil (account deleted).
func (s *Service) PreviewPost(ctx context.Context, p *store.Post, acc *store.Account) PostPreview {
	pv := PostPreview{Post: p, Account: acc, Media: []PreviewMedia{}, Problems: []string{}}
	var files []providers.MediaFile
	for _, id := range p.Media {
		m, err := s.Store.GetMedia(ctx, id)
		if err != nil {
			pv.Media = append(pv.Media, PreviewMedia{ID: id, Missing: true})
			pv.Problems = append(pv.Problems, "media "+id+" is missing")
			continue
		}
		pv.Media = append(pv.Media, PreviewMedia{ID: m.ID, Filename: m.Filename, Mime: m.Mime, Size: m.Size, Path: "/media/" + m.Key()})
		files = append(files, providers.MediaFile{ID: m.ID, Filename: m.Filename, Mime: m.Mime, Size: m.Size, PublicURL: s.MediaURL(m),
			Open: func() (io.ReadCloser, error) { return s.OpenMedia(ctx, m) }})
	}
	provider := ""
	if acc != nil {
		provider = acc.Provider
	}
	pv.Layout, pv.Title, pv.Texts = layout(provider, p.Parts, p.Options)
	// Pre-flight checks only make sense for posts that are still to be published.
	if acc != nil && (p.Status == store.StatusDraft || p.Status == store.StatusScheduled || p.Status == store.StatusFailed) {
		pv.Problems = append(pv.Problems, s.validate(acc, p.Parts, files, p.Options)...)
	}
	if acc == nil {
		pv.Problems = append(pv.Problems, "account "+p.AccountID+" no longer exists")
	}
	return pv
}

func layout(provider string, parts []string, opts map[string]any) (string, *PreviewText, []PreviewText) {
	count := providers.CharCount
	if f, err := providers.Get(provider); err == nil && f.Count != nil {
		count = f.Count
	}
	block := func(label, text string, limit, fold int) PreviewText {
		return PreviewText{Label: label, Text: text, Count: count(text), Limit: limit, Unit: "chars", Fold: fold}
	}
	joined := strings.Join(parts, "\n\n")
	optTitle, _ := opts["title"].(string)
	optTitle = strings.TrimSpace(optTitle)
	each := func(label string, limit int) []PreviewText {
		out := make([]PreviewText, 0, len(parts))
		for _, t := range parts {
			out = append(out, block(label, t, limit, 0))
		}
		return out
	}
	switch provider {
	case "x":
		limit := 280
		if v, ok := opts["max_chars"].(float64); ok && v > 0 {
			limit = int(v)
		}
		return "thread", nil, each("post", limit)
	case "threads":
		return "thread", nil, each("post", 500)
	case "linkedin-page":
		var out []PreviewText
		for i, t := range parts {
			if i == 0 {
				out = append(out, block("post", t, 3000, 210))
			} else {
				out = append(out, block("comment", t, 1250, 0))
			}
		}
		return "post_comments", nil, out
	case "instagram":
		return "caption", nil, []PreviewText{block("caption", joined, 2200, 125)}
	case "facebook":
		return "caption", nil, []PreviewText{block("caption", joined, 63206, 480)}
	case "tiktok":
		if mt, _ := opts["media_type"].(string); strings.EqualFold(mt, "PHOTO") {
			var title *PreviewText
			if optTitle != "" {
				t := block("title", optTitle, 90, 0)
				title = &t
			}
			return "caption", title, []PreviewText{block("description", joined, 4000, 0)}
		}
		return "caption", nil, []PreviewText{block("caption", joined, 2200, 0)}
	case "youtube":
		desc := strings.TrimSpace(joined)
		title := optTitle
		if title == "" { // same fallback as the connector: the first line of the text
			title = desc
			if i := strings.IndexByte(title, '\n'); i >= 0 {
				title = title[:i]
			}
			if r := []rune(strings.TrimSpace(title)); len(r) > 100 {
				title = string(r[:100])
			}
			title = strings.TrimSpace(title)
		}
		t := block("title", title, 100, 0)
		d := PreviewText{Label: "description", Text: desc, Count: len(desc), Limit: 5000, Unit: "bytes"}
		return "video", &t, []PreviewText{d}
	case "website":
		var title *PreviewText
		if optTitle != "" {
			t := block("title", optTitle, 0, 0)
			title = &t
		}
		return "article", title, []PreviewText{block("body", joined, 0, 0)}
	}
	return "thread", nil, each("post", 0)
}
