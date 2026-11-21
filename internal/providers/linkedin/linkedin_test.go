package linkedin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Seeyko/relay/internal/providers"
)

const org = "urn:li:organization:42"

type fake struct {
	t     *testing.T
	srv   *httptest.Server
	mu    sync.Mutex
	steps []string
	h     func(w http.ResponseWriter, r *http.Request, body []byte) bool
}

// newFake starts a server; h returns false for unexpected requests.
func newFake(t *testing.T, h func(w http.ResponseWriter, r *http.Request, body []byte) bool) *fake {
	t.Helper()
	f := &fake{t: t, h: h}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if r.URL.Path != "/oauth" {
			if r.Header.Get("X-Restli-Protocol-Version") != "2.0.0" && !strings.HasPrefix(r.URL.Path, "/upload/") {
				t.Errorf("%s %s: missing Rest.li 2.0 header", r.Method, r.URL.Path)
			}
			if strings.HasPrefix(r.URL.Path, "/rest/") && r.Header.Get("LinkedIn-Version") != DefaultVersion {
				t.Errorf("%s %s: LinkedIn-Version %q", r.Method, r.URL.Path, r.Header.Get("LinkedIn-Version"))
			}
			if strings.HasPrefix(r.URL.Path, "/v2/") && r.Header.Get("LinkedIn-Version") != "" {
				t.Errorf("%s: /v2 must not get a LinkedIn-Version", r.URL.Path)
			}
		}
		if !f.h(w, r, b) {
			t.Errorf("unexpected %s %s?%s", r.Method, r.URL.EscapedPath(), r.URL.RawQuery)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fake) step(s string) {
	f.mu.Lock()
	f.steps = append(f.steps, s)
	f.mu.Unlock()
}

func (f *fake) client(t *testing.T, extra map[string]string, persist func(map[string]string) error) *Client {
	t.Helper()
	fac, err := providers.Get("linkedin-page")
	if err != nil {
		t.Fatal(err)
	}
	creds := map[string]string{"organization_id": "42", "access_token": "tok", "base_url": f.srv.URL, "token_url": f.srv.URL + "/oauth"}
	for k, v := range extra {
		creds[k] = v
	}
	c, err := fac.New(creds, persist, f.srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	cl := c.(*Client)
	cl.PollEvery = time.Millisecond
	return cl
}

func media(name, mime, body string) providers.MediaFile {
	return providers.MediaFile{Filename: name, Mime: mime, Size: int64(len(body)), Open: func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(body)), nil
	}}
}

func decode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("body %q: %v", b, err)
	}
	return m
}

func content(t *testing.T, b []byte) string {
	t.Helper()
	c, _ := json.Marshal(decode(t, b)["content"])
	return string(c)
}

func TestNewRequiresCredentials(t *testing.T) {
	fac, _ := providers.Get("linkedin-page")
	if _, err := fac.New(map[string]string{"access_token": "x"}, nil, nil); err == nil {
		t.Fatal("organization_id must be required")
	}
	if _, err := fac.New(map[string]string{"organization_id": "1"}, nil, nil); err == nil {
		t.Fatal("a token must be required")
	}
	c, err := fac.New(map[string]string{"organization_id": "urn:li:organization:7", "access_token": "x", "api_version": "202512"}, nil, nil)
	if err != nil || c.(*Client).OrgID != "7" || c.(*Client).Version != "202512" || c.(*Client).Base != DefaultBaseURL {
		t.Fatalf("%v %+v", err, c)
	}
}

func TestIdentifyAndFollowers(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("auth %q", r.Header.Get("Authorization"))
		}
		switch {
		case r.URL.Path == "/rest/organizations/42":
			w.Write([]byte(`{"id":42,"localizedName":"RideMyPark","vanityName":"ridemypark","$URN":"urn:li:organization:42"}`))
		case r.URL.EscapedPath() == "/rest/networkSizes/urn%3Ali%3Aorganization%3A42" && r.URL.RawQuery == "edgeType=COMPANY_FOLLOWED_BY_MEMBER":
			w.Write([]byte(`{"firstDegreeSize":1234}`))
		default:
			return false
		}
		return true
	})
	c := f.client(t, nil, nil)
	id, err := c.Identify(context.Background())
	if err != nil || id.RemoteUserID != "42" || id.Name != "RideMyPark" || id.Handle != "ridemypark" {
		t.Fatalf("%v %+v", err, id)
	}
	m, err := c.AccountMetrics(context.Background())
	if err != nil || m["followers"] != 1234 {
		t.Fatalf("%v %v", err, m)
	}
}

