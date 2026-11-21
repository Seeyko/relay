package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// OAuth 2.1 data for agents that connect to /mcp (see internal/oauth). Only SHA-256 hashes of codes and
// tokens are stored: a copy of the database gives no usable credential.
const oauthSchema = `
CREATE TABLE IF NOT EXISTS oauth_clients (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL DEFAULT '',
  redirect_uris TEXT NOT NULL,
  created_at TIMESTAMP NOT NULL
);
CREATE TABLE IF NOT EXISTS oauth_grants (
  id TEXT PRIMARY KEY,
  client_id TEXT NOT NULL REFERENCES oauth_clients(id) ON DELETE CASCADE,
  scope TEXT NOT NULL,
  resource TEXT NOT NULL,
  created_at TIMESTAMP NOT NULL,
  expires_at TIMESTAMP NOT NULL,
  last_used_at TIMESTAMP
);
CREATE TABLE IF NOT EXISTS oauth_codes (
  code_hash TEXT PRIMARY KEY,
  client_id TEXT NOT NULL REFERENCES oauth_clients(id) ON DELETE CASCADE,
  redirect_uri TEXT NOT NULL,
  challenge TEXT NOT NULL,
  scope TEXT NOT NULL,
  resource TEXT NOT NULL,
  created_at TIMESTAMP NOT NULL,
  expires_at TIMESTAMP NOT NULL,
  used_at TIMESTAMP,
  grant_id TEXT REFERENCES oauth_grants(id) ON DELETE SET NULL
);
CREATE TABLE IF NOT EXISTS oauth_tokens (
  token_hash TEXT PRIMARY KEY,
  grant_id TEXT NOT NULL REFERENCES oauth_grants(id) ON DELETE CASCADE,
  kind TEXT NOT NULL,
  scope TEXT NOT NULL,
  created_at TIMESTAMP NOT NULL,
  expires_at TIMESTAMP NOT NULL,
  used_at TIMESTAMP
);
CREATE INDEX IF NOT EXISTS oauth_tokens_grant ON oauth_tokens(grant_id);
`

// Token kinds.
const (
	TokenAccess  = "access"
	TokenRefresh = "refresh"
)

var (
	// ErrOAuthReused: a code or a rotated refresh token was presented again. The grant made from it is gone.
	ErrOAuthReused = errors.New("oauth: credential reused, grant revoked")
	// ErrOAuthClient: the token belongs to another client.
	ErrOAuthClient = errors.New("oauth: token issued to another client")
)

type OAuthClient struct {
	ID           string    `json:"client_id"`
	Name         string    `json:"client_name"`
	RedirectURIs []string  `json:"redirect_uris"`
	CreatedAt    time.Time `json:"created_at"`
}

type OAuthCode struct {
	Hash        string
	ClientID    string
	RedirectURI string
	Challenge   string
	Scope       string
	Resource    string
	CreatedAt   time.Time
	ExpiresAt   time.Time
}

// OAuthGrant is one approved connection of an app. Deleting it revokes every token issued under it.
type OAuthGrant struct {
	ID         string     `json:"id"`
	ClientID   string     `json:"client_id"`
	ClientName string     `json:"client_name"`
	Scope      string     `json:"scope"`
	Resource   string     `json:"-"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  time.Time  `json:"expires_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

type OAuthToken struct {
	Hash      string
	GrantID   string
	Kind      string
	Scope     string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// OAuthAccess is what a valid access token grants.
type OAuthAccess struct {
	GrantID  string
	ClientID string
	Scope    string
	Resource string
}

func (s *Store) CountOAuthClients(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM oauth_clients`).Scan(&n)
	return n, err
}

// PruneOAuthClients deletes clients registered before cut that never got a grant (abandoned registrations).
func (s *Store) PruneOAuthClients(ctx context.Context, cut time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM oauth_clients WHERE created_at < ? AND id NOT IN (SELECT client_id FROM oauth_grants)`, cut.UTC())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *Store) CreateOAuthClient(ctx context.Context, c *OAuthClient) error {
	c.CreatedAt = time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `INSERT INTO oauth_clients(id,name,redirect_uris,created_at) VALUES(?,?,?,?)`, c.ID, c.Name, js(c.RedirectURIs), c.CreatedAt)
	return err
}

