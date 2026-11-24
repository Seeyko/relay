package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Seeyko/relay/internal/store"
)

// orgsFixture: the first organization (Default) with two accounts, My Little Garden with two TikTok accounts
// (several accounts of one network in one organization), and a draft on each account.
type orgsFixture struct {
	def, mlg       string
	defAcc, mlgAcc []string
	defPost        string
	mlgPost        string
}

func newOrgs(t *testing.T, e *tenv) orgsFixture {
	t.Helper()
	ctx := context.Background()
	os, err := e.st.ListOrgs(ctx)
	if err != nil || len(os) != 1 || os[0].Name != "Default" {
		t.Fatalf("first organization: %v %v", os, err)
	}
	f := orgsFixture{def: os[0].ID}
	r := e.restJSON(token, "POST", "/v1/tools/create_organization", map[string]any{"name": "My Little Garden"})
	if r.code != 200 || r.json["slug"] != "my-little-garden" {
		t.Fatalf("create_organization: %d %s", r.code, r.body)
	}
	f.mlg = r.json["id"].(string)
	add := func(org, provider, name string) string {
		a := &store.Account{Provider: provider, Name: name, Handle: name, RemoteUserID: name, OrgID: org}
		if err := e.st.CreateAccount(ctx, a); err != nil {
			t.Fatal(err)
		}
		p := &store.Post{AccountID: a.ID, Status: store.StatusDraft, Parts: []string{"hello " + name}}
		if err := e.st.CreatePost(ctx, p); err != nil {
			t.Fatal(err)
		}
		return a.ID + "|" + p.ID
	}
	for _, x := range []string{add(f.def, "x", "rmp"), add(f.def, "tiktok", "rmp-tt")} {
		ap := strings.Split(x, "|")
		f.defAcc, f.defPost = append(f.defAcc, ap[0]), ap[1]
	}
	for _, x := range []string{add("my-little-garden", "tiktok", "jardin-tuto"), add("My Little Garden", "tiktok", "jardin-humour")} {
		ap := strings.Split(x, "|")
		f.mlgAcc, f.mlgPost = append(f.mlgAcc, ap[0]), ap[1]
	}
	return f
}

