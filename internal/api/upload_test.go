package api_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Seeyko/relay/internal/core"
	"github.com/Seeyko/relay/internal/store"
)

// put sends a file to an upload URL the way an agent does with curl: no Authorization header.
func put(t *testing.T, e *env, uploadURL string, body []byte) (int, []byte) {
	t.Helper()
	r, _ := http.NewRequest("PUT", e.srv.URL+strings.TrimPrefix(uploadURL, "https://relay.example"), bytes.NewReader(body))
	r.Header.Set("Content-Type", "video/mp4")
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

// An agent with only the MCP connection sends a video from its disk: create_media_upload gives a one-time
// URL, a plain PUT stores the file, finalize_media_upload returns the media, and schedule_post uses it.
func TestCreateMediaUploadFromLocalFile(t *testing.T) {
	e := newEnv(t)
	video := bytes.Repeat([]byte("mp4!"), 2048)

	text, isErr := e.callTool("create_media_upload", map[string]any{"filename": "clip.mp4", "size": len(video)})
	if isErr {
		t.Fatalf("create_media_upload: %s", text)
	}
	var up core.MediaUpload
	if err := json.Unmarshal([]byte(text), &up); err != nil {
		t.Fatalf("%v: %s", err, text)
	}
	if !strings.HasPrefix(up.ID, "med_") || up.Method != "PUT" || up.Headers["Content-Type"] != "video/mp4" ||
		!strings.HasPrefix(up.UploadURL, "https://relay.example/v1/uploads/") || !strings.Contains(up.Curl, up.UploadURL) {
		t.Fatalf("upload: %+v", up)
	}

	// Not sent yet: finalize says so.
	if text, isErr := e.callTool("finalize_media_upload", map[string]any{"id": up.ID}); !isErr || !strings.Contains(text, "PUT the file") {
		t.Fatalf("finalize before upload: %v %s", isErr, text)
	}
	// A truncated body is refused and the URL still works afterwards.
	if code, raw := put(t, e, up.UploadURL, video[:100]); code != 400 {
		t.Fatalf("short body: %d %s", code, raw)
	}
	// A longer body is refused too.
	if code, raw := put(t, e, up.UploadURL, append(append([]byte{}, video...), 'x')); code != 400 {
		t.Fatalf("long body: %d %s", code, raw)
	}
	if code, raw := put(t, e, up.UploadURL, video); code != 201 {
		t.Fatalf("upload: %d %s", code, raw)
	}
	// One file per URL.
	if code, raw := put(t, e, up.UploadURL, video); code != 403 {
		t.Fatalf("second upload: %d %s", code, raw)
	}

	text, isErr = e.callTool("finalize_media_upload", map[string]any{"id": up.ID})
	var m core.SharedMedia
	if isErr || json.Unmarshal([]byte(text), &m) != nil {
		t.Fatalf("finalize: %s", text)
	}
	if m.ID != up.ID || m.Mime != "video/mp4" || m.Size != int64(len(video)) || m.Filename != "clip.mp4" || !strings.HasPrefix(m.URL, "https://relay.example/media/med_") {
		t.Fatalf("media %+v", m)
	}
	// The stored bytes are the file.
	resp, err := http.Get(e.srv.URL + strings.TrimPrefix(m.URL, "https://relay.example"))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Equal(got, video) {
		t.Fatalf("stored %d bytes, want %d", len(got), len(video))
	}

	// The media id works in schedule_post.
	acc := connectFakeX(t, e, &xThreadServer{})
	code, _, raw := e.req("POST", "/v1/posts", map[string]any{"account_ids": []string{acc.ID}, "scheduled_at": "draft", "text": "hi", "media": []string{up.ID}})
	var posts []store.Post
	if code != 200 || json.Unmarshal(raw, &posts) != nil || len(posts) != 1 || len(posts[0].Media) != 1 || posts[0].Media[0] != up.ID {
		t.Fatalf("schedule: %d %s", code, raw)
	}
}

// Upload URLs cannot be forged or changed, and expire.
func TestMediaUploadURLGuards(t *testing.T) {
	e := newEnv(t)
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	e.svc.Now = func() time.Time { return now }

	if code, raw := put(t, e, "/v1/uploads/not-a-token", []byte("x")); code != 403 {
		t.Fatalf("forged: %d %s", code, raw)
	}
	text, isErr := e.callTool("create_media_upload", map[string]any{"filename": "a.jpg", "size": 3})
	var up core.MediaUpload
	if isErr || json.Unmarshal([]byte(text), &up) != nil {
		t.Fatalf("create: %s", text)
	}
	if up.Headers["Content-Type"] != "image/jpeg" || !up.ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("upload %+v", up)
	}
	// One changed character in the sealed token: refused.
	tampered := up.UploadURL[:len(up.UploadURL)-2] + map[bool]string{true: "AA", false: "BB"}[!strings.HasSuffix(up.UploadURL, "AA")]
	if code, raw := put(t, e, tampered, []byte("abc")); code != 403 {
		t.Fatalf("tampered: %d %s", code, raw)
	}
	now = now.Add(time.Hour + time.Second)
	if code, raw := put(t, e, up.UploadURL, []byte("abc")); code != 403 {
		t.Fatalf("expired: %d %s", code, raw)
	}

	// Bad requests.
	for _, args := range []map[string]any{
		{"filename": "notes.txt", "size": 3},
		{"filename": "a.mp4"},
		{"filename": "a.mp4", "size": 600 << 20},
		{"size": 3},
	} {
		if text, isErr := e.callTool("create_media_upload", args); !isErr {
			t.Fatalf("%v should fail: %s", args, text)
		}
	}

	// REST: same tools.
	code, body, raw := e.req("POST", "/v1/media/uploads", map[string]any{"filename": "b.png", "size": 4})
	if code != 200 || body["upload_url"] == nil {
		t.Fatalf("rest create: %d %s", code, raw)
	}
	if code, _, raw := e.req("POST", "/v1/media/"+body["id"].(string)+"/finalize", nil); code != 400 {
		t.Fatalf("rest finalize before upload: %d %s", code, raw)
	}
}
