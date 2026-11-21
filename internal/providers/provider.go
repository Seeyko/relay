// Package providers defines the interface every social network connector implements.
package providers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
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
	Parts []string // len > 1 means a thread
	// Media is the media of the first part (backward compatible: connectors that only know one
	// set of media read this).
	Media []MediaFile
	// PartMedia is the media of every part, aligned with Parts (PartMedia[0] is Media). It is nil or
	// shorter than Parts when the later parts have no media. Read it through MediaOf.
	PartMedia [][]MediaFile
	Options   map[string]any
}

// MediaOf returns the media of part i (0 is the first part).
func (r PublishRequest) MediaOf(i int) []MediaFile {
	if i == 0 {
		return r.Media
	}
	if i > 0 && i < len(r.PartMedia) {
		return r.PartMedia[i]
	}
	return nil
}

// LaterMedia returns the number of the first part after the first one that has media (2 for the second
// part), or 0 when only the first part has media.
func (r PublishRequest) LaterMedia() int {
	for i := 1; i < len(r.Parts) && i < len(r.PartMedia); i++ {
		if len(r.PartMedia[i]) > 0 {
			return i + 1
		}
	}
	return 0
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
	// Visuals are the post's images, in order: the photos of a photo post or carousel, the cover
	// (thumbnail) of a video. Never the video file itself. The URLs are usually signed CDN links that
	// expire, so Relay downloads them during the import.
	Visuals []Visual
}

// Visual is one image of an imported post.
type Visual struct {
	URL string
	// Cover is true when the image is the cover/thumbnail of a video, not a photo of the post.
	Cover bool
}

// AddVisual appends a visual when its URL is set and not already listed.
func (h *HistoryItem) AddVisual(url string, cover bool) {
	if url == "" {
		return
	}
	for _, v := range h.Visuals {
		if v.URL == url {
			return
		}
	}
	h.Visuals = append(h.Visuals, Visual{URL: url, Cover: cover})
}

// Identity is what we learn about the account when it is connected.
type Identity struct {
	RemoteUserID string
	Handle       string
	Name         string
	// AvatarURL is the profile picture when the identity call returns it (often a signed CDN link).
	AvatarURL string
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

// Avatarer is implemented by clients that can give the account's profile picture (or Page logo).
// AvatarURL returns a downloadable image URL, usually a CDN link that expires: Relay downloads it.
// "" with a nil error means the network has no picture for the account, or the token lacks the scope.
// Clients reuse the URL seen by an earlier Identify or AccountMetrics call when they can, so the
// daily refresh costs no extra request.
type Avatarer interface {
	AvatarURL(ctx context.Context) (string, error)
}

// Deleter is implemented by clients that can delete a published post from the network.
// remoteIDs are the ids Relay stored for the post, in order: the post itself, then the other parts of its
// thread (replies on X and Threads, comments of the account on Facebook and LinkedIn). The connector
// removes all of them. An id the network no longer knows counts as deleted, so a second call after a
// partial failure finishes the job.
type Deleter interface {
	Delete(ctx context.Context, remoteIDs []string) error
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
	// MediaPerPart says the connector attaches media to every part of a thread (X, Threads). Without it,
	// a later part with media is refused before publishing (its comments or replies are text only).
	MediaPerPart bool
	// Count (optional) counts characters the way the network does for its text limit (default CharCount).
	// The dashboard preview uses it for its counters.
	Count func(text string) int
}

// DocumentTitle is the title of a document post built from images (LinkedIn options.images_as_document):
// options.document_title, else the first line of the first part, cut to 100 characters.
func DocumentTitle(opts map[string]any, parts []string) string {
	t, _ := opts["document_title"].(string)
	t = strings.TrimSpace(t)
	if t == "" && len(parts) > 0 {
		t = strings.TrimSpace(parts[0])
		if i := strings.IndexByte(t, '\n'); i >= 0 {
			t = strings.TrimSpace(t[:i])
		}
		if r := []rune(t); len(r) > MaxDocumentTitle {
			t = strings.TrimSpace(string(r[:MaxDocumentTitle]))
		}
	}
	if t == "" {
		t = "Document"
	}
	return t
}

// MaxDocumentTitle is the longest title Relay gives a document post.
const MaxDocumentTitle = 100

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

// NotFound says whether err is an HTTP 404 from the network.
func NotFound(err error) bool {
	var he *HTTPError
	return errors.As(err, &he) && he.Status == http.StatusNotFound
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
