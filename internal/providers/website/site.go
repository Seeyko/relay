package website

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Seeyko/relay/internal/providers"
)

// The content API of protocol v2 (docs/SITE_PROTOCOL.md). Every method checks the capability it
// needs before calling the site and returns a *CapError when it is missing. Responses are returned
// as raw JSON so the site's own fields (extra, optional ones) reach the agent unchanged.

// Capabilities of protocol v2.
const (
	CapArticles     = "articles"
	CapMedia        = "media"
	CapDrafts       = "drafts"
	CapEdit         = "edit"
	CapDelete       = "delete"
	CapSchedule     = "schedule"
	CapTranslations = "translations"
	CapTaxonomy     = "taxonomy"
	CapAuthors      = "authors"
	CapMetrics      = "metrics"
	CapComments     = "comments"
)

// KnownCapabilities are the capabilities of protocol v2, in the order of the spec.
var KnownCapabilities = []string{CapArticles, CapMedia, CapDrafts, CapEdit, CapDelete, CapSchedule, CapTranslations, CapTaxonomy, CapAuthors, CapMetrics, CapComments}

// Need returns the site's info when it speaks v2 and declares every capability given.
func (c *Client) Need(ctx context.Context, caps ...string) (*Info, error) {
	i, err := c.Info(ctx)
	if err != nil {
		return nil, err
	}
	name := i.Name
	if name == "" {
		name = c.Root
	}
	if !i.V2() {
		what := "the site content API"
		if len(caps) > 0 {
			what = caps[0]
		}
		return nil, &CapError{Site: name, Cap: what, V1: true}
	}
	for _, cp := range caps {
		if !i.Has(cp) {
			return nil, &CapError{Site: name, Cap: cp}
		}
	}
	return i, nil
}

func (c *Client) v2(ctx context.Context, method, path string, in any, caps ...string) (json.RawMessage, error) {
	if _, err := c.Need(ctx, caps...); err != nil {
		return nil, err
	}
	var out json.RawMessage
	if err := c.json(ctx, method, path, in, &out); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func pid(id string) (string, error) {
	if strings.TrimSpace(id) == "" {
		return "", providers.Invalid("article id is required")
	}
	return "/posts/" + url.PathEscape(id), nil
}

// ListQuery filters GET /posts.
type ListQuery struct {
	Status string
	Lang   string
	Since  string
	Limit  int
	Cursor string
}

// ListArticles is GET /posts.
func (c *Client) ListArticles(ctx context.Context, q ListQuery) (json.RawMessage, error) {
	v := url.Values{}
	switch q.Status {
	case "", "draft", "published", "scheduled":
	default:
		return nil, providers.Invalid("status must be draft, published or scheduled")
	}
	for k, s := range map[string]string{"status": q.Status, "lang": q.Lang, "since": q.Since, "cursor": q.Cursor} {
		if s != "" {
			v.Set(k, s)
		}
	}
	if q.Limit > 0 {
		v.Set("limit", fmt.Sprint(min(q.Limit, 100)))
	}
	p := "/posts"
	if len(v) > 0 {
		p += "?" + v.Encode()
	}
	return c.v2(ctx, "GET", p, nil, CapArticles)
}

// GetArticle is GET /posts/{id}.
func (c *Client) GetArticle(ctx context.Context, id string) (json.RawMessage, error) {
	p, err := pid(id)
	if err != nil {
		return nil, err
	}
	return c.v2(ctx, "GET", p, nil, CapArticles)
}

// ArticleStatus reads the status of an article.
func (c *Client) ArticleStatus(ctx context.Context, id string) (string, error) {
	raw, err := c.GetArticle(ctx, id)
	if err != nil {
		return "", err
	}
	var a struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", fmt.Errorf("website: invalid article JSON: %w", err)
	}
	return a.Status, nil
}

func (c *Client) checkLang(i *Info, lang string) error {
	if lang == "" || len(i.Languages) == 0 {
		return nil
	}
	for _, l := range i.Languages {
		if l == lang {
			return nil
		}
	}
	return providers.Invalid(fmt.Sprintf("language %q is not one of the site's languages (%s)", lang, strings.Join(i.Languages, ", ")))
}

func statusCaps(status string) ([]string, error) {
	switch status {
	case "published":
		return []string{CapArticles}, nil
	case "draft":
		return []string{CapArticles, CapDrafts}, nil
	case "scheduled":
		return []string{CapArticles, CapSchedule}, nil
	}
	return nil, providers.Invalid("status must be draft, published or scheduled")
}