func TestPublishTextThreadAsComments(t *testing.T) {
	f := newFake(t, nil)
	f.h = func(w http.ResponseWriter, r *http.Request, b []byte) bool {
		switch {
		case r.Method == "POST" && r.URL.Path == "/rest/posts":
			m := decode(t, b)
			if m["author"] != org || m["visibility"] != "PUBLIC" || m["lifecycleState"] != "PUBLISHED" || m["content"] != nil {
				t.Errorf("post body %v", m)
			}
			dist := m["distribution"].(map[string]any)
			if dist["feedDistribution"] != "MAIN_FEED" {
				t.Errorf("distribution %v", dist)
			}
			f.step("post:" + m["commentary"].(string))
			w.Header().Set("x-restli-id", "urn:li:share:100")
			w.WriteHeader(201)
		case r.Method == "POST" && r.URL.EscapedPath() == "/v2/socialActions/urn%3Ali%3Ashare%3A100/comments":
			m := decode(t, b)
			if m["actor"] != org || m["object"] != "urn:li:share:100" || m["parentComment"] != nil {
				t.Errorf("comment body %v", m)
			}
			text := m["message"].(map[string]any)["text"].(string)
			f.step("comment:" + text)
			n := strconv.Itoa(len(f.steps))
			w.WriteHeader(201)
			w.Write([]byte(`{"id":"` + n + `","object":"urn:li:activity:9","actor":"` + org + `"}`))
		default:
			return false
		}
		return true
	}
	c := f.client(t, nil, nil)
	text := "Hello (world) #skate @[Devtestco](urn:li:organization:2414183) a_b # c*"
	res, err := c.Publish(context.Background(), providers.PublishRequest{Parts: []string{text, "second (2)", "third"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.RemoteID != "urn:li:share:100" || res.URL != "https://www.linkedin.com/feed/update/urn:li:share:100" {
		t.Fatalf("%+v", res)
	}
	want := []string{"urn:li:share:100", "urn:li:comment:(urn:li:activity:9,2)", "urn:li:comment:(urn:li:activity:9,3)"}
	if strings.Join(res.RemoteIDs, "|") != strings.Join(want, "|") {
		t.Fatalf("ids %v", res.RemoteIDs)
	}
	steps := []string{
		`post:Hello \(world\) #skate @[Devtestco](urn:li:organization:2414183) a\_b \# c\*`,
		"comment:second (2)",
		"comment:third",
	}
	if strings.Join(f.steps, "|") != strings.Join(steps, "|") {
		t.Fatalf("steps\n%v\nwant\n%v", f.steps, steps)
	}
}

func TestPublishResumesThreadOnTheActivity(t *testing.T) {
	f := newFake(t, nil)
	f.h = func(w http.ResponseWriter, r *http.Request, b []byte) bool {
		if r.Method == "POST" && r.URL.EscapedPath() == "/v2/socialActions/urn%3Ali%3Aactivity%3A9/comments" {
			if m := decode(t, b); m["object"] != "urn:li:activity:9" {
				t.Errorf("object %v", m["object"])
			}
			w.WriteHeader(201)
			w.Write([]byte(`{"commentUrn":"urn:li:comment:(urn:li:activity:9,77)"}`))
			return true
		}
		return false
	}
	res, err := f.client(t, nil, nil).Publish(context.Background(), providers.PublishRequest{
		Parts: []string{"rest"}, Options: map[string]any{"reply_to": "urn:li:comment:(urn:li:activity:9,3)"},
	})
	if err != nil || len(res.RemoteIDs) != 1 || res.RemoteIDs[0] != "urn:li:comment:(urn:li:activity:9,77)" {
		t.Fatalf("%v %+v", err, res)
	}
}

func TestPublishThreadStopsHalfway(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, b []byte) bool {
		switch {
		case r.URL.Path == "/rest/posts":
			w.Header().Set("x-restli-id", "urn:li:ugcPost:5")
			w.WriteHeader(201)
		case strings.HasPrefix(r.URL.Path, "/v2/socialActions/"):
			w.WriteHeader(403)
			w.Write([]byte(`{"status":403,"message":"no"}`))
		default:
			return false
		}
		return true
	})
	res, err := f.client(t, nil, nil).Publish(context.Background(), providers.PublishRequest{Parts: []string{"a", "b"}})
	if err == nil || res == nil || len(res.RemoteIDs) != 1 || res.RemoteID != "urn:li:ugcPost:5" {
		t.Fatalf("%v %+v", err, res)
	}
}

// imageServer answers the Images API: initialize, PUT upload, status polling (PROCESSING once, then AVAILABLE).
func imageServer(t *testing.T, f *fake, w http.ResponseWriter, r *http.Request, b []byte, polls map[string]int) bool {
	switch {
	case r.Method == "POST" && r.URL.Path == "/rest/images" && r.URL.RawQuery == "action=initializeUpload":
		m := decode(t, b)
		if m["initializeUploadRequest"].(map[string]any)["owner"] != org {
			t.Errorf("owner %v", m)
		}
		n := strconv.Itoa(len(polls) + 1)
		polls["urn:li:image:I"+n] = 0
		w.Write([]byte(`{"value":{"uploadUrl":"` + f.srv.URL + `/upload/img` + n + `","image":"urn:li:image:I` + n + `"}}`))
	case r.Method == "PUT" && strings.HasPrefix(r.URL.Path, "/upload/img"):
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("upload without token")
		}
		f.step("put:" + r.URL.Path + ":" + string(b) + ":" + r.Header.Get("Content-Type"))
		w.WriteHeader(201)
	case r.Method == "GET" && strings.HasPrefix(r.URL.EscapedPath(), "/rest/images/urn%3Ali%3Aimage%3A"):
		urn := strings.TrimPrefix(r.URL.Path, "/rest/images/")
		polls[urn]++
		if polls[urn] == 1 {
			w.Write([]byte(`{"status":"PROCESSING"}`))
		} else {
			w.Write([]byte(`{"status":"AVAILABLE","id":"` + urn + `"}`))
		}
	default:
		return false
	}
	return true
}

