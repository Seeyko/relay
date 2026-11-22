// Package website connects any site that implements the Relay Site Protocol (docs/SITE_PROTOCOL.md).
// One provider for every site: your own sites, client sites, anything that speaks the protocol.
// The client speaks v2 when the site serves /relay/v2/info with version "2", else v1.
package website

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Seeyko/relay/internal/providers"
)

func init() {
	providers.Register(providers.Factory{
		Name: "website",
		Description: "Any website implementing the Relay Site Protocol v1 or v2 (articles, drafts, edit, translations, taxonomy, media, metrics, comments, site extensions). " +
			"Post body = Markdown (or HTML with body_format html); options: title (required), slug, excerpt, tags, category, lang, cover (media id), author_id, " +
			"status (published|draft|scheduled), published_at, body_format (markdown|html), extra; or article_id alone to publish an article already on the site " +
			"(site_publish_article scheduled_at). Manage the site's articles with the site_* tools.",
		CredentialFields: []string{"base_url", "token"},
		Validate: func(req providers.PublishRequest) []string {
			var out []string
			if id, _ := req.Options["article_id"].(string); id != "" {
				// Publication of an article that already exists on the site: its content lives there.
				return nil
			}
			if t, _ := req.Options["title"].(string); strings.TrimSpace(t) == "" {
				out = append(out, "website articles need options.title")
			}
			switch st, _ := req.Options["status"].(string); st {
			case "", "published", "draft":
			case "scheduled":
				if pa, _ := req.Options["published_at"].(string); pa == "" {
					out = append(out, "website: status scheduled needs options.published_at")
				}
			default:
				out = append(out, fmt.Sprintf("website: options.status must be published, draft or scheduled, not %q", st))
			}
			switch bf, _ := req.Options["body_format"].(string); bf {
			case "", "markdown", "html":
			default:
				out = append(out, fmt.Sprintf("website: options.body_format must be markdown or html, not %q", bf))
			}
			return out
		},
		New: func(c map[string]string, _ func(map[string]string) error, hc *http.Client) (providers.Client, error) {
			if c["base_url"] == "" || c["token"] == "" {
				return nil, errors.New("website: base_url and token are required")
			}
			return New(c["base_url"], c["token"], hc)
		},
	})
}

// Client talks to one site. Build it with New.
type Client struct {
	// Root is the site's base URL without the /relay/vN suffix.
	Root  string
	Token string
	HC    *http.Client
}

// New checks the base URL and builds a client.
func New(baseURL, token string, hc *http.Client) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("website: invalid base_url %q", baseURL)
	}
	if hc == nil {
		hc = http.DefaultClient
	}
	return &Client{Root: strings.TrimRight(baseURL, "/"), Token: token, HC: hc}, nil
}

// Extension is a site-defined action listed in /info (v2).
type Extension struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Method      string         `json:"method"`
	Path        string         `json:"path"`
	InputSchema map[string]any `json:"input_schema"`
	Admin       bool           `json:"admin"`
	// ReadOnly defaults to true for GET and false otherwise.
	ReadOnly *bool `json:"read_only,omitempty"`
}

// IsReadOnly says whether the action changes nothing on the site.
func (e Extension) IsReadOnly() bool {
	if e.ReadOnly != nil {
		return *e.ReadOnly
	}
	return strings.EqualFold(e.Method, "GET")
}

type Info struct {
	Name         string      `json:"name"`
	URL          string      `json:"url"`
	Version      string      `json:"version"`
	Capabilities []string    `json:"capabilities"`
	Languages    []string    `json:"languages,omitempty"`
	Extensions   []Extension `json:"extensions,omitempty"`
}

// Has says whether the site declares a capability.
func (i *Info) Has(cap string) bool {
	for _, x := range i.Capabilities {
		if x == cap {
			return true
		}
	}
	return false
}

// V2 says whether the site speaks protocol v2.
func (i *Info) V2() bool { return i.Version == "2" }

// Extension finds an extension by name.
func (i *Info) Extension(name string) (Extension, bool) {
	for _, e := range i.Extensions {
		if e.Name == name {
			return e, true
		}
	}
	return Extension{}, false
}

// InfoTTL is how long /info is cached per site and token.
var InfoTTL = 5 * time.Minute

