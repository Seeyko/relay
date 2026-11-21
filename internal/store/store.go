// Package store persists accounts, posts, media, comments and agent tasks in SQLite.
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("not found")

type Store struct {
	db   *sql.DB
	ro   *sql.DB // read-only handle for agent SQL queries
	path string
}

func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite: one writer, keeps things simple and safe
	s := &Store{db: db, path: path}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	ro, err := sql.Open("sqlite", fmt.Sprintf("file:%s?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(5000)", path))
	if err != nil {
		db.Close()
		return nil, err
	}
	ro.SetMaxOpenConns(2)
	s.ro = ro
	return s, nil
}

func (s *Store) Close() error {
	s.ro.Close()
	return s.db.Close()
}

func NewID(prefix string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return prefix + "_" + hex.EncodeToString(b)
}

const schema = `
CREATE TABLE IF NOT EXISTS accounts (
  id TEXT PRIMARY KEY,
  provider TEXT NOT NULL,
  name TEXT NOT NULL,
  handle TEXT NOT NULL DEFAULT '',
  remote_user_id TEXT NOT NULL DEFAULT '',
  credentials BLOB,
  created_at TIMESTAMP NOT NULL
);
CREATE TABLE IF NOT EXISTS posts (
  id TEXT PRIMARY KEY,
  group_id TEXT NOT NULL,
  account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  status TEXT NOT NULL,
  parts TEXT NOT NULL,
  media TEXT NOT NULL DEFAULT '[]',
  options TEXT NOT NULL DEFAULT '{}',
  scheduled_at TIMESTAMP,
  published_at TIMESTAMP,
  remote_id TEXT NOT NULL DEFAULT '',
  remote_ids TEXT NOT NULL DEFAULT '[]',
  remote_url TEXT NOT NULL DEFAULT '',
  error TEXT NOT NULL DEFAULT '',
  attempts INTEGER NOT NULL DEFAULT 0,
  next_attempt_at TIMESTAMP,
  metrics TEXT NOT NULL DEFAULT '{}',
  metrics_at TIMESTAMP,
  source TEXT NOT NULL DEFAULT 'relay',
  created_at TIMESTAMP NOT NULL,
  updated_at TIMESTAMP NOT NULL
);
CREATE INDEX IF NOT EXISTS posts_due ON posts(status, scheduled_at);
CREATE UNIQUE INDEX IF NOT EXISTS posts_remote ON posts(account_id, remote_id) WHERE remote_id != '';
CREATE TABLE IF NOT EXISTS media (
  id TEXT PRIMARY KEY,
  filename TEXT NOT NULL,
  mime TEXT NOT NULL,
  size INTEGER NOT NULL,
  path TEXT NOT NULL,
  created_at TIMESTAMP NOT NULL
);
CREATE TABLE IF NOT EXISTS comments (
  account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  remote_id TEXT NOT NULL,
  post_id TEXT NOT NULL DEFAULT '',
  author TEXT NOT NULL DEFAULT '',
  text TEXT NOT NULL DEFAULT '',
  url TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMP,
  seen_at TIMESTAMP NOT NULL,
  replied INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (account_id, remote_id)
);
CREATE TABLE IF NOT EXISTS metric_snapshots (
  post_id TEXT NOT NULL REFERENCES posts(id) ON DELETE CASCADE,
  at TIMESTAMP NOT NULL,
  metrics TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS metric_snapshots_post ON metric_snapshots(post_id, at);
CREATE TABLE IF NOT EXISTS account_snapshots (
  account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  at TIMESTAMP NOT NULL,
  metrics TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS account_snapshots_acc ON account_snapshots(account_id, at);
CREATE TABLE IF NOT EXISTS agent_tasks (
  id TEXT PRIMARY KEY,
  account_id TEXT NOT NULL DEFAULT '',
  kind TEXT NOT NULL,
  payload TEXT NOT NULL DEFAULT '{}',
  status TEXT NOT NULL,
  result TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMP NOT NULL,
  claimed_at TIMESTAMP,
  done_at TIMESTAMP
);
CREATE TABLE IF NOT EXISTS passkeys (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL DEFAULT '',
  credential TEXT NOT NULL,
  created_at TIMESTAMP NOT NULL,
  last_used_at TIMESTAMP
);
CREATE TABLE IF NOT EXISTS ui_sessions (
  token_hash TEXT PRIMARY KEY,
  passkey_id TEXT NOT NULL REFERENCES passkeys(id) ON DELETE CASCADE,
  created_at TIMESTAMP NOT NULL,
  expires_at TIMESTAMP NOT NULL
);
`