// CreateArticle is POST /posts. The body follows the spec; unknown fields are passed through.
func (c *Client) CreateArticle(ctx context.Context, body map[string]any) (json.RawMessage, error) {
	if t, _ := body["title"].(string); strings.TrimSpace(t) == "" {
		return nil, providers.Invalid("title is required")
	}
	status, _ := body["status"].(string)
	caps, err := statusCaps(status)
	if err != nil {
		return nil, err
	}
	if status == "scheduled" {
		if pa, _ := body["published_at"].(string); pa == "" {
			return nil, providers.Invalid("status scheduled needs published_at")
		}
	}
	if body["body_html"] != nil && body["body_markdown"] != nil {
		return nil, providers.Invalid("give body_html or body_markdown, not both")
	}
	if body["author_id"] != nil {
		caps = append(caps, CapAuthors)
	}
	i, err := c.Need(ctx, caps...)
	if err != nil {
		return nil, err
	}
	lang, _ := body["lang"].(string)
	if err := c.checkLang(i, lang); err != nil {
		return nil, err
	}
	return c.v2(ctx, "POST", "/posts", body)
}

// UpdateArticle is PATCH /posts/{id}. status is refused (use PublishArticle / UnpublishArticle).
func (c *Client) UpdateArticle(ctx context.Context, id string, body map[string]any) (json.RawMessage, error) {
	p, err := pid(id)
	if err != nil {
		return nil, err
	}
	if _, ok := body["status"]; ok {
		return nil, providers.Invalid("status cannot be changed by an edit: publish or unpublish the article")
	}
	if len(body) == 0 {
		return nil, providers.Invalid("nothing to change")
	}
	if body["body_html"] != nil && body["body_markdown"] != nil {
		return nil, providers.Invalid("give body_html or body_markdown, not both")
	}
	caps := []string{CapEdit}
	if body["author_id"] != nil {
		caps = append(caps, CapAuthors)
	}
	i, err := c.Need(ctx, caps...)
	if err != nil {
		return nil, err
	}
	if lang, _ := body["lang"].(string); lang != "" {
		if err := c.checkLang(i, lang); err != nil {
			return nil, err
		}
	}
	return c.v2(ctx, "PATCH", p, body)
}

// PublishArticle is POST /posts/{id}/publish. A future publishedAt schedules the article.
func (c *Client) PublishArticle(ctx context.Context, id, publishedAt string) (json.RawMessage, error) {
	p, err := pid(id)
	if err != nil {
		return nil, err
	}
	caps := []string{CapDrafts}
	var body any
	if publishedAt != "" {
		t, err := time.Parse(time.RFC3339, publishedAt)
		if err != nil {
			return nil, providers.Invalid("published_at must be RFC 3339 with an offset")
		}
		if t.After(time.Now()) {
			caps = append(caps, CapSchedule)
		}
		body = map[string]string{"published_at": publishedAt}
	}
	return c.v2(ctx, "POST", p+"/publish", body, caps...)
}

// UnpublishArticle is POST /posts/{id}/unpublish.
func (c *Client) UnpublishArticle(ctx context.Context, id string) (json.RawMessage, error) {
	p, err := pid(id)
	if err != nil {
		return nil, err
	}
	return c.v2(ctx, "POST", p+"/unpublish", nil, CapDrafts)
}

// DeleteArticle is DELETE /posts/{id}.
func (c *Client) DeleteArticle(ctx context.Context, id string) error {
	p, err := pid(id)
	if err != nil {
		return err
	}
	_, err = c.v2(ctx, "DELETE", p, nil, CapDelete)
	return err
}

// Taxonomy is GET /taxonomy.
func (c *Client) Taxonomy(ctx context.Context) (json.RawMessage, error) {
	return c.v2(ctx, "GET", "/taxonomy", nil, CapTaxonomy)
}

// Authors is GET /authors.
func (c *Client) Authors(ctx context.Context) (json.RawMessage, error) {
	return c.v2(ctx, "GET", "/authors", nil, CapAuthors)
}

// Translations is GET /posts/{id}/translations.
func (c *Client) Translations(ctx context.Context, id string) (json.RawMessage, error) {
	p, err := pid(id)
	if err != nil {
		return nil, err
	}
	return c.v2(ctx, "GET", p+"/translations", nil, CapTranslations)
}

func (c *Client) translationPath(ctx context.Context, id, lang string) (string, error) {
	p, err := pid(id)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(lang) == "" {
		return "", providers.Invalid("lang is required")
	}
	i, err := c.Need(ctx, CapTranslations)
	if err != nil {
		return "", err
	}
	if err := c.checkLang(i, lang); err != nil {
		return "", err
	}
	return p + "/translations/" + url.PathEscape(lang), nil
}

// SetTranslation is PUT /posts/{id}/translations/{lang} with {title, excerpt, body_html}.
func (c *Client) SetTranslation(ctx context.Context, id, lang string, body map[string]any) (json.RawMessage, error) {
	p, err := c.translationPath(ctx, id, lang)
	if err != nil {
		return nil, err
	}
	if t, _ := body["title"].(string); strings.TrimSpace(t) == "" {
		return nil, providers.Invalid("title is required")
	}
	return c.v2(ctx, "PUT", p, body)
}

