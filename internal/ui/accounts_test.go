package ui

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Seeyko/relay/internal/connect"
	"github.com/Seeyko/relay/internal/core"
	"github.com/Seeyko/relay/internal/secret"
	"github.com/Seeyko/relay/internal/store"

	_ "github.com/Seeyko/relay/internal/providers/threads"
)

// signedIn creates a member with role and returns the session cookie header.
func signedIn(t *testing.T, st *store.Store, role, email string) map[string]string {
	t.Helper()
	ctx := context.Background()
	u, err := st.CreateUser(ctx, &store.User{Email: email, Role: role, WebAuthnID: "wa-" + email})
	if err != nil {
		t.Fatal(err)
	}
	tok := randomToken()
	if err := st.CreateSession(ctx, hashToken(tok), u.ID, "", "email", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	return map[string]string{"Cookie": "relay_session=" + tok}
}

func TestAccountsSheetRoles(t *testing.T) {
	var m *connect.Manager
	h := newHarnessWith(t, func(u *UI) {
		box, _ := secret.New("k")
		svc := &core.Service{Store: u.Store, Box: box, Log: u.Log}
		nets := connect.FromEnv(func(k string) string {
			return map[string]string{"RELAY_THREADS_APP_ID": "th-id", "RELAY_THREADS_APP_SECRET": "th-secret"}[k]
		})
		var err error
		if m, err = connect.New("http://localhost", svc, nets, u.Log); err != nil {
			t.Fatal(err)
		}
		u.Connect = m
	})
	owner := signedIn(t, h.st, store.RoleOwner, "o@example.com")
	admin := signedIn(t, h.st, store.RoleAdmin, "a@example.com")
	editor := signedIn(t, h.st, store.RoleEditor, "e@example.com")
	viewer := signedIn(t, h.st, store.RoleViewer, "v@example.com")

	for _, s := range []map[string]string{owner, admin} {
		code, out := h.do("GET", "/ui/api/accounts", nil, s)
		nets, _ := out["networks"].([]any)
		if code != 200 || len(nets) != 1 || nets[0].(map[string]any)["redirect_uri"] != "http://localhost/connect/threads/callback" {
			t.Fatalf("accounts: %d %v", code, out)
		}
		code, out = h.do("POST", "/ui/api/accounts/connect", map[string]any{"network": "threads"}, s)
		if u, _ := out["url"].(string); code != 200 || !strings.HasPrefix(u, "http://localhost/connect/threads/start?t=") {
			t.Fatalf("connect: %d %v", code, out)
		}
		if _, me := h.do("GET", "/ui/api/me", nil, s); me["accounts"] != true {
			t.Fatalf("me: %v", me)
		}
	}
	for _, s := range []map[string]string{editor, viewer} {
		if code, _ := h.do("GET", "/ui/api/accounts", nil, s); code != 403 {
			t.Fatalf("editor/viewer list: %d", code)
		}
		if code, _ := h.do("POST", "/ui/api/accounts/connect", map[string]any{"network": "threads"}, s); code != 403 {
			t.Fatalf("editor/viewer connect: %d", code)
		}
		if _, me := h.do("GET", "/ui/api/me", nil, s); me["accounts"] != false {
			t.Fatalf("me: %v", me)
		}
	}
	if code, _ := h.do("POST", "/ui/api/accounts/connect", map[string]any{"network": "threads"}, nil); code != 401 {
		t.Fatalf("anonymous: %d", code)
	}
	o := map[string]string{"Cookie": owner["Cookie"], "Origin": "https://evil.example"}
	if code, _ := h.do("POST", "/ui/api/accounts/connect", map[string]any{"network": "threads"}, o); code != 403 {
		t.Fatalf("cross-origin: %d", code)
	}
	if code, _ := h.do("POST", "/ui/api/accounts/connect", map[string]any{"network": "x"}, owner); code != 400 {
		t.Fatalf("unconfigured network: %d", code)
	}
	if got := len(m.Networks()); got != 1 {
		t.Fatal(got)
	}
}