func TestPublishImages(t *testing.T) {
	polls := map[string]int{}
	var bodies []string
	f := newFake(t, nil)
	f.h = func(w http.ResponseWriter, r *http.Request, b []byte) bool {
		if r.Method == "POST" && r.URL.Path == "/rest/posts" {
			bodies = append(bodies, content(t, b))
			w.Header().Set("x-restli-id", "urn:li:share:"+strconv.Itoa(len(bodies)))
			w.WriteHeader(201)
			return true
		}
		return imageServer(t, f, w, r, b, polls)
	}
	c := f.client(t, nil, nil)
	ctx := context.Background()
	one := media("a.jpg", "image/jpeg", "A")
	one.Alt = "a ramp"
	if _, err := c.Publish(ctx, providers.PublishRequest{Parts: []string{"one"}, Media: []providers.MediaFile{one}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Publish(ctx, providers.PublishRequest{Parts: []string{"two"}, Media: []providers.MediaFile{media("b.png", "image/png", "B"), media("c.gif", "image/gif", "C")}}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		`{"media":{"altText":"a ramp","id":"urn:li:image:I1"}}`,
		`{"multiImage":{"images":[{"id":"urn:li:image:I2"},{"id":"urn:li:image:I3"}]}}`,
	}
	if strings.Join(bodies, "|") != strings.Join(want, "|") {
		t.Fatalf("contents\n%v\nwant\n%v", bodies, want)
	}
	if strings.Join(f.steps, "|") != "put:/upload/img1:A:image/jpeg|put:/upload/img2:B:image/png|put:/upload/img3:C:image/gif" {
		t.Fatalf("uploads %v", f.steps)
	}
	for urn, n := range polls {
		if n != 2 {
			t.Errorf("%s polled %d times", urn, n)
		}
	}
}

func TestPublishVideoMultipart(t *testing.T) {
	video := strings.Repeat("v", 10)
	polls := 0
	f := newFake(t, nil)
	f.h = func(w http.ResponseWriter, r *http.Request, b []byte) bool {
		switch {
		case r.Method == "POST" && r.URL.Path == "/rest/videos" && r.URL.RawQuery == "action=initializeUpload":
			req := decode(t, b)["initializeUploadRequest"].(map[string]any)
			if req["owner"] != org || req["fileSizeBytes"] != float64(10) || req["uploadCaptions"] != false || req["uploadThumbnail"] != false {
				t.Errorf("init %v", req)
			}
			u := f.srv.URL + "/upload/vid?part="
			w.Write([]byte(`{"value":{"video":"urn:li:video:V1","uploadToken":"tk","uploadInstructions":[` +
				`{"uploadUrl":"` + u + `0","firstByte":0,"lastByte":3},{"uploadUrl":"` + u + `1","firstByte":4,"lastByte":7},{"uploadUrl":"` + u + `2","firstByte":8,"lastByte":9}]}}`))
		case r.Method == "PUT" && r.URL.Path == "/upload/vid":
			p := r.URL.Query().Get("part")
			f.step("part" + p + ":" + string(b))
			w.Header().Set("ETag", `"etag`+p+`"`)
		case r.Method == "POST" && r.URL.Path == "/rest/videos" && r.URL.RawQuery == "action=finalizeUpload":
			fin, _ := json.Marshal(decode(t, b))
			f.step("finalize:" + string(fin))
		case r.Method == "GET" && r.URL.EscapedPath() == "/rest/videos/urn%3Ali%3Avideo%3AV1":
			polls++
			if polls < 3 {
				w.Write([]byte(`{"status":"PROCESSING"}`))
			} else {
				w.Write([]byte(`{"status":"AVAILABLE"}`))
			}
		case r.Method == "POST" && r.URL.Path == "/rest/posts":
			f.step("post:" + content(t, b))
			w.Header().Set("x-restli-id", "urn:li:ugcPost:7")
			w.WriteHeader(201)
		default:
			return false
		}
		return true
	}
	res, err := f.client(t, nil, nil).Publish(context.Background(), providers.PublishRequest{
		Parts: []string{"clip"}, Media: []providers.MediaFile{media("v.mp4", "video/mp4", video)}, Options: map[string]any{"title": "Session"},
	})
	if err != nil || res.RemoteID != "urn:li:ugcPost:7" {
		t.Fatalf("%v %+v", err, res)
	}
	want := []string{
		"part0:vvvv", "part1:vvvv", "part2:vv",
		`finalize:{"finalizeUploadRequest":{"uploadToken":"tk","uploadedPartIds":["etag0","etag1","etag2"],"video":"urn:li:video:V1"}}`,
		`post:{"media":{"id":"urn:li:video:V1","title":"Session"}}`,
	}
	if strings.Join(f.steps, "|") != strings.Join(want, "|") || polls != 3 {
		t.Fatalf("steps (%d polls)\n%v\nwant\n%v", polls, f.steps, want)
	}
}

func TestPublishVideoProcessingFailed(t *testing.T) {
	f := newFake(t, nil)
	f.h = func(w http.ResponseWriter, r *http.Request, b []byte) bool {
		switch {
		case r.URL.RawQuery == "action=initializeUpload":
			w.Write([]byte(`{"value":{"video":"urn:li:video:V","uploadInstructions":[{"uploadUrl":"` + f.srv.URL + `/upload/v","firstByte":0,"lastByte":1}]}}`))
		case r.URL.Path == "/upload/v":
			w.Header().Set("ETag", "e")
		case r.URL.RawQuery == "action=finalizeUpload":
		case r.Method == "GET":
			w.Write([]byte(`{"status":"PROCESSING_FAILED","processingFailureReason":"bad codec"}`))
		default:
			return false
		}
		return true
	}
	_, err := f.client(t, nil, nil).Publish(context.Background(), providers.PublishRequest{Parts: []string{"x"}, Media: []providers.MediaFile{media("v.mp4", "video/mp4", "vv")}})
	if !errors.Is(err, providers.ErrInvalidPost) || !strings.Contains(err.Error(), "bad codec") {
		t.Fatalf("%v", err)
	}
}

func TestPublishDocumentAndArticle(t *testing.T) {
	polls := map[string]int{}
	var bodies []string
	f := newFake(t, nil)
	f.h = func(w http.ResponseWriter, r *http.Request, b []byte) bool {
		switch {
		case r.Method == "POST" && r.URL.Path == "/rest/documents" && r.URL.RawQuery == "action=initializeUpload":
			w.Write([]byte(`{"value":{"uploadUrl":"` + f.srv.URL + `/upload/doc","document":"urn:li:document:D1"}}`))
		case r.Method == "PUT" && r.URL.Path == "/upload/doc":
			f.step("doc:" + string(b) + ":" + r.Header.Get("Content-Type"))
		case r.Method == "GET" && r.URL.EscapedPath() == "/rest/documents/urn%3Ali%3Adocument%3AD1":
			w.Write([]byte(`{"status":"AVAILABLE"}`))
		case r.Method == "POST" && r.URL.Path == "/rest/posts":
			bodies = append(bodies, content(t, b))
			w.Header().Set("x-restli-id", "urn:li:share:1")
			w.WriteHeader(201)
		default:
			return imageServer(t, f, w, r, b, polls)
		}
		return true
	}
	c := f.client(t, nil, nil)
	ctx := context.Background()
	if _, err := c.Publish(ctx, providers.PublishRequest{Parts: []string{"deck"}, Media: []providers.MediaFile{media("Guide 2026.pdf", "application/pdf", "%PDF")}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Publish(ctx, providers.PublishRequest{Parts: []string{"read"}, Options: map[string]any{"link": "https://ridemypark.com/blog/x", "link_title": "X", "link_description": "Y"},
		Media: []providers.MediaFile{media("t.jpg", "image/jpeg", "T")}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Publish(ctx, providers.PublishRequest{Parts: []string{"bare link"}, Options: map[string]any{"link": "https://ridemypark.com"}}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		`{"media":{"id":"urn:li:document:D1","title":"Guide 2026"}}`,
		`{"article":{"description":"Y","source":"https://ridemypark.com/blog/x","thumbnail":"urn:li:image:I1","title":"X"}}`,
		`{"article":{"source":"https://ridemypark.com","title":"https://ridemypark.com"}}`,
	}
	if strings.Join(bodies, "|") != strings.Join(want, "|") {
		t.Fatalf("contents\n%v\nwant\n%v", bodies, want)
	}
	if f.steps[0] != "doc:%PDF:application/pdf" {
		t.Fatalf("steps %v", f.steps)
	}
}

func TestRefreshWhenExpiredAndOn401(t *testing.T) {
	var refreshes int
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, b []byte) bool {
		switch r.URL.Path {
		case "/oauth":
			if r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
				t.Errorf("content type %q", r.Header.Get("Content-Type"))
			}
			r.Body = io.NopCloser(strings.NewReader(string(b)))
			r.ParseForm()
			if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("client_id") != "cid" || r.Form.Get("client_secret") != "sec" {
				t.Errorf("refresh form %v", r.Form)
			}
			refreshes++
			tok := "fresh" + strconv.Itoa(refreshes)
			w.Write([]byte(`{"access_token":"` + tok + `","expires_in":5184000,"refresh_token":"rt` + strconv.Itoa(refreshes) + `","refresh_token_expires_in":31536000}`))
		case "/rest/organizations/42":
			auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if auth != "fresh"+strconv.Itoa(refreshes) || refreshes == 0 {
				w.WriteHeader(401)
				w.Write([]byte(`{"status":401,"code":"EXPIRED_ACCESS_TOKEN"}`))
				return true
			}
			w.Write([]byte(`{"id":42,"localizedName":"P"}`))
		default:
			return false
		}
		return true
	})
	var saved map[string]string
	persist := func(m map[string]string) error {
		saved = map[string]string{}
		for k, v := range m {
			saved[k] = v
		}
		return nil
	}
	past := strconv.FormatInt(time.Now().Add(-time.Hour).Unix(), 10)
	c := f.client(t, map[string]string{"refresh_token": "rt0", "client_id": "cid", "client_secret": "sec", "expires_at": past}, persist)
	if _, err := c.Identify(context.Background()); err != nil {
		t.Fatal(err)
	}
	if refreshes != 1 || saved["access_token"] != "fresh1" || saved["refresh_token"] != "rt1" {
		t.Fatalf("refreshes=%d saved=%v", refreshes, saved)
	}
	if exp, _ := strconv.ParseInt(saved["expires_at"], 10, 64); exp < time.Now().Add(59*24*time.Hour).Unix() {
		t.Fatalf("expires_at %v", saved["expires_at"])
	}
	if saved["refresh_token_expires_at"] == "" {
		t.Fatal("refresh token expiry not tracked")
	}
	// The server now rejects fresh1: a 401 forces one refresh and a retry.
	f.h = func(w http.ResponseWriter, r *http.Request, b []byte) bool {
		switch r.URL.Path {
		case "/oauth":
			w.Write([]byte(`{"access_token":"fresh2","expires_in":5184000}`))
		case "/rest/organizations/42":
			if r.Header.Get("Authorization") != "Bearer fresh2" {
				w.WriteHeader(401)
				return true
			}
			w.Write([]byte(`{"id":42}`))
		default:
			return false
		}
		return true
	}
	if _, err := c.Identify(context.Background()); err != nil || saved["access_token"] != "fresh2" || saved["refresh_token"] != "rt1" {
		t.Fatalf("%v %v", err, saved)
	}
}

func TestRefreshRejectedIsNotRetried(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, b []byte) bool {
		if r.URL.Path == "/oauth" {
			w.WriteHeader(400)
			w.Write([]byte(`{"error":"invalid_request","error_description":"The provided authorization grant or refresh token is invalid, expired or revoked"}`))
			return true
		}
		return false
	})
	c := f.client(t, map[string]string{"access_token": "", "refresh_token": "rt", "client_id": "cid", "client_secret": "s"}, nil)
	_, err := c.Identify(context.Background())
	if err == nil || providers.Retryable(err) {
		t.Fatalf("%v", err)
	}
}

func TestMetrics(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		if r.URL.Path != "/rest/organizationalEntityShareStatistics" {
			return false
		}
		q := r.URL.RawQuery
		switch {
		case q == "q=organizationalEntity&organizationalEntity=urn%3Ali%3Aorganization%3A42&shares=List(urn%3Ali%3Ashare%3A1,urn%3Ali%3Ashare%3A2)":
			w.Write([]byte(`{"elements":[{"organizationalEntity":"urn:li:organization:42","share":"urn:li:share:1","totalShareStatistics":` +
				`{"clickCount":78,"commentCount":24,"engagement":0.022886324947985624,"impressionCount":5287,"likeCount":14,"shareCount":5,"uniqueImpressionsCount":3000}}]}`))
		case q == "q=organizationalEntity&organizationalEntity=urn%3Ali%3Aorganization%3A42&ugcPosts=List(urn%3Ali%3AugcPost%3A9)":
			w.Write([]byte(`{"elements":[{"ugcPost":"urn:li:ugcPost:9","totalShareStatistics":{"impressionCount":10,"likeCount":1}}]}`))
		default:
			t.Errorf("query %s", q)
		}
		return true
	})
	m, err := f.client(t, nil, nil).Metrics(context.Background(), []string{"urn:li:share:1", "urn:li:share:2", "urn:li:ugcPost:9", "urn:li:comment:(urn:li:activity:1,2)"})
	if err != nil {
		t.Fatal(err)
	}
	s1 := m["urn:li:share:1"]
	if s1["impression_count"] != 5287 || s1["unique_impressions"] != 3000 || s1["clicks"] != 78 || s1["like_count"] != 14 || s1["comment_count"] != 24 || s1["share_count"] != 5 || s1["engagement_ppm"] != 22886 {
		t.Fatalf("share 1 %v", s1)
	}
	if s2, ok := m["urn:li:share:2"]; !ok || s2["impression_count"] != 0 {
		t.Fatalf("share without activity must read as zero: %v", m)
	}
	if m["urn:li:ugcPost:9"]["impression_count"] != 10 || len(m) != 3 {
		t.Fatalf("%v", m)
	}
}

