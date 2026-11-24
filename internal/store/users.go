package store

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Team members of the dashboard (see internal/team). Only SHA-256 hashes of invitation links, sign-in links,
// email codes and session cookies are stored.
const usersSchema = `
CREATE TABLE IF NOT EXISTS users (
  id TEXT PRIMARY KEY,
  email TEXT NOT NULL DEFAULT '',
  name TEXT NOT NULL DEFAULT '',
  role TEXT NOT NULL,
  webauthn_id TEXT NOT NULL,
  created_at TIMESTAMP NOT NULL,
  invited_by TEXT NOT NULL DEFAULT '',
  last_login_at TIMESTAMP,
  disabled INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS users_email ON users(email) WHERE email <> '';
CREATE TABLE IF NOT EXISTS user_sessions (
  token_hash TEXT PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  passkey_id TEXT REFERENCES passkeys(id) ON DELETE CASCADE,
  method TEXT NOT NULL,
  created_at TIMESTAMP NOT NULL,
  expires_at TIMESTAMP NOT NULL
);
CREATE INDEX IF NOT EXISTS user_sessions_user ON user_sessions(user_id);
CREATE TABLE IF NOT EXISTS auth_tokens (
  id TEXT PRIMARY KEY,
  kind TEXT NOT NULL,
  token_hash TEXT NOT NULL UNIQUE,
  user_id TEXT REFERENCES users(id) ON DELETE CASCADE,
  email TEXT NOT NULL DEFAULT '',
  role TEXT NOT NULL DEFAULT '',
  lang TEXT NOT NULL DEFAULT '',
  invited_by TEXT NOT NULL DEFAULT '',
  attempts INTEGER NOT NULL DEFAULT 0,
  created_at TIMESTAMP NOT NULL,
  expires_at TIMESTAMP NOT NULL,
  used_at TIMESTAMP
);
CREATE INDEX IF NOT EXISTS auth_tokens_kind ON auth_tokens(kind, email);
CREATE TABLE IF NOT EXISTS audit_log (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  at TIMESTAMP NOT NULL,
  actor TEXT NOT NULL,
  action TEXT NOT NULL,
  target TEXT NOT NULL DEFAULT '',
  detail TEXT NOT NULL DEFAULT '',
  ip TEXT NOT NULL DEFAULT ''
);
`

// Roles, most privileged first.
const (
	RoleOwner  = "owner"
	RoleAdmin  = "admin"
	RoleEditor = "editor"
	RoleViewer = "viewer"
)

// ValidRole reports whether r is one of the four roles.
func ValidRole(r string) bool {
	return r == RoleOwner || r == RoleAdmin || r == RoleEditor || r == RoleViewer
}

// Token kinds of auth_tokens.
const (
	TokenInvite = "invite"
	TokenLogin  = "login"
	TokenCode   = "code"
)

// LegacyOwnerHandle is the WebAuthn user handle of the passkeys registered before team members existed.
// The first owner keeps it so those passkeys keep signing in.
const LegacyOwnerHandle = "relay-owner"

var (
	ErrLastOwner   = errors.New("the last owner cannot be removed, disabled or demoted")
	ErrEmailTaken  = errors.New("this email already belongs to a member")
	ErrTokenUsed   = errors.New("this link was already used")
	ErrTokenExpiry = errors.New("this link expired")
)

type User struct {
	ID          string     `json:"id"`
	Email       string     `json:"email"`
	Name        string     `json:"name"`
	Role        string     `json:"role"`
	WebAuthnID  string     `json:"-"`
	CreatedAt   time.Time  `json:"created_at"`
	InvitedBy   string     `json:"invited_by,omitempty"`
	LastLoginAt *time.Time `json:"last_login_at,omitempty"`
	Disabled    bool       `json:"disabled"`
	Passkeys    int        `json:"passkeys"`
	// Orgs are the organization ids the member sees; nil (omitted) for an owner, who sees all of them.
	Orgs []string `json:"orgs,omitempty"`
}

