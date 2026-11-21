package api_test

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Seeyko/relay/internal/store"
)

func TestUpdateCredentials(t *testing.T) {
	e := newEnv(t)
	var who atomic.Value
	who.Store("42")
	var lastAuth atomic.Value
	acc := connectFakeX(t, e, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastAuth.Store(r.Header.Get("Authorization"))
		w.Write([]byte(`{"data":{"id":"` + who.Load().(string) + `","username":"alice"}}`))
	}))
	ctx := context.Background()
	// a post attached to the account must survive the update
	p := &store.Post{AccountID: acc.ID, Status: store.StatusDraft, Parts: []string{"hi"}}
	if err := e.svc.Store.CreatePost(ctx, p); err != nil {
		t.Fatal(err)
	}
	code, body, raw := e.req("PATCH", "/v1/accounts/"+acc.ID+"/credentials", map[string]any{"credentials": map[string]string{"access_token": "NEWTOKEN"}})
	if code != 200 || body["id"] != acc.ID {
		t.Fatalf("%d %s", code, raw)
	}
	if a, _ := lastAuth.Load().(string); !strings.Contains(a, "NEWTOKEN") {
		t.Fatalf("new token not used to verify: %q", a)
	}
	got, err := e.svc.Store.GetAccount(ctx, acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	creds := map[string]string{}
	if err := e.svc.Box.OpenJSON(got.Credentials, &creds); err != nil {
		t.Fatal(err)
	}
	if creds["access_token"] != "NEWTOKEN" || creds["consumer_key"] != "a" || creds["user_id"] != "42" {
		t.Fatalf("merged credentials: %v", creds)
	}
	if _, err := e.svc.Store.GetPost(ctx, p.ID); err != nil {
		t.Fatalf("post lost: %v", err)
	}
	// credentials of another remote account are refused and nothing changes
	who.Store("99")
	code, _, raw = e.req("PATCH", "/v1/accounts/"+acc.ID+"/credentials", map[string]any{"credentials": map[string]string{"access_token": "OTHER"}})
	if code != 400 || !strings.Contains(string(raw), "not to this account") {
		t.Fatalf("other account accepted: %d %s", code, raw)
	}
	got, _ = e.svc.Store.GetAccount(ctx, acc.ID)
	e.svc.Box.OpenJSON(got.Credentials, &creds)
	if creds["access_token"] != "NEWTOKEN" {
		t.Fatalf("refused update changed the credentials: %v", creds)
	}
	if code, _, _ := e.req("PATCH", "/v1/accounts/"+acc.ID+"/credentials", map[string]any{"credentials": map[string]string{}}); code != 400 {
		t.Fatalf("empty update: %d", code)
	}
	if code, _, _ := e.req("PATCH", "/v1/accounts/acc_missing/credentials", map[string]any{"credentials": map[string]string{"x": "y"}}); code != 404 {
		t.Fatalf("missing account: %d", code)
	}
}
