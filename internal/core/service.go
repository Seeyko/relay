// Package core holds Relay's business logic, shared by the REST API, the MCP server and the scheduler.
package core

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/Seeyko/relay/internal/blob"
	"github.com/Seeyko/relay/internal/providers"
	"github.com/Seeyko/relay/internal/secret"
	"github.com/Seeyko/relay/internal/store"
)

type Service struct {
	Store    *store.Store
	Box      *secret.Box
	MediaDir string
	// Blob (optional) stores media in an S3-compatible bucket instead of MediaDir.
	Blob *blob.S3
	// MediaPublicURL (optional) is a public base URL of the bucket (custom domain or r2.dev). When empty,
	// media stay private in the bucket and Relay serves them at BaseURL/media/.
	MediaPublicURL string
	BaseURL        string // public URL of this Relay instance, used for media links
	HTTP           *http.Client
	WebhookURL     string
	WebhookSecret  string
	MaxAttempts    int
	Location       *time.Location // owner's timezone, used to group analytics by day/hour
	Log            *slog.Logger
	Now            func() time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now().UTC()
}

// ErrInvalid marks a caller mistake (bad input), surfaced as 400.
type ErrInvalid struct{ Msg string }

func (e *ErrInvalid) Error() string { return e.Msg }

func invalid(format string, a ...any) error { return &ErrInvalid{Msg: fmt.Sprintf(format, a...)} }

// ---------- accounts ----------

func (s *Service) ConnectAccount(ctx context.Context, provider, name string, creds map[string]string) (*store.Account, error) {
	f, err := providers.Get(provider)
	if err != nil {
		return nil, invalid("%v", err)
	}
	cl, err := f.New(creds, nil, s.HTTP)
	if err != nil {
		return nil, invalid("%v", err)
	}
	id, err := cl.Identify(ctx)
	if err != nil {
		return nil, fmt.Errorf("could not verify credentials with %s: %w", provider, err)
	}
	creds["user_id"], creds["username"] = id.RemoteUserID, id.Handle
	sealed, err := s.Box.SealJSON(creds)
	if err != nil {
		return nil, err
	}
	if name == "" {
		name = id.Name
	}
	acc := &store.Account{Provider: provider, Name: name, Handle: id.Handle, RemoteUserID: id.RemoteUserID, Credentials: sealed}
	if err := s.Store.CreateAccount(ctx, acc); err != nil {
		return nil, err
	}
	return acc, nil
}

func (s *Service) client(ctx context.Context, acc *store.Account) (providers.Client, error) {
	f, err := providers.Get(acc.Provider)
	if err != nil {
		return nil, err
	}
	creds := map[string]string{}
	if err := s.Box.OpenJSON(acc.Credentials, &creds); err != nil {
		return nil, fmt.Errorf("decrypt credentials of %s: %w (did RELAY_ENCRYPTION_KEY change?)", acc.ID, err)
	}
	persist := func(updated map[string]string) error {
		sealed, err := s.Box.SealJSON(updated)
		if err != nil {
			return err
		}
		acc.Credentials = sealed
		return s.Store.UpdateAccount(ctx, acc)
	}
	return f.New(creds, persist, s.HTTP)
}

// ---------- posts ----------

type ScheduleInput struct {
	AccountIDs  []string       `json:"account_ids"`
	Text        string         `json:"text"`
	Thread      []string       `json:"thread"` // alternative to text: several parts
	Media       []string       `json:"media"`
	ScheduledAt string         `json:"scheduled_at"` // RFC3339; empty = now; "draft" = keep as draft
	Options     map[string]any `json:"options"`
	// PerAccount overrides text per account id (e.g. shorter version for X).
	PerAccount map[string]string `json:"per_account"`
	// Force skips the pre-flight validation.
	Force bool `json:"force"`
	// PlannedAt (RFC3339, drafts only) is when the draft is meant to go out. It does not publish anything:
	// approve_post schedules the draft at that time if it is still in the future.
	PlannedAt string `json:"planned_at"`
}

func (in *ScheduleInput) parts() []string {
	if len(in.Thread) > 0 {
		return in.Thread
	}
	if strings.TrimSpace(in.Text) == "" {
		return nil
	}
	return []string{in.Text}
}

