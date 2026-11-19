// Package website connects any site that implements the Relay Site Protocol (docs/SITE_PROTOCOL.md).
// One provider for every site: your own sites, client sites, anything that speaks the protocol.
package website

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Seeyko/relay/internal/providers"
)

func init() {
	providers.Register(providers.Factory{
		Name: "website",
		Description: "Any website implementing the Relay Site Protocol v1 (articles, media, metrics, comments). " +
			"Post body = Markdown; options: title, slug, excerpt, tags, lang, cover (media id), status (published|draft), extra.",
		CredentialFields: []string{"base_url", "token"},
		Validate: func(req providers.PublishRequest) []string {
			if t, _ := req.Options["title"].(string); strings.TrimSpace(t) == "" {
				return []string{"website articles need options.title"}
			}
			return nil
		},
		New: func(c map[string]string, _ func(map[string]string) error, hc *http.Client) (providers.Client, error) {
			if c["base_url"] == "" || c["token"] == "" {
				return nil, errors.New("website: base_url and token are required")
			}
			u, err := url.Parse(c["base_url"])
			if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
				return nil, fmt.Errorf("website: invalid base_url %q", c["base_url"])
			}
			return &Client{Base: strings.TrimRight(c["base_url"], "/") + "/relay/v1", Token: c["token"], HC: hc}, nil
		},
	})
}

type Client struct {
	Base  string
	Token string
	HC    *http.Client
}

func (c *Client) do(ctx context.Context, method, path string, ct string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/json")
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	resp, err := c.HC.Do(req)
	if err != nil {
		return err
	}
	b, err := providers.ReadBody(resp)
	if err != nil {
		return err
	}
	if out != nil && len(bytes.TrimSpace(b)) > 0 {
		return json.Unmarshal(b, out)
	}
	return nil
}

func (c *Client) json(ctx context.Context, method, path string, in, out any) error {
	var rd io.Reader
	ct := ""
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		rd, ct = bytes.NewReader(b), "application/json"
	}
	return c.do(ctx, method, path, ct, rd, out)
}

type Info struct {
	Name         string   `json:"name"`
	URL          string   `json:"url"`
	Version      string   `json:"version"`
	Capabilities []string `json:"capabilities"`
}

func (c *Client) Info(ctx context.Context) (*Info, error) {
	var i Info
	return &i, c.json(ctx, "GET", "/info", nil, &i)
}

func (c *Client) has(ctx context.Context, cap string) bool {
	i, err := c.Info(ctx)
	if err != nil {
		return false
	}
	for _, x := range i.Capabilities {
		if x == cap {
			return true
		}
	}
	return false
}

func (c *Client) Identify(ctx context.Context) (*providers.Identity, error) {
	i, err := c.Info(ctx)
	if err != nil {
		return nil, err
	}
	if i.Version != "" && i.Version != "1" {
		return nil, fmt.Errorf("website: unsupported protocol version %q", i.Version)
	}
	return &providers.Identity{RemoteUserID: i.URL, Handle: i.URL, Name: i.Name}, nil
}

type mediaOut struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

func (c *Client) upload(ctx context.Context, m providers.MediaFile) (*mediaOut, error) {
	f, err := m.Open()
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("alt", m.Alt)
	fw, err := w.CreateFormFile("file", m.Filename)
	if err != nil {
		return nil, err
	}
	if _, err := io.Copy(fw, f); err != nil {
		return nil, err
	}
	w.Close()
	var out mediaOut
	return &out, c.do(ctx, "POST", "/media", w.FormDataContentType(), &buf, &out)
}

type article struct {
	Title        string         `json:"title"`
	Slug         string         `json:"slug,omitempty"`
	BodyMarkdown string         `json:"body_markdown"`
	Excerpt      string         `json:"excerpt,omitempty"`
	Tags         []string       `json:"tags,omitempty"`
	Lang         string         `json:"lang,omitempty"`
	CoverURL     string         `json:"cover_url,omitempty"`
	Media        []mediaOut     `json:"media,omitempty"`
	Status       string         `json:"status"`
	PublishedAt  string         `json:"published_at,omitempty"`
	Extra        map[string]any `json:"extra,omitempty"`
}

func optStr(o map[string]any, k string) string {
	v, _ := o[k].(string)
	return v
}