// Invite is a pending invitation (the link itself is never stored).
type Invite struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	Role      string    `json:"role"`
	Lang      string    `json:"lang,omitempty"`
	InvitedBy string    `json:"invited_by,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	// Orgs are the organizations the invitee will see (ignored for an owner).
	Orgs []string `json:"orgs,omitempty"`
}

// AuthToken is a row of auth_tokens: an invitation, a sign-in link or an email code.
type AuthToken struct {
	ID        string
	Kind      string
	Hash      string
	UserID    string
	Email     string
	Role      string
	Lang      string
	InvitedBy string
	Attempts  int
	CreatedAt time.Time
	ExpiresAt time.Time
	Orgs      []string // invitations: the organizations of the future member
}

// NormEmail lower-cases and trims an address.
func NormEmail(e string) string { return strings.ToLower(strings.TrimSpace(e)) }

// migrateUsers attaches what existed before team members to an owner: passkeys, dashboard sessions and OAuth
// grants. It runs on every open and does nothing once everything has a user.
func (s *Store) migrateUsers() error {
	var users, orphans int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&users); err != nil {
		return err
	}
	if err := s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM passkeys WHERE user_id IS NULL) + (SELECT COUNT(*) FROM ui_sessions) + (SELECT COUNT(*) FROM oauth_grants WHERE user_id IS NULL)`).Scan(&orphans); err != nil {
		return err
	}
	if orphans == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var owner string
	err = tx.QueryRow(`SELECT id FROM users WHERE role=? ORDER BY created_at LIMIT 1`, RoleOwner).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		owner = NewID("usr")
		if _, err := tx.Exec(`INSERT INTO users(id,email,name,role,webauthn_id,created_at) VALUES(?,?,?,?,?,?)`,
			owner, "", "", RoleOwner, LegacyOwnerHandle, time.Now().UTC()); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	for _, q := range []string{
		`UPDATE passkeys SET user_id=? WHERE user_id IS NULL`,
		`INSERT OR IGNORE INTO user_sessions(token_hash,user_id,passkey_id,method,created_at,expires_at)
		 SELECT token_hash, ?, passkey_id, 'passkey', created_at, expires_at FROM ui_sessions`,
		`UPDATE oauth_grants SET user_id=? WHERE user_id IS NULL`,
	} {
		if _, err := tx.Exec(q, owner); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`DELETE FROM ui_sessions`); err != nil {
		return err
	}
	return tx.Commit()
}

// EnsureOwnerEmail gives the first owner an email when it has none (RELAY_OWNER_EMAIL), and creates that owner
// on a fresh instance so it can sign in by email. An owner that already has an email keeps it.
func (s *Store) EnsureOwnerEmail(ctx context.Context, email string) error {
	email = NormEmail(email)
	if email == "" {
		return nil
	}
	var id, cur string
	err := s.db.QueryRowContext(ctx, `SELECT id,email FROM users WHERE role=? ORDER BY created_at LIMIT 1`, RoleOwner).Scan(&id, &cur)
	if errors.Is(err, sql.ErrNoRows) {
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE email=?`, email).Scan(&n); err != nil || n > 0 {
			return err
		}
		_, err = s.CreateUser(ctx, &User{Email: email, Role: RoleOwner, WebAuthnID: LegacyOwnerHandle})
		return err
	}
	if err != nil || cur != "" {
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE users SET email=? WHERE id=?`, email, id)
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return ErrEmailTaken
	}
	return err
}

const userCols = `u.id,u.email,u.name,u.role,u.webauthn_id,u.created_at,u.invited_by,u.last_login_at,u.disabled,
	(SELECT COUNT(*) FROM passkeys p WHERE p.user_id=u.id)`

func scanUser(r scanner) (*User, error) {
	u := &User{}
	var last sql.NullTime
	err := r.Scan(&u.ID, &u.Email, &u.Name, &u.Role, &u.WebAuthnID, &u.CreatedAt, &u.InvitedBy, &last, &u.Disabled, &u.Passkeys)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if last.Valid {
		t := last.Time
		u.LastLoginAt = &t
	}
	return u, nil
}

