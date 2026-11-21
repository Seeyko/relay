// Package email sends Relay's account emails (invitations, sign-in links, approval codes) through Resend.
//
// The messages are short HTML with a plain-text part, in English or French, without images, tracking pixels
// or third-party content. They never contain an API token.
package email

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/mail"
	"strings"
	"time"
)

// Message is one email.
type Message struct {
	To      string
	Subject string
	HTML    string
	Text    string
}

// Sender delivers a Message.
type Sender interface {
	Send(ctx context.Context, m Message) error
}

// DefaultEndpoint is Resend's send API.
const DefaultEndpoint = "https://api.resend.com/emails"

// Resend sends through the Resend API (POST /emails).
type Resend struct {
	APIKey   string
	From     string // e.g. "RideMyPark <no-reply@ridemypark.com>"
	Endpoint string // DefaultEndpoint when empty (tests point it at a fake server)
	HTTP     *http.Client
}

// NewResend checks the settings. Both the key and the sender address are required.
func NewResend(apiKey, from string) (*Resend, error) {
	if apiKey == "" || from == "" {
		return nil, errors.New("email: RELAY_RESEND_API_KEY and RELAY_EMAIL_FROM are both required")
	}
	if _, err := mail.ParseAddress(from); err != nil {
		return nil, fmt.Errorf("email: RELAY_EMAIL_FROM: %w", err)
	}
	return &Resend{APIKey: apiKey, From: from, HTTP: &http.Client{Timeout: 20 * time.Second}}, nil
}