func (s *Service) Schedule(ctx context.Context, in ScheduleInput) ([]*store.Post, error) {
	if len(in.AccountIDs) == 0 {
		return nil, invalid("account_ids is required")
	}
	if len(in.parts()) == 0 && len(in.PerAccount) == 0 {
		return nil, invalid("text or thread is required")
	}
	status := store.StatusScheduled
	var at *time.Time
	switch strings.TrimSpace(in.ScheduledAt) {
	case "", "now":
		t := s.now()
		at = &t
	case "draft":
		status = store.StatusDraft
	default:
		t, err := time.Parse(time.RFC3339, in.ScheduledAt)
		if err != nil {
			return nil, invalid("scheduled_at must be RFC3339 (e.g. 2026-11-20T09:00:00+01:00): %v", err)
		}
		t = t.UTC()
		at = &t
	}
	var planned *time.Time
	if strings.TrimSpace(in.PlannedAt) != "" {
		if status != store.StatusDraft {
			return nil, invalid("planned_at is only for drafts (scheduled_at: \"draft\"); use scheduled_at to schedule a post")
		}
		t, err := parseTime("planned_at", in.PlannedAt)
		if err != nil {
			return nil, err
		}
		planned = &t
	}
	for _, m := range in.Media {
		if _, err := s.Store.GetMedia(ctx, m); err != nil {
			return nil, invalid("unknown media %q", m)
		}
	}
	accs := make([]*store.Account, 0, len(in.AccountIDs))
	for _, id := range in.AccountIDs {
		a, err := s.Store.GetAccount(ctx, id)
		if err != nil {
			return nil, invalid("unknown account %q", id)
		}
		accs = append(accs, a)
	}
	files, err := s.mediaFiles(ctx, in.Media)
	if err != nil {
		return nil, err
	}
	group := store.NewID("grp")
	plan := map[string][]string{}
	var problems []string
	for _, a := range accs {
		parts := in.parts()
		if t, ok := in.PerAccount[a.ID]; ok {
			parts = []string{t}
		}
		if len(parts) == 0 {
			return nil, invalid("no text for account %s", a.ID)
		}
		plan[a.ID] = parts
		if !in.Force {
			for _, pr := range s.validate(a, parts, files, in.Options) {
				problems = append(problems, a.Provider+" ("+a.ID+"): "+pr)
			}
		}
	}
	if len(problems) > 0 {
		return nil, invalid("post rejected by pre-flight checks (use force to override): %s", strings.Join(problems, "; "))
	}
	var out []*store.Post
	for _, a := range accs {
		opts := map[string]any{}
		for k, v := range in.Options {
			opts[k] = v
		}
		opts["kind"] = kindOf(plan[a.ID], files, opts)
		p := &store.Post{GroupID: group, AccountID: a.ID, Status: status, Parts: plan[a.ID], Media: in.Media, Options: opts, ScheduledAt: at, PlannedAt: planned}
		if err := s.Store.CreatePost(ctx, p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	s.emit("post.scheduled", map[string]any{"group_id": group, "posts": out})
	return out, nil
}

type UpdateInput struct {
	Text        *string        `json:"text"`
	Thread      []string       `json:"thread"`
	Media       []string       `json:"media"`
	ScheduledAt *string        `json:"scheduled_at"`
	Options     map[string]any `json:"options"`
	PlannedAt   *string        `json:"planned_at"` // drafts only: RFC3339, "" or "none" removes it
}

// parseTime reads an RFC3339 time with its offset and returns it in UTC.
func parseTime(field, v string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(v))
	if err != nil {
		return time.Time{}, invalid("%s must be RFC3339 with an offset (e.g. 2026-11-20T09:00:00+01:00): %v", field, err)
	}
	return t.UTC(), nil
}

func (s *Service) UpdatePost(ctx context.Context, id string, in UpdateInput) (*store.Post, error) {
	p, err := s.Store.GetPost(ctx, id)
	if err != nil {
		return nil, err
	}
	if p.Status != store.StatusDraft && p.Status != store.StatusScheduled && p.Status != store.StatusFailed {
		return nil, invalid("post is %s and can no longer be edited", p.Status)
	}
	if in.Text != nil {
		p.Parts = []string{*in.Text}
	}
	if len(in.Thread) > 0 {
		p.Parts = in.Thread
	}
	if in.Media != nil {
		p.Media = in.Media
	}
	if in.Options != nil {
		p.Options = in.Options
	}
	if in.ScheduledAt != nil {
		switch *in.ScheduledAt {
		case "draft":
			// Back to draft: the old schedule becomes the planned time, unless planned_at says otherwise.
			if p.Status != store.StatusDraft && p.ScheduledAt != nil && in.PlannedAt == nil {
				t := *p.ScheduledAt
				p.PlannedAt = &t
			}
			p.Status, p.ScheduledAt = store.StatusDraft, nil
		case "", "now":
			t := s.now()
			p.Status, p.ScheduledAt, p.PlannedAt = store.StatusScheduled, &t, nil
		default:
			t, err := parseTime("scheduled_at", *in.ScheduledAt)
			if err != nil {
				return nil, err
			}
			p.Status, p.ScheduledAt, p.PlannedAt = store.StatusScheduled, &t, nil
		}
		p.Attempts, p.NextAttemptAt, p.Error = 0, nil, ""
	}
	if in.PlannedAt != nil {
		if p.Status != store.StatusDraft {
			return nil, invalid("planned_at is only for drafts; this post is %s (use scheduled_at)", p.Status)
		}
		switch v := strings.TrimSpace(*in.PlannedAt); v {
		case "", "none":
			p.PlannedAt = nil
		default:
			t, err := parseTime("planned_at", v)
			if err != nil {
				return nil, err
			}
			p.PlannedAt = &t
		}
	}
	return p, s.Store.UpdatePost(ctx, p)
}

// Approve releases a draft. scheduledAt (RFC3339 or "now") wins when given; otherwise the draft is
// scheduled at its planned time if that is still in the future, else it publishes within one tick.
func (s *Service) Approve(ctx context.Context, id, scheduledAt string) (*store.Post, error) {
	p, err := s.Store.GetPost(ctx, id)
	if err != nil {
		return nil, err
	}
	if p.Status != store.StatusDraft {
		return nil, invalid("post is %s, not a draft", p.Status)
	}
	at := strings.TrimSpace(scheduledAt)
	if at == "draft" {
		return nil, invalid("scheduled_at must be RFC3339 or 'now' to approve a draft")
	}
	if at == "" {
		at = "now"
		if p.PlannedAt != nil && p.PlannedAt.After(s.now()) {
			at = p.PlannedAt.Format(time.RFC3339)
		}
	}
	return s.UpdatePost(ctx, id, UpdateInput{ScheduledAt: &at})
}

// PublishNow publishes synchronously, regardless of the schedule.
func (s *Service) PublishNow(ctx context.Context, id string) (*store.Post, error) {
	p, err := s.Store.GetPost(ctx, id)
	if err != nil {
		return nil, err
	}
	if p.Status == store.StatusPublished || p.Status == store.StatusPublishing {
		return nil, invalid("post is already %s", p.Status)
	}
	p.Status = store.StatusPublishing
	if err := s.Store.UpdatePost(ctx, p); err != nil {
		return nil, err
	}
	s.Publish(ctx, p)
	return s.Store.GetPost(ctx, id)
}

func (s *Service) mediaFiles(ctx context.Context, ids []string) ([]providers.MediaFile, error) {
	var out []providers.MediaFile
	for _, id := range ids {
		m, err := s.Store.GetMedia(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("media %s: %w", id, err)
		}
		out = append(out, providers.MediaFile{ID: m.ID, Filename: m.Filename, Mime: m.Mime, Size: m.Size,
			Open:      func() (io.ReadCloser, error) { return s.OpenMedia(ctx, m) },
			PublicURL: s.MediaURL(m)})
	}
	return out, nil
}

var backoff = []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour}

