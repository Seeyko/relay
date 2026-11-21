package core

import (
	"context"
	"slices"
	"strings"

	"github.com/Seeyko/relay/internal/store"
)

// FeedbackKinds are the accepted feedback categories.
var FeedbackKinds = []string{"missing_feature", "bug", "improvement", "other"}

type FeedbackInput struct {
	Kind      string
	Title     string
	Body      string
	Media     []string       // existing media ids
	MediaURLs []string       // downloaded into the media library
	Context   map[string]any // what the agent was doing: tool, arguments, network, error...
	Source    string
	Role      string
}

const (
	maxFeedbackTitle = 200
	maxFeedbackBody  = 20000
	maxFeedbackMedia = 10
)

// SubmitFeedback records a request for a missing capability or a bug report and emits feedback.new.
func (s *Service) SubmitFeedback(ctx context.Context, in FeedbackInput) (*store.Feedback, error) {
	in.Title, in.Body = strings.TrimSpace(in.Title), strings.TrimSpace(in.Body)
	if in.Kind == "" {
		in.Kind = "missing_feature"
	}
	switch {
	case !slices.Contains(FeedbackKinds, in.Kind):
		return nil, invalid("kind must be one of %s", strings.Join(FeedbackKinds, ", "))
	case in.Title == "" || in.Body == "":
		return nil, invalid("title and body are required: say what the user asked, what you tried and what is missing")
	case len([]rune(in.Title)) > maxFeedbackTitle:
		return nil, invalid("title is limited to %d characters", maxFeedbackTitle)
	case len(in.Body) > maxFeedbackBody:
		return nil, invalid("body is limited to %d bytes", maxFeedbackBody)
	case len(in.Media)+len(in.MediaURLs) > maxFeedbackMedia:
		return nil, invalid("at most %d media per feedback", maxFeedbackMedia)
	}
	ids := []string{}
	for _, id := range in.Media {
		if _, err := s.Store.GetMedia(ctx, id); err != nil {
			return nil, invalid("unknown media %s", id)
		}
		ids = append(ids, id)
	}
	for _, u := range in.MediaURLs {
		m, err := s.SaveMediaFromURL(ctx, u)
		if err != nil {
			return nil, invalid("media_urls %s: %v", u, err)
		}
		ids = append(ids, m.ID)
	}
	f := &store.Feedback{Kind: in.Kind, Title: in.Title, Body: in.Body, Media: ids, Context: in.Context, Source: in.Source, Role: in.Role}
	if err := s.Store.CreateFeedback(ctx, f); err != nil {
		return nil, err
	}
	s.emit("feedback.new", f)
	return f, nil
}

// UpdateFeedback changes the status of a feedback (maintainer agents and humans).
func (s *Service) UpdateFeedback(ctx context.Context, id, status, resolution, prURL string) (*store.Feedback, error) {
	f, err := s.Store.GetFeedback(ctx, id)
	if err != nil {
		return nil, err
	}
	if status != "" {
		if !slices.Contains(store.FeedbackStatuses, status) {
			return nil, invalid("status must be one of %s", strings.Join(store.FeedbackStatuses, ", "))
		}
		f.Status = status
	}
	if resolution != "" {
		f.Resolution = resolution
	}
	if prURL != "" {
		if !strings.HasPrefix(prURL, "https://") {
			return nil, invalid("pr_url must be an https URL")
		}
		f.PRURL = prURL
	}
	if err := s.Store.UpdateFeedback(ctx, f); err != nil {
		return nil, err
	}
	s.emit("feedback.updated", f)
	return f, nil
}
