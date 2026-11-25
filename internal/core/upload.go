package core

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/Seeyko/relay/internal/store"
)

// uploadTTL is how long an upload URL from CreateMediaUpload accepts the file.
const uploadTTL = time.Hour

// MediaUpload is a one-time URL an agent sends a local file to, with a plain HTTP PUT and no token: for an
// agent that only has the MCP connection (no API key to call POST /v1/media) and a file on its disk.
type MediaUpload struct {
	// ID is the media id the file gets once sent (give it to finalize_media_upload, then to schedule_post).
	ID        string            `json:"id"`
	UploadURL string            `json:"upload_url"`
	Method    string            `json:"method"`
	Headers   map[string]string `json:"headers"`
	Size      int64             `json:"size"`
	ExpiresAt time.Time         `json:"expires_at"`
	Curl      string            `json:"curl"`
}

// uploadClaim is sealed (encrypted and authenticated with the instance key) into the upload URL, so the
// URL needs no server state and cannot be forged or changed.
type uploadClaim struct {
	ID       string `json:"id"`
	Filename string `json:"f"`
	Mime     string `json:"m"`
	Size     int64  `json:"s"`
	OrgID    string `json:"o,omitempty"`
	Expires  int64  `json:"e"`
}

// ErrUploadDenied is a bad, expired or already used upload URL (HTTP 403).
var ErrUploadDenied = errors.New("upload url is invalid, expired or already used: call create_media_upload again")

// CreateMediaUpload prepares the upload of one file of exactly size bytes.
func (s *Service) CreateMediaUpload(ctx context.Context, filename, mimeType string, size int64) (*MediaUpload, error) {
	filename = filepath.Base(strings.TrimSpace(filename))
	if filename == "" || filename == "." || filename == "/" {
		return nil, invalid("filename is required (e.g. clip.mp4)")
	}
	mt, err := mediaType(filename, strings.TrimSpace(mimeType))
	if err != nil {
		return nil, err
	}
	if size <= 0 {
		return nil, invalid("size is required: the exact file size in bytes")
	}
	if size > maxMedia {
		return nil, invalid("media larger than 512 MB")
	}
	if s.BaseURL == "" {
		return nil, invalid("uploads need the public URL of this Relay instance (RELAY_BASE_URL): use POST /v1/media instead")
	}
	c := uploadClaim{ID: store.NewID("med"), Filename: filename, Mime: mt, Size: size, Expires: s.now().Add(uploadTTL).Unix()}
	// The media lands in the caller's organization, as with POST /v1/media.
	if ids, limited := store.OrgsFrom(ctx); limited && len(ids) == 1 {
		c.OrgID = ids[0]
	}
	sealed, err := s.Box.SealJSON(c)
	if err != nil {
		return nil, err
	}
	u := strings.TrimRight(s.BaseURL, "/") + "/v1/uploads/" + base64.RawURLEncoding.EncodeToString(sealed)
	return &MediaUpload{
		ID: c.ID, UploadURL: u, Method: "PUT", Headers: map[string]string{"Content-Type": mt}, Size: size,
		ExpiresAt: time.Unix(c.Expires, 0).UTC(),
		Curl:      fmt.Sprintf("curl -sf -X PUT -H 'Content-Type: %s' --data-binary @%s '%s'", mt, shellQuote(filename), u),
	}, nil
}

func shellQuote(s string) string {
	if strings.ContainsAny(s, " '\"$`\\!*?;&|<>()") {
		return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
	}
	return s
}

// ReceiveMediaUpload stores the body of a PUT to an upload URL. The body must have the announced size.
func (s *Service) ReceiveMediaUpload(ctx context.Context, token string, body io.Reader) (*store.Media, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return nil, ErrUploadDenied
	}
	var c uploadClaim
	if err := s.Box.OpenJSON(raw, &c); err != nil || !strings.HasPrefix(c.ID, "med_") {
		return nil, ErrUploadDenied
	}
	if s.now().Unix() > c.Expires {
		return nil, ErrUploadDenied
	}
	if _, err := s.Store.GetMedia(ctx, c.ID); err == nil {
		return nil, ErrUploadDenied // one file per URL
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	sr := &sizeReader{r: io.LimitReader(body, c.Size+1)}
	m, err := s.saveMedia(ctx, sr, c.Filename, c.Mime, c.ID, c.OrgID)
	if err != nil {
		return nil, err
	}
	if sr.n != c.Size {
		s.deleteMedia(ctx, m.ID) // a truncated or different file: refuse it, the URL stays usable
		return nil, invalid("received %d bytes, announced %d: send the whole file", sr.n, c.Size)
	}
	return m, nil
}

type sizeReader struct {
	r io.Reader
	n int64
}

func (s *sizeReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	s.n += int64(n)
	return n, err
}

// FinalizeMediaUpload checks that the file of an upload arrived and returns the media with its public url.
func (s *Service) FinalizeMediaUpload(ctx context.Context, id string) (*SharedMedia, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, invalid("id is required (the id returned by create_media_upload)")
	}
	m, err := s.Store.GetMedia(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, invalid("media %s has not arrived: PUT the file to its upload_url first", id)
	}
	if err != nil {
		return nil, err
	}
	return &SharedMedia{ID: m.ID, Filename: m.Filename, Mime: m.Mime, Size: m.Size, URL: s.ShareURL(m)}, nil
}
