// Package providers defines the interface every social network connector implements.
package providers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"
)

// ErrUnsupported is returned when a network does not offer a capability through its API.
var ErrUnsupported = errors.New("not supported by this provider")

// ErrInvalidPost marks content the network will never accept (no retry).
var ErrInvalidPost = errors.New("invalid post")

// Invalid builds an ErrInvalidPost error with a message.
func Invalid(msg string) error { return fmt.Errorf("%w: %s", ErrInvalidPost, msg) }

// MediaFile is a local media file ready to upload.
type MediaFile struct {
	ID       string
	Filename string
	Mime     string
	Size     int64
	Open     func() (io.ReadCloser, error)
	// PublicURL is where the network can download the file (needed by Instagram, which pulls media).
	PublicURL string
	Alt       string
}

// PublishRequest is one post (or thread) for one account.
type PublishRequest struct {
	Parts   []string // len > 1 means a thread
	Media   []MediaFile
	Options map[string]any
}

type PublishResult struct {
	RemoteID  string   // id of the first post
	RemoteIDs []string // all ids for a thread
	URL       string
}

type Comment struct {
	RemoteID  string
	Author    string
	Text      string
	URL       string
	CreatedAt *time.Time
}

type HistoryItem struct {
	RemoteID  string
	Text      string
	URL       string
	CreatedAt time.Time
	Metrics   map[string]int64
	// Kind classifies the post when the network tells us: text, image, video, carousel, reply, repost, quote, article.
	Kind string
}

// Identity is what we learn about the account when it is connected.
type Identity struct {
	RemoteUserID string
	Handle       string
	Name         string
}

// Client is a connector bound to one account's credentials.
type Client interface {
	Identify(ctx context.Context) (*Identity, error)
	Publish(ctx context.Context, req PublishRequest) (*PublishResult, error)
	Metrics(ctx context.Context, remoteIDs []string) (map[string]map[string]int64, error)
	Comments(ctx context.Context, remoteID string) ([]Comment, error)
	Reply(ctx context.Context, toRemoteID, text string) (*PublishResult, error)
	History(ctx context.Context, since time.Time, max int) ([]HistoryItem, error)
}

// AccountStatser is implemented by clients that can report account-level counters
// (followers, following, total posts...). Keys should use the normalized names when possible:
// followers, following, posts, likes_total, views_total.
type AccountStatser interface {
	AccountMetrics(ctx context.Context) (map[string]int64, error)
}

// Factory builds a client from decrypted credentials. persist lets a client save refreshed tokens.
type Factory struct {
	Name        string
	Description string
	// CredentialFields documents what the account creation call must provide.
	CredentialFields []string
	New              func(creds map[string]string, persist func(map[string]string) error, hc *http.Client) (Client, error)
	// Validate checks a post before it is scheduled (character limits, required media...).
	// It returns human-readable problems; nil means OK. Optional.
	Validate func(req PublishRequest) []string
}

// CharCount counts characters the way most networks do (Unicode code points).
func CharCount(s string) int { return len([]rune(s)) }

var registry = map[string]Factory{}

func Register(f Factory) { registry[f.Name] = f }

func Get(name string) (Factory, error) {
	f, ok := registry[name]
	if !ok {
		return Factory{}, fmt.Errorf("unknown provider %q", name)
	}
	return f, nil
}

func All() []Factory {
	out := make([]Factory, 0, len(registry))
	for _, f := range registry {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// HTTPError carries a non-2xx response from a network API.
type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("http %d: %s", e.Status, truncate(e.Body, 500))
}

// Retryable says whether the scheduler should try again later.
func Retryable(err error) bool {
	var he *HTTPError
	if errors.As(err, &he) {
		return he.Status == 429 || he.Status >= 500
	}
	return !errors.Is(err, ErrUnsupported) && !errors.Is(err, ErrInvalidPost)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// ReadBody reads a response and returns an HTTPError on non-2xx statuses.
func ReadBody(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &HTTPError{Status: resp.StatusCode, Body: string(b)}
	}
	return b, nil
}
