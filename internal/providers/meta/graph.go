// Package meta holds the Facebook Page and Instagram connectors (Meta Graph API).
package meta

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"

	"github.com/Seeyko/relay/internal/providers"
)

const DefaultVersion = "v23.0"

// graph is a tiny Graph API client authenticated with one access token.
type graph struct {
	Base  string // e.g. https://graph.facebook.com/v23.0
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
	if form == nil {
		form = url.Values{}
	}
	form.Set("access_token", g.Token)
	req, err := http.NewRequestWithContext(ctx, "POST", g.Base+path, strings.NewReader(form.Encode()))
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
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fields["access_token"] = g.Token
	for k, v := range fields {
		_ = w.WriteField(k, v)
	}
	fw, err := w.CreateFormFile(field, m.Filename)
	if err != nil {
		return err
	}
	if _, err := io.Copy(fw, f); err != nil {
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
		return err
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

type idResp struct {
	ID     string `json:"id"`
	PostID string `json:"post_id"`
}