func (s *Store) GetOAuthClient(ctx context.Context, id string) (*OAuthClient, error) {
	c := &OAuthClient{}
	var uris string
	err := s.db.QueryRowContext(ctx, `SELECT id,name,redirect_uris,created_at FROM oauth_clients WHERE id=?`, id).Scan(&c.ID, &c.Name, &uris, &c.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(uris), &c.RedirectURIs); err != nil {
		return nil, err
	}
	return c, nil
}

func (s *Store) CreateOAuthCode(ctx context.Context, c *OAuthCode) error {
	now := time.Now().UTC()
	// Housekeeping: codes live seconds, keep them an hour for reuse detection, then drop them.
	_, _ = s.db.ExecContext(ctx, `DELETE FROM oauth_codes WHERE expires_at < ?`, now.Add(-time.Hour))
	_, err := s.db.ExecContext(ctx, `INSERT INTO oauth_codes(code_hash,client_id,redirect_uri,challenge,scope,resource,created_at,expires_at) VALUES(?,?,?,?,?,?,?,?)`,
		c.Hash, c.ClientID, c.RedirectURI, c.Challenge, c.Scope, c.Resource, c.CreatedAt.UTC(), c.ExpiresAt.UTC())
	return err
}

func insertTokens(ctx context.Context, tx *sql.Tx, toks []*OAuthToken) error {
	for _, t := range toks {
		if _, err := tx.ExecContext(ctx, `INSERT INTO oauth_tokens(token_hash,grant_id,kind,scope,created_at,expires_at) VALUES(?,?,?,?,?,?)`,
			t.Hash, t.GrantID, t.Kind, t.Scope, t.CreatedAt.UTC(), t.ExpiresAt.UTC()); err != nil {
			return err
		}
	}
	return nil
}

// ExchangeOAuthCode redeems an authorization code in one transaction. The code is burned on first sight,
// whatever happens next. A code seen a second time deletes the grant issued from it (RFC 6749 §4.1.2) and
// returns ErrOAuthReused. validate checks the request against the code; when it passes, the grant g (ID,
// expiry) is created from the code and tokens(grantID) are stored.
func (s *Store) ExchangeOAuthCode(ctx context.Context, hash string, validate func(*OAuthCode) error, g *OAuthGrant, tokens func(*OAuthGrant) []*OAuthToken) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	c := &OAuthCode{Hash: hash}
	var used sql.NullTime
	var grantID sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT client_id,redirect_uri,challenge,scope,resource,created_at,expires_at,used_at,grant_id FROM oauth_codes WHERE code_hash=?`, hash).
		Scan(&c.ClientID, &c.RedirectURI, &c.Challenge, &c.Scope, &c.Resource, &c.CreatedAt, &c.ExpiresAt, &used, &grantID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if used.Valid {
		if grantID.Valid {
			if _, err := tx.ExecContext(ctx, `DELETE FROM oauth_grants WHERE id=?`, grantID.String); err != nil {
				return err
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return ErrOAuthReused
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `UPDATE oauth_codes SET used_at=? WHERE code_hash=?`, now, hash); err != nil {
		return err
	}
	if verr := validate(c); verr != nil {
		if err := tx.Commit(); err != nil {
			return err
		}
		return verr
	}
	g.ClientID, g.Scope, g.Resource, g.CreatedAt = c.ClientID, c.Scope, c.Resource, now
	if _, err := tx.ExecContext(ctx, `INSERT INTO oauth_grants(id,client_id,scope,resource,created_at,expires_at,last_used_at) VALUES(?,?,?,?,?,?,?)`,
		g.ID, g.ClientID, g.Scope, g.Resource, g.CreatedAt, g.ExpiresAt.UTC(), now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE oauth_codes SET grant_id=? WHERE code_hash=?`, g.ID, hash); err != nil {
		return err
	}
	if err := insertTokens(ctx, tx, tokens(g)); err != nil {
		return err
	}
	return tx.Commit()
}

