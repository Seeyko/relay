package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

const feedbackSchema = `
CREATE TABLE IF NOT EXISTS feedback (
  id TEXT PRIMARY KEY,
  kind TEXT NOT NULL,
  title TEXT NOT NULL,
  body TEXT NOT NULL,
  media TEXT NOT NULL DEFAULT '[]',
  context TEXT NOT NULL DEFAULT '{}',
  source TEXT NOT NULL DEFAULT '',
  role TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL,
  resolution TEXT NOT NULL DEFAULT '',
  pr_url TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMP NOT NULL,
  updated_at TIMESTAMP NOT NULL
);
CREATE INDEX IF NOT EXISTS feedback_status ON feedback(status, created_at);
`

// Feedback statuses: open → triaged → in_progress → done | rejected | duplicate.
var FeedbackStatuses = []string{"open", "triaged", "in_progress", "done", "rejected", "duplicate"}

// Feedback is a request from an agent (or a human) for something Relay cannot do yet,
// or a bug report. Agents that maintain Relay read the open ones and turn them into PRs.
type Feedback struct {
	ID         string         `json:"id"`
	Kind       string         `json:"kind"` // missing_feature, bug, improvement, other
	Title      string         `json:"title"`
	Body       string         `json:"body"`
	Media      []string       `json:"media"` // media ids
	Context    map[string]any `json:"context,omitempty"`
	Source     string         `json:"source"` // mcp, rest
	Role       string         `json:"role"`   // admin, drafter
	Status     string         `json:"status"`
	Resolution string         `json:"resolution,omitempty"`
	PRURL      string         `json:"pr_url,omitempty"`
	CreatedAt  time.Time      `json:"created_at"`
	UpdatedAt  time.Time      `json:"updated_at"`
}

const feedbackCols = `id,kind,title,body,media,context,source,role,status,resolution,pr_url,created_at,updated_at`

func scanFeedback(r scanner) (*Feedback, error) {
	f := &Feedback{}
	var media, ctx string
	if err := r.Scan(&f.ID, &f.Kind, &f.Title, &f.Body, &media, &ctx, &f.Source, &f.Role, &f.Status, &f.Resolution, &f.PRURL, &f.CreatedAt, &f.UpdatedAt); err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(media), &f.Media)
	_ = json.Unmarshal([]byte(ctx), &f.Context)
	if f.Media == nil {
		f.Media = []string{}
	}
	return f, nil
}

func (s *Store) CreateFeedback(ctx context.Context, f *Feedback) error {
	if f.ID == "" {
		f.ID = NewID("fb")
	}
	if f.Media == nil {
		f.Media = []string{}
	}
	f.Status = "open"
	f.CreatedAt = time.Now().UTC()
	f.UpdatedAt = f.CreatedAt
	_, err := s.db.ExecContext(ctx, `INSERT INTO feedback(`+feedbackCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		f.ID, f.Kind, f.Title, f.Body, js(f.Media), js(f.Context), f.Source, f.Role, f.Status, f.Resolution, f.PRURL, f.CreatedAt, f.UpdatedAt)
	return err
}

func (s *Store) GetFeedback(ctx context.Context, id string) (*Feedback, error) {
	f, err := scanFeedback(s.db.QueryRowContext(ctx, `SELECT `+feedbackCols+` FROM feedback WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return f, err
}

// ListFeedback returns feedback with one of the statuses (all when empty), oldest first.
func (s *Store) ListFeedback(ctx context.Context, statuses []string, limit int) ([]*Feedback, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := `SELECT ` + feedbackCols + ` FROM feedback`
	var args []any
	if len(statuses) > 0 {
		q += ` WHERE status IN (?` + strings.Repeat(",?", len(statuses)-1) + `)`
		for _, st := range statuses {
			args = append(args, st)
		}
	}
	q += ` ORDER BY created_at LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Feedback
	for rows.Next() {
		f, err := scanFeedback(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (s *Store) UpdateFeedback(ctx context.Context, f *Feedback) error {
	f.UpdatedAt = time.Now().UTC()
	res, err := s.db.ExecContext(ctx, `UPDATE feedback SET status=?, resolution=?, pr_url=?, updated_at=? WHERE id=?`, f.Status, f.Resolution, f.PRURL, f.UpdatedAt, f.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