// CreateUser inserts u (ID, WebAuthnID and CreatedAt are set when empty).
func (s *Store) CreateUser(ctx context.Context, u *User) (*User, error) {
	if u.ID == "" {
		u.ID = NewID("usr")
	}
	if u.WebAuthnID == "" {
		u.WebAuthnID = u.ID
	}
	u.Email = NormEmail(u.Email)
	u.CreatedAt = time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `INSERT INTO users(id,email,name,role,webauthn_id,created_at,invited_by) VALUES(?,?,?,?,?,?,?)`,
		u.ID, u.Email, u.Name, u.Role, u.WebAuthnID, u.CreatedAt, u.InvitedBy)
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return nil, ErrEmailTaken
	}
	if err != nil {
		return nil, err
	}
	if u.Role != RoleOwner { // in its organizations (u.Orgs), else in the only one there is
		ids := u.Orgs
		if len(ids) == 0 {
			if all, err := s.AllOrgIDs(ctx); err == nil && len(all) == 1 {
				ids = all
			}
		}
		if err := s.SetUserOrgs(ctx, u.ID, ids); err != nil {
			return nil, err
		}
		u.Orgs = dedupe(ids)
	}
	return u, nil
}

func (s *Store) GetUser(ctx context.Context, id string) (*User, error) {
	return s.withOrgs(ctx)(scanUser(s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM users u WHERE u.id=?`, id)))
}

// withOrgs fills the organizations of a user that was just read.
func (s *Store) withOrgs(ctx context.Context) func(*User, error) (*User, error) {
	return func(u *User, err error) (*User, error) {
		if err != nil {
			return nil, err
		}
		return u, s.fillUserOrgs(ctx, u)
	}
}

func (s *Store) UserByEmail(ctx context.Context, email string) (*User, error) {
	email = NormEmail(email)
	if email == "" {
		return nil, ErrNotFound
	}
	return s.withOrgs(ctx)(scanUser(s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM users u WHERE u.email=?`, email)))
}