// Publish sends a post already marked "publishing" and records the outcome.
func (s *Service) Publish(ctx context.Context, p *store.Post) {
	err := s.publish(ctx, p)
	now := s.now()
	if err == nil {
		p.Status, p.Error, p.PublishedAt = store.StatusPublished, "", &now
		p.Attempts++
		if uerr := s.Store.UpdatePost(ctx, p); uerr != nil {
			s.Log.Error("save published post", "post", p.ID, "err", uerr)
		}
		s.Log.Info("published", "post", p.ID, "url", p.RemoteURL)
		s.emit("post.published", p)
		return
	}
	p.Attempts++
	p.Error = err.Error()
	max := s.MaxAttempts
	if max <= 0 {
		max = 4
	}
	// A partially published thread must not be re-sent from scratch.
	partial := len(p.RemoteIDs) > 0
	if providers.Retryable(err) && p.Attempts < max && !partial {
		d := backoff[min(p.Attempts-1, len(backoff)-1)]
		next := now.Add(d)
		p.Status, p.NextAttemptAt = store.StatusScheduled, &next
		s.Log.Warn("publish failed, will retry", "post", p.ID, "attempt", p.Attempts, "in", d, "err", err)
	} else {
		p.Status = store.StatusFailed
		s.Log.Error("publish failed", "post", p.ID, "err", err)
		s.emit("post.failed", p)
	}
	if uerr := s.Store.UpdatePost(ctx, p); uerr != nil {
		s.Log.Error("save failed post", "post", p.ID, "err", uerr)
	}
}