// listIDs calls a REST route answering a JSON array and returns the ids.
func listIDs(t *testing.T, e *tenv, tok, path string, hdr map[string]string) (int, []string) {
	t.Helper()
	req, _ := http.NewRequest("GET", e.base+path, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	r := e.do(req)
	var arr []map[string]any
	_ = json.Unmarshal(r.body, &arr)
	var ids []string
	for _, x := range arr {
		ids = append(ids, x["id"].(string))
	}
	return r.code, ids
}

func TestOrganizationsScopeEveryCall(t *testing.T) {
	e := newTeamAPI(t)
	f := newOrgs(t, e)
	ctx := context.Background()

	// The API token sees every organization; org (argument, header or ?org= on /mcp) narrows to one.
	if _, ids := listIDs(t, e, token, "/v1/accounts", nil); len(ids) != 4 {
		t.Fatalf("all accounts: %v", ids)
	}
	if _, ids := listIDs(t, e, token, "/v1/accounts?org=my-little-garden", nil); len(ids) != 2 {
		t.Fatalf("org argument: %v", ids)
	}
	if _, ids := listIDs(t, e, token, "/v1/accounts", map[string]string{"X-Relay-Org": "Default"}); len(ids) != 2 || ids[0] != f.defAcc[0] {
		t.Fatalf("X-Relay-Org: %v", ids)
	}
	if code, _ := listIDs(t, e, token, "/v1/accounts", map[string]string{"X-Relay-Org": "nope"}); code != 404 {
		t.Fatalf("unknown org: %d", code)
	}
	r := e.restJSON(token, "POST", "/v1/tools/list_organizations", map[string]any{})
	if r.code != 200 || !strings.Contains(string(r.body), `"accounts": 2`) && !strings.Contains(string(r.body), `"accounts":2`) {
		t.Fatalf("list_organizations: %d %s", r.code, r.body)
	}

	// A new account goes to the organization of the call; with several and none given, it is refused.
	if err := e.st.CreateAccount(ctx, &store.Account{Provider: "x", Name: "n"}); err == nil {
		t.Fatal("account without organization accepted")
	}
	a := &store.Account{Provider: "x", Name: "popote"}
	if err := e.st.CreateAccount(store.WithOrgs(ctx, []string{f.mlg}), a); err != nil || a.OrgID != f.mlg || a.Org != "My Little Garden" {
		t.Fatalf("account in the call's organization: %+v %v", a, err)
	}

	// An editor of My Little Garden only sees My Little Garden, whatever the id they try.
	ed, err := e.st.CreateUser(ctx, &store.User{Email: "ed@example.com", Role: store.RoleEditor, Orgs: []string{f.mlg}})
	if err != nil {
		t.Fatal(err)
	}
	etok := e.grant(ed.ID, "relay:drafts")
	if _, ids := listIDs(t, e, etok, "/v1/accounts", nil); len(ids) != 3 {
		t.Fatalf("editor accounts: %v", ids)
	}
	if _, ids := listIDs(t, e, etok, "/v1/accounts?org=Default", nil); len(ids) != 0 {
		t.Fatalf("editor narrowed to an organization they do not see: %v", ids)
	}
	if r := e.rest(etok, "GET", "/v1/posts/"+f.defPost); r.code != 404 {
		t.Fatalf("other organization's post: %d %s", r.code, r.body)
	}
	if r := e.rest(etok, "GET", "/v1/posts/"+f.mlgPost); r.code != 200 {
		t.Fatalf("own post: %d %s", r.code, r.body)
	}
	if r := e.rest(etok, "GET", "/v1/posts?status=draft"); r.code != 200 || strings.Contains(string(r.body), f.defPost) || !strings.Contains(string(r.body), f.mlgPost) {
		t.Fatalf("list_posts: %d %s", r.code, r.body)
	}
	r = e.restJSON(etok, "POST", "/v1/posts", map[string]any{"account_ids": []string{f.defAcc[0]}, "text": "hi", "scheduled_at": "draft"})
	if r.code == 200 || r.code == 201 {
		t.Fatalf("draft on another organization's account: %d %s", r.code, r.body)
	}
	r = e.restJSON(etok, "POST", "/v1/posts", map[string]any{"account_ids": f.mlgAcc, "text": "deux comptes TikTok", "scheduled_at": "draft", "force": true})
	if r.code != 200 && r.code != 201 {
		t.Fatalf("draft on two TikTok accounts of the organization: %d %s", r.code, r.body)
	}
	if r := e.restJSON(etok, "POST", "/v1/tools/create_organization", map[string]any{"name": "Evil"}); r.code != 403 {
		t.Fatalf("editor creates an organization: %d %s", r.code, r.body)
	}

	// A limited admin cannot read every organization through SQL, nor manage organizations.
	ad, _ := e.st.CreateUser(ctx, &store.User{Email: "ad@example.com", Role: store.RoleAdmin, Orgs: []string{f.mlg}})
	atok := e.grant(ad.ID, "relay:admin")
	if r := e.restJSON(atok, "POST", "/v1/query", map[string]any{"query": "SELECT COUNT(*) FROM posts"}); r.code != 400 {
		t.Fatalf("sql_query of a limited admin: %d %s", r.code, r.body)
	}
	if r := e.restJSON(token, "POST", "/v1/query", map[string]any{"query": "SELECT COUNT(*) FROM posts"}); r.code != 200 {
		t.Fatalf("sql_query of the API token: %d %s", r.code, r.body)
	}
	if r := e.restJSON(atok, "POST", "/v1/tools/update_organization", map[string]any{"org": f.mlg, "name": "X"}); r.code == 200 {
		t.Fatalf("limited admin renames an organization: %s", r.body)
	}
	if r := e.restJSON(atok, "POST", "/v1/tools/move_account", map[string]any{"account_id": f.mlgAcc[0], "org": f.def}); r.code != 404 {
		t.Fatalf("limited admin moves to an organization they do not see: %d %s", r.code, r.body)
	}

	// An owner's app limited to My Little Garden when approved sees only it.
	ow, _ := e.st.CreateUser(ctx, &store.User{Email: "ow@example.com", Role: store.RoleOwner})
	if _, ids := listIDs(t, e, e.grant(ow.ID, "relay:admin"), "/v1/accounts", nil); len(ids) != 5 {
		t.Fatalf("owner app: %v", ids)
	}
	if _, ids := listIDs(t, e, e.grantOrgs(ow.ID, "relay:admin", []string{f.mlg}), "/v1/accounts", nil); len(ids) != 3 {
		t.Fatalf("owner app limited to an organization: %v", ids)
	}

	// The owner moves an account; it then follows the new organization.
	if r := e.restJSON(token, "POST", "/v1/tools/move_account", map[string]any{"account_id": f.defAcc[1], "org": "my-little-garden"}); r.code != 200 || r.json["org_id"] != f.mlg {
		t.Fatalf("move_account: %d %s", r.code, r.body)
	}
	if _, ids := listIDs(t, e, etok, "/v1/accounts", nil); len(ids) != 4 {
		t.Fatalf("editor after the move: %v", ids)
	}
	if r := e.restJSON(token, "POST", "/v1/tools/delete_organization", map[string]any{"org": f.mlg}); r.code != 409 {
		t.Fatalf("delete a non-empty organization: %d %s", r.code, r.body)
	}
}

func TestOrganizationsMCPTools(t *testing.T) {
	e := newTeamAPI(t)
	newOrgs(t, e)
	r := e.mcpCall(token, "tools/list", map[string]any{})
	if !strings.Contains(string(r.body), `"list_organizations"`) || !strings.Contains(string(r.body), "limits this call to it") {
		t.Fatalf("tools/list: %s", r.body)
	}
	r = e.mcpCall(token, "tools/call", map[string]any{"name": "list_accounts", "arguments": map[string]any{"org": "my-little-garden"}})
	if n := strings.Count(string(r.body), `\"provider\"`); n != 2 {
		t.Fatalf("list_accounts with org: %d %s", n, r.body)
	}
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "list_accounts", "arguments": map[string]any{}}})
	req, _ := http.NewRequest("POST", e.base+"/mcp?org=default", strings.NewReader(string(b)))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	if r := e.do(req); strings.Count(string(r.body), `\"provider\"`) != 2 || !strings.Contains(string(r.body), "rmp") {
		t.Fatalf("/mcp?org=: %s", r.body)
	}
}

