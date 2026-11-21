// Package meta holds the Facebook Page and Instagram connectors (Meta Graph API).
package meta

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
	"strconv"
	"strings"

	"github.com/Seeyko/relay/internal/providers"
)

const DefaultVersion = "v26.0"

// commentTarget is the object the parts are commented on instead of creating a post: the first post of a
// thread being resumed (options.thread_root, set by the scheduler), or options.reply_to.
func commentTarget(o map[string]any) string {
	if v, _ := o["thread_root"].(string); v != "" {
		return v
	}
	v, _ := o["reply_to"].(string)
	return v
}

// partial returns res when it holds ids (some comments were posted), else nil.
func partial(res *providers.PublishResult) *providers.PublishResult {
	if len(res.RemoteIDs) == 0 {
		return nil
	}
	res.RemoteID = res.RemoteIDs[0]
	return res
}

// commentProblems checks thread parts 2..n, which Facebook and Instagram publish as comments.
func commentProblems(parts []string, limit int, network string) []string {
	var errs []string
	for i := 1; i < len(parts); i++ {
		if strings.TrimSpace(parts[i]) == "" {
			errs = append(errs, fmt.Sprintf("part %d is empty (parts after the first are posted as comments)", i+1))
		}
		if n := providers.CharCount(parts[i]); n > limit {
			errs = append(errs, fmt.Sprintf("part %d becomes a %s comment, limited to %d characters (got %d)", i+1, network, limit, n))
		}
	}
	return errs
}

// graph is a tiny Graph API client authenticated with one access token.
type graph struct {
	Base  string // e.g. https://graph.facebook.com/v26.0
	Token string
	HC    *http.Client
}

func (g *graph) get(ctx context.Context, path string, q url.Values, out any) error {
	if q == nil {
		q = url.Values{}
	}
	q.Set("access_token", g.Token)
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	req, err := http.NewRequestWithContext(ctx, "GET", g.Base+path+sep+q.Encode(), nil)
	if err != nil {
		return err
	}
	return g.send(req, out)
}

func (g *graph) post(ctx context.Context, path string, form url.Values, out any) error {
	return g.postForm(ctx, "", path, form, out)
}

// postForm is post against another host (base "" means g.Base), e.g. a video upload host.
func (g *graph) postForm(ctx context.Context, base, path string, form url.Values, out any) error {
	if form == nil {
		form = url.Values{}
	}
	form.Set("access_token", g.Token)
	if base == "" {
		base = g.Base
	}
	req, err := http.NewRequestWithContext(ctx, "POST", base+path, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return g.send(req, out)
}

func (g *graph) postFile(ctx context.Context, base, path string, fields map[string]string, field string, m providers.MediaFile, out any) error {
	f, err := m.Open()
	if err != nil {
		return err
	}
	defer f.Close()
	return g.postMultipart(ctx, base, path, fields, field, m.Filename, f, out)
}

// postMultipart sends fields plus one file part read from r (multipart/form-data).
func (g *graph) postMultipart(ctx context.Context, base, path string, fields map[string]string, field, filename string, r io.Reader, out any) error {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("access_token", g.Token)
	for k, v := range fields {
		_ = w.WriteField(k, v)
	}
	fw, err := w.CreateFormFile(field, filename)
	if err != nil {
		return err
	}
	if _, err := io.Copy(fw, r); err != nil {
		return err
	}
	w.Close()
	if base == "" {
		base = g.Base
	}
	req, err := http.NewRequestWithContext(ctx, "POST", base+path, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	return g.send(req, out)
}

func (g *graph) send(req *http.Request, out any) error {
	resp, err := g.HC.Do(req)
	if err != nil {
		return err
	}
	b, err := providers.ReadBody(resp)
	if err != nil {
		var he *providers.HTTPError
		if errors.As(err, &he) {
			return classify(he)
		}
		return err
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

// graphErrBody is the documented Graph API error envelope.
// https://developers.facebook.com/docs/graph-api/guides/error-handling
type graphErrBody struct {
	Error struct {
		Message     string `json:"message"`
		Type        string `json:"type"`
		Code        int    `json:"code"`
		Subcode     int    `json:"error_subcode"`
		IsTransient bool   `json:"is_transient"`
	} `json:"error"`
}

// classify maps Graph error codes onto the HTTP status that providers.Retryable understands.
// Graph answers most errors with HTTP 400, including rate limits, so the status alone is not enough:
// throttling codes become 429, transient ones 503, and auth/permission/content errors are never retried.
func classify(he *providers.HTTPError) error {
	var ge graphErrBody
	if json.Unmarshal([]byte(he.Body), &ge) != nil || ge.Error.Code == 0 {
		return he
	}
	switch c := ge.Error.Code; {
	case c == 4 || c == 17 || c == 32 || c == 341 || c == 613 || c == 80001:
		he.Status = http.StatusTooManyRequests
	case c == 190 || c == 102 || c == 10 || c == 100 || c == 506 || c == 368 || (c >= 200 && c <= 299):
		if he.Status >= 500 || he.Status == http.StatusTooManyRequests {
			he.Status = http.StatusBadRequest
		}
	case c == 1 || c == 2 || ge.Error.IsTransient:
		if he.Status < 500 {
			he.Status = http.StatusServiceUnavailable
		}
	}
	return he
}

// flexInt decodes a JSON number or a numeric string (Graph returns upload offsets as strings).
type flexInt int64

func (n *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*n = 0
		return nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	*n = flexInt(v)
	return err
}

type idResp struct {
	ID     string `json:"id"`
	PostID string `json:"post_id"`
}
