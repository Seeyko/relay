package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// An instance from before organizations: accounts, media and members get the first organization, owners
// keep seeing everything, and opening it again changes nothing.
func TestMigrateOrgs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	for _, q := range []string{
		`CREATE TABLE accounts (id TEXT PRIMARY KEY, provider TEXT NOT NULL, name TEXT NOT NULL, handle TEXT NOT NULL DEFAULT '', remote_user_id TEXT NOT NULL DEFAULT '', credentials BLOB, created_at TIMESTAMP NOT NULL)`,
		`CREATE TABLE media (id TEXT PRIMARY KEY, filename TEXT NOT NULL, mime TEXT NOT NULL, size INTEGER NOT NULL, path TEXT NOT NULL, created_at TIMESTAMP NOT NULL)`,
		`INSERT INTO accounts(id,provider,name,created_at) VALUES('acc_1','x','RMP','` + now + `'),('acc_2','tiktok','RMP','` + now + `')`,
		`INSERT INTO media(id,filename,mime,size,path,created_at) VALUES('med_1','a.png','image/png',1,'med_1.png','` + now + `')`,
	} {
		if _, err := raw.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	raw.Close()

	DefaultOrgName = "Ride My Park"
	t.Cleanup(func() { DefaultOrgName = "Default" })
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ed, err := st.CreateUser(ctx, &User{Email: "e@example.com", Role: RoleEditor})
	if err != nil {
		t.Fatal(err)
	}
	st.Close()

	for i := 0; i < 2; i++ {
		st, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		os, _ := st.ListOrgs(ctx)
		if len(os) != 1 || os[0].Name != "Ride My Park" || os[0].Slug != "ride-my-park" || os[0].Accounts != 2 {
			t.Fatalf("open %d: orgs %+v", i, os)
		}
		accs, _ := st.ListAccounts(WithOrgs(ctx, []string{os[0].ID}))
		if len(accs) != 2 || accs[0].Org != "Ride My Park" {
			t.Fatalf("accounts: %+v", accs)
		}
		ms, _ := st.ListMedia(WithOrgs(ctx, []string{os[0].ID}))
		if len(ms) != 1 {
			t.Fatalf("media: %+v", ms)
		}
		ids, full, _ := st.UserOrgs(ctx, ed.ID)
		if full || len(ids) != 1 || ids[0] != os[0].ID {
			t.Fatalf("member created on a one-organization instance: %v %v", ids, full)
		}
		st.Close()
	}
}

func TestOrgScope(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	def, _ := st.TargetOrg(ctx, "")
	mlg, err := st.CreateOrg(ctx, "My Little Garden", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateOrg(ctx, "my little garden", ""); err != ErrOrgExists {
		t.Fatalf("duplicate name: %v", err)
	}
	if _, err := st.TargetOrg(ctx, ""); err != ErrOrgRequired {
		t.Fatalf("two organizations, none given: %v", err)
	}
	a := &Account{Provider: "tiktok", Name: "a", OrgID: "my-little-garden"}
	b := &Account{Provider: "tiktok", Name: "b", OrgID: def.ID}
	for _, x := range []*Account{a, b} {
		if err := st.CreateAccount(ctx, x); err != nil {
			t.Fatal(err)
		}
	}
	p := &Post{AccountID: b.ID, Status: StatusDraft, Parts: []string{"x"}}
	if err := st.CreatePost(ctx, p); err != nil {
		t.Fatal(err)
	}
	in := WithOrgs(ctx, []string{mlg.ID})
	if _, err := st.GetAccount(in, b.ID); err != ErrNotFound {
		t.Fatalf("account of another organization: %v", err)
	}
	if _, err := st.GetPost(in, p.ID); err != ErrNotFound {
		t.Fatalf("post of another organization: %v", err)
	}
	if err := st.DeletePost(in, p.ID); err != ErrNotFound {
		t.Fatalf("delete a post of another organization: %v", err)
	}
	if err := st.DeleteAccount(in, b.ID); err != ErrNotFound {
		t.Fatalf("delete an account of another organization: %v", err)
	}
	if ps, _ := st.ListPosts(in, PostFilter{}); len(ps) != 0 {
		t.Fatalf("posts: %+v", ps)
	}
	if _, err := st.Query(in, "SELECT 1", 1); err != ErrOrgScoped {
		t.Fatalf("SQL while limited: %v", err)
	}
	// Narrowing never widens: limited to My Little Garden, then to Default, sees nothing.
	none := WithOrgs(in, []string{def.ID})
	if accs, _ := st.ListAccounts(none); len(accs) != 0 {
		t.Fatalf("narrowed twice: %+v", accs)
	}
	if os, _ := st.ListOrgs(in); len(os) != 1 || os[0].ID != mlg.ID {
		t.Fatalf("ListOrgs limited: %+v", os)
	}
	if _, err := st.ResolveOrg(in, def.Slug); err != ErrNotFound {
		t.Fatalf("resolve an organization outside the scope: %v", err)
	}
	// Media uploaded in one organization are listed there only.
	m := &Media{Filename: "a.png", Mime: "image/png", Path: "a.png"}
	if err := st.CreateMedia(in, m); err != nil || m.OrgID != mlg.ID {
		t.Fatalf("media org: %+v %v", m, err)
	}
	if ms, _ := st.ListMedia(WithOrgs(ctx, []string{def.ID})); len(ms) != 0 {
		t.Fatalf("media of another organization listed: %+v", ms)
	}
	if _, err := st.GetMedia(WithOrgs(ctx, []string{def.ID}), m.ID); err != nil {
		t.Fatalf("a media id still resolves (links are public): %v", err)
	}
	if err := st.DeleteOrg(ctx, mlg.ID); err != ErrOrgNotEmpty {
		t.Fatalf("delete a non-empty organization: %v", err)
	}
	if moved, err := st.MoveAccount(ctx, a.ID, def.ID); err != nil || moved.OrgID != def.ID {
		t.Fatalf("move: %+v %v", moved, err)
	}
	if err := st.DeleteOrg(ctx, mlg.ID); err != nil {
		t.Fatalf("delete an empty organization: %v", err)
	}
	if err := st.DeleteOrg(ctx, def.ID); err == nil {
		t.Fatal("the last organization was deleted")
	}
	if Slugify("  Périnée — Jardin ") != "perinee-jardin" {
		t.Fatalf("slug: %q", Slugify("  Périnée — Jardin "))
	}
}

// A former owner keeps every organization; a member's list is replaced by UpdateUser.
func TestUserOrgsOnRoleChange(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	mlg, _ := st.CreateOrg(ctx, "MLG", "")
	_, _ = st.CreateUser(ctx, &User{Email: "keep@example.com", Role: RoleOwner})
	o, _ := st.CreateUser(ctx, &User{Email: "o@example.com", Role: RoleOwner})
	admin := RoleAdmin
	u, err := st.UpdateUser(ctx, o.ID, UserPatch{Role: &admin})
	if err != nil || len(u.Orgs) != 2 {
		t.Fatalf("demoted owner: %+v %v", u, err)
	}
	only := []string{mlg.ID}
	if u, err = st.UpdateUser(ctx, o.ID, UserPatch{Orgs: &only}); err != nil || len(u.Orgs) != 1 || u.Orgs[0] != mlg.ID {
		t.Fatalf("set orgs: %+v %v", u, err)
	}
}
