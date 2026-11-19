package ui

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Seeyko/relay/internal/store"
)

// ---------- minimal CBOR encoder for the software authenticator ----------

func cborHead(major byte, n uint64) []byte {
	switch {
	case n < 24:
		return []byte{major<<5 | byte(n)}
	case n < 256:
		return []byte{major<<5 | 24, byte(n)}
	default:
		b := []byte{major<<5 | 25, 0, 0}
		binary.BigEndian.PutUint16(b[1:], uint16(n))
		return b
	}
}

func cborInt(v int) []byte {
	if v >= 0 {
		return cborHead(0, uint64(v))
	}
	return cborHead(1, uint64(-1-v))
}
func cborBytes(b []byte) []byte  { return append(cborHead(2, uint64(len(b))), b...) }
func cborText(s string) []byte   { return append(cborHead(3, uint64(len(s))), s...) }
func cborMap(n int) []byte       { return cborHead(5, uint64(n)) }
func cat(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

type softKey struct {
	priv   *ecdsa.PrivateKey
	credID []byte
	count  uint32
	rpID   string
	origin string
}

func newSoftKey(rpID, origin string) *softKey {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	id := make([]byte, 16)
	_, _ = rand.Read(id)
	return &softKey{priv: k, credID: id, rpID: rpID, origin: origin}
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func unb64(s string) []byte {
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
	if err != nil {
		panic(err)
	}
	return b
}

func (k *softKey) clientData(typ, challenge string) []byte {
	b, _ := json.Marshal(map[string]any{"type": typ, "challenge": challenge, "origin": k.origin, "crossOrigin": false})
	return b
}

func (k *softKey) authData(attested bool) []byte {
	h := sha256.Sum256([]byte(k.rpID))
	flags := byte(0x01 | 0x04) // UP | UV
	if attested {
		flags |= 0x40
	}
	k.count++
	cnt := make([]byte, 4)
	binary.BigEndian.PutUint32(cnt, k.count)
	ad := cat(h[:], []byte{flags}, cnt)
	if attested {
		x := k.priv.PublicKey.X.FillBytes(make([]byte, 32))
		y := k.priv.PublicKey.Y.FillBytes(make([]byte, 32))
		cose := cat(cborMap(5), cborInt(1), cborInt(2), cborInt(3), cborInt(-7), cborInt(-1), cborInt(1), cborInt(-2), cborBytes(x), cborInt(-3), cborBytes(y))
		l := make([]byte, 2)
		binary.BigEndian.PutUint16(l, uint16(len(k.credID)))
		ad = cat(ad, make([]byte, 16), l, k.credID, cose)
	}
	return ad
}

func (k *softKey) create(challenge string) map[string]any {
	ad := k.authData(true)
	att := cat(cborMap(3), cborText("fmt"), cborText("none"), cborText("attStmt"), cborMap(0), cborText("authData"), cborBytes(ad))
	return map[string]any{
		"id": b64(k.credID), "rawId": b64(k.credID), "type": "public-key",
		"response": map[string]any{"attestationObject": b64(att), "clientDataJSON": b64(k.clientData("webauthn.create", challenge))},
	}
}

func (k *softKey) get(challenge string, userHandle []byte) map[string]any {
	ad := k.authData(false)
	cd := k.clientData("webauthn.get", challenge)
	ch := sha256.Sum256(cd)
	digest := sha256.Sum256(cat(ad, ch[:]))
	sig, _ := ecdsa.SignASN1(rand.Reader, k.priv, digest[:])
	return map[string]any{
		"id": b64(k.credID), "rawId": b64(k.credID), "type": "public-key",
		"response": map[string]any{"authenticatorData": b64(ad), "clientDataJSON": b64(cd), "signature": b64(sig), "userHandle": b64(userHandle)},
	}
}

// ---------- harness ----------

const testToken = "admin-token-0123456789"

type harness struct {
	t      *testing.T
	srv    *httptest.Server
	st     *store.Store
	client *http.Client
	origin string
	ui     *UI
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	srv := httptest.NewUnstartedServer(nil)
	st, err := store.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	// http://localhost is the one non-https origin allowed (cookies then drop Secure and the __Host- prefix).
	base := "http://localhost"
	u, err := New(base, st, testToken, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	u.Register(mux)
	srv.Config.Handler = mux
	srv.Start()
	t.Cleanup(srv.Close)
	jar, _ := cookiejar.New(nil)
	return &harness{t: t, srv: srv, st: st, client: &http.Client{Jar: jar}, origin: base, ui: u}
}

func (h *harness) do(method, path string, body any, hdr map[string]string) (int, map[string]any) {
	h.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, h.srv.URL+path, rd)
	req.Header.Set("Origin", h.origin)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	resp, err := h.client.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func challengeOf(t *testing.T, opts map[string]any) string {
	pk, ok := opts["publicKey"].(map[string]any)
	if !ok {
		t.Fatalf("no publicKey in %v", opts)
	}
	return pk["challenge"].(string)
}

func (h *harness) register(k *softKey) {
	h.t.Helper()
	auth := map[string]string{"Authorization": "Bearer " + testToken}
	code, opts := h.do("POST", "/ui/api/register/begin", map[string]any{"name": "test key"}, auth)
	if code != 200 {
		h.t.Fatalf("register/begin: %d %v", code, opts)
	}
	pk := opts["publicKey"].(map[string]any)
	sel := pk["authenticatorSelection"].(map[string]any)
	if sel["userVerification"] != "required" || sel["residentKey"] != "required" {
		h.t.Fatalf("authenticator selection must require UV and a resident key: %v", sel)
	}
	code, res := h.do("POST", "/ui/api/register/finish", k.create(challengeOf(h.t, opts)), auth)
	if code != 200 {
		h.t.Fatalf("register/finish: %d %v", code, res)
	}
}

func (h *harness) login(k *softKey) (int, map[string]any) {
	h.t.Helper()
	code, opts := h.do("POST", "/ui/api/login/begin", map[string]any{}, nil)
	if code != 200 {
		h.t.Fatalf("login/begin: %d %v", code, opts)
	}
	return h.do("POST", "/ui/api/login/finish", k.get(challengeOf(h.t, opts), ownerID), nil)
}

// ---------- tests ----------

func TestPasskeyFlow(t *testing.T) {
	h := newHarness(t)
	k := newSoftKey("localhost", h.origin)

	if code, _ := h.do("GET", "/ui/api/calendar?from=2026-01-01T00:00:00Z&to=2026-01-08T00:00:00Z", nil, nil); code != 401 {
		t.Fatalf("calendar without session: want 401, got %d", code)
	}
	if code, _ := h.do("POST", "/ui/api/register/begin", map[string]any{}, map[string]string{"Authorization": "Bearer wrong-token-0000000000"}); code != 401 {
		t.Fatalf("register with a wrong token: want 401, got %d", code)
	}
	if code, _ := h.do("POST", "/ui/api/register/begin", map[string]any{}, nil); code != 401 {
		t.Fatalf("register without token: want 401, got %d", code)
	}

	h.register(k)
	pks, _ := h.st.ListPasskeys(t.Context())
	if len(pks) != 1 || pks[0].Name != "test key" {
		t.Fatalf("passkey not stored: %+v", pks)
	}

	// A login signed by another key with the same credential id must fail.
	impostor := newSoftKey("localhost", h.origin)
	impostor.credID = k.credID
	if code, _ := h.login(impostor); code != 401 {
		t.Fatalf("impostor login: want 401, got %d", code)
	}

	if code, res := h.login(k); code != 200 {
		t.Fatalf("login: %d %v", code, res)
	}
	code, me := h.do("GET", "/ui/api/me", nil, nil)
	if code != 200 || me["logged_in"] != true {
		t.Fatalf("me after login: %d %v", code, me)
	}

	// Calendar returns posts with their account and media.
	ctx := t.Context()
	acc := &store.Account{Provider: "x", Name: "Example", Handle: "example"}
	if err := h.st.CreateAccount(ctx, acc); err != nil {
		t.Fatal(err)
	}
	m := &store.Media{Filename: "a.jpg", Mime: "image/jpeg", Size: 3, Path: "/data/media/med_abc.jpg"}
	if err := h.st.CreateMedia(ctx, m); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 1, 3, 9, 30, 0, 0, time.UTC)
	p := &store.Post{GroupID: "grp_1", AccountID: acc.ID, Status: store.StatusScheduled, Parts: []string{"hello <script>"}, Media: []string{m.ID}, ScheduledAt: &at}
	if err := h.st.CreatePost(ctx, p); err != nil {
		t.Fatal(err)
	}
	code, cal := h.do("GET", "/ui/api/calendar?from=2026-01-01T00:00:00Z&to=2026-01-08T00:00:00Z", nil, nil)
	if code != 200 {
		t.Fatalf("calendar: %d %v", code, cal)
	}
	posts := cal["posts"].([]any)
	if len(posts) != 1 {
		t.Fatalf("want 1 post, got %v", cal)
	}
	got := posts[0].(map[string]any)
	if got["provider"] != "x" || got["handle"] != "example" || got["status"] != "scheduled" {
		t.Fatalf("bad post: %v", got)
	}
	if media := got["media"].([]any); len(media) != 1 || media[0].(map[string]any)["url"] != "/media/med_abc.jpg" {
		t.Fatalf("bad media: %v", got["media"])
	}

	// A range over 45 days is refused.
	if code, _ := h.do("GET", "/ui/api/calendar?from=2026-01-01T00:00:00Z&to=2026-03-01T00:00:00Z", nil, nil); code != 400 {
		t.Fatalf("huge range: want 400, got %d", code)
	}

	// Logout kills the session server-side.
	if code, _ := h.do("POST", "/ui/api/logout", map[string]any{}, nil); code != 204 {
		t.Fatalf("logout: %d", code)
	}
	if code, _ := h.do("GET", "/ui/api/calendar?from=2026-01-01T00:00:00Z&to=2026-01-08T00:00:00Z", nil, nil); code != 401 {
		t.Fatalf("calendar after logout: want 401, got %d", code)
	}
}

func TestPasskeyProtections(t *testing.T) {
	h := newHarness(t)
	k := newSoftKey("localhost", h.origin)
	h.register(k)

	// Cross-origin POSTs are refused before anything else.
	if code, _ := h.do("POST", "/ui/api/login/begin", map[string]any{}, map[string]string{"Origin": "https://evil.example"}); code != 403 {
		t.Fatalf("cross-origin: want 403, got %d", code)
	}
	if code, _ := h.do("POST", "/ui/api/login/begin", map[string]any{}, map[string]string{"Origin": ""}); code != 403 {
		t.Fatalf("no origin: want 403, got %d", code)
	}

	// A challenge can be used once only.
	_, opts := h.do("POST", "/ui/api/login/begin", map[string]any{}, nil)
	ch := challengeOf(t, opts)
	if code, _ := h.do("POST", "/ui/api/login/finish", k.get(ch, ownerID), nil); code != 200 {
		t.Fatalf("first use: %d", code)
	}
	if code, _ := h.do("POST", "/ui/api/login/finish", k.get(ch, ownerID), nil); code != 400 {
		t.Fatalf("replay: want 400, got %d", code)
	}

	// A response signed for another origin (phishing site) is rejected.
	phish := *k
	phish.origin = "https://relay-login.example"
	_, opts = h.do("POST", "/ui/api/login/begin", map[string]any{}, nil)
	if code, _ := h.do("POST", "/ui/api/login/finish", phish.get(challengeOf(t, opts), ownerID), nil); code != 401 {
		t.Fatalf("wrong origin: want 401, got %d", code)
	}
	k.count = phish.count

	// A sign counter that goes backwards (cloned key) is rejected.
	k.count = 0
	_, opts = h.do("POST", "/ui/api/login/begin", map[string]any{}, nil)
	if code, _ := h.do("POST", "/ui/api/login/finish", k.get(challengeOf(t, opts), ownerID), nil); code != 401 {
		t.Fatalf("cloned counter: want 401, got %d", code)
	}

	// Security headers are set on every dashboard response.
	resp, err := h.client.Get(h.srv.URL + "/ui/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	csp := resp.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src 'self'") || !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Fatalf("missing CSP: %q", csp)
	}
	if resp.Header.Get("X-Frame-Options") != "DENY" {
		t.Fatal("missing X-Frame-Options")
	}
}

func TestRateLimit(t *testing.T) {
	h := newHarness(t)
	var last int
	for i := 0; i < 25; i++ {
		last, _ = h.do("POST", "/ui/api/login/begin", map[string]any{}, nil)
	}
	if last != 429 {
		t.Fatalf("want 429 after 20 attempts, got %d", last)
	}
}

func TestNewRequiresHTTPS(t *testing.T) {
	for _, base := range []string{"http://relay.example.com", "relay.example.com", ""} {
		if _, err := New(base, nil, testToken, slog.Default()); err == nil {
			t.Fatalf("%q should be refused", base)
		}
	}
	if _, err := New("https://relay.example.com", nil, testToken, slog.Default()); err != nil {
		t.Fatal(err)
	}
}

func TestRegistrationClosed(t *testing.T) {
	h := newHarness(t)
	k := newSoftKey("localhost", h.origin)
	h.register(k)
	h.ui.RegistrationClosed = true
	auth := map[string]string{"Authorization": "Bearer " + testToken}
	if code, _ := h.do("POST", "/ui/api/register/begin", map[string]any{}, auth); code != 403 {
		t.Fatalf("register/begin while closed: want 403, got %d", code)
	}
	if code, _ := h.do("POST", "/ui/api/register/finish", map[string]any{}, auth); code != 403 {
		t.Fatalf("register/finish while closed: want 403, got %d", code)
	}
	if _, me := h.do("GET", "/ui/api/me", nil, nil); me["registration_open"] != false {
		t.Fatalf("me should report registration closed: %v", me)
	}
	if code, res := h.login(k); code != 200 {
		t.Fatalf("existing passkey must still log in: %d %v", code, res)
	}
}