// DeleteTranslation is DELETE /posts/{id}/translations/{lang}.
func (c *Client) DeleteTranslation(ctx context.Context, id, lang string) error {
	p, err := c.translationPath(ctx, id, lang)
	if err != nil {
		return err
	}
	_, err = c.v2(ctx, "DELETE", p, nil)
	return err
}

// UploadMedia sends a file (multipart) to POST /media.
func (c *Client) UploadMedia(ctx context.Context, m providers.MediaFile) (*Media, error) {
	if _, err := c.Need(ctx, CapMedia); err != nil {
		return nil, err
	}
	return c.UploadFile(ctx, m)
}

// ImportMedia asks the site to download a file: POST /media {"url", "alt"}.
func (c *Client) ImportMedia(ctx context.Context, rawURL, alt string) (*Media, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, providers.Invalid("url must be an http(s) URL")
	}
	if _, err := c.Need(ctx, CapMedia); err != nil {
		return nil, err
	}
	body := map[string]string{"url": rawURL}
	if alt != "" {
		body["alt"] = alt
	}
	var out Media
	return &out, c.json(ctx, "POST", "/media", body, &out)
}

var extName = regexp.MustCompile(`^[a-z0-9_]+$`)
var placeholder = regexp.MustCompile(`\{([A-Za-z0-9_]+)\}`)

// CheckExtension reports what is wrong with an extension's description ("" when it is fine).
func CheckExtension(e Extension) string {
	switch {
	case !extName.MatchString(e.Name):
		return fmt.Sprintf("name %q must match [a-z0-9_]+", e.Name)
	case !map[string]bool{"GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true}[strings.ToUpper(e.Method)]:
		return fmt.Sprintf("method %q must be GET, POST, PUT, PATCH or DELETE", e.Method)
	case !strings.HasPrefix(e.Path, "/x/"):
		return fmt.Sprintf("path %q must start with /x/", e.Path)
	case strings.ContainsAny(e.Path, "?#") || strings.Contains(e.Path, ".."):
		return fmt.Sprintf("path %q must be a plain path", e.Path)
	case e.InputSchema != nil && e.InputSchema["type"] != nil && e.InputSchema["type"] != "object":
		return "input_schema must be a JSON Schema of type object"
	case strings.TrimSpace(e.Description) == "":
		return "description is empty"
	}
	return ""
}

// CallExtension calls a site-defined action with its arguments.
func (c *Client) CallExtension(ctx context.Context, e Extension, args map[string]any) (json.RawMessage, error) {
	if _, err := c.Need(ctx); err != nil {
		return nil, err
	}
	if msg := CheckExtension(e); msg != "" {
		return nil, fmt.Errorf("website: extension %s is badly described by the site: %s", e.Name, msg)
	}
	if args == nil {
		args = map[string]any{}
	}
	if req, ok := e.InputSchema["required"].([]any); ok {
		var missing []string
		for _, r := range req {
			if k, ok := r.(string); ok {
				if _, ok := args[k]; !ok {
					missing = append(missing, k)
				}
			}
		}
		if len(missing) > 0 {
			return nil, providers.Invalid(fmt.Sprintf("%s: missing required argument(s) %s", e.Name, strings.Join(missing, ", ")))
		}
	}
	rest := map[string]any{}
	for k, v := range args {
		rest[k] = v
	}
	var perr error
	path := placeholder.ReplaceAllStringFunc(e.Path, func(m string) string {
		k := m[1 : len(m)-1]
		v, ok := rest[k]
		if !ok {
			perr = providers.Invalid(fmt.Sprintf("%s: argument %s is required (it is part of the path)", e.Name, k))
			return m
		}
		delete(rest, k)
		s := scalar(v)
		if s == "" || s == "." || s == ".." {
			perr = providers.Invalid(fmt.Sprintf("%s: invalid value for %s", e.Name, k))
			return m
		}
		return url.PathEscape(s)
	})
	if perr != nil {
		return nil, perr
	}
	method := strings.ToUpper(e.Method)
	var out json.RawMessage
	var err error
	if method == "GET" || method == "DELETE" {
		q := url.Values{}
		keys := make([]string, 0, len(rest))
		for k := range rest {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			q.Set(k, scalar(rest[k]))
		}
		if len(q) > 0 {
			path += "?" + q.Encode()
		}
		err = c.json(ctx, method, path, nil, &out)
	} else {
		err = c.json(ctx, method, path, rest, &out)
	}
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// scalar formats an argument for a query string or a path: arrays comma-joined, objects as JSON.
func scalar(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool, int, int64:
		return fmt.Sprint(x)
	case json.Number:
		return x.String()
	case []any:
		parts := make([]string, len(x))
		for i, p := range x {
			parts[i] = scalar(p)
		}
		return strings.Join(parts, ",")
	}
	b, _ := json.Marshal(v)
	return string(b)
}
