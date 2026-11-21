package email

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLang(t *testing.T) {
	for in, want := range map[string]string{"": "en", "fr": "fr", "fr-FR,fr;q=0.9,en;q=0.8": "fr", "en-US,fr;q=0.5": "en", "de-DE": "en", "FR": "fr"} {
		if got := Lang(in); got != want {
			t.Fatalf("Lang(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMessages(t *testing.T) {
	m := Invite("fr", "a@example.com", `<b>Eve</b>`, "editor", "https://relay.example/ui/accept?token=abc")
	if m.Subject != "Invitation à Relay" || !strings.Contains(m.Text, "éditeur") || !strings.Contains(m.Text, "https://relay.example/ui/accept?token=abc") {
		t.Fatalf("french invitation: %+v", m)
	}
	if strings.Contains(m.HTML, "<b>Eve</b>") || !strings.Contains(m.HTML, "&lt;b&gt;Eve&lt;/b&gt;") {
		t.Fatal("names must be escaped in the HTML")
	}
	if strings.Contains(strings.ToLower(m.HTML), "<img") || strings.Contains(m.HTML, "<script") {
		t.Fatal("no images (tracking pixels) nor scripts")
	}
	if c := Code("en", "a@example.com", "Claude", "012345"); !strings.Contains(c.Text, "012345") || !strings.Contains(c.HTML, "012345") {
		t.Fatalf("code: %+v", c)
	}
	if s := SignIn("", "a@example.com", "https://x/ui/signin?token=t"); s.Subject != "Sign in to Relay" {
		t.Fatalf("default language: %+v", s)
	}
}

func TestResendSend(t *testing.T) {
	var got map[string]any
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&got)
		if got["subject"] == "fail" {
			w.WriteHeader(422)
			_, _ = w.Write([]byte(`{"message":"domain not verified"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"1"}`))
	}))
	defer srv.Close()
	r, err := NewResend("re_k", "Relay <no-reply@example.com>")
	if err != nil {
		t.Fatal(err)
	}
	r.Endpoint = srv.URL
	if err := r.Send(context.Background(), Message{To: "a@example.com", Subject: "hi", HTML: "<p>x</p>", Text: "x"}); err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer re_k" || got["from"] != "Relay <no-reply@example.com>" || got["to"].([]any)[0] != "a@example.com" || got["text"] != "x" {
		t.Fatalf("request: %s %v", auth, got)
	}
	if err := r.Send(context.Background(), Message{To: "a@example.com", Subject: "fail"}); err == nil || !strings.Contains(err.Error(), "domain not verified") {
		t.Fatalf("error: %v", err)
	}
	if _, err := NewResend("", "x@example.com"); err == nil {
		t.Fatal("key required")
	}
	if _, err := NewResend("k", "not an address"); err == nil {
		t.Fatal("bad sender refused")
	}
}
