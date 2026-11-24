package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Organizations split one Relay instance between several brands or apps: every account belongs to one
// organization, and everything hanging off an account (posts, comments, metrics, browser tasks) follows it.
// Media belong to the organization they were uploaded for.
//
// Who sees what:
//   - Owners, RELAY_API_TOKEN, RELAY_DRAFT_TOKEN and in-process calls (scheduler, collector) see every organization.
//   - Other members see the organizations listed for them in org_members.
//   - An OAuth app may be limited further to some organizations when it is approved.
//   - A caller can narrow any call to one organization (the org argument of tools, X-Relay-Org, ?org=).
//
// The scope travels in the context (WithOrgs) and the store applies it to every read and write that names an
// account, a post, a comment, a task or lists media, so a call outside the caller's organizations gets
// ErrNotFound, exactly like an id that does not exist.
const orgsSchema = `
CREATE TABLE IF NOT EXISTS orgs (
  id TEXT PRIMARY KEY,
  slug TEXT NOT NULL UNIQUE,
  name TEXT NOT NULL,
  created_at TIMESTAMP NOT NULL
);
CREATE TABLE IF NOT EXISTS org_members (
  org_id TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  PRIMARY KEY (org_id, user_id)
);
CREATE INDEX IF NOT EXISTS org_members_user ON org_members(user_id);
`

// DefaultOrgName names the organization that receives what existed before organizations (RELAY_DEFAULT_ORG).
var DefaultOrgName = "Default"

var (
	ErrOrgRequired = errors.New("several organizations exist: give org (id, slug or name, see list_organizations)")
	ErrOrgNotEmpty = errors.New("this organization still has accounts: move or remove them first")
	ErrOrgExists   = errors.New("an organization with this name or slug already exists")
	ErrOrgScoped   = errors.New("this call reads every organization: it is only open to owners and the API token, without org")
)

type Org struct {
	ID        string    `json:"id"`
	Slug      string    `json:"slug"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	Accounts  int       `json:"accounts"`
}

// ---------- scope ----------

type orgScopeKey struct{}

type orgScope struct{ ids []string }

// WithOrgs limits the store to these organization ids. A context that is already limited keeps only the
// organizations in both lists (it can only narrow). An empty list sees nothing.
func WithOrgs(ctx context.Context, ids []string) context.Context {
	if cur, ok := OrgsFrom(ctx); ok {
		keep := map[string]bool{}
		for _, id := range cur {
			keep[id] = true
		}
		var both []string
		for _, id := range ids {
			if keep[id] {
				both = append(both, id)
			}
		}
		ids = both
	}
	return context.WithValue(ctx, orgScopeKey{}, orgScope{ids: dedupe(ids)})
}

// OrgsFrom returns the organizations a context is limited to; limited is false when it sees all of them.
func OrgsFrom(ctx context.Context) (ids []string, limited bool) {
	s, ok := ctx.Value(orgScopeKey{}).(orgScope)
	if !ok {
		return nil, false
	}
	return append([]string{}, s.ids...), true
}

// Unscoped drops the organization limit (internal checks such as "is this remote account connected anywhere").
func Unscoped(ctx context.Context) context.Context {
	return context.WithValue(ctx, orgScopeKey{}, nil)
}

func dedupe(ids []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, id := range ids {
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// orgCond is " AND <col> IN (...)" for a column holding an organization id, "" when ctx sees everything.
func orgCond(ctx context.Context, col string) (string, []any) {
	ids, limited := OrgsFrom(ctx)
	if !limited {
		return "", nil
	}
	if len(ids) == 0 {
		return " AND 0", nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return " AND " + col + " IN (" + strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",") + ")", args
}

// accountCond is orgCond for a column holding an account id.
func accountCond(ctx context.Context, col string) (string, []any) {
	c, args := orgCond(ctx, "org_id")
	if c == "" {
		return "", nil
	}
	if c == " AND 0" {
		return c, nil
	}
	return " AND " + col + " IN (SELECT id FROM accounts WHERE 1=1" + c + ")", args
}

// ---------- migration ----------

// migrateOrgs makes sure at least one organization exists and gives it whatever has none yet: on an instance
// that existed before organizations, every account and media, and every member who is not an owner (so they
// keep seeing what they saw).
func (s *Store) migrateOrgs() error {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM orgs`).Scan(&n); err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var def string
	if n == 0 {
		def = NewID("org")
		name := strings.TrimSpace(DefaultOrgName)
		if name == "" {
			name = "Default"
		}
		slug := Slugify(name)
		if slug == "" {
			slug = "default"
		}
		if _, err := tx.Exec(`INSERT INTO orgs(id,slug,name,created_at) VALUES(?,?,?,?)`, def, slug, name, time.Now().UTC()); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO org_members(org_id,user_id) SELECT ?, id FROM users WHERE role<>?`, def, RoleOwner); err != nil {
			return err
		}
	} else if err := tx.QueryRow(`SELECT id FROM orgs ORDER BY created_at, id LIMIT 1`).Scan(&def); err != nil {
		return err
	}
	for _, q := range []string{`UPDATE accounts SET org_id=? WHERE org_id IS NULL OR org_id=''`, `UPDATE media SET org_id=? WHERE org_id IS NULL OR org_id=''`} {
		if _, err := tx.Exec(q, def); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ---------- organizations ----------

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

// Slugify turns a name into a lower-case slug ("My Little Garden" -> "my-little-garden").
func Slugify(name string) string {
	r := strings.NewReplacer("à", "a", "â", "a", "ä", "a", "é", "e", "è", "e", "ê", "e", "ë", "e", "î", "i", "ï", "i", "ô", "o", "ö", "o", "ù", "u", "û", "u", "ü", "u", "ç", "c")
	s := slugRe.ReplaceAllString(r.Replace(strings.ToLower(strings.TrimSpace(name))), "-")
	s = strings.Trim(s, "-")
	if len(s) > 48 {
		s = strings.Trim(s[:48], "-")
	}
	return s
}

// ListOrgs lists the organizations ctx may see, oldest first, with their number of accounts.
func (s *Store) ListOrgs(ctx context.Context) ([]*Org, error) {
	c, args := orgCond(ctx, "o.id")
	rows, err := s.db.QueryContext(ctx, `SELECT o.id,o.slug,o.name,o.created_at,(SELECT COUNT(*) FROM accounts a WHERE a.org_id=o.id)
FROM orgs o WHERE 1=1`+c+` ORDER BY o.created_at, o.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Org{}
	for rows.Next() {
		o := &Org{}
		if err := rows.Scan(&o.ID, &o.Slug, &o.Name, &o.CreatedAt, &o.Accounts); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// AllOrgIDs lists every organization id, whatever ctx may see.
func (s *Store) AllOrgIDs(ctx context.Context) ([]string, error) {
	os, err := s.ListOrgs(Unscoped(ctx))
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(os))
	for i, o := range os {
		ids[i] = o.ID
	}
	return ids, nil
}

// ResolveOrg finds an organization ctx may see by id, slug or name (case-insensitive).
func (s *Store) ResolveOrg(ctx context.Context, ref string) (*Org, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, ErrNotFound
	}
	os, err := s.ListOrgs(ctx)
	if err != nil {
		return nil, err
	}
	for _, o := range os {
		if o.ID == ref || strings.EqualFold(o.Slug, ref) || strings.EqualFold(o.Name, ref) || o.Slug == Slugify(ref) {
			return o, nil
		}
	}
	return nil, ErrNotFound
}