func (s *Service) publish(ctx context.Context, p *store.Post) error {
	acc, err := s.Store.GetAccount(ctx, p.AccountID)
	if err != nil {
		return fmt.Errorf("account %s: %w", p.AccountID, err)
	}
	cl, err := s.client(ctx, acc)
	if err != nil {
		return err
	}
	media, err := s.mediaFiles(ctx, p.Media)
	if err != nil {
		return err
	}
	// Resume a thread that stopped half-way: publish only the missing parts, chained to the last one sent.
	if done := len(p.RemoteIDs); done > 0 && done < len(p.Parts) {
		opts := map[string]any{}
		for k, v := range p.Options {
			opts[k] = v
		}
		opts["reply_to"] = p.RemoteIDs[done-1]
		res, err := cl.Publish(ctx, providers.PublishRequest{Parts: p.Parts[done:], Options: opts})
		if res != nil {
			p.RemoteIDs = append(p.RemoteIDs, res.RemoteIDs...)
		}
		return err
	}
	res, err := cl.Publish(ctx, providers.PublishRequest{Parts: p.Parts, Media: media, Options: p.Options})
	if res != nil {
		p.RemoteID, p.RemoteIDs, p.RemoteURL = res.RemoteID, res.RemoteIDs, res.URL
		if p.RemoteID == "" && len(res.RemoteIDs) > 0 {
			p.RemoteID = res.RemoteIDs[0]
		}
	}
	return err
}

// validate runs the provider's pre-flight checks.
func (s *Service) validate(a *store.Account, parts []string, media []providers.MediaFile, opts map[string]any) []string {
	f, err := providers.Get(a.Provider)
	if err != nil || f.Validate == nil {
		return nil
	}
	return f.Validate(providers.PublishRequest{Parts: parts, Media: media, Options: opts})
}

type ValidationResult struct {
	AccountID string   `json:"account_id"`
	Provider  string   `json:"provider"`
	OK        bool     `json:"ok"`
	Problems  []string `json:"problems"`
	Chars     []int    `json:"chars_per_part"`
}

// Validate checks a post against every target network without scheduling it.
func (s *Service) Validate(ctx context.Context, in ScheduleInput) ([]ValidationResult, error) {
	files, err := s.mediaFiles(ctx, in.Media)
	if err != nil {
		return nil, invalid("%v", err)
	}
	var out []ValidationResult
	for _, id := range in.AccountIDs {
		a, err := s.Store.GetAccount(ctx, id)
		if err != nil {
			return nil, invalid("unknown account %q", id)
		}
		parts := in.parts()
		if t, ok := in.PerAccount[a.ID]; ok {
			parts = []string{t}
		}
		r := ValidationResult{AccountID: a.ID, Provider: a.Provider, Problems: s.validate(a, parts, files, in.Options)}
		if len(parts) == 0 {
			r.Problems = append(r.Problems, "no text")
		}
		for _, p := range parts {
			r.Chars = append(r.Chars, providers.CharCount(p))
		}
		r.OK = len(r.Problems) == 0
		if r.Problems == nil {
			r.Problems = []string{}
		}
		out = append(out, r)
	}
	return out, nil
}

func kindOf(parts []string, media []providers.MediaFile, opts map[string]any) string {
	if k, _ := opts["kind"].(string); k != "" {
		return k
	}
	if _, ok := opts["reply_to"]; ok {
		return "reply"
	}
	if len(parts) > 1 {
		return "thread"
	}
	switch {
	case len(media) > 1:
		return "carousel"
	case len(media) == 1 && strings.HasPrefix(media[0].Mime, "video/"):
		return "video"
	case len(media) == 1:
		return "image"
	}
	return "text"
}