// columns added after the first release: table, column, definition.
var addedColumns = [][3]string{
	{"posts", "planned_at", "TIMESTAMP"},
}

func (s *Store) migrate() error {
	if _, err := s.db.Exec(schema); err != nil {
		return err
	}
	if _, err := s.db.Exec(feedbackSchema); err != nil {
		return err
	}
	for _, c := range addedColumns {
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name=?`, c[0], c[1]).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			if _, err := s.db.Exec(`ALTER TABLE ` + c[0] + ` ADD COLUMN ` + c[1] + ` ` + c[2]); err != nil {
				return fmt.Errorf("migrate %s.%s: %w", c[0], c[1], err)
			}
		}
	}
	return nil
}

// ---------- Accounts ----------

type Account struct {
	ID           string    `json:"id"`
	Provider     string    `json:"provider"`
	Name         string    `json:"name"`
	Handle       string    `json:"handle"`
	RemoteUserID string    `json:"remote_user_id"`
	Credentials  []byte    `json:"-"` // encrypted blob
	CreatedAt    time.Time `json:"created_at"`
}

func (s *Store) CreateAccount(ctx context.Context, a *Account) error {
	if a.ID == "" {
		a.ID = NewID("acc")
	}
	a.CreatedAt = time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `INSERT INTO accounts(id,provider,name,handle,remote_user_id,credentials,created_at) VALUES(?,?,?,?,?,?,?)`,
		a.ID, a.Provider, a.Name, a.Handle, a.RemoteUserID, a.Credentials, a.CreatedAt)
	return err
}

func (s *Store) UpdateAccount(ctx context.Context, a *Account) error {
	_, err := s.db.ExecContext(ctx, `UPDATE accounts SET name=?, handle=?, remote_user_id=?, credentials=? WHERE id=?`,
		a.Name, a.Handle, a.RemoteUserID, a.Credentials, a.ID)
	return err
}

func (s *Store) GetAccount(ctx context.Context, id string) (*Account, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id,provider,name,handle,remote_user_id,credentials,created_at FROM accounts WHERE id=?`, id)
	a := &Account{}
	if err := row.Scan(&a.ID, &a.Provider, &a.Name, &a.Handle, &a.RemoteUserID, &a.Credentials, &a.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return a, nil
}

func (s *Store) ListAccounts(ctx context.Context) ([]*Account, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,provider,name,handle,remote_user_id,credentials,created_at FROM accounts ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Account
	for rows.Next() {
		a := &Account{}
		if err := rows.Scan(&a.ID, &a.Provider, &a.Name, &a.Handle, &a.RemoteUserID, &a.Credentials, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) DeleteAccount(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM accounts WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---------- Posts ----------

const (
	StatusDraft      = "draft"
	StatusScheduled  = "scheduled"
	StatusPublishing = "publishing"
	StatusPublished  = "published"
	StatusFailed     = "failed"
)

type Post struct {
	ID            string           `json:"id"`
	GroupID       string           `json:"group_id"`
	AccountID     string           `json:"account_id"`
	Status        string           `json:"status"`
	Parts         []string         `json:"parts"`   // one element = single post, several = thread
	Media         []string         `json:"media"`   // media ids attached to the first part
	Options       map[string]any   `json:"options"` // provider-specific (title, privacy, ...)
	ScheduledAt   *time.Time       `json:"scheduled_at,omitempty"`
	PlannedAt     *time.Time       `json:"planned_at,omitempty"` // drafts only: intended time; approve_post schedules it then, it never publishes by itself
	PublishedAt   *time.Time       `json:"published_at,omitempty"`
	RemoteID      string           `json:"remote_id,omitempty"`
	RemoteIDs     []string         `json:"remote_ids,omitempty"`
	RemoteURL     string           `json:"remote_url,omitempty"`
	Error         string           `json:"error,omitempty"`
	Attempts      int              `json:"attempts"`
	NextAttemptAt *time.Time       `json:"next_attempt_at,omitempty"`
	Metrics       map[string]int64 `json:"metrics"`
	MetricsAt     *time.Time       `json:"metrics_at,omitempty"`
	Source        string           `json:"source"`
	CreatedAt     time.Time        `json:"created_at"`
	UpdatedAt     time.Time        `json:"updated_at"`
}

const postCols = `id,group_id,account_id,status,parts,media,options,scheduled_at,published_at,remote_id,remote_ids,remote_url,error,attempts,next_attempt_at,metrics,metrics_at,source,created_at,updated_at,planned_at`

type scanner interface{ Scan(...any) error }

func scanPost(r scanner) (*Post, error) {
	p := &Post{}
	var parts, media, opts, rids, metrics string
	var sched, pub, next, mAt, planned sql.NullTime
	if err := r.Scan(&p.ID, &p.GroupID, &p.AccountID, &p.Status, &parts, &media, &opts, &sched, &pub, &p.RemoteID, &rids, &p.RemoteURL, &p.Error, &p.Attempts, &next, &metrics, &mAt, &p.Source, &p.CreatedAt, &p.UpdatedAt, &planned); err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(parts), &p.Parts)
	_ = json.Unmarshal([]byte(media), &p.Media)
	_ = json.Unmarshal([]byte(opts), &p.Options)
	_ = json.Unmarshal([]byte(rids), &p.RemoteIDs)
	_ = json.Unmarshal([]byte(metrics), &p.Metrics)
	p.ScheduledAt = nt(sched)
	p.PublishedAt = nt(pub)
	p.NextAttemptAt = nt(next)
	p.MetricsAt = nt(mAt)
	p.PlannedAt = nt(planned)
	if p.Media == nil {
		p.Media = []string{}
	}
	if p.Options == nil {
		p.Options = map[string]any{}
	}
	if p.Metrics == nil {
		p.Metrics = map[string]int64{}
	}
	return p, nil
}

func nt(t sql.NullTime) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time.UTC()
	return &v
}

func js(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func (s *Store) CreatePost(ctx context.Context, p *Post) error {
	now := time.Now().UTC()
	if p.ID == "" {
		p.ID = NewID("post")
	}
	if p.GroupID == "" {
		p.GroupID = p.ID
	}
	if p.Source == "" {
		p.Source = "relay"
	}
	if p.Media == nil {
		p.Media = []string{}
	}
	if p.Options == nil {
		p.Options = map[string]any{}
	}
	if p.Metrics == nil {
		p.Metrics = map[string]int64{}
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = now
	}
	p.UpdatedAt = now
	_, err := s.db.ExecContext(ctx, `INSERT INTO posts(`+postCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		p.ID, p.GroupID, p.AccountID, p.Status, js(p.Parts), js(p.Media), js(p.Options), p.ScheduledAt, p.PublishedAt,
		p.RemoteID, js(p.RemoteIDs), p.RemoteURL, p.Error, p.Attempts, p.NextAttemptAt, js(p.Metrics), p.MetricsAt, p.Source, p.CreatedAt, p.UpdatedAt, p.PlannedAt)
	return err
}

// UpsertImported stores a post fetched from a network's history; existing rows only get metrics refreshed.
// For an existing row, p.ID, p.Source and p.Media are set from the stored post.
func (s *Store) UpsertImported(ctx context.Context, p *Post) (created bool, err error) {
	var id, source, media string
	err = s.db.QueryRowContext(ctx, `SELECT id, source, media FROM posts WHERE account_id=? AND remote_id=?`, p.AccountID, p.RemoteID).Scan(&id, &source, &media)
	if err == nil {
		p.ID, p.Source, p.Media = id, source, []string{}
		_ = json.Unmarshal([]byte(media), &p.Media)
		_, err = s.db.ExecContext(ctx, `UPDATE posts SET metrics=?, metrics_at=?, updated_at=? WHERE id=?`, js(p.Metrics), time.Now().UTC(), time.Now().UTC(), id)
		return false, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	p.Source = "imported"
	p.Status = StatusPublished
	now := time.Now().UTC()
	p.MetricsAt = &now
	return true, s.CreatePost(ctx, p)
}

// SetImportedMedia attaches media to an imported post that has none yet. It never touches a post
// created in Relay nor one that already has media; it returns false when nothing was changed.
func (s *Store) SetImportedMedia(ctx context.Context, postID string, media []string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE posts SET media=?, updated_at=? WHERE id=? AND source='imported' AND media IN ('[]','null','')`, js(media), time.Now().UTC(), postID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (s *Store) UpdatePost(ctx context.Context, p *Post) error {
	p.UpdatedAt = time.Now().UTC()
	res, err := s.db.ExecContext(ctx, `UPDATE posts SET status=?,parts=?,media=?,options=?,scheduled_at=?,published_at=?,remote_id=?,remote_ids=?,remote_url=?,error=?,attempts=?,next_attempt_at=?,metrics=?,metrics_at=?,updated_at=?,planned_at=? WHERE id=?`,
		p.Status, js(p.Parts), js(p.Media), js(p.Options), p.ScheduledAt, p.PublishedAt, p.RemoteID, js(p.RemoteIDs), p.RemoteURL, p.Error, p.Attempts, p.NextAttemptAt, js(p.Metrics), p.MetricsAt, p.UpdatedAt, p.PlannedAt, p.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) GetPost(ctx context.Context, id string) (*Post, error) {
	p, err := scanPost(s.db.QueryRowContext(ctx, `SELECT `+postCols+` FROM posts WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return p, err
}

func (s *Store) DeletePost(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM posts WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

type PostFilter struct {
	Status    string
	AccountID string
	GroupID   string
	Source    string
	From, To  *time.Time // on published_at, scheduled_at, planned_at (drafts) or created_at
	Limit     int
	Offset    int
}

func (s *Store) ListPosts(ctx context.Context, f PostFilter) ([]*Post, error) {
	var where []string
	var args []any
	if f.Status != "" {
		where = append(where, "status=?")
		args = append(args, f.Status)
	}
	if f.AccountID != "" {
		where = append(where, "account_id=?")
		args = append(args, f.AccountID)
	}
	if f.GroupID != "" {
		where = append(where, "group_id=?")
		args = append(args, f.GroupID)
	}
	if f.Source != "" {
		where = append(where, "source=?")
		args = append(args, f.Source)
	}
	if f.From != nil {
		where = append(where, "COALESCE(published_at, scheduled_at, planned_at, created_at) >= ?")
		args = append(args, f.From.UTC())
	}
	if f.To != nil {
		where = append(where, "COALESCE(published_at, scheduled_at, planned_at, created_at) < ?")
		args = append(args, f.To.UTC())
	}
	q := `SELECT ` + postCols + ` FROM posts`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY COALESCE(published_at, scheduled_at, planned_at, created_at) DESC"
	if f.Limit <= 0 || f.Limit > 1000 {
		f.Limit = 100
	}
	q += fmt.Sprintf(" LIMIT %d OFFSET %d", f.Limit, f.Offset)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Post{}
	for rows.Next() {
		p, err := scanPost(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ClaimDue atomically moves due scheduled posts to "publishing" and returns them.
func (s *Store) ClaimDue(ctx context.Context, now time.Time, max int) ([]*Post, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM posts WHERE status=? AND scheduled_at <= ? AND (next_attempt_at IS NULL OR next_attempt_at <= ?) ORDER BY scheduled_at LIMIT ?`,
		StatusScheduled, now.UTC(), now.UTC(), max)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	var out []*Post
	for _, id := range ids {
		res, err := s.db.ExecContext(ctx, `UPDATE posts SET status=?, updated_at=? WHERE id=? AND status=?`, StatusPublishing, now.UTC(), id, StatusScheduled)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			p, err := s.GetPost(ctx, id)
			if err != nil {
				return nil, err
			}
			out = append(out, p)
		}
	}
	return out, nil
}

// RecoverStuck puts posts left in "publishing" (crash mid-publish) back to scheduled.
func (s *Store) RecoverStuck(ctx context.Context, olderThan time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE posts SET status=? WHERE status=? AND updated_at < ?`, StatusScheduled, StatusPublishing, olderThan.UTC())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---------- Media ----------

type Media struct {
	ID        string    `json:"id"`
	Filename  string    `json:"filename"`
	Mime      string    `json:"mime"`
	Size      int64     `json:"size"`
	Path      string    `json:"-"`
	CreatedAt time.Time `json:"created_at"`
}

// S3Prefix marks a media Path that lives in the object store; the rest is the object key.
const S3Prefix = "s3:"

// InBucket reports whether the file is in the object store rather than on local disk.
func (m *Media) InBucket() bool { return strings.HasPrefix(m.Path, S3Prefix) }

// Key is the public file name (med_<id>.<ext>), the same locally and in the bucket.
func (m *Media) Key() string {
	p := strings.ReplaceAll(strings.TrimPrefix(m.Path, S3Prefix), "\\", "/")
	return p[strings.LastIndex(p, "/")+1:]
}

func (s *Store) CreateMedia(ctx context.Context, m *Media) error {
	if m.ID == "" {
		m.ID = NewID("med")
	}
	m.CreatedAt = time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `INSERT INTO media(id,filename,mime,size,path,created_at) VALUES(?,?,?,?,?,?)`, m.ID, m.Filename, m.Mime, m.Size, m.Path, m.CreatedAt)
	return err
}

// DeleteMedia removes a media row (the caller removes the file).
func (s *Store) DeleteMedia(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM media WHERE id=?`, id)
	return err
}

func (s *Store) GetMedia(ctx context.Context, id string) (*Media, error) {
	m := &Media{}
	err := s.db.QueryRowContext(ctx, `SELECT id,filename,mime,size,path,created_at FROM media WHERE id=?`, id).Scan(&m.ID, &m.Filename, &m.Mime, &m.Size, &m.Path, &m.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return m, err
}

func (s *Store) ListMedia(ctx context.Context) ([]*Media, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,filename,mime,size,path,created_at FROM media ORDER BY created_at DESC LIMIT 500`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Media{}
	for rows.Next() {
		m := &Media{}
		if err := rows.Scan(&m.ID, &m.Filename, &m.Mime, &m.Size, &m.Path, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ---------- Comments ----------

type Comment struct {
	AccountID string     `json:"account_id"`
	RemoteID  string     `json:"remote_id"`
	PostID    string     `json:"post_id"`
	Author    string     `json:"author"`
	Text      string     `json:"text"`
	URL       string     `json:"url,omitempty"`
	CreatedAt *time.Time `json:"created_at,omitempty"`
	SeenAt    time.Time  `json:"seen_at"`
	Replied   bool       `json:"replied"`
}

// UpsertComment returns true when the comment was not known before.
func (s *Store) UpsertComment(ctx context.Context, c *Comment) (bool, error) {
	c.SeenAt = time.Now().UTC()
	res, err := s.db.ExecContext(ctx, `INSERT INTO comments(account_id,remote_id,post_id,author,text,url,created_at,seen_at) VALUES(?,?,?,?,?,?,?,?)
	  ON CONFLICT(account_id,remote_id) DO NOTHING`, c.AccountID, c.RemoteID, c.PostID, c.Author, c.Text, c.URL, c.CreatedAt, c.SeenAt)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

func (s *Store) MarkReplied(ctx context.Context, accountID, remoteID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE comments SET replied=1 WHERE account_id=? AND remote_id=?`, accountID, remoteID)
	return err
}

type CommentFilter struct {
	AccountID   string
	PostID      string
	OnlyPending bool
	Limit       int
}

func (s *Store) ListComments(ctx context.Context, f CommentFilter) ([]*Comment, error) {
	var where []string
	var args []any
	if f.AccountID != "" {
		where = append(where, "account_id=?")
		args = append(args, f.AccountID)
	}
	if f.PostID != "" {
		where = append(where, "post_id=?")
		args = append(args, f.PostID)
	}
	if f.OnlyPending {
		where = append(where, "replied=0")
	}
	q := `SELECT account_id,remote_id,post_id,author,text,url,created_at,seen_at,replied FROM comments`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	if f.Limit <= 0 || f.Limit > 1000 {
		f.Limit = 200
	}
	q += fmt.Sprintf(" ORDER BY COALESCE(created_at, seen_at) DESC LIMIT %d", f.Limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Comment{}
	for rows.Next() {
		c := &Comment{}
		var ca sql.NullTime
		if err := rows.Scan(&c.AccountID, &c.RemoteID, &c.PostID, &c.Author, &c.Text, &c.URL, &ca, &c.SeenAt, &c.Replied); err != nil {
			return nil, err
		}
		c.CreatedAt = nt(ca)
		out = append(out, c)
	}
	return out, rows.Err()
}

// ---------- Agent tasks (browser agent queue) ----------

type AgentTask struct {
	ID        string         `json:"id"`
	AccountID string         `json:"account_id"`
	Kind      string         `json:"kind"`
	Payload   map[string]any `json:"payload"`
	Status    string         `json:"status"` // pending, claimed, done, failed
	Result    string         `json:"result,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
	ClaimedAt *time.Time     `json:"claimed_at,omitempty"`
	DoneAt    *time.Time     `json:"done_at,omitempty"`
}

func (s *Store) CreateTask(ctx context.Context, t *AgentTask) error {
	if t.ID == "" {
		t.ID = NewID("task")
	}
	t.Status = "pending"
	t.CreatedAt = time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `INSERT INTO agent_tasks(id,account_id,kind,payload,status,created_at) VALUES(?,?,?,?,?,?)`, t.ID, t.AccountID, t.Kind, js(t.Payload), t.Status, t.CreatedAt)
	return err
}

func scanTask(r scanner) (*AgentTask, error) {
	t := &AgentTask{}
	var payload string
	var ca, da sql.NullTime
	if err := r.Scan(&t.ID, &t.AccountID, &t.Kind, &payload, &t.Status, &t.Result, &t.CreatedAt, &ca, &da); err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(payload), &t.Payload)
	t.ClaimedAt, t.DoneAt = nt(ca), nt(da)
	return t, nil
}

const taskCols = `id,account_id,kind,payload,status,result,created_at,claimed_at,done_at`

// ClaimTask hands the oldest pending task (or one claimed more than 10 minutes ago) to the agent.
func (s *Store) ClaimTask(ctx context.Context) (*AgentTask, error) {
	now := time.Now().UTC()
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM agent_tasks WHERE status='pending' OR (status='claimed' AND claimed_at < ?) ORDER BY created_at LIMIT 1`, now.Add(-10*time.Minute)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE agent_tasks SET status='claimed', claimed_at=? WHERE id=?`, now, id); err != nil {
		return nil, err
	}
	return s.GetTask(ctx, id)
}

func (s *Store) GetTask(ctx context.Context, id string) (*AgentTask, error) {
	t, err := scanTask(s.db.QueryRowContext(ctx, `SELECT `+taskCols+` FROM agent_tasks WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return t, err
}

func (s *Store) CompleteTask(ctx context.Context, id string, ok bool, result string) error {
	status := "done"
	if !ok {
		status = "failed"
	}
	res, err := s.db.ExecContext(ctx, `UPDATE agent_tasks SET status=?, result=?, done_at=? WHERE id=?`, status, result, time.Now().UTC(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) ListTasks(ctx context.Context, status string) ([]*AgentTask, error) {
	q := `SELECT ` + taskCols + ` FROM agent_tasks`
	var args []any
	if status != "" {
		q += ` WHERE status=?`
		args = append(args, status)
	}
	q += ` ORDER BY created_at DESC LIMIT 200`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*AgentTask{}
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ---------- Metric history ----------

type Snapshot struct {
	At      time.Time        `json:"at"`
	Metrics map[string]int64 `json:"metrics"`
}

func (s *Store) AddPostSnapshot(ctx context.Context, postID string, at time.Time, m map[string]int64) error {
	if len(m) == 0 {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO metric_snapshots(post_id,at,metrics) VALUES(?,?,?)`, postID, at.UTC(), js(m))
	return err
}

func (s *Store) AddAccountSnapshot(ctx context.Context, accountID string, at time.Time, m map[string]int64) error {
	if len(m) == 0 {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO account_snapshots(account_id,at,metrics) VALUES(?,?,?)`, accountID, at.UTC(), js(m))
	return err
}

func (s *Store) snapshots(ctx context.Context, q string, args ...any) ([]Snapshot, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Snapshot{}
	for rows.Next() {
		var sn Snapshot
		var m string
		if err := rows.Scan(&sn.At, &m); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(m), &sn.Metrics)
		out = append(out, sn)
	}
	return out, rows.Err()
}

func (s *Store) PostSnapshots(ctx context.Context, postID string) ([]Snapshot, error) {
	return s.snapshots(ctx, `SELECT at, metrics FROM metric_snapshots WHERE post_id=? ORDER BY at`, postID)
}

func (s *Store) AccountSnapshots(ctx context.Context, accountID string, since time.Time) ([]Snapshot, error) {
	return s.snapshots(ctx, `SELECT at, metrics FROM account_snapshots WHERE account_id=? AND at >= ? ORDER BY at`, accountID, since.UTC())
}

// LatestAccountMetrics returns the counters of the newest snapshot of an account (nil when there is none).
func (s *Store) LatestAccountMetrics(ctx context.Context, accountID string) (map[string]int64, error) {
	sn, err := s.snapshots(ctx, `SELECT at, metrics FROM account_snapshots WHERE account_id=? ORDER BY at DESC LIMIT 1`, accountID)
	if err != nil || len(sn) == 0 {
		return nil, err
	}
	return sn[0].Metrics, nil
}

// ---------- Read-only SQL for agents ----------

type QueryResult struct {
	Columns   []string `json:"columns"`
	Rows      [][]any  `json:"rows"`
	Truncated bool     `json:"truncated"`
}

var forbiddenSQL = []string{"attach", "detach", "pragma", "vacuum", "load_extension"}

// Query runs one read-only statement (SELECT / WITH) on a query_only connection.
// Account credentials are never readable.
func (s *Store) Query(ctx context.Context, q string, maxRows int) (*QueryResult, error) {
	lq := strings.ToLower(strings.TrimSpace(q))
	if !strings.HasPrefix(lq, "select") && !strings.HasPrefix(lq, "with") {
		return nil, errors.New("only SELECT or WITH queries are allowed")
	}
	if strings.Contains(strings.TrimRight(lq, "; \n\t"), ";") {
		return nil, errors.New("one statement only")
	}
	for _, f := range forbiddenSQL {
		if strings.Contains(lq, f) {
			return nil, fmt.Errorf("%q is not allowed", f)
		}
	}
	if strings.Contains(lq, "credentials") {
		return nil, errors.New("account credentials are not queryable")
	}
	if strings.Contains(lq, "passkeys") || strings.Contains(lq, "ui_sessions") || strings.Contains(lq, "sqlite_") {
		return nil, errors.New("dashboard login data is not queryable")
	}
	if maxRows <= 0 || maxRows > 1000 {
		maxRows = 200
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := s.ro.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	keep := []int{}
	res := &QueryResult{Rows: [][]any{}}
	for i, c := range cols {
		if strings.EqualFold(c, "credentials") { // SELECT * on accounts: hide the (encrypted) secrets
			continue
		}
		keep = append(keep, i)
		res.Columns = append(res.Columns, c)
	}
	for rows.Next() {
		if len(res.Rows) >= maxRows {
			res.Truncated = true
			break
		}
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		row := make([]any, 0, len(keep))
		for _, i := range keep {
			v := vals[i]
			if b, ok := v.([]byte); ok {
				v = string(b)
			}
			row = append(row, v)
		}
		res.Rows = append(res.Rows, row)
	}
	return res, rows.Err()
}

// Schema is shown to agents so they can write queries.
const Schema = `accounts(id, provider, name, handle, remote_user_id, created_at)
posts(id, group_id, account_id, status, parts JSON array, media JSON array, options JSON, scheduled_at, planned_at (drafts only), published_at,
      remote_id, remote_ids JSON, remote_url, error, attempts, metrics JSON (latest), metrics_at, source 'relay'|'imported', created_at)
metric_snapshots(post_id, at, metrics JSON)      -- one row per collection, for growth curves
account_snapshots(account_id, at, metrics JSON)  -- followers, following, posts over time
comments(account_id, remote_id, post_id, author, text, url, created_at, seen_at, replied 0|1)
media(id, filename, mime, size, created_at)
agent_tasks(id, account_id, kind, payload JSON, status, result, created_at, claimed_at, done_at)
feedback(id, kind, title, body, media JSON, context JSON, source, role, status, resolution, pr_url, created_at, updated_at)
Use json_extract(metrics, '$.like_count') to read a metric.`

// ---------- dashboard passkeys and sessions ----------

type Passkey struct {
	ID         string     `json:"id"` // base64url credential id
	Name       string     `json:"name"`
	Credential string     `json:"-"` // JSON-encoded webauthn credential (public key, sign count, flags)
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

func (s *Store) AddPasskey(ctx context.Context, p *Passkey) error {
	p.CreatedAt = time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `INSERT INTO passkeys(id,name,credential,created_at) VALUES(?,?,?,?)`, p.ID, p.Name, p.Credential, p.CreatedAt)
	return err
}

func (s *Store) ListPasskeys(ctx context.Context) ([]*Passkey, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,credential,created_at,last_used_at FROM passkeys ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Passkey{}
	for rows.Next() {
		p := &Passkey{}
		var last sql.NullTime
		if err := rows.Scan(&p.ID, &p.Name, &p.Credential, &p.CreatedAt, &last); err != nil {
			return nil, err
		}
		if last.Valid {
			t := last.Time
			p.LastUsedAt = &t
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// TouchPasskey stores the credential after a login (new sign count) and the time it was used.
func (s *Store) TouchPasskey(ctx context.Context, id, credential string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE passkeys SET credential=?, last_used_at=? WHERE id=?`, credential, time.Now().UTC(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeletePasskey(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM passkeys WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) CreateUISession(ctx context.Context, tokenHash, passkeyID string, expires time.Time) error {
	now := time.Now().UTC()
	_, _ = s.db.ExecContext(ctx, `DELETE FROM ui_sessions WHERE expires_at < ?`, now)
	_, err := s.db.ExecContext(ctx, `INSERT INTO ui_sessions(token_hash,passkey_id,created_at,expires_at) VALUES(?,?,?,?)`, tokenHash, passkeyID, now, expires.UTC())
	return err
}

// UISessionValid reports whether a session exists and has not expired.
func (s *Store) UISessionValid(ctx context.Context, tokenHash string) (bool, error) {
	var exp time.Time
	err := s.db.QueryRowContext(ctx, `SELECT expires_at FROM ui_sessions WHERE token_hash=?`, tokenHash).Scan(&exp)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return time.Now().Before(exp), nil
}

func (s *Store) DeleteUISession(ctx context.Context, tokenHash string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM ui_sessions WHERE token_hash=?`, tokenHash)
	return err
}