// ResolveOrgs resolves several references (see ResolveOrg) to ids.
func (s *Store) ResolveOrgs(ctx context.Context, refs []string) ([]string, error) {
	var ids []string
	for _, r := range refs {
		o, err := s.ResolveOrg(ctx, r)
		if err != nil {
			return nil, fmt.Errorf("organization %q: %w", r, err)
		}
		ids = append(ids, o.ID)
	}
	return dedupe(ids), nil
}

// TargetOrg is the organization a new account or media goes to: ref when given, else the only organization
// ctx sees. With several and no ref it is ErrOrgRequired.
func (s *Store) TargetOrg(ctx context.Context, ref string) (*Org, error) {
	if ref != "" {
		return s.ResolveOrg(ctx, ref)
	}
	os, err := s.ListOrgs(ctx)
	if err != nil {
		return nil, err
	}
	switch len(os) {
	case 1:
		return os[0], nil
	case 0:
		return nil, ErrNotFound
	}
	return nil, ErrOrgRequired
}

func (s *Store) CreateOrg(ctx context.Context, name, slug string) (*Org, error) {
	name = strings.Join(strings.Fields(name), " ")
	if name == "" || len(name) > 80 {
		return nil, errors.New("name is required (80 characters at most)")
	}
	if slug == "" {
		slug = name
	}
	slug = Slugify(slug)
	if slug == "" {
		return nil, errors.New("the name needs at least one letter or digit")
	}
	if err := s.orgNameFree(ctx, "", name, slug); err != nil {
		return nil, err
	}
	o := &Org{ID: NewID("org"), Slug: slug, Name: name, CreatedAt: time.Now().UTC()}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO orgs(id,slug,name,created_at) VALUES(?,?,?,?)`, o.ID, o.Slug, o.Name, o.CreatedAt); err != nil {
		return nil, err
	}
	return o, nil
}

func (s *Store) orgNameFree(ctx context.Context, self, name, slug string) error {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM orgs WHERE id<>? AND (slug=? OR lower(name)=lower(?))`, self, slug, name).Scan(&n)
	if err == nil && n > 0 {
		return ErrOrgExists
	}
	return err
}

