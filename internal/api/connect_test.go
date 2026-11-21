package api_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Seeyko/relay/internal/api"
	"github.com/Seeyko/relay/internal/connect"
	"github.com/Seeyko/relay/internal/mcp"
)

// start_account_connection: admin keys get a start link, drafter keys are refused (tool and REST routes).
func TestStartAccountConnectionRoles(t *testing.T) {
	e := newEnv(t)
	nets := connect.FromEnv(func(k string) string {
		return map[string]string{"RELAY_THREADS_APP_ID": "th-id", "RELAY_THREADS_APP_SECRET": "th-secret"}[k]
	})
	m, err := connect.New("https://relay.example", e.svc, nets, e.svc.Log)
	if err != nil {
		t.Fatal(err)
	}
	const drafter = "drafter-token-0123456"
	a := &api.API{Svc: e.svc, MCP: mcp.NewServer("relay", "test", append(mcp.BuildTools(e.svc), mcp.ConnectTools(m)...)),
		Token: token, DraftToken: drafter, Log: e.svc.Log, Connect: m}
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)
	call := func(tok, path, body string) (int, map[string]any) {
		req, _ := http.NewRequest("POST", srv.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		out := map[string]any{}
		_ = json.Unmarshal(raw, &out)
		return resp.StatusCode, out
	}

	code, out := call(token, "/v1/accounts/connect", `{"network":"threads"}`)
	u, _ := out["url"].(string)
	if code != 200 || !strings.HasPrefix(u, "https://relay.example/connect/threads/start?t=") ||
		out["redirect_uri"] != "https://relay.example/connect/threads/callback" {
		t.Fatalf("admin: %d %v", code, out)
	}
	if code, out := call(token, "/v1/accounts/connect", `{"network":"tiktok"}`); code != 400 || !strings.Contains(out["error"].(string), "configured: threads") {
		t.Fatalf("unconfigured network: %d %v", code, out)
	}
	if code, _ := call(token, "/v1/accounts/acc_missing/reconnect", ``); code != 404 {
		t.Fatalf("missing account: %d", code)
	}
	for _, path := range []string{"/v1/accounts/connect", "/v1/tools/start_account_connection"} {
		if code, out := call(drafter, path, `{"network":"threads"}`); code != 403 {
			t.Fatalf("drafter on %s: %d %v", path, code, out)
		}
	}
	// The tool is hidden from drafter keys.
	req, _ := http.NewRequest("POST", srv.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Authorization", "Bearer "+drafter)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(raw), "start_account_connection") {
		t.Fatal("drafter sees start_account_connection")
	}
	// The pages are public (they check their own state), the rest of /connect/ is not found.
	r2, _ := http.Get(srv.URL + "/connect/threads/start?t=nope")
	r2.Body.Close()
	if r2.StatusCode != http.StatusGone {
		t.Fatalf("start page: %d", r2.StatusCode)
	}
}