type cached struct {
	info *Info
	at   time.Time
}

var infoCache = struct {
	sync.Mutex
	m map[string]cached
}{m: map[string]cached{}}

func (c *Client) cacheKey() string {
	h := sha256.Sum256([]byte(c.Token))
	return c.Root + "\n" + hex.EncodeToString(h[:])
}

// SiteError is a non-2xx answer of the site, with the message of its {"error": ...} body.
// It unwraps to the providers.HTTPError, so retries work as for any network.
type SiteError struct {
	Status  int
	Message string
	err     *providers.HTTPError
}

func (e *SiteError) Error() string {
	return fmt.Sprintf("site answered %d: %s", e.Status, e.Message)
}

func (e *SiteError) Unwrap() error { return e.err }

func siteError(err error) error {
	var he *providers.HTTPError
	if !errors.As(err, &he) {
		return err
	}
	msg := strings.TrimSpace(he.Body)
	var body struct {
		Error string `json:"error"`
	}
	if json.Unmarshal([]byte(he.Body), &body) == nil && body.Error != "" {
		msg = body.Error
	}
	if len(msg) > 500 {
		msg = msg[:500] + "..."
	}
	if msg == "" {
		msg = http.StatusText(he.Status)
	}
	return &SiteError{Status: he.Status, Message: msg, err: he}
}

// StatusOf returns the HTTP status of a site error, 0 for other errors.
func StatusOf(err error) int {
	var he *providers.HTTPError
	if errors.As(err, &he) {
		return he.Status
	}
	return 0
}

// CapError says the site lacks a capability (or speaks v1, which has no content API).
type CapError struct {
	Site string
	Cap  string
	V1   bool
}

func (e *CapError) Error() string {
	if e.V1 {
		return fmt.Sprintf("site %s speaks Relay Site Protocol v1: %s needs v2 (docs/SITE_PROTOCOL.md)", e.Site, e.Cap)
	}
	return fmt.Sprintf("site %s does not support %q (not in the capabilities of its /info)", e.Site, e.Cap)
}

func (e *CapError) Unwrap() error { return providers.ErrUnsupported }

func (c *Client) raw(ctx context.Context, method, fullURL, ct string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, fullURL, body)
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
		return siteError(err)
	}
	if out == nil || len(bytes.TrimSpace(b)) == 0 {
		return nil
	}
	if rm, ok := out.(*json.RawMessage); ok {
		*rm = append((*rm)[:0], b...)
		return nil
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("website: invalid JSON from %s %s: %w", method, fullURL, err)
	}
	return nil
}

// base is the protocol root of the site (…/relay/v1 or …/relay/v2).
func (c *Client) base(ctx context.Context) (string, *Info, error) {
	i, err := c.Info(ctx)
	if err != nil {
		return "", nil, err
	}
	if i.V2() {
		return c.Root + "/relay/v2", i, nil
	}
	return c.Root + "/relay/v1", i, nil
}