// UpdateOrg renames an organization (and/or changes its slug). Empty values are kept.
func (s *Store) UpdateOrg(ctx context.Context, id, name, slug string) (*Org, error) {
	o, err := s.ResolveOrg(ctx, id)
	if err != nil {
		return nil, err
	}
	if name = strings.Join(strings.Fields(name), " "); name != "" {
		if len(name) > 80 {
			return nil, errors.New("80 characters at most")
		}
		o.Name = name
	}
	if slug != "" {
		if o.Slug = Slugify(slug); o.Slug == "" {
			return nil, errors.New("the slug needs at least one letter or digit")
		}
	}
	if err := s.orgNameFree(ctx, o.ID, o.Name, o.Slug); err != nil {
		return nil, err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE orgs SET name=?, slug=? WHERE id=?`, o.Name, o.Slug, o.ID)
	return o, err
}

// DeleteOrg removes an empty organization (no accounts). Its media go to no organization: only callers that see
// every organization list them.
func (s *Store) DeleteOrg(ctx context.Context, id string) error {
	o, err := s.ResolveOrg(ctx, id)
	if err != nil {
		return err
	}
	if o.Accounts > 0 {
		return ErrOrgNotEmpty
	}
	all, err := s.AllOrgIDs(ctx)
	if err != nil {
		return err
	}
	if len(all) <= 1 {
		return errors.New("the last organization cannot be removed")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE media SET org_id=NULL WHERE org_id=?`, o.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM orgs WHERE id=?`, o.ID); err != nil {
		return err
	}
	return tx.Commit()
}

// MoveAccount puts an account (with its posts, comments and metrics) in another organization. Both must be
// visible to ctx.
func (s *Store) MoveAccount(ctx context.Context, accountID, orgRef string) (*Account, error) {
	a, err := s.GetAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}
	o, err := s.ResolveOrg(ctx, orgRef)
	if err != nil {
		return nil, err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE accounts SET org_id=? WHERE id=?`, o.ID, a.ID); err != nil {
		return nil, err
	}
	return s.GetAccount(ctx, a.ID)
}

// ---------- members ----------

// UserOrgs returns the organizations a member sees: every one for an owner (full true), else their own list.
func (s *Store) UserOrgs(ctx context.Context, userID string) (ids []string, full bool, err error) {
	var role string
	if err := s.db.QueryRowContext(ctx, `SELECT role FROM users WHERE id=?`, userID).Scan(&role); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, ErrNotFound
		}
		return nil, false, err
	}
	if role == RoleOwner {
		return nil, true, nil
	}
	ids, err = s.memberOrgs(ctx, s.db, userID)
	return ids, false, err
}

type queryer interface {
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
}

func (s *Store) memberOrgs(ctx context.Context, db queryer, userID string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT m.org_id FROM org_members m JOIN orgs o ON o.id=m.org_id WHERE m.user_id=? ORDER BY o.created_at, o.id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// SetUserOrgs replaces the organizations of a member.
func (s *Store) SetUserOrgs(ctx context.Context, userID string, orgIDs []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := setUserOrgs(ctx, tx, userID, orgIDs); err != nil {
		return err
	}
	return tx.Commit()
}

func setUserOrgs(ctx context.Context, tx *sql.Tx, userID string, orgIDs []string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM org_members WHERE user_id=?`, userID); err != nil {
		return err
	}
	for _, id := range dedupe(orgIDs) {
		if _, err := tx.ExecContext(ctx, `INSERT INTO org_members(org_id,user_id) SELECT id, ? FROM orgs WHERE id=?`, userID, id); err != nil {
			return err
		}
	}
	return nil
}

// fillUserOrgs sets u.Orgs (ids) for each user; owners get nil (every organization).
func (s *Store) fillUserOrgs(ctx context.Context, us ...*User) error {
	if len(us) == 0 {
		return nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT m.user_id, m.org_id FROM org_members m JOIN orgs o ON o.id=m.org_id ORDER BY o.created_at, o.id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	by := map[string][]string{}
	for rows.Next() {
		var u, o string
		if err := rows.Scan(&u, &o); err != nil {
			return err
		}
		by[u] = append(by[u], o)
	}
	for _, u := range us {
		if u.Role == RoleOwner {
			u.Orgs = nil
			continue
		}
		u.Orgs = by[u.ID]
		if u.Orgs == nil {
			u.Orgs = []string{}
		}
	}
	return rows.Err()
}

// Subset reports whether every id of a is in b.
func Subset(a, b []string) bool {
	in := map[string]bool{}
	for _, id := range b {
		in[id] = true
	}
	for _, id := range a {
		if !in[id] {
			return false
		}
	}
	return true
}

func orgsJSON(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	sorted := append([]string{}, ids...)
	sort.Strings(sorted)
	b, _ := json.Marshal(sorted)
	return string(b)
}

func parseOrgs(s string) []string {
	if s == "" {
		return nil
	}
	var ids []string
	_ = json.Unmarshal([]byte(s), &ids)
	return ids
}