// ---------- metrics & comments ----------

// RefreshMetrics updates metrics of published posts (all accounts, or one) published after since.
func (s *Service) RefreshMetrics(ctx context.Context, accountID string, since time.Time) (int, error) {
	return s.refreshMetrics(ctx, accountID, since, nil)
}

// refreshMetrics updates metrics (and records a snapshot) of published posts; due filters which posts to fetch.
func (s *Service) refreshMetrics(ctx context.Context, accountID string, since time.Time, due func(*store.Post) bool) (int, error) {
	accs, err := s.accounts(ctx, accountID)
	if err != nil {
		return 0, err
	}
	updated := 0
	var errs []error
	for _, acc := range accs {
		all, err := s.Store.ListPosts(ctx, store.PostFilter{AccountID: acc.ID, Status: store.StatusPublished, From: &since, Limit: 1000})
		if err != nil {
			return updated, err
		}
		posts := all[:0]
		for _, p := range all {
			if p.RemoteID != "" && (due == nil || due(p)) {
				posts = append(posts, p)
			}
		}
		if len(posts) == 0 {
			continue
		}
		cl, err := s.client(ctx, acc)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		ids := make([]string, 0, len(posts))
		for _, p := range posts {
			if p.RemoteID != "" {
				ids = append(ids, p.RemoteID)
			}
		}
		m, err := cl.Metrics(ctx, ids)
		if err != nil && !errors.Is(err, providers.ErrUnsupported) {
			errs = append(errs, fmt.Errorf("%s: %w", acc.ID, err))
		}
		now := s.now()
		for _, p := range posts {
			if v, ok := m[p.RemoteID]; ok {
				p.Metrics, p.MetricsAt = v, &now
				if err := s.Store.UpdatePost(ctx, p); err != nil {
					return updated, err
				}
				if err := s.Store.AddPostSnapshot(ctx, p.ID, now, v); err != nil {
					return updated, err
				}
				updated++
			}
		}
	}
	return updated, errors.Join(errs...)
}

// RefreshComments pulls replies of one post and stores new ones. Returns the new comments.
func (s *Service) RefreshComments(ctx context.Context, postID string) ([]*store.Comment, error) {
	p, err := s.Store.GetPost(ctx, postID)
	if err != nil {
		return nil, err
	}
	if p.RemoteID == "" {
		return nil, invalid("post %s is not published", postID)
	}
	acc, err := s.Store.GetAccount(ctx, p.AccountID)
	if err != nil {
		return nil, err
	}
	cl, err := s.client(ctx, acc)
	if err != nil {
		return nil, err
	}
	cs, err := cl.Comments(ctx, p.RemoteID)
	if err != nil {
		return nil, err
	}
	var fresh []*store.Comment
	for _, c := range cs {
		sc := &store.Comment{AccountID: acc.ID, RemoteID: c.RemoteID, PostID: p.ID, Author: c.Author, Text: c.Text, URL: c.URL, CreatedAt: c.CreatedAt}
		isNew, err := s.Store.UpsertComment(ctx, sc)
		if err != nil {
			return fresh, err
		}
		if isNew {
			fresh = append(fresh, sc)
		}
	}
	if len(fresh) > 0 {
		s.emit("comment.new", map[string]any{"post_id": p.ID, "comments": fresh})
	}
	return fresh, nil
}

func (s *Service) ReplyToComment(ctx context.Context, accountID, commentRemoteID, text string) (*providers.PublishResult, error) {
	if strings.TrimSpace(text) == "" {
		return nil, invalid("text is required")
	}
	acc, err := s.Store.GetAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}
	cl, err := s.client(ctx, acc)
	if err != nil {
		return nil, err
	}
	res, err := cl.Reply(ctx, commentRemoteID, text)
	if err != nil {
		return nil, err
	}
	_ = s.Store.MarkReplied(ctx, accountID, commentRemoteID)
	return res, nil
}

// ImportOptions tunes ImportHistory.
type ImportOptions struct {
	// SkipVisuals imports text and metrics only, without downloading the posts' images.
	SkipVisuals bool
}

// ImportResult counts what an import did.
type ImportResult struct {
	Created int `json:"created"`
	Updated int `json:"updated"`
	// Visuals is the number of images downloaded into the media library.
	Visuals int `json:"visuals"`
	// VisualsFailed is the number of images that could not be downloaded (see the log).
	VisualsFailed int `json:"visuals_failed,omitempty"`
}

