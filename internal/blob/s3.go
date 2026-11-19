// Package blob stores media files in an S3-compatible bucket (Cloudflare R2, AWS S3, MinIO, Backblaze B2...).
// It speaks plain S3 REST with AWS Signature V4, path-style, so it needs no SDK.
package blob

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

type S3 struct {
	Endpoint  string // e.g. https://<account>.r2.cloudflarestorage.com
	Bucket    string
	Region    string // "auto" for R2
	AccessKey string
	SecretKey string
	HTTP      *http.Client
	now       func() time.Time
}

func (s *S3) Validate() error {
	u, err := url.Parse(s.Endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return errors.New("S3 endpoint must be an absolute http(s) URL")
	}
	if s.Bucket == "" || s.AccessKey == "" || s.SecretKey == "" {
		return errors.New("S3 bucket, access key id and secret access key are required")
	}
	if s.Region == "" {
		s.Region = "auto"
	}
	return nil
}

func (s *S3) objectURL(key string) string {
	return strings.TrimRight(s.Endpoint, "/") + "/" + s.Bucket + "/" + escapePath(key)
}

func escapePath(key string) string {
	parts := strings.Split(key, "/")
	for i, p := range parts {
		parts[i] = uriEncode(p, true)
	}
	return strings.Join(parts, "/")
}

// Put uploads body (size bytes, whose SHA-256 is payloadSHA in hex).
func (s *S3) Put(ctx context.Context, key string, body io.Reader, size int64, payloadSHA, contentType string) error {
	req, err := http.NewRequestWithContext(ctx, "PUT", s.objectURL(key), body)
	if err != nil {
		return err
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", contentType)
	s.sign(req, payloadSHA)
	resp, err := s.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("s3 put %s: http %d: %s", key, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

// Get streams an object. rangeHeader (optional) is forwarded as is. The caller closes the body.
func (s *S3) Get(ctx context.Context, key, rangeHeader string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", s.objectURL(key), nil)
	if err != nil {
		return nil, err
	}
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
	s.sign(req, emptySHA)
	resp, err := s.client().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 && resp.StatusCode != 206 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		resp.Body.Close()
		return nil, &StatusError{Code: resp.StatusCode, Body: strings.TrimSpace(string(b))}
	}
	return resp, nil
}

func (s *S3) Delete(ctx context.Context, key string) error {
	req, err := http.NewRequestWithContext(ctx, "DELETE", s.objectURL(key), nil)
	if err != nil {
		return err
	}
	s.sign(req, emptySHA)
	resp, err := s.client().Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 && resp.StatusCode != 404 {
		return fmt.Errorf("s3 delete %s: http %d", key, resp.StatusCode)
	}
	return nil
}

type StatusError struct {
	Code int
	Body string
}

func (e *StatusError) Error() string { return fmt.Sprintf("s3: http %d: %s", e.Code, e.Body) }

func (s *S3) client() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	return http.DefaultClient
}

// ---------- AWS Signature Version 4 ----------
// https://docs.aws.amazon.com/AmazonS3/latest/API/sig-v4-header-based-auth.html

const emptySHA = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func (s *S3) sign(req *http.Request, payloadSHA string) {
	now := time.Now
	if s.now != nil {
		now = s.now
	}
	t := now().UTC()
	amzDate := t.Format("20060102T150405Z")
	day := t.Format("20060102")
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadSHA)

	// Canonical headers: host plus every header we set (all lower-cased, sorted).
	hdrs := map[string]string{"host": req.URL.Host}
	for k, v := range req.Header {
		hdrs[strings.ToLower(k)] = strings.Join(strings.Fields(strings.Join(v, ",")), " ")
	}
	names := make([]string, 0, len(hdrs))
	for k := range hdrs {
		names = append(names, k)
	}
	sort.Strings(names)
	var ch strings.Builder
	for _, k := range names {
		ch.WriteString(k + ":" + hdrs[k] + "\n")
	}
	signed := strings.Join(names, ";")

	canonical := strings.Join([]string{
		req.Method,
		escapePathRaw(req.URL),
		canonicalQuery(req.URL.Query()),
		ch.String(),
		signed,
		payloadSHA,
	}, "\n")
	scope := day + "/" + s.Region + "/s3/aws4_request"
	sum := sha256.Sum256([]byte(canonical))
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(sum[:])

	k := hmacSHA([]byte("AWS4"+s.SecretKey), day)
	k = hmacSHA(k, s.Region)
	k = hmacSHA(k, "s3")
	k = hmacSHA(k, "aws4_request")
	sig := hex.EncodeToString(hmacSHA(k, toSign))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+s.AccessKey+"/"+scope+", SignedHeaders="+signed+", Signature="+sig)
}

func escapePathRaw(u *url.URL) string {
	p := u.EscapedPath()
	if p == "" {
		return "/"
	}
	return p
}

func canonicalQuery(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vs := append([]string(nil), q[k]...)
		sort.Strings(vs)
		for _, v := range vs {
			parts = append(parts, uriEncode(k, false)+"="+uriEncode(v, false))
		}
	}
	return strings.Join(parts, "&")
}

func uriEncode(s string, keepSlash bool) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && keepSlash:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func hmacSHA(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}