func (c *Client) Publish(ctx context.Context, req providers.PublishRequest) (*providers.PublishResult, error) {
	o := req.Options
	if o == nil {
		o = map[string]any{}
	}
	a := article{
		Title: optStr(o, "title"), Slug: optStr(o, "slug"), Excerpt: optStr(o, "excerpt"), Lang: optStr(o, "lang"),
		BodyMarkdown: strings.Join(req.Parts, "\n\n"), Status: optStr(o, "status"),
		PublishedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if a.Title == "" {
		return nil, providers.Invalid("website: options.title is required")
	}
	if a.Status == "" {
		a.Status = "published"
	}
	if tags, ok := o["tags"].([]any); ok {
		for _, t := range tags {
			if s, ok := t.(string); ok {
				a.Tags = append(a.Tags, s)
			}
		}
	}
	if ex, ok := o["extra"].(map[string]any); ok {
		a.Extra = ex
	}
	cover := optStr(o, "cover")
	for _, m := range req.Media {
		up, err := c.upload(ctx, m)
		if err != nil {
			return nil, fmt.Errorf("website: upload %s: %w", m.Filename, err)
		}
		if m.ID == cover || (cover == "" && a.CoverURL == "") {
			a.CoverURL = up.URL
		}
		a.Media = append(a.Media, *up)
	}
	var out struct {
		ID  string `json:"id"`
		URL string `json:"url"`
	}
	if err := c.json(ctx, "POST", "/posts", a, &out); err != nil {
		return nil, err
	}
	return &providers.PublishResult{RemoteID: out.ID, RemoteIDs: []string{out.ID}, URL: out.URL}, nil
}

func (c *Client) Metrics(ctx context.Context, ids []string) (map[string]map[string]int64, error) {
	var out struct {
		Metrics map[string]map[string]int64 `json:"metrics"`
	}
	if err := c.json(ctx, "GET", "/metrics?ids="+url.QueryEscape(strings.Join(ids, ",")), nil, &out); err != nil {
		var he *providers.HTTPError
		if errors.As(err, &he) && he.Status == 404 {
			return nil, providers.ErrUnsupported
		}
		return nil, err
	}
	return out.Metrics, nil
}

type commentOut struct {
	ID        string     `json:"id"`
	Author    string     `json:"author"`
	Text      string     `json:"text"`
	URL       string     `json:"url"`
	CreatedAt *time.Time `json:"created_at"`
}

func (c *Client) Comments(ctx context.Context, remoteID string) ([]providers.Comment, error) {
	if !c.has(ctx, "comments") {
		return nil, providers.ErrUnsupported
	}
	var out struct {
		Items []commentOut `json:"items"`
	}
	if err := c.json(ctx, "GET", "/posts/"+url.PathEscape(remoteID)+"/comments", nil, &out); err != nil {
		return nil, err
	}
	res := make([]providers.Comment, 0, len(out.Items))
	for _, it := range out.Items {
		res = append(res, providers.Comment{RemoteID: it.ID, Author: it.Author, Text: it.Text, URL: it.URL, CreatedAt: it.CreatedAt})
	}
	return res, nil
}

func (c *Client) Reply(ctx context.Context, to, text string) (*providers.PublishResult, error) {
	var out struct {
		ID  string `json:"id"`
		URL string `json:"url"`
	}
	if err := c.json(ctx, "POST", "/comments/"+url.PathEscape(to)+"/replies", map[string]string{"text": text}, &out); err != nil {
		return nil, err
	}
	return &providers.PublishResult{RemoteID: out.ID, RemoteIDs: []string{out.ID}, URL: out.URL}, nil
}

func (c *Client) History(ctx context.Context, since time.Time, max int) ([]providers.HistoryItem, error) {
	if max <= 0 {
		max = 10000
	}
	var res []providers.HistoryItem
	cursor := ""
	for len(res) < max {
		q := url.Values{"limit": {"100"}}
		if !since.IsZero() {
			q.Set("since", since.UTC().Format(time.RFC3339))
		}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var out struct {
			Items []struct {
				ID          string           `json:"id"`
				URL         string           `json:"url"`
				Title       string           `json:"title"`
				Excerpt     string           `json:"excerpt"`
				PublishedAt time.Time        `json:"published_at"`
				Metrics     map[string]int64 `json:"metrics"`
			} `json:"items"`
			NextCursor string `json:"next_cursor"`
		}
		if err := c.json(ctx, "GET", "/posts?"+q.Encode(), nil, &out); err != nil {
			return res, err
		}
		for _, it := range out.Items {
			text := it.Title
			if it.Excerpt != "" {
				text += "\n\n" + it.Excerpt
			}
			res = append(res, providers.HistoryItem{RemoteID: it.ID, Text: text, URL: it.URL, CreatedAt: it.PublishedAt, Metrics: it.Metrics})
		}
		if out.NextCursor == "" || len(out.Items) == 0 {
			break
		}
		cursor = out.NextCursor
	}
	if len(res) > max {
		res = res[:max]
	}
	return res, nil
}