// ImportHistory archives past posts of an account (for the "Mon contenu" archive). Each imported post
// also gets its visuals (photos, video covers) copied into the media library, once: a re-import fills
// the imported posts that have none yet, and never touches posts created in Relay.
func (s *Service) ImportHistory(ctx context.Context, accountID string, since time.Time, max int, opts ImportOptions) (ImportResult, error) {
	var res ImportResult
	acc, err := s.Store.GetAccount(ctx, accountID)
	if err != nil {
		return res, err
	}
	cl, err := s.client(ctx, acc)
	if err != nil {
		return res, err
	}
	items, herr := cl.History(ctx, since, max)
	for _, it := range items {
		ca := it.CreatedAt.UTC()
		p := &store.Post{AccountID: acc.ID, Parts: []string{it.Text}, RemoteID: it.RemoteID, RemoteIDs: []string{it.RemoteID}, RemoteURL: it.URL, PublishedAt: &ca, Metrics: it.Metrics, CreatedAt: ca}
		if it.Kind != "" {
			p.Options = map[string]any{"kind": it.Kind}
		}
		isNew, err := s.Store.UpsertImported(ctx, p)
		if err != nil {
			return res, err
		}
		if err := s.Store.AddPostSnapshot(ctx, p.ID, s.now(), it.Metrics); err != nil {
			return res, err
		}
		if isNew {
			res.Created++
		} else {
			res.Updated++
		}
		if !opts.SkipVisuals && p.Source == "imported" && len(p.Media) == 0 && len(it.Visuals) > 0 {
			ok, failed := s.importVisuals(ctx, acc, p.ID, it)
			res.Visuals += ok
			res.VisualsFailed += failed
		}
	}
	return res, herr
}

const (
	// maxVisuals caps the images kept per imported post.
	maxVisuals = 10
	// maxVisualSize skips larger images (the networks serve display-size images well below this).
	maxVisualSize = 15 << 20
	visualTimeout = time.Minute
)

// importVisuals downloads the visuals of an imported post into the media library and attaches them.
// Best effort: a failed download is logged and skipped, it never fails the import.
func (s *Service) importVisuals(ctx context.Context, acc *store.Account, postID string, it providers.HistoryItem) (ok, failed int) {
	vis := it.Visuals
	if len(vis) > maxVisuals {
		vis = vis[:maxVisuals]
	}
	var ids []string
	for i, v := range vis {
		name := fmt.Sprintf("%s-%s-%d", acc.Provider, safeName(it.RemoteID), i+1)
		m, err := s.saveVisual(ctx, v.URL, name)
		if err != nil {
			failed++
			s.Log.Warn("import: visual download failed", "account", acc.ID, "post", postID, "remote_id", it.RemoteID, "err", err)
			continue
		}
		ids = append(ids, m.ID)
	}
	if len(ids) == 0 {
		return 0, failed
	}
	set, err := s.Store.SetImportedMedia(ctx, postID, ids)
	if err != nil || !set {
		// Another import attached media in the meantime: drop ours, keep theirs.
		for _, id := range ids {
			s.deleteMedia(ctx, id)
		}
		if err != nil {
			s.Log.Warn("import: attach visuals", "post", postID, "err", err)
		}
		return 0, failed
	}
	return len(ids), failed
}

// saveVisual downloads one image (never a video) of at most maxVisualSize into the media library.
// The network URLs are signed and expire, so Relay keeps its own copy instead of the link.
func (s *Service) saveVisual(ctx context.Context, rawURL, name string) (*store.Media, error) {
	if !strings.HasPrefix(rawURL, "https://") && !strings.HasPrefix(rawURL, "http://") {
		return nil, fmt.Errorf("not an http(s) url")
	}
	ctx, cancel := context.WithTimeout(ctx, visualTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http %d", resp.StatusCode)
	}
	if resp.ContentLength > maxVisualSize {
		return nil, fmt.Errorf("image larger than %d MB", maxVisualSize>>20)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxVisualSize+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxVisualSize {
		return nil, fmt.Errorf("image larger than %d MB", maxVisualSize>>20)
	}
	mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if !strings.HasPrefix(mt, "image/") {
		// CDNs sometimes answer application/octet-stream (or nothing): trust the bytes.
		mt = http.DetectContentType(b)
	}
	ext, ok := imageExt[mt]
	if !ok {
		return nil, fmt.Errorf("not an image (%s)", mt)
	}
	return s.SaveMedia(ctx, bytes.NewReader(b), name+ext, mt)
}

var imageExt = map[string]string{
	"image/jpeg": ".jpg", "image/png": ".png", "image/gif": ".gif", "image/webp": ".webp",
	"image/heic": ".heic", "image/heif": ".heif", "image/avif": ".avif",
}

// safeName keeps a remote id usable in a file name (LinkedIn ids are URNs with colons).
func safeName(id string) string {
	b := []byte(id)
	for i, c := range b {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			b[i] = '-'
		}
	}
	if len(b) > 64 {
		b = b[len(b)-64:]
	}
	return string(b)
}

