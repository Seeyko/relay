package ui

import (
	"context"
	"testing"
	"time"

	"github.com/Seeyko/relay/internal/store"
)

// The header switcher (?org=) and each member's organizations filter the calendar, the drafts and the accounts.
func TestDashboardOrganizations(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	def, _ := h.st.TargetOrg(ctx, "")
	mlg, err := h.st.CreateOrg(ctx, "My Little Garden", "")
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Add(time.Hour)
	for _, org := range []string{def.ID, mlg.ID, mlg.ID} {
		a := &store.Account{Provider: "tiktok", Name: "acc-" + org, OrgID: org}
		if err := h.st.CreateAccount(ctx, a); err != nil {
			t.Fatal(err)
		}
		if err := h.st.CreatePost(ctx, &store.Post{AccountID: a.ID, Status: store.StatusDraft, Parts: []string{"x"}, PlannedAt: &at}); err != nil {
			t.Fatal(err)
		}
	}
	owner := signedIn(t, h.st, store.RoleOwner, "o@example.com")
	u, _ := h.st.CreateUser(ctx, &store.User{Email: "a@example.com", Role: store.RoleAdmin, WebAuthnID: "wa-a", Orgs: []string{mlg.ID}})
	tok := randomToken()
	if err := h.st.CreateSession(ctx, hashToken(tok), u.ID, "", "email", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	admin := map[string]string{"Cookie": "relay_session=" + tok}

	count := func(s map[string]string, path, key string) int {
		t.Helper()
		code, out := h.do("GET", path, nil, s)
		if code != 200 {
			t.Fatalf("%s: %d %v", path, code, out)
		}
		l, _ := out[key].([]any)
		return len(l)
	}
	cal := "/ui/api/calendar?from=" + at.Add(-24*time.Hour).Format(time.RFC3339) + "&to=" + at.Add(24*time.Hour).Format(time.RFC3339)
	if n := count(owner, cal, "posts"); n != 3 {
		t.Fatalf("owner calendar: %d", n)
	}
	if n := count(owner, cal+"&org="+mlg.Slug, "posts"); n != 2 {
		t.Fatalf("owner calendar, one organization: %d", n)
	}
	if n := count(owner, "/ui/api/accounts?org="+def.ID, "accounts"); n != 1 {
		t.Fatalf("owner accounts, Default: %d", n)
	}
	if n := count(admin, "/ui/api/drafts", "groups"); n != 2 {
		t.Fatalf("limited admin drafts: %d", n)
	}
	if code, _ := h.do("GET", "/ui/api/accounts?org="+def.ID, nil, admin); code != 404 {
		t.Fatalf("limited admin asks for another organization: %d", code)
	}
	_, me := h.do("GET", "/ui/api/me", nil, admin)
	if l, _ := me["orgs"].([]any); len(l) != 1 || me["manage_orgs"] != false {
		t.Fatalf("me of a limited admin: %v", me)
	}
	_, me = h.do("GET", "/ui/api/me", nil, owner)
	if l, _ := me["orgs"].([]any); len(l) != 2 || me["manage_orgs"] != true {
		t.Fatalf("me of the owner: %v", me)
	}
	// Owners create and rename organizations; admins cannot.
	if code, _ := h.do("POST", "/ui/api/orgs", map[string]any{"name": "Popote"}, admin); code != 403 {
		t.Fatalf("admin creates an organization: %d", code)
	}
	if code, out := h.do("POST", "/ui/api/orgs", map[string]any{"name": "Popote"}, owner); code != 200 {
		t.Fatalf("owner creates an organization: %d %v", code, out)
	}
	if code, out := h.do("POST", "/ui/api/orgs/"+mlg.ID, map[string]any{"name": "Jardin"}, owner); code != 200 {
		t.Fatalf("rename: %d %v", code, out)
	}
}