// FirstOwner is the oldest owner: the one the API token registers passkeys for.
func (s *Store) FirstOwner(ctx context.Context) (*User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM users u WHERE u.role=? ORDER BY u.created_at LIMIT 1`, RoleOwner))
}

func (s *Store) ListUsers(ctx context.Context) ([]*User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+userCols+` FROM users u ORDER BY u.created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*User{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	return out, s.fillUserOrgs(ctx, out...)
}

// UserPatch is a change to a member; nil fields are kept.
type UserPatch struct {
	Email    *string
	Name     *string
	Role     *string
	Disabled *bool
	// Orgs replaces the member's organizations (ids). Ignored for an owner.
	Orgs *[]string
}

// UpdateUser applies p in one transaction and refuses to leave the instance without an active owner.
func (s *Store) UpdateUser(ctx context.Context, id string, p UserPatch) (*User, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	u, err := scanUser(tx.QueryRowContext(ctx, `SELECT `+userCols+` FROM users u WHERE u.id=?`, id))
	if err != nil {
		return nil, err
	}
	if p.Email != nil {
		u.Email = NormEmail(*p.Email)
	}
	if p.Name != nil {
		u.Name = *p.Name
	}
	wasOwner := u.Role == RoleOwner && !u.Disabled
	if p.Role != nil {
		u.Role = *p.Role
	}
	if p.Disabled != nil {
		u.Disabled = *p.Disabled
	}
	if wasOwner && (u.Role != RoleOwner || u.Disabled) {
		if err := lastOwnerCheck(ctx, tx, id); err != nil {
			return nil, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE users SET email=?,name=?,role=?,disabled=? WHERE id=?`, u.Email, u.Name, u.Role, u.Disabled, id); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, ErrEmailTaken
		}
		return nil, err
	}
	if u.Disabled { // sign out everywhere
		if _, err := tx.ExecContext(ctx, `DELETE FROM user_sessions WHERE user_id=?`, id); err != nil {
			return nil, err
		}
	}
	switch {
	case p.Orgs != nil && u.Role != RoleOwner:
		if err := setUserOrgs(ctx, tx, id, *p.Orgs); err != nil {
			return nil, err
		}
	case wasOwner && u.Role != RoleOwner:
		// A former owner keeps seeing every organization that exists now.
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO org_members(org_id,user_id) SELECT id, ? FROM orgs`, id); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return u, s.fillUserOrgs(ctx, u)
}

// lastOwnerCheck fails when no active owner other than id would remain.
func lastOwnerCheck(ctx context.Context, tx *sql.Tx, id string) error {
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE role=? AND disabled=0 AND id<>?`, RoleOwner, id).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return ErrLastOwner
	}
	return nil
}

// DeleteUser removes a member with its passkeys, sessions, pending links and the apps it approved.
func (s *Store) DeleteUser(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	u, err := scanUser(tx.QueryRowContext(ctx, `SELECT `+userCols+` FROM users u WHERE u.id=?`, id))
	if err != nil {
		return err
	}
	if u.Role == RoleOwner && !u.Disabled {
		if err := lastOwnerCheck(ctx, tx, id); err != nil {
			return err
		}
	}
	for _, q := range []string{
		`DELETE FROM user_sessions WHERE user_id=?`,
		`DELETE FROM passkeys WHERE user_id=?`,
		`DELETE FROM oauth_grants WHERE user_id=?`,
		`DELETE FROM auth_tokens WHERE user_id=?`,
		`DELETE FROM users WHERE id=?`,
	} {
		if _, err := tx.ExecContext(ctx, q, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ---------- sessions ----------

// CreateSession stores a dashboard session of a member. passkeyID is empty for an email sign-in.
func (s *Store) CreateSession(ctx context.Context, tokenHash, userID, passkeyID, method string, expires time.Time) error {
	now := time.Now().UTC()
	_, _ = s.db.ExecContext(ctx, `DELETE FROM user_sessions WHERE expires_at < ?`, now)
	var pk any
	if passkeyID != "" {
		pk = passkeyID
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO user_sessions(token_hash,user_id,passkey_id,method,created_at,expires_at) VALUES(?,?,?,?,?,?)`,
		tokenHash, userID, pk, method, now, expires.UTC()); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE users SET last_login_at=? WHERE id=?`, now, userID)
	return err
}

// SessionUser returns the active member of a live session.
func (s *Store) SessionUser(ctx context.Context, tokenHash string) (*User, error) {
	return s.withOrgs(ctx)(scanUser(s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM user_sessions x JOIN users u ON u.id=x.user_id
		WHERE x.token_hash=? AND x.expires_at > ? AND u.disabled=0`, tokenHash, time.Now().UTC())))
}

func (s *Store) DeleteSession(ctx context.Context, tokenHash string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM user_sessions WHERE token_hash=?`, tokenHash)
	return err
}

// ---------- invitations, sign-in links and email codes ----------

// CreateAuthToken stores t. For a sign-in link or an email code, the member's previous ones of the same kind
// are deleted: only the latest works.
func (s *Store) CreateAuthToken(ctx context.Context, t *AuthToken) error {
	now := time.Now().UTC()
	if t.ID == "" {
		t.ID = NewID("tok")
	}
	t.CreatedAt = now
	t.Email = NormEmail(t.Email)
	_, _ = s.db.ExecContext(ctx, `DELETE FROM auth_tokens WHERE expires_at < ?`, now.Add(-24*time.Hour))
	if t.Kind != TokenInvite && t.UserID != "" {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM auth_tokens WHERE kind=? AND user_id=?`, t.Kind, t.UserID); err != nil {
			return err
		}
	}
	var uid any
	if t.UserID != "" {
		uid = t.UserID
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO auth_tokens(id,kind,token_hash,user_id,email,role,lang,invited_by,created_at,expires_at,orgs) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		t.ID, t.Kind, t.Hash, uid, t.Email, t.Role, t.Lang, t.InvitedBy, now, t.ExpiresAt.UTC(), orgsJSON(t.Orgs))
	return err
}