func TestOrganizationsTeam(t *testing.T) {
	e := newTeamAPI(t)
	f := newOrgs(t, e)
	ctx := context.Background()
	ad, _ := e.st.CreateUser(ctx, &store.User{Email: "ad@example.com", Role: store.RoleAdmin, Orgs: []string{f.mlg}})
	other, _ := e.st.CreateUser(ctx, &store.User{Email: "rmp@example.com", Role: store.RoleEditor, Orgs: []string{f.def}})
	atok := e.grant(ad.ID, "relay:admin")

	// Invitations: a limited admin gives only their organizations (and theirs by default).
	if r := e.restJSON(atok, "POST", "/v1/members", map[string]any{"email": "x@example.com", "role": "editor", "orgs": []string{"Default"}}); r.code != 400 && r.code != 403 {
		t.Fatalf("invite to an organization they do not see: %d %s", r.code, r.body)
	}
	r := e.restJSON(atok, "POST", "/v1/members", map[string]any{"email": "v@example.com", "role": "viewer"})
	if r.code != 200 || !strings.Contains(string(r.body), f.mlg) {
		t.Fatalf("invite with the admin's organizations: %d %s", r.code, r.body)
	}
	// The API token must say which organization when there are several.
	if r := e.restJSON(token, "POST", "/v1/members", map[string]any{"email": "w@example.com", "role": "viewer"}); r.code != 400 {
		t.Fatalf("invite without orgs, several organizations: %d %s", r.code, r.body)
	}
	if r := e.restJSON(token, "POST", "/v1/members", map[string]any{"email": "w@example.com", "role": "viewer", "orgs": []string{"default", "my-little-garden"}}); r.code != 200 {
		t.Fatalf("invite to two organizations: %d %s", r.code, r.body)
	}

	// The members list of a limited admin hides members of other organizations.
	r = e.rest(atok, "GET", "/v1/members")
	if r.code != 200 || strings.Contains(string(r.body), "rmp@example.com") || strings.Contains(string(r.body), "w@example.com") && strings.Contains(string(r.body), f.def) {
		t.Fatalf("list_members of a limited admin: %s", r.body)
	}
	// ... and cannot change them.
	if r := e.restJSON(atok, "PATCH", "/v1/members/"+other.ID, map[string]any{"role": "viewer"}); r.code != 403 {
		t.Fatalf("change a member of another organization: %d %s", r.code, r.body)
	}
	// The API token changes a member's organizations.
	if r := e.restJSON(token, "PATCH", "/v1/members/"+other.ID, map[string]any{"orgs": []string{"my-little-garden"}}); r.code != 200 || !strings.Contains(string(r.body), f.mlg) || strings.Contains(string(r.body), f.def) {
		t.Fatalf("update_member orgs: %d %s", r.code, r.body)
	}
	if ids, full, _ := e.st.UserOrgs(ctx, other.ID); full || len(ids) != 1 || ids[0] != f.mlg {
		t.Fatalf("stored orgs: %v %v", ids, full)
	}
}
