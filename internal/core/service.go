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

	"github.com/Seeyko/relay/internal/providers"
	"github.com/Seeyko/relay/internal/secret"
	"github.com/Seeyko/relay/internal/store"
)

type Service struct {
	Store         *store.Store
	Box           *secret.Box
	MediaDir      string
	BaseURL       string // public URL of this Relay instance, used for media links
	HTTP          *http.Client
	WebhookURL    string
	WebhookSecret string
	MaxAttempts   int
	Log           *slog.Logger
	Now           func() time.Time
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
	group := store.NewID("grp")
	var out []*store.Post
	for _, a := range accs {
		parts := in.parts()
		if t, ok := in.PerAccount[a.ID]; ok {
			parts = []string{t}
		}
		if len(parts) == 0 {
			return nil, invalid("no text for account %s", a.ID)
		}
		p := &store.Post{GroupID: group, AccountID: a.ID, Status: status, Parts: parts, Media: in.Media, Options: in.Options, ScheduledAt: at}
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
			p.Status, p.ScheduledAt = store.StatusDraft, nil
		case "", "now":
			t := s.now()
			p.Status, p.ScheduledAt = store.StatusScheduled, &t
		default:
			t, err := time.Parse(time.RFC3339, *in.ScheduledAt)
			if err != nil {
				return nil, invalid("scheduled_at must be RFC3339: %v", err)
			}
			t = t.UTC()
			p.Status, p.ScheduledAt = store.StatusScheduled, &t
		}
		p.Attempts, p.NextAttemptAt, p.Error = 0, nil, ""
	}
	return p, s.Store.UpdatePost(ctx, p)
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
		p := m.Path
		out = append(out, providers.MediaFile{ID: m.ID, Filename: m.Filename, Mime: m.Mime, Size: m.Size,
			Open:      func() (io.ReadCloser, error) { return os.Open(p) },
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
	res, err := cl.Publish(ctx, providers.PublishRequest{Parts: p.Parts, Media: media, Options: p.Options})
	if res != nil {
		p.RemoteID, p.RemoteIDs, p.RemoteURL = res.RemoteID, res.RemoteIDs, res.URL
		if p.RemoteID == "" && len(res.RemoteIDs) > 0 {
			p.RemoteID = res.RemoteIDs[0]
		}
	}
	return err
}

// ---------- metrics & comments ----------

// RefreshMetrics updates metrics of published posts (all accounts, or one) published after since.
func (s *Service) RefreshMetrics(ctx context.Context, accountID string, since time.Time) (int, error) {
	accs, err := s.accounts(ctx, accountID)
	if err != nil {
		return 0, err
	}
	updated := 0
	var errs []error
	for _, acc := range accs {
		posts, err := s.Store.ListPosts(ctx, store.PostFilter{AccountID: acc.ID, Status: store.StatusPublished, From: &since, Limit: 1000})
		if err != nil {
			return updated, err
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

// ImportHistory archives past posts of an account (for the "Mon contenu" archive).
func (s *Service) ImportHistory(ctx context.Context, accountID string, since time.Time, max int) (created, updated int, err error) {
	acc, err := s.Store.GetAccount(ctx, accountID)
	if err != nil {
		return 0, 0, err
	}
	cl, err := s.client(ctx, acc)
	if err != nil {
		return 0, 0, err
	}
	items, herr := cl.History(ctx, since, max)
	for _, it := range items {
		ca := it.CreatedAt.UTC()
		p := &store.Post{AccountID: acc.ID, Parts: []string{it.Text}, RemoteID: it.RemoteID, RemoteIDs: []string{it.RemoteID}, RemoteURL: it.URL, PublishedAt: &ca, Metrics: it.Metrics, CreatedAt: ca}
		isNew, err := s.Store.UpsertImported(ctx, p)
		if err != nil {
			return created, updated, err
		}
		if isNew {
			created++
		} else {
			updated++
		}
	}
	return created, updated, herr
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

// Collect refreshes metrics and comments for posts published in the last `window`.
func (s *Service) Collect(ctx context.Context, window time.Duration) {
	since := s.now().Add(-window)
	if n, err := s.RefreshMetrics(ctx, "", since); err != nil {
		s.Log.Warn("collect metrics", "updated", n, "err", err)
	} else {
		s.Log.Info("collect metrics", "updated", n)
	}
	posts, err := s.Store.ListPosts(ctx, store.PostFilter{Status: store.StatusPublished, Source: "relay", From: &since, Limit: 500})
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
	n, err := io.Copy(f, io.LimitReader(r, maxMedia+1))
	f.Close()
	if err != nil {
		os.Remove(dst)
		return nil, err
	}
	if n > maxMedia {
		os.Remove(dst)
		return nil, invalid("media larger than 512 MB")
	}
	if filename == "" {
		filename = id + ext
	}
	m := &store.Media{ID: id, Filename: filepath.Base(filename), Mime: mimeType, Size: n, Path: dst}
	return m, s.Store.CreateMedia(ctx, m)
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
	if s.BaseURL == "" {
		return ""
	}
	return strings.TrimRight(s.BaseURL, "/") + "/media/" + filepath.Base(m.Path)
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