func (r *Resend) Send(ctx context.Context, m Message) error {
	body, _ := json.Marshal(map[string]any{
		"from": r.From, "to": []string{m.To}, "subject": m.Subject, "html": m.HTML, "text": m.Text,
	})
	ep := r.Endpoint
	if ep == "" {
		ep = DefaultEndpoint
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ep, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+r.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "relay")
	hc := r.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(b, &e)
		if e.Message == "" {
			e.Message = strings.TrimSpace(string(b))
		}
		return fmt.Errorf("resend: %d %s", resp.StatusCode, e.Message)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// ---------- messages ----------

// Lang picks "fr" or "en" from an explicit language or an Accept-Language header. English is the default.
func Lang(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	for _, part := range strings.Split(v, ",") {
		tag := strings.TrimSpace(strings.SplitN(part, ";", 2)[0])
		if strings.HasPrefix(tag, "fr") {
			return "fr"
		}
		if strings.HasPrefix(tag, "en") {
			return "en"
		}
	}
	return "en"
}

var roleNames = map[string]map[string]string{
	"en": {"owner": "owner", "admin": "admin", "editor": "editor", "viewer": "viewer"},
	"fr": {"owner": "propriétaire", "admin": "admin", "editor": "éditeur", "viewer": "lecteur"},
}

type view struct {
	Lang   string
	Title  string
	Lines  []string
	Button string
	Link   string
	Code   string
	Foot   string
}

var page = template.Must(template.New("mail").Parse(`<!doctype html>
<html lang="{{.Lang}}"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>{{.Title}}</title></head>
<body style="margin:0;padding:0;background:#f5f5f4;">
<table role="presentation" width="100%" cellspacing="0" cellpadding="0" style="background:#f5f5f4;"><tr><td align="center" style="padding:32px 16px;">
<table role="presentation" width="100%" cellspacing="0" cellpadding="0" style="max-width:440px;background:#ffffff;border-radius:14px;">
<tr><td style="padding:32px 32px 8px;font:600 20px/1.3 -apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,Helvetica,Arial,sans-serif;color:#1c1917;">{{.Title}}</td></tr>
{{range .Lines}}<tr><td style="padding:8px 32px;font:15px/1.5 -apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,Helvetica,Arial,sans-serif;color:#44403c;">{{.}}</td></tr>
{{end}}{{if .Code}}<tr><td style="padding:16px 32px;font:600 32px/1 ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;letter-spacing:6px;color:#1c1917;">{{.Code}}</td></tr>
{{end}}{{if .Link}}<tr><td style="padding:20px 32px;"><a href="{{.Link}}" style="display:inline-block;background:#1c1917;color:#ffffff;text-decoration:none;font:600 15px/1 -apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,Helvetica,Arial,sans-serif;padding:14px 22px;border-radius:999px;">{{.Button}}</a></td></tr>
<tr><td style="padding:0 32px 8px;font:12px/1.5 -apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,Helvetica,Arial,sans-serif;color:#78716c;word-break:break-all;">{{.Link}}</td></tr>
{{end}}<tr><td style="padding:16px 32px 32px;font:12px/1.5 -apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,Helvetica,Arial,sans-serif;color:#a8a29e;">{{.Foot}}</td></tr>
</table></td></tr></table>
</body></html>`))

func render(to string, v view) Message {
	var b strings.Builder
	_ = page.Execute(&b, v)
	var t strings.Builder
	t.WriteString(v.Title + "\n\n")
	for _, l := range v.Lines {
		t.WriteString(l + "\n")
	}
	if v.Code != "" {
		t.WriteString("\n" + v.Code + "\n")
	}
	if v.Link != "" {
		t.WriteString("\n" + v.Button + ": " + v.Link + "\n")
	}
	t.WriteString("\n" + v.Foot + "\n")
	return Message{To: to, Subject: v.Title, HTML: b.String(), Text: t.String()}
}

// Invite is the invitation to join the team. inviter is a name or an address ("" when sent with the API token).
func Invite(lang, to, inviter, role, link string) Message {
	lang = Lang(lang)
	rn := roleNames[lang][role]
	if rn == "" {
		rn = role
	}
	if lang == "fr" {
		who := "Vous êtes invité"
		if inviter != "" {
			who = inviter + " vous invite"
		}
		return render(to, view{Lang: lang, Title: "Invitation à Relay", Lines: []string{who + " à rejoindre Relay en tant que " + rn + "."},
			Button: "Accepter", Link: link, Foot: "Ce lien expire dans 7 jours. Si vous ne vous attendiez pas à cette invitation, ignorez cet email."})
	}
	who := "You are invited"
	if inviter != "" {
		who = inviter + " invited you"
	}
	return render(to, view{Lang: lang, Title: "Invitation to Relay", Lines: []string{who + " to join Relay as " + rn + "."},
		Button: "Accept", Link: link, Foot: "This link expires in 7 days. If you did not expect this invitation, ignore this email."})
}

// SignIn is the sign-in link.
func SignIn(lang, to, link string) Message {
	if Lang(lang) == "fr" {
		return render(to, view{Lang: "fr", Title: "Connexion à Relay", Lines: []string{"Cliquez pour vous connecter."},
			Button: "Se connecter", Link: link, Foot: "Ce lien expire dans 15 minutes et ne sert qu'une fois. Si vous n'avez rien demandé, ignorez cet email."})
	}
	return render(to, view{Lang: "en", Title: "Sign in to Relay", Lines: []string{"Click to sign in."},
		Button: "Sign in", Link: link, Foot: "This link expires in 15 minutes and works once. If you did not ask for it, ignore this email."})
}

// Code is the 6-digit code that confirms an app approval.
func Code(lang, to, app, code string) Message {
	if Lang(lang) == "fr" {
		return render(to, view{Lang: "fr", Title: "Code de confirmation Relay", Lines: []string{"Code pour autoriser " + app + " :"},
			Code: code, Foot: "Ce code expire dans 10 minutes. Si vous n'avez rien demandé, ignorez cet email."})
	}
	return render(to, view{Lang: "en", Title: "Relay confirmation code", Lines: []string{"Code to approve " + app + ":"},
		Code: code, Foot: "This code expires in 10 minutes. If you did not ask for it, ignore this email."})
}
