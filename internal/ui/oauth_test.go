package ui

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Seeyko/relay/internal/store"
)

// The fresh passkey check of the OAuth consent page, with a real WebAuthn assertion.
func TestFreshAssertion(t *testing.T) {
	h := newHarness(t)
	k := newSoftKey("localhost", h.origin)
	h.register(k)
	ctx := t.Context()
	owner, err := h.st.FirstOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}

	sign := func(key *softKey) (any, []byte) {
		opts, st, err := h.ui.BeginAssertion(ctx, owner.ID)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(opts)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		if uv := m["publicKey"].(map[string]any)["userVerification"]; uv != "required" {
			t.Fatalf("user verification must be required, got %v", uv)
		}
		cred, _ := json.Marshal(key.get(challengeOf(t, m), ownerID))
		return st, cred
	}

	st, cred := sign(k)
	if err := h.ui.FinishAssertion(ctx, owner.ID, st, cred); err != nil {
		t.Fatalf("valid assertion refused: %v", err)
	}

	// Presence without user verification (no PIN, no biometrics) is not enough.
	k.noUV = true
	st, cred = sign(k)
	if err := h.ui.FinishAssertion(ctx, owner.ID, st, cred); err == nil {
		t.Fatal("assertion without user verification accepted")
	}
	k.noUV = false

	// Another key with the same credential id, or another origin, fails.
	impostor := newSoftKey("localhost", h.origin)
	impostor.credID = k.credID
	st, cred = sign(impostor)
	if err := h.ui.FinishAssertion(ctx, owner.ID, st, cred); err == nil {
		t.Fatal("impostor accepted")
	}
	phish := *k
	phish.origin = "https://relay-login.example"
	st, cred = sign(&phish)
	if err := h.ui.FinishAssertion(ctx, owner.ID, st, cred); err == nil {
		t.Fatal("assertion for another origin accepted")
	}
	k.count = phish.count

	// An answer to another challenge fails.
	st1, _ := sign(k)
	_, cred2 := sign(k)
	if err := h.ui.FinishAssertion(ctx, owner.ID, st1, cred2); err == nil {
		t.Fatal("assertion of another challenge accepted")
	}

	// The check expires.
	st, cred = sign(k)
	h.ui.now = func() time.Time { return time.Now().Add(ceremonyTTL + time.Second) }
	if err := h.ui.FinishAssertion(ctx, owner.ID, st, cred); err == nil {
		t.Fatal("expired assertion accepted")
	}
	h.ui.now = time.Now
	if err := h.ui.FinishAssertion(ctx, owner.ID, nil, cred); err == nil {
		t.Fatal("assertion without state accepted")
	}

	// A passkey check started for one member cannot approve for another.
	st, cred = sign(k)
	if err := h.ui.FinishAssertion(ctx, "usr_other", st, cred); err == nil {
		t.Fatal("assertion accepted for another member")
	}

	// LoginMethods follows the passkeys.
	if pk, _ := h.ui.LoginMethods(ctx); !pk {
		t.Fatal("LoginMethods")
	}
}

func TestAppsList(t *testing.T) {
	h2 := newHarnessWith(t, func(u *UI) { u.Apps = true })
	k := newSoftKey("localhost", h2.origin)
	h2.register(k)
	ctx := t.Context()

	c := &store.OAuthClient{ID: "relay_c_test", Name: "Claude", RedirectURIs: []string{"https://claude.ai/api/mcp/auth_callback"}}
	if err := h2.st.CreateOAuthClient(ctx, c); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := h2.st.CreateOAuthCode(ctx, &store.OAuthCode{Hash: "h", ClientID: c.ID, RedirectURI: c.RedirectURIs[0], Challenge: "x", Scope: "relay:drafts", Resource: "r", CreatedAt: now, ExpiresAt: now.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	g := &store.OAuthGrant{ID: "ogr_1", ExpiresAt: now.Add(time.Hour)}
	if err := h2.st.ExchangeOAuthCode(ctx, "h", func(*store.OAuthCode) error { return nil }, g, func(*store.OAuthGrant) []*store.OAuthToken { return nil }); err != nil {
		t.Fatal(err)
	}

	if code, _ := h2.do("GET", "/ui/api/apps", nil, nil); code != 401 {
		t.Fatalf("apps without session: want 401, got %d", code)
	}
	if code, _ := h2.do("POST", "/ui/api/apps/ogr_1/revoke", map[string]any{}, nil); code != 401 {
		t.Fatalf("revoke without session: want 401, got %d", code)
	}
	if code, _ := h2.login(k); code != 200 {
		t.Fatal("login failed")
	}
	_, me := h2.do("GET", "/ui/api/me", nil, nil)
	if me["apps"] != true {
		t.Fatalf("me must announce apps: %v", me)
	}
	code, out := h2.do("GET", "/ui/api/apps", nil, nil)
	apps, _ := out["apps"].([]any)
	if code != 200 || len(apps) != 1 || apps[0].(map[string]any)["client_name"] != "Claude" || apps[0].(map[string]any)["scope"] != "relay:drafts" {
		t.Fatalf("apps: %d %v", code, out)
	}
	// Revoke is a same-origin JSON POST (the dashboard's CSRF defence).
	if code, _ := h2.do("POST", "/ui/api/apps/ogr_1/revoke", map[string]any{}, map[string]string{"Origin": "https://evil.example"}); code != 403 {
		t.Fatalf("cross-origin revoke: want 403, got %d", code)
	}
	if code, _ := h2.do("POST", "/ui/api/apps/ogr_1/revoke", nil, nil); code != 415 {
		t.Fatalf("form revoke: want 415, got %d", code)
	}
	if code, _ := h2.do("POST", "/ui/api/apps/ogr_1/revoke", map[string]any{}, nil); code != 204 {
		t.Fatalf("revoke: want 204, got %d", code)
	}
	if code, _ := h2.do("POST", "/ui/api/apps/ogr_1/revoke", map[string]any{}, nil); code != 404 {
		t.Fatalf("revoke twice: want 404, got %d", code)
	}
	if gs, _ := h2.st.ListOAuthGrants(ctx); len(gs) != 0 {
		t.Fatalf("grant still there: %v", gs)
	}
}