func (s *Service) accounts(ctx context.Context, id string) ([]*store.Account, error) {
	if id != "" {
		a, err := s.Store.GetAccount(ctx, id)
		if err != nil {
			return nil, err
		}
		return []*store.Account{a}, nil
	}
	return s.Store.ListAccounts(ctx)
}

// MetricsDue implements the collection cadence: hourly for posts younger than 48h, every 6h until 7 days,
// daily until the end of the tracking window. This builds growth curves while keeping API usage low.
func MetricsDue(p *store.Post, now time.Time) bool {
	if p.PublishedAt == nil {
		return false
	}
	if p.MetricsAt == nil {
		return true
	}
	age, since := now.Sub(*p.PublishedAt), now.Sub(*p.MetricsAt)
	slack := 5 * time.Minute // ticks are not exact
	switch {
	case age < 48*time.Hour:
		return since >= time.Hour-slack
	case age < 7*24*time.Hour:
		return since >= 6*time.Hour-slack
	default:
		return since >= 24*time.Hour-slack
	}
}

// Collect refreshes due metrics, account counters and comments of recent posts.
func (s *Service) Collect(ctx context.Context, window time.Duration) {
	now := s.now()
	since := now.Add(-window)
	n, err := s.refreshMetrics(ctx, "", since, func(p *store.Post) bool { return MetricsDue(p, now) })
	s.Log.Info("collect metrics", "updated", n, "err", err)
	if err := s.SnapshotAccounts(ctx); err != nil {
		s.Log.Warn("collect account metrics", "err", err)
	}
	recent := now.Add(-7 * 24 * time.Hour)
	posts, err := s.Store.ListPosts(ctx, store.PostFilter{Status: store.StatusPublished, Source: "relay", From: &recent, Limit: 500})
	if err != nil {
		s.Log.Error("collect comments", "err", err)
		return
	}
	for _, p := range posts {
		if _, err := s.RefreshComments(ctx, p.ID); err != nil && !errors.Is(err, providers.ErrUnsupported) {
			s.Log.Warn("collect comments", "post", p.ID, "err", err)
		}
	}
}