func (c *Client) do(ctx context.Context, method, path, ct string, body io.Reader, out any) error {
	b, _, err := c.base(ctx)
	if err != nil {
		return err
	}
	return c.raw(ctx, method, b+path, ct, body, out)
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

// Info returns the site's /info (cached for InfoTTL), detecting the protocol version.
func (c *Client) Info(ctx context.Context) (*Info, error) { return c.info(ctx, false) }

// Refresh reads /info again, ignoring the cache.
func (c *Client) Refresh(ctx context.Context) (*Info, error) { return c.info(ctx, true) }

func (c *Client) info(ctx context.Context, refresh bool) (*Info, error) {
	key := c.cacheKey()
	if !refresh {
		infoCache.Lock()
		e, ok := infoCache.m[key]
		infoCache.Unlock()
		if ok && time.Since(e.at) < InfoTTL {
			return e.info, nil
		}
	}
	i, err := c.detect(ctx)
	if err != nil {
		return nil, err
	}
	infoCache.Lock()
	infoCache.m[key] = cached{info: i, at: time.Now()}
	infoCache.Unlock()
	return i, nil
}

func (c *Client) detect(ctx context.Context) (*Info, error) {
	var v2 Info
	err2 := c.raw(ctx, "GET", c.Root+"/relay/v2/info", "", nil, &v2)
	if err2 == nil && v2.Version == "2" {
		return &v2, nil
	}
	var v1 Info
	err1 := c.raw(ctx, "GET", c.Root+"/relay/v1/info", "", nil, &v1)
	if err1 == nil {
		if v1.Version != "" && v1.Version != "1" {
			return nil, fmt.Errorf("website: /relay/v1/info announces version %q: serve version 2 under /relay/v2", v1.Version)
		}
		return &v1, nil
	}
	if err2 == nil { // v2 path answered, but not with version 2
		return nil, fmt.Errorf("website: unsupported protocol version %q at /relay/v2/info", v2.Version)
	}
	// Prefer the v2 error unless it is a plain "not here".
	if s := StatusOf(err2); s != 0 && s != 404 && s != 405 {
		return nil, err2
	}
	return nil, err1
}

func (c *Client) has(ctx context.Context, cap string) bool {
	i, err := c.Info(ctx)
	if err != nil {
		return false
	}
	return i.Has(cap)
}

func (c *Client) Identify(ctx context.Context) (*providers.Identity, error) {
	i, err := c.Refresh(ctx)
	if err != nil {
		return nil, err
	}
	if !i.V2() && i.Version != "" && i.Version != "1" {
		return nil, fmt.Errorf("website: unsupported protocol version %q", i.Version)
	}
	if i.URL == "" {
		return nil, errors.New("website: /info has no url")
	}
	return &providers.Identity{RemoteUserID: i.URL, Handle: i.URL, Name: i.Name}, nil
}

// Media is a file stored by the site.
type Media struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

// UploadFile sends a file with POST /media (multipart).
func (c *Client) UploadFile(ctx context.Context, m providers.MediaFile) (*Media, error) {
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
	var out Media
	return &out, c.do(ctx, "POST", "/media", w.FormDataContentType(), &buf, &out)
}

type article struct {
	Title        string         `json:"title"`
	Slug         string         `json:"slug,omitempty"`
	BodyMarkdown string         `json:"body_markdown,omitempty"`
	BodyHTML     string         `json:"body_html,omitempty"`
	Excerpt      string         `json:"excerpt,omitempty"`
	Tags         []string       `json:"tags,omitempty"`
	Category     string         `json:"category,omitempty"`
	Lang         string         `json:"lang,omitempty"`
	CoverURL     string         `json:"cover_url,omitempty"`
	AuthorID     string         `json:"author_id,omitempty"`
	Media        []Media        `json:"media,omitempty"` // v1 only
	Status       string         `json:"status"`
	PublishedAt  string         `json:"published_at,omitempty"`
	Extra        map[string]any `json:"extra,omitempty"`
}

func optStr(o map[string]any, k string) string {
	switch v := o[k].(type) {
	case string:
		return v
	case float64:
		return fmt.Sprint(v)
	}
	return ""
}

func optStrs(o map[string]any, k string) []string {
	var out []string
	switch v := o[k].(type) {
	case []any:
		for _, t := range v {
			if s, ok := t.(string); ok {
				out = append(out, s)
			}
		}
	case []string:
		out = v
	}
	return out
}

func (c *Client) Publish(ctx context.Context, req providers.PublishRequest) (*providers.PublishResult, error) {
	i, err := c.Info(ctx)
	if err != nil {
		return nil, err
	}
	o := req.Options
	if o == nil {
		o = map[string]any{}
	}
	if id := optStr(o, "article_id"); id != "" {
		return c.publishExisting(ctx, i, id)
	}
	body := strings.Join(req.Parts, "\n\n")
	a := article{
		Title: optStr(o, "title"), Slug: optStr(o, "slug"), Excerpt: optStr(o, "excerpt"), Lang: optStr(o, "lang"),
		Status: optStr(o, "status"), Tags: optStrs(o, "tags"), PublishedAt: optStr(o, "published_at"),
	}
	if a.Title == "" {
		return nil, providers.Invalid("website: options.title is required")
	}
	if a.Status == "" {
		a.Status = "published"
	}
	if ex, ok := o["extra"].(map[string]any); ok {
		a.Extra = ex
	}
	if i.V2() {
		switch a.Status {
		case "published":
		case "draft":
			if !i.Has("drafts") {
				return nil, providers.Invalid("website: this site does not accept drafts (no \"drafts\" capability)")
			}
		case "scheduled":
			if !i.Has("schedule") {
				return nil, providers.Invalid("website: this site cannot schedule articles (no \"schedule\" capability): let Relay schedule the post instead")
			}
			if a.PublishedAt == "" {
				return nil, providers.Invalid("website: status scheduled needs options.published_at")
			}
		default:
			return nil, providers.Invalid(fmt.Sprintf("website: unknown status %q", a.Status))
		}
		if len(req.Media) > 0 && !i.Has("media") {
			return nil, providers.Invalid("website: this site does not accept media (no \"media\" capability)")
		}
		a.Category, a.AuthorID = optStr(o, "category"), optStr(o, "author_id")
		if optStr(o, "body_format") == "html" {
			a.BodyHTML = body
		} else {
			a.BodyMarkdown = body
		}
	} else {
		a.BodyMarkdown = body
		if a.PublishedAt == "" {
			a.PublishedAt = time.Now().UTC().Format(time.RFC3339)
		}
	}
	cover := optStr(o, "cover")
	for _, m := range req.Media {
		up, err := c.UploadFile(ctx, m)
		if err != nil {
			return nil, fmt.Errorf("website: upload %s: %w", m.Filename, err)
		}
		if m.ID == cover || (cover == "" && a.CoverURL == "") {
			a.CoverURL = up.URL
		}
		if !i.V2() {
			a.Media = append(a.Media, *up)
		}
	}
	var out struct {
		ID  string `json:"id"`
		URL string `json:"url"`
	}
	if err := c.json(ctx, "POST", "/posts", a, &out); err != nil {
		return nil, err
	}
	if out.ID == "" {
		return nil, errors.New("website: POST /posts answered without an id")
	}
	return &providers.PublishResult{RemoteID: out.ID, RemoteIDs: []string{out.ID}, URL: out.URL}, nil
}

// publishExisting publishes an article that is already on the site (options.article_id: a draft scheduled
// in Relay with site_publish_article scheduled_at). An article already published is not published again,
// so a retry after a crash is safe.
func (c *Client) publishExisting(ctx context.Context, i *Info, id string) (*providers.PublishResult, error) {
	if !i.V2() {
		return nil, providers.Invalid("website: options.article_id needs a Site Protocol v2 site")
	}
	raw, err := c.GetArticle(ctx, id)
	var se *SiteError
	if errors.As(err, &se) && se.Status == 404 {
		return nil, providers.Invalid(fmt.Sprintf("website: article %s is no longer on the site", id))
	}
	if err != nil {
		return nil, err
	}
	var a struct {
		ID     string `json:"id"`
		URL    string `json:"url"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("website: invalid article JSON: %w", err)
	}
	if a.Status != "published" {
		if raw, err = c.PublishArticle(ctx, id, ""); err != nil {
			return nil, err
		}
		var p struct {
			URL string `json:"url"`
		}
		if json.Unmarshal(raw, &p) == nil && p.URL != "" {
			a.URL = p.URL
		}
	}
	if a.ID == "" {
		a.ID = id
	}
	return &providers.PublishResult{RemoteID: a.ID, RemoteIDs: []string{a.ID}, URL: a.URL}, nil
}

func (c *Client) Metrics(ctx context.Context, ids []string) (map[string]map[string]int64, error) {
	if i, err := c.Info(ctx); err == nil && i.V2() && !i.Has("metrics") {
		return nil, providers.ErrUnsupported
	}
	var out struct {
		Metrics map[string]map[string]int64 `json:"metrics"`
	}
	if err := c.json(ctx, "GET", "/metrics?ids="+url.QueryEscape(strings.Join(ids, ",")), nil, &out); err != nil {
		if StatusOf(err) == 404 {
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
	i, err := c.Info(ctx)
	if err != nil {
		return nil, err
	}
	if max <= 0 {
		max = 10000
	}
	var res []providers.HistoryItem
	cursor := ""
	for len(res) < max {
		q := url.Values{"limit": {"100"}}
		if i.V2() {
			q.Set("status", "published")
		}
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
		if out.NextCursor == "" || len(out.Items) == 0 || out.NextCursor == cursor {
			break
		}
		cursor = out.NextCursor
	}
	if len(res) > max {
		res = res[:max]
	}
	return res, nil
}
