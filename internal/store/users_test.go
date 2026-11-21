package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// oldSchema is the login data of a single-owner instance, before team members.
const oldSchema = `
CREATE TABLE passkeys (id TEXT PRIMARY KEY, name TEXT NOT NULL DEFAULT '', credential TEXT NOT NULL, created_at TIMESTAMP NOT NULL, last_used_at TIMESTAMP);
CREATE TABLE ui_sessions (token_hash TEXT PRIMARY KEY, passkey_id TEXT NOT NULL REFERENCES passkeys(id) ON DELETE CASCADE, created_at TIMESTAMP NOT NULL, expires_at TIMESTAMP NOT NULL);
CREATE TABLE oauth_clients (id TEXT PRIMARY KEY, name TEXT NOT NULL DEFAULT '', redirect_uris TEXT NOT NULL, created_at TIMESTAMP NOT NULL);
CREATE TABLE oauth_grants (id TEXT PRIMARY KEY, client_id TEXT NOT NULL REFERENCES oauth_clients(id) ON DELETE CASCADE, scope TEXT NOT NULL, resource TEXT NOT NULL, created_at TIMESTAMP NOT NULL, expires_at TIMESTAMP NOT NULL, last_used_at TIMESTAMP);
CREATE TABLE oauth_codes (code_hash TEXT PRIMARY KEY, client_id TEXT NOT NULL REFERENCES oauth_clients(id) ON DELETE CASCADE, redirect_uri TEXT NOT NULL, challenge TEXT NOT NULL, scope TEXT NOT NULL, resource TEXT NOT NULL, created_at TIMESTAMP NOT NULL, expires_at TIMESTAMP NOT NULL, used_at TIMESTAMP, grant_id TEXT REFERENCES oauth_grants(id) ON DELETE SET NULL);
`

func TestMigrateSingleOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, q := range []string{oldSchema,
		`INSERT INTO passkeys(id,name,credential,created_at) VALUES('pk1','phone','{}','` + now.Format(time.RFC3339) + `')`,
		`INSERT INTO ui_sessions(token_hash,passkey_id,created_at,expires_at) VALUES('sess-hash','pk1','` + now.Format(time.RFC3339) + `','` + now.Add(time.Hour).Format(time.RFC3339) + `')`,
		`INSERT INTO oauth_clients(id,name,redirect_uris,created_at) VALUES('c1','Claude','[]','` + now.Format(time.RFC3339) + `')`,
		`INSERT INTO oauth_grants(id,client_id,scope,resource,created_at,expires_at) VALUES('g1','c1','relay:admin','r','` + now.Format(time.RFC3339) + `','` + now.Add(time.Hour).Format(time.RFC3339) + `')`,
	} {
		if _, err := raw.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	raw.Close()

	for i := 0; i < 2; i++ { // the second open must change nothing
		st, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		ctx := t.Context()
		us, _ := st.ListUsers(ctx)
		if len(us) != 1 || us[0].Role != RoleOwner || us[0].WebAuthnID != LegacyOwnerHandle || us[0].Email != "" || us[0].Passkeys != 1 {
			t.Fatalf("open %d: want one owner with the legacy handle and its passkey, got %+v", i, us)
		}
		owner := us[0]
		pks, _ := st.ListPasskeys(ctx, owner.ID)
		if len(pks) != 1 || pks[0].ID != "pk1" {
			t.Fatalf("passkey not attached to the owner: %+v", pks)
		}
		u, err := st.SessionUser(ctx, "sess-hash")
		if err != nil || u.ID != owner.ID {
			t.Fatalf("the open session must still sign the owner in: %v %v", u, err)
		}
		var n int
		_ = st.db.QueryRow(`SELECT COUNT(*) FROM ui_sessions`).Scan(&n)
		if n != 0 {
			t.Fatal("old sessions table must be emptied")
		}
		gs, _ := st.ListOAuthGrants(ctx)
		if len(gs) != 1 || gs[0].UserID != owner.ID {
			t.Fatalf("grant not attached to the owner: %+v", gs)
		}
		st.Close()
	}

	// RELAY_OWNER_EMAIL fills an empty owner email, and never replaces one.
	st, _ := Open(path)
	defer st.Close()
	ctx := t.Context()
	if err := st.EnsureOwnerEmail(ctx, "  Tom@Example.COM "); err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureOwnerEmail(ctx, "other@example.com"); err != nil {
		t.Fatal(err)
	}
	o, _ := st.FirstOwner(ctx)
	if o.Email != "tom@example.com" {
		t.Fatalf("owner email: %q", o.Email)
	}
	if u, err := st.UserByEmail(ctx, "TOM@example.com"); err != nil || u.ID != o.ID {
		t.Fatal("lookup by email must ignore case")
	}

	// Login data stays out of agent SQL.
	for _, q := range []string{"SELECT * FROM users", "SELECT * FROM user_sessions", "SELECT * FROM auth_tokens", "SELECT * FROM audit_log"} {
		if _, err := st.Query(ctx, q, 10); err == nil {
			t.Fatalf("%q must be refused", q)
		}
	}
}

func TestFreshInstanceOwnerEmail(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := t.Context()
	if us, _ := st.ListUsers(ctx); len(us) != 0 {
		t.Fatal("a fresh instance has no member")
	}
	if err := st.EnsureOwnerEmail(ctx, "owner@example.com"); err != nil {
		t.Fatal(err)
	}
	o, err := st.FirstOwner(ctx)
	if err != nil || o.Email != "owner@example.com" || o.WebAuthnID != LegacyOwnerHandle {
		t.Fatalf("owner from RELAY_OWNER_EMAIL: %+v %v", o, err)
	}
}

func TestLastOwnerProtected(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := t.Context()
	o, _ := st.CreateUser(ctx, &User{Email: "o@example.com", Role: RoleOwner})
	ed, _ := st.CreateUser(ctx, &User{Email: "e@example.com", Role: RoleEditor})
	viewer, admin, yes := RoleViewer, RoleAdmin, true
	if _, err := st.UpdateUser(ctx, o.ID, UserPatch{Role: &viewer}); !errors.Is(err, ErrLastOwner) {
		t.Fatalf("demote the last owner: %v", err)
	}
	if _, err := st.UpdateUser(ctx, o.ID, UserPatch{Disabled: &yes}); !errors.Is(err, ErrLastOwner) {
		t.Fatalf("disable the last owner: %v", err)
	}
	if err := st.DeleteUser(ctx, o.ID); !errors.Is(err, ErrLastOwner) {
		t.Fatalf("remove the last owner: %v", err)
	}
	// Other members change freely.
	if _, err := st.UpdateUser(ctx, ed.ID, UserPatch{Role: &admin}); err != nil {
		t.Fatal(err)
	}
	// With a second owner, the first one may go.
	owner := RoleOwner
	if _, err := st.UpdateUser(ctx, ed.ID, UserPatch{Role: &owner}); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteUser(ctx, o.ID); err != nil {
		t.Fatalf("remove one of two owners: %v", err)
	}
	if _, err := st.CreateUser(ctx, &User{Email: "E@example.com", Role: RoleViewer}); !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("emails are unique ignoring case: %v", err)
	}
}
