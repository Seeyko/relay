package core

import (
	"context"
	"errors"
	"strings"

	"github.com/Seeyko/relay/internal/store"
)

// SharedMedia is one media file as an agent shares it with a person: the metadata, and a link that
// opens in a browser without a Relay login (the same public link the networks download from).
type SharedMedia struct {
	ID       string `json:"id"`
	Filename string `json:"filename,omitempty"`
	Mime     string `json:"mime,omitempty"`
	Size     int64  `json:"size,omitempty"`
	// URL is absolute when RELAY_BASE_URL (or a public bucket URL) is set, else /media/<file>.
	URL string `json:"url,omitempty"`
	// Part is the thread part (1-based) the media belongs to, when listed from a post.
	Part    int  `json:"part,omitempty"`
	Missing bool `json:"missing,omitempty"`
}

// ShareURL is MediaURL, or the instance-relative /media/<file> path when Relay has no base URL.
func (s *Service) ShareURL(m *store.Media) string {
	if u := s.MediaURL(m); u != "" {
		return u
	}
	return "/media/" + m.Key()
}

// MediaLinks returns the given media ids and, when postID is set, the media of that post in
// publishing order (first part, then each later part of a thread). Unknown ids come back Missing.
func (s *Service) MediaLinks(ctx context.Context, ids []string, postID string) ([]SharedMedia, error) {
	type ref struct {
		id   string
		part int
	}
	var refs []ref
	if postID = strings.TrimSpace(postID); postID != "" {
		p, err := s.Store.GetPost(ctx, postID)
		if err != nil {
			return nil, err
		}
		parts := postPartMedia(p)
		for i, pids := range parts {
			for _, id := range pids {
				r := ref{id: id}
				if len(parts) > 1 {
					r.part = i + 1
				}
				refs = append(refs, r)
			}
		}
	}
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" {
			refs = append(refs, ref{id: id})
		}
	}
	if len(refs) == 0 {
		return nil, &ErrInvalid{Msg: "give media ids or a post id"}
	}
	out := make([]SharedMedia, 0, len(refs))
	for _, r := range refs {
		m, err := s.Store.GetMedia(ctx, r.id)
		if errors.Is(err, store.ErrNotFound) {
			out = append(out, SharedMedia{ID: r.id, Part: r.part, Missing: true})
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, SharedMedia{ID: m.ID, Filename: m.Filename, Mime: m.Mime, Size: m.Size, URL: s.ShareURL(m), Part: r.part})
	}
	return out, nil
}