func TestCommentsAndReply(t *testing.T) {
	f := newFake(t, nil)
	f.h = func(w http.ResponseWriter, r *http.Request, b []byte) bool {
		switch {
		case r.Method == "GET" && r.URL.EscapedPath() == "/v2/socialActions/urn%3Ali%3Ashare%3A1/comments":
			if r.URL.Query().Get("count") != "100" || r.URL.Query().Get("start") != "0" {
				t.Errorf("paging %s", r.URL.RawQuery)
			}
			w.Write([]byte(`{"paging":{"start":0,"count":100,"total":3},"elements":[` +
				`{"$URN":"urn:li:comment:(urn:li:activity:9,1)","id":"1","actor":"urn:li:person:abc","object":"urn:li:activity:9","message":{"text":"nice park"},"created":{"time":1763700000000}},` +
				`{"id":"2","actor":"urn:li:organization:42","object":"urn:li:activity:9","message":{"text":"thanks"}},` +
				`{"id":"3","actor":"urn:li:organization:7","object":"urn:li:activity:9","message":{"text":"partner"}}]}`))
		case r.Method == "POST" && r.URL.EscapedPath() == "/v2/socialActions/urn%3Ali%3Ashare%3A1/comments":
			m := decode(t, b)
			if m["object"] != "urn:li:share:1" || m["parentComment"] != nil || m["actor"] != org {
				t.Errorf("top-level reply %v", m)
			}
			w.Header().Set("x-restli-id", "55")
			w.WriteHeader(201)
			w.Write([]byte(`{"id":"55","object":"urn:li:activity:9"}`))
		case r.Method == "POST" && r.URL.EscapedPath() == "/v2/socialActions/urn%3Ali%3Acomment%3A%28urn%3Ali%3Aactivity%3A9%2C1%29/comments":
			m := decode(t, b)
			if m["object"] != "urn:li:activity:9" || m["parentComment"] != "urn:li:comment:(urn:li:activity:9,1)" || m["message"].(map[string]any)["text"] != "merci !" {
				t.Errorf("nested reply %v", m)
			}
			w.WriteHeader(201)
			w.Write([]byte(`{"commentUrn":"urn:li:comment:(urn:li:activity:9,56)"}`))
		default:
			return false
		}
		return true
	}
	c := f.client(t, nil, nil)
	ctx := context.Background()
	cs, err := c.Comments(ctx, "urn:li:share:1")
	if err != nil || len(cs) != 2 {
		t.Fatalf("%v %+v", err, cs)
	}
	if cs[0].RemoteID != "urn:li:comment:(urn:li:activity:9,1)" || cs[0].Author != "urn:li:person:abc" || cs[0].Text != "nice park" || cs[0].CreatedAt == nil ||
		cs[0].URL != "https://www.linkedin.com/feed/update/urn:li:share:1?commentUrn=urn%3Ali%3Acomment%3A%28urn%3Ali%3Aactivity%3A9%2C1%29" {
		t.Fatalf("%+v", cs[0])
	}
	if cs[1].RemoteID != "urn:li:comment:(urn:li:activity:9,3)" {
		t.Fatalf("%+v", cs[1])
	}
	res, err := c.Reply(ctx, "urn:li:share:1", "hello")
	if err != nil || res.RemoteID != "urn:li:comment:(urn:li:activity:9,55)" {
		t.Fatalf("%v %+v", err, res)
	}
	res, err = c.Reply(ctx, cs[0].RemoteID, "merci !")
	if err != nil || res.RemoteID != "urn:li:comment:(urn:li:activity:9,56)" {
		t.Fatalf("%v %+v", err, res)
	}
	if _, err := c.Reply(ctx, "urn:li:share:1", strings.Repeat("x", 1251)); !errors.Is(err, providers.ErrInvalidPost) {
		t.Fatalf("long reply: %v", err)
	}
}