func scanToken(r scanner) (*AuthToken, *time.Time, error) {
	t := &AuthToken{}
	var uid sql.NullString
	var used sql.NullTime
	var orgs string
	err := r.Scan(&t.ID, &t.Kind, &t.Hash, &uid, &t.Email, &t.Role, &t.Lang, &t.InvitedBy, &t.Attempts, &t.CreatedAt, &t.ExpiresAt, &used, &orgs)
	t.Orgs = parseOrgs(orgs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	t.UserID = uid.String
	if used.Valid {
		u := used.Time
		return t, &u, nil
	}
	return t, nil, nil
}

const tokenCols = `id,kind,token_hash,user_id,email,role,lang,invited_by,attempts,created_at,expires_at,used_at,orgs`

// PeekAuthToken returns a live, unused token of kind by hash, without using it.
func (s *Store) PeekAuthToken(ctx context.Context, kind, hash string, now time.Time) (*AuthToken, error) {
	t, used, err := scanToken(s.db.QueryRowContext(ctx, `SELECT `+tokenCols+` FROM auth_tokens WHERE kind=? AND token_hash=?`, kind, hash))
	if err != nil {
		return nil, err
	}
	if used != nil {
		return nil, ErrTokenUsed
	}
	if !now.Before(t.ExpiresAt) {
		return nil, ErrTokenExpiry
	}
	return t, nil
}

// UseAuthToken burns a live token of kind by hash and returns it. Two concurrent uses: only one wins.
func (s *Store) UseAuthToken(ctx context.Context, kind, hash string, now time.Time) (*AuthToken, error) {
	t, err := s.PeekAuthToken(ctx, kind, hash, now)
	if err != nil {
		return nil, err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE auth_tokens SET used_at=? WHERE id=? AND used_at IS NULL`, now.UTC(), t.ID)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrTokenUsed
	}
	return t, nil
}

// AcceptInvite burns the invitation and creates its member in one transaction.
func (s *Store) AcceptInvite(ctx context.Context, hash, name string, now time.Time) (*User, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	t, used, err := scanToken(tx.QueryRowContext(ctx, `SELECT `+tokenCols+` FROM auth_tokens WHERE kind=? AND token_hash=?`, TokenInvite, hash))
	if err != nil {
		return nil, err
	}
	if used != nil {
		return nil, ErrTokenUsed
	}
	if !now.Before(t.ExpiresAt) {
		return nil, ErrTokenExpiry
	}
	if _, err := tx.ExecContext(ctx, `UPDATE auth_tokens SET used_at=? WHERE id=?`, now.UTC(), t.ID); err != nil {
		return nil, err
	}
	u := &User{ID: NewID("usr"), Email: t.Email, Name: name, Role: t.Role, InvitedBy: t.InvitedBy, CreatedAt: now.UTC()}
	u.WebAuthnID = u.ID
	if _, err := tx.ExecContext(ctx, `INSERT INTO users(id,email,name,role,webauthn_id,created_at,invited_by) VALUES(?,?,?,?,?,?,?)`,
		u.ID, u.Email, u.Name, u.Role, u.WebAuthnID, u.CreatedAt, u.InvitedBy); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, ErrEmailTaken
		}
		return nil, err
	}
	if u.Role != RoleOwner {
		if err := setUserOrgs(ctx, tx, u.ID, t.Orgs); err != nil {
			return nil, err
		}
		u.Orgs = dedupe(t.Orgs)
	}
	// Other pending invitations to the same address are void now.
	if _, err := tx.ExecContext(ctx, `DELETE FROM auth_tokens WHERE kind=? AND email=? AND used_at IS NULL`, TokenInvite, t.Email); err != nil {
		return nil, err
	}
	return u, tx.Commit()
}

// ListInvites returns the pending invitations (not used, not expired), newest first.
func (s *Store) ListInvites(ctx context.Context, now time.Time) ([]*Invite, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,email,role,lang,invited_by,created_at,expires_at,orgs FROM auth_tokens
		WHERE kind=? AND used_at IS NULL AND expires_at > ? ORDER BY created_at DESC`, TokenInvite, now.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Invite{}
	for rows.Next() {
		i := &Invite{}
		var orgs string
		if err := rows.Scan(&i.ID, &i.Email, &i.Role, &i.Lang, &i.InvitedBy, &i.CreatedAt, &i.ExpiresAt, &orgs); err != nil {
			return nil, err
		}
		i.Orgs = parseOrgs(orgs)
		out = append(out, i)
	}
	return out, rows.Err()
}

// GetInvite returns a pending invitation by id.
func (s *Store) GetInvite(ctx context.Context, id string, now time.Time) (*AuthToken, error) {
	t, used, err := scanToken(s.db.QueryRowContext(ctx, `SELECT `+tokenCols+` FROM auth_tokens WHERE kind=? AND id=?`, TokenInvite, id))
	if err != nil {
		return nil, err
	}
	if used != nil || !now.Before(t.ExpiresAt) {
		return nil, ErrNotFound
	}
	return t, nil
}

// RenewInvite gives a pending invitation a new link hash and expiry (the old link stops working).
func (s *Store) RenewInvite(ctx context.Context, id, hash string, expires time.Time) error {
	res, err := s.db.ExecContext(ctx, `UPDATE auth_tokens SET token_hash=?, expires_at=?, created_at=? WHERE kind=? AND id=? AND used_at IS NULL`,
		hash, expires.UTC(), time.Now().UTC(), TokenInvite, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteInvites deletes pending invitations by id, or every pending one to email when id is empty.
func (s *Store) DeleteInvites(ctx context.Context, id, email string) error {
	var res sql.Result
	var err error
	if id != "" {
		res, err = s.db.ExecContext(ctx, `DELETE FROM auth_tokens WHERE kind=? AND id=? AND used_at IS NULL`, TokenInvite, id)
	} else {
		res, err = s.db.ExecContext(ctx, `DELETE FROM auth_tokens WHERE kind=? AND email=? AND used_at IS NULL`, TokenInvite, NormEmail(email))
	}
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 && id != "" {
		return ErrNotFound
	}
	return nil
}

// CheckCode verifies the member's latest email code. hash(code) computes the hash for the stored row (its id is
// the salt). Each wrong answer counts; at maxAttempts the code is burned. A right answer burns it too.
func (s *Store) CheckCode(ctx context.Context, userID string, hash func(id string) string, maxAttempts int, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	t, used, err := scanToken(tx.QueryRowContext(ctx, `SELECT `+tokenCols+` FROM auth_tokens WHERE kind=? AND user_id=? ORDER BY created_at DESC LIMIT 1`, TokenCode, userID))
	if err != nil {
		return err
	}
	if used != nil || t.Attempts >= maxAttempts {
		return ErrTokenUsed
	}
	if !now.Before(t.ExpiresAt) {
		return ErrTokenExpiry
	}
	if subtle.ConstantTimeCompare([]byte(hash(t.ID)), []byte(t.Hash)) != 1 {
		n := t.Attempts + 1
		q := `UPDATE auth_tokens SET attempts=? WHERE id=?`
		if n >= maxAttempts {
			q = `UPDATE auth_tokens SET attempts=?, used_at=CURRENT_TIMESTAMP WHERE id=?`
		}
		if _, err := tx.ExecContext(ctx, q, n, t.ID); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return errors.New("wrong code")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE auth_tokens SET used_at=? WHERE id=?`, now.UTC(), t.ID); err != nil {
		return err
	}
	return tx.Commit()
}

// ---------- audit ----------

type AuditEntry struct {
	At     time.Time `json:"at"`
	Actor  string    `json:"actor"`
	Action string    `json:"action"`
	Target string    `json:"target,omitempty"`
	Detail string    `json:"detail,omitempty"`
	IP     string    `json:"ip,omitempty"`
}

func (s *Store) AddAudit(ctx context.Context, e *AuditEntry) error {
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO audit_log(at,actor,action,target,detail,ip) VALUES(?,?,?,?,?,?)`, e.At.UTC(), e.Actor, e.Action, e.Target, e.Detail, e.IP)
	return err
}

// ListAudit returns the latest entries, newest first.
func (s *Store) ListAudit(ctx context.Context, limit int) ([]*AuditEntry, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT at,actor,action,target,detail,ip FROM audit_log ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*AuditEntry{}
	for rows.Next() {
		e := &AuditEntry{}
		if err := rows.Scan(&e.At, &e.Actor, &e.Action, &e.Target, &e.Detail, &e.IP); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