// RotateOAuthRefresh trades a refresh token for new tokens in one transaction. The old refresh token stays
// stored as used: if it comes back, the whole grant is deleted and ErrOAuthReused is returned.
// validate may refuse the request (expiry, client, scope) without burning the token.
func (s *Store) RotateOAuthRefresh(ctx context.Context, hash string, validate func(*OAuthGrant, *OAuthToken) error, tokens func(*OAuthGrant) []*OAuthToken) (*OAuthGrant, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	t := &OAuthToken{Hash: hash}
	g := &OAuthGrant{}
	var used, last sql.NullTime
	err = tx.QueryRowContext(ctx, `SELECT t.grant_id,t.kind,t.scope,t.created_at,t.expires_at,t.used_at,g.client_id,g.scope,g.resource,g.created_at,g.expires_at,g.last_used_at
		FROM oauth_tokens t JOIN oauth_grants g ON g.id=t.grant_id WHERE t.token_hash=? AND t.kind=?`, hash, TokenRefresh).
		Scan(&t.GrantID, &t.Kind, &t.Scope, &t.CreatedAt, &t.ExpiresAt, &used, &g.ClientID, &g.Scope, &g.Resource, &g.CreatedAt, &g.ExpiresAt, &last)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	g.ID = t.GrantID
	if used.Valid {
		if _, err := tx.ExecContext(ctx, `DELETE FROM oauth_grants WHERE id=?`, g.ID); err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return nil, ErrOAuthReused
	}
	if err := validate(g, t); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `UPDATE oauth_tokens SET used_at=? WHERE token_hash=?`, now, hash); err != nil {
		return nil, err
	}
	// Housekeeping: drop this grant's expired tokens (a used refresh token is kept until it expires).
	if _, err := tx.ExecContext(ctx, `DELETE FROM oauth_tokens WHERE grant_id=? AND expires_at < ?`, g.ID, now); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE oauth_grants SET last_used_at=? WHERE id=?`, now, g.ID); err != nil {
		return nil, err
	}
	if err := insertTokens(ctx, tx, tokens(g)); err != nil {
		return nil, err
	}
	return g, tx.Commit()
}

// OAuthAccessToken looks up a live access token. Expired tokens and tokens of expired grants are not found.
func (s *Store) OAuthAccessToken(ctx context.Context, hash string, now time.Time) (*OAuthAccess, error) {
	a := &OAuthAccess{}
	var exp, gexp time.Time
	var last sql.NullTime
	err := s.db.QueryRowContext(ctx, `SELECT t.grant_id,t.scope,t.expires_at,g.client_id,g.resource,g.expires_at,g.last_used_at
		FROM oauth_tokens t JOIN oauth_grants g ON g.id=t.grant_id WHERE t.token_hash=? AND t.kind=?`, hash, TokenAccess).
		Scan(&a.GrantID, &a.Scope, &exp, &a.ClientID, &a.Resource, &gexp, &last)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if !now.Before(exp) || !now.Before(gexp) {
		return nil, ErrNotFound
	}
	if !last.Valid || now.Sub(last.Time) > time.Minute { // "last used", at most one write a minute
		_, _ = s.db.ExecContext(ctx, `UPDATE oauth_grants SET last_used_at=? WHERE id=?`, now.UTC(), a.GrantID)
	}
	return a, nil
}

// RevokeOAuthToken deletes the grant of an access or refresh token (RFC 7009: the app is disconnected).
// clientID, when set, must be the client the token was issued to. An unknown token is not an error.
func (s *Store) RevokeOAuthToken(ctx context.Context, hash, clientID string) error {
	var grantID, owner string
	err := s.db.QueryRowContext(ctx, `SELECT g.id,g.client_id FROM oauth_tokens t JOIN oauth_grants g ON g.id=t.grant_id WHERE t.token_hash=?`, hash).Scan(&grantID, &owner)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if clientID != "" && clientID != owner {
		return ErrOAuthClient
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM oauth_grants WHERE id=?`, grantID)
	return err
}

// ListOAuthGrants returns the live connections, newest first. Expired grants are deleted on the way.
func (s *Store) ListOAuthGrants(ctx context.Context) ([]*OAuthGrant, error) {
	_, _ = s.db.ExecContext(ctx, `DELETE FROM oauth_grants WHERE expires_at < ?`, time.Now().UTC())
	rows, err := s.db.QueryContext(ctx, `SELECT g.id,g.client_id,c.name,g.scope,g.resource,g.created_at,g.expires_at,g.last_used_at
		FROM oauth_grants g JOIN oauth_clients c ON c.id=g.client_id ORDER BY g.created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*OAuthGrant{}
	for rows.Next() {
		g := &OAuthGrant{}
		var last sql.NullTime
		if err := rows.Scan(&g.ID, &g.ClientID, &g.ClientName, &g.Scope, &g.Resource, &g.CreatedAt, &g.ExpiresAt, &last); err != nil {
			return nil, err
		}
		if last.Valid {
			t := last.Time
			g.LastUsedAt = &t
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// DeleteOAuthGrant revokes a connection and every token issued under it.
func (s *Store) DeleteOAuthGrant(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM oauth_grants WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