func TestHistory(t *testing.T) {
	now := time.Now()
	ms := func(d time.Duration) string { return strconv.FormatInt(now.Add(-d).UnixMilli(), 10) }
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		switch r.URL.Path {
		case "/rest/posts":
			if r.Header.Get("X-RestLi-Method") != "FINDER" {
				t.Errorf("missing FINDER header")
			}
			q := r.URL.Query()
			if r.URL.RawQuery[:len("q=author&author=urn%3Ali%3Aorganization%3A42")] != "q=author&author=urn%3Ali%3Aorganization%3A42" || q.Get("sortBy") != "CREATED" || q.Get("count") != "100" {
				t.Errorf("query %s", r.URL.RawQuery)
			}
			if q.Get("start") != "0" {
				w.Write([]byte(`{"elements":[]}`))
				return true
			}
			w.Write([]byte(`{"paging":{"start":0,"count":100},"elements":[` +
				`{"id":"urn:li:share:1","commentary":"Ride {hashtag|\\#|skate} with @[Devtestco](urn:li:organization:1) \\(now\\)","publishedAt":` + ms(time.Hour) + `,"lifecycleState":"PUBLISHED","distribution":{"feedDistribution":"MAIN_FEED"},"content":{"media":{"id":"urn:li:video:V"}}},` +
				`{"id":"urn:li:share:2","commentary":"","createdAt":` + ms(2*time.Hour) + `,"lifecycleState":"PUBLISHED","distribution":{"feedDistribution":"MAIN_FEED"},"reshareContext":{"parent":"urn:li:share:0"}},` +
				`{"id":"urn:li:share:3","commentary":"ad","createdAt":` + ms(3*time.Hour) + `,"lifecycleState":"PUBLISHED","distribution":{"feedDistribution":"NONE"}},` +
				`{"id":"urn:li:ugcPost:4","commentary":"pics","createdAt":` + ms(4*time.Hour) + `,"lifecycleState":"PUBLISHED","distribution":{"feedDistribution":"MAIN_FEED"},"content":{"multiImage":{"images":[{"id":"urn:li:image:A"},{"id":"urn:li:image:B"}]}}},` +
				`{"id":"urn:li:share:6","commentary":"link","createdAt":` + ms(5*time.Hour) + `,"lifecycleState":"PUBLISHED","distribution":{"feedDistribution":"MAIN_FEED"},"content":{"article":{"source":"https://x.example","thumbnail":"urn:li:image:T"}}},` +
				`{"id":"urn:li:share:7","commentary":"deck","createdAt":` + ms(6*time.Hour) + `,"lifecycleState":"PUBLISHED","distribution":{"feedDistribution":"MAIN_FEED"},"content":{"media":{"id":"urn:li:document:D"}}},` +
				`{"id":"urn:li:share:5","commentary":"old","createdAt":` + ms(48*time.Hour) + `,"lifecycleState":"PUBLISHED","distribution":{"feedDistribution":"MAIN_FEED"}}]}`))
		case "/rest/organizationalEntityShareStatistics":
			w.Write([]byte(`{"elements":[{"share":"urn:li:share:1","totalShareStatistics":{"impressionCount":50,"likeCount":3}}]}`))
		case "/rest/images":
			if r.URL.RawQuery != "ids=List(urn%3Ali%3Aimage%3AA,urn%3Ali%3Aimage%3AB,urn%3Ali%3Aimage%3AT)" {
				t.Errorf("images query %s", r.URL.RawQuery)
			}
			w.Write([]byte(`{"results":{"urn:li:image:A":{"id":"urn:li:image:A","downloadUrl":"https://media.licdn.com/A","status":"AVAILABLE"},` +
				`"urn:li:image:B":{"downloadUrl":"https://media.licdn.com/B"},"urn:li:image:T":{"downloadUrl":"https://media.licdn.com/T"}},"errors":{}}`))
		case "/rest/videos":
			if r.URL.RawQuery != "ids=List(urn%3Ali%3Avideo%3AV)" {
				t.Errorf("videos query %s", r.URL.RawQuery)
			}
			w.Write([]byte(`{"results":{"urn:li:video:V":{"downloadUrl":"https://media.licdn.com/V.mp4","thumbnail":"https://media.licdn.com/V-thumb"}}}`))
		default:
			return false
		}
		return true
	})
	items, err := f.client(t, nil, nil).History(context.Background(), now.Add(-24*time.Hour), 0)
	if err != nil {
		t.Fatal(err)
	}
	wantVis := [][]providers.Visual{
		{{URL: "https://media.licdn.com/V-thumb", Cover: true}},
		nil, // repost
		{{URL: "https://media.licdn.com/A"}, {URL: "https://media.licdn.com/B"}},
		{{URL: "https://media.licdn.com/T"}},
		nil, // document: no image in the API
	}
	for i, w := range wantVis {
		if i < len(items) && !reflect.DeepEqual(items[i].Visuals, w) {
			t.Errorf("item %s visuals %+v want %+v", items[i].RemoteID, items[i].Visuals, w)
		}
	}
	if len(items) != 5 {
		t.Fatalf("%+v", items)
	}
	if items[0].Text != "Ride #skate with Devtestco (now)" || items[0].Kind != "video" || items[0].Metrics["impression_count"] != 50 || items[0].URL != "https://www.linkedin.com/feed/update/urn:li:share:1" {
		t.Fatalf("%+v", items[0])
	}
	if items[1].Kind != "repost" || items[2].Kind != "carousel" || items[2].RemoteID != "urn:li:ugcPost:4" || items[3].Kind != "article" || items[4].Kind != "document" {
		t.Fatalf("%+v", items)
	}
}

