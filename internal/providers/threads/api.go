package threads

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/Seeyko/relay/internal/providers"
)

// api is a tiny Threads Graph client authenticated with one access token.
type api struct {
	Base  string // e.g. https://graph.threads.net/v1.0
	Token string
	HC    *http.Client
}

func (a *api) get(ctx context.Context, path string, q url.Values, out any) error {
	v := url.Values{}
	for k, x := range q {
		v[k] = x
	}
	v.Set("access_token", a.Token)
	req, err := http.NewRequestWithContext(ctx, "GET", a.Base+path+"?"+v.Encode(), nil)
	if err != nil {
		return err
	}
	return a.send(req, out)
}

func (a *api) post(ctx context.Context, path string, form url.Values, out any) error {
	v := url.Values{}
	for k, x := range form {
		v[k] = x
	}
	v.Set("access_token", a.Token)
	req, err := http.NewRequestWithContext(ctx, "POST", a.Base+path, strings.NewReader(v.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return a.send(req, out)
}

func (a *api) send(req *http.Request, out any) error {
	resp, err := a.HC.Do(req)
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

type errBody struct {
	Error struct {
		Message     string `json:"message"`
		Code        int    `json:"code"`
		Subcode     int    `json:"error_subcode"`
		UserMsg     string `json:"error_user_msg"`
		IsTransient bool   `json:"is_transient"`
	} `json:"error"`
}

// classify maps Graph error codes onto the HTTP status that providers.Retryable understands.
// Threads answers most errors with HTTP 400, rate limits included: throttling becomes 429,
// transient errors 503, and auth/permission/content errors stay non-retryable.
func classify(he *providers.HTTPError) error {
	var e errBody
	if json.Unmarshal([]byte(he.Body), &e) != nil || e.Error.Code == 0 {
		return he
	}
	switch c := e.Error.Code; {
	case c == 4 || c == 17 || c == 32 || c == 613 || c == 80001:
		he.Status = http.StatusTooManyRequests
	case c == 190 || c == 102 || c == 10 || c == 100 || c == 368 || (c >= 200 && c <= 299):
		if he.Status >= 500 || he.Status == http.StatusTooManyRequests {
			he.Status = http.StatusBadRequest
		}
	case c == 1 || c == 2 || e.Error.IsTransient:
		if he.Status < 500 {
			he.Status = http.StatusServiceUnavailable
		}
	}
	return he
}

// graphError decodes a Graph error body.
func graphError(err error) (code, subcode int, msg string) {
	var he *providers.HTTPError
	if !errors.As(err, &he) {
		return 0, 0, ""
	}
	var e errBody
	_ = json.Unmarshal([]byte(he.Body), &e)
	msg = e.Error.UserMsg
	if msg == "" {
		msg = e.Error.Message
	}
	return e.Error.Code, e.Error.Subcode, msg
}