// SnapshotAccounts records followers/following/posts for every account whose network reports them,
// at most once every 6 hours per account.
func (s *Service) SnapshotAccounts(ctx context.Context) error {
	accs, err := s.Store.ListAccounts(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, a := range accs {
		last, err := s.Store.AccountSnapshots(ctx, a.ID, s.now().Add(-6*time.Hour+5*time.Minute))
		if err == nil && len(last) > 0 {
			continue
		}
		if _, err := s.SnapshotAccount(ctx, a); err != nil && !errors.Is(err, providers.ErrUnsupported) {
			errs = append(errs, fmt.Errorf("%s: %w", a.ID, err))
		}
	}
	return errors.Join(errs...)
}

func (s *Service) SnapshotAccount(ctx context.Context, a *store.Account) (map[string]int64, error) {
	cl, err := s.client(ctx, a)
	if err != nil {
		return nil, err
	}
	st, ok := cl.(providers.AccountStatser)
	if !ok {
		return nil, providers.ErrUnsupported
	}
	m, err := st.AccountMetrics(ctx)
	if err != nil {
		return nil, err
	}
	return m, s.Store.AddAccountSnapshot(ctx, a.ID, s.now(), m)
}

// ---------- media ----------

const maxMedia = 512 << 20

func (s *Service) SaveMedia(ctx context.Context, r io.Reader, filename, mimeType string) (*store.Media, error) {
	if err := os.MkdirAll(s.MediaDir, 0o755); err != nil {
		return nil, err
	}
	id := store.NewID("med")
	ext := strings.ToLower(filepath.Ext(filename))
	if mimeType == "" || mimeType == "application/octet-stream" {
		if t := mime.TypeByExtension(ext); t != "" {
			mimeType = t
		}
	}
	if !strings.HasPrefix(mimeType, "image/") && !strings.HasPrefix(mimeType, "video/") {
		return nil, invalid("unsupported media type %q (images and videos only)", mimeType)
	}
	dst := filepath.Join(s.MediaDir, id+ext)
	f, err := os.Create(dst)
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(r, maxMedia+1))
	if err != nil {
		f.Close()
		os.Remove(dst)
		return nil, err
	}
	if n > maxMedia {
		f.Close()
		os.Remove(dst)
		return nil, invalid("media larger than 512 MB")
	}
	if filename == "" {
		filename = id + ext
	}
	m := &store.Media{ID: id, Filename: filepath.Base(filename), Mime: mimeType, Size: n, Path: dst}
	if s.Blob != nil {
		// Upload to the bucket, then drop the local copy.
		key := id + ext
		if _, err := f.Seek(0, io.SeekStart); err == nil {
			err = s.Blob.Put(ctx, key, f, n, hex.EncodeToString(h.Sum(nil)), mimeType)
		}
		f.Close()
		os.Remove(dst)
		if err != nil {
			return nil, fmt.Errorf("upload media to bucket: %w", err)
		}
		m.Path = store.S3Prefix + key
	} else {
		f.Close()
	}
	return m, s.Store.CreateMedia(ctx, m)
}

// deleteMedia removes a media file (disk or bucket) and its row. Best effort.
func (s *Service) deleteMedia(ctx context.Context, id string) {
	m, err := s.Store.GetMedia(ctx, id)
	if err != nil {
		return
	}
	if m.InBucket() {
		if s.Blob != nil {
			_ = s.Blob.Delete(ctx, m.Key())
		}
	} else {
		_ = os.Remove(m.Path)
	}
	_ = s.Store.DeleteMedia(ctx, id)
}

// OpenMedia reads a media file from disk or from the bucket.
func (s *Service) OpenMedia(ctx context.Context, m *store.Media) (io.ReadCloser, error) {
	if !m.InBucket() {
		return os.Open(m.Path)
	}
	if s.Blob == nil {
		return nil, fmt.Errorf("media %s is in a bucket but no bucket is configured (RELAY_S3_*)", m.ID)
	}
	resp, err := s.Blob.Get(ctx, m.Key(), "")
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

func (s *Service) SaveMediaFromURL(ctx context.Context, rawURL string) (*store.Media, error) {
	if !strings.HasPrefix(rawURL, "https://") && !strings.HasPrefix(rawURL, "http://") {
		return nil, invalid("url must be http(s)")
	}
	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return nil, invalid("%v", err)
	}
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("download %s: http %d", rawURL, resp.StatusCode)
	}
	name := path.Base(req.URL.Path)
	mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	return s.SaveMedia(ctx, resp.Body, name, mt)
}

// MediaURL is the public, unauthenticated link of a media file (ids are random, 64 bits).
func (s *Service) MediaURL(m *store.Media) string {
	if m.InBucket() && s.MediaPublicURL != "" {
		return strings.TrimRight(s.MediaPublicURL, "/") + "/" + m.Key()
	}
	if s.BaseURL == "" {
		return ""
	}
	return strings.TrimRight(s.BaseURL, "/") + "/media/" + m.Key()
}

// ---------- webhooks ----------

func (s *Service) emit(event string, data any) {
	if s.WebhookURL == "" {
		return
	}
	body, _ := json.Marshal(map[string]any{"event": event, "at": s.now(), "data": data})
	go func() {
		req, err := http.NewRequest("POST", s.WebhookURL, bytes.NewReader(body))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Relay-Event", event)
		if s.WebhookSecret != "" {
			mac := hmac.New(sha256.New, []byte(s.WebhookSecret))
			mac.Write(body)
			req.Header.Set("X-Relay-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		}
		resp, err := s.HTTP.Do(req)
		if err != nil {
			s.Log.Warn("webhook", "event", event, "err", err)
			return
		}
		resp.Body.Close()
	}()
}