func TestValidate(t *testing.T) {
	fac, _ := providers.Get("linkedin-page")
	img := media("a.jpg", "image/jpeg", "x")
	vid := providers.MediaFile{Filename: "v.mp4", Mime: "video/mp4", Size: 1 << 20}
	pdf := media("d.pdf", "application/pdf", "x")
	cases := []struct {
		name string
		req  providers.PublishRequest
		want string
	}{
		{"ok text", providers.PublishRequest{Parts: []string{"hi"}}, ""},
		{"empty", providers.PublishRequest{}, "empty post"},
		{"too long", providers.PublishRequest{Parts: []string{strings.Repeat("é", 3001)}}, "3000"},
		{"long comment part", providers.PublishRequest{Parts: []string{"a", strings.Repeat("x", 1251)}}, "1250"},
		{"video alone", providers.PublishRequest{Parts: []string{"a"}, Media: []providers.MediaFile{vid, img}}, "only media"},
		{"doc alone", providers.PublishRequest{Parts: []string{"a"}, Media: []providers.MediaFile{pdf, img}}, "only media"},
		{"too many images", providers.PublishRequest{Parts: []string{"a"}, Media: func() []providers.MediaFile {
			var m []providers.MediaFile
			for i := 0; i < 21; i++ {
				m = append(m, img)
			}
			return m
		}()}, "at most 20"},
		{"webp", providers.PublishRequest{Parts: []string{"a"}, Media: []providers.MediaFile{media("a.webp", "image/webp", "x")}}, "JPEG/PNG/GIF"},
		{"mov", providers.PublishRequest{Parts: []string{"a"}, Media: []providers.MediaFile{{Filename: "v.mov", Mime: "video/quicktime", Size: 1 << 20}}}, "MP4"},
		{"tiny video", providers.PublishRequest{Parts: []string{"a"}, Media: []providers.MediaFile{{Filename: "v.mp4", Mime: "video/mp4", Size: 10}}}, "75 KB"},
		{"link + 2 images", providers.PublishRequest{Parts: []string{"a"}, Media: []providers.MediaFile{img, img}, Options: map[string]any{"link": "https://x.com"}}, "at most one image"},
		{"bad link", providers.PublishRequest{Parts: []string{"a"}, Options: map[string]any{"link": "x.com"}}, "http(s)"},
		{"ok link only", providers.PublishRequest{Options: map[string]any{"link": "https://x.com"}}, ""},
		{"ok video", providers.PublishRequest{Parts: []string{"a"}, Media: []providers.MediaFile{vid}}, ""},
	}
	for _, tc := range cases {
		got := strings.Join(fac.Validate(tc.req), "; ")
		if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}

func TestLittleText(t *testing.T) {
	cases := map[string]string{
		`a\b|c{d}e<f>g*h_i~j[k](l)@m`:               `a\\b\|c\{d\}e\<f\>g\*h\_i\~j\[k\]\(l\)\@m`,
		"#skate and #Ride2026_x but # alone and C#": `#skate and #Ride2026\_x but \# alone and C\#`,
		"hi @[Acme](urn:li:organization:9) (x)":     `hi @[Acme](urn:li:organization:9) \(x\)`,
		"émoji 🛹 #été":                              "émoji 🛹 #été",
	}
	for in, want := range cases {
		if got := escapeLittle(in); got != want {
			t.Errorf("escape %q = %q, want %q", in, got, want)
		}
	}
	if got := unescapeLittle(escapeLittle(`a(b) [c] x_y`)); got != `a(b) [c] x_y` {
		t.Errorf("round trip %q", got)
	}
}

func TestRetryableErrors(t *testing.T) {
	for body, retry := range map[string]bool{
		`{"status":500,"message":"Unable to obtain activity"}`: true,
		`{"status":422,"message":"Unable to obtain activity"}`: true,
		`{"status":400,"code":"FIELD_LENGTH_TOO_LONG"}`:        false,
	} {
		var le liError
		_ = json.Unmarshal([]byte(body), &le)
		err := mapError(&providers.HTTPError{Status: le.Status, Body: body})
		if providers.Retryable(err) != retry {
			t.Errorf("%s: retryable=%v", body, !retry)
		}
	}
	if !providers.Retryable(mapError(&providers.HTTPError{Status: 409, Body: `{}`})) {
		t.Error("409 conflict should be retried")
	}
}
