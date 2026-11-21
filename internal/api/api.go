// Package api is Relay's REST API. Every route maps onto the same tools the MCP server exposes.
package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Seeyko/relay/internal/blob"
	"github.com/Seeyko/relay/internal/core"
	"github.com/Seeyko/relay/internal/mcp"
	"github.com/Seeyko/relay/internal/oauth"
	"github.com/Seeyko/relay/internal/store"
	"github.com/Seeyko/relay/internal/team"
	"github.com/Seeyko/relay/internal/ui"
)

type API struct {
	Svc   *core.Service
	MCP   *mcp.Server
	Token string
	// DraftToken (optional) gives a key that can only stage drafts: hand it to sub-agents.
	DraftToken string
	Log        *slog.Logger
	Version    string
	// UI (optional) is the read-only passkey dashboard mounted under /ui/.
	UI *ui.UI
	// OAuth (optional) lets agents connect to /mcp with OAuth 2.1, approved with the dashboard passkey.
	OAuth *oauth.Server
}

func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"status": "ok", "version": a.Version})
	})
	// Public media (random ids): networks like Instagram download from here.
	mux.HandleFunc("GET /media/{file}", func(w http.ResponseWriter, r *http.Request) {
		name := filepath.Base(r.PathValue("file"))
		if !strings.HasPrefix(name, "med_") {
			http.NotFound(w, r)
			return
		}
		local := filepath.Join(a.Svc.MediaDir, name)
		if _, err := os.Stat(local); err == nil || a.Svc.Blob == nil {
			http.ServeFile(w, r, local)
			return
		}
		a.serveFromBucket(w, r, name)
	})

	if a.UI != nil {
		a.UI.Register(mux)
		mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/ui/", http.StatusFound) })
	}
	// Metadata and OAuth endpoints answer 404 (not 401) when OAuth is off, so clients fall back to static tokens.
	mux.HandleFunc("/.well-known/", http.NotFound)
	mux.HandleFunc("/oauth/", http.NotFound)
	if a.OAuth != nil {
		a.OAuth.Register(mux)
	}

	authed := http.NewServeMux()
	authed.Handle("POST /mcp", a.MCP)
	authed.Handle("GET /mcp", a.MCP)

	authed.HandleFunc("GET /v1/tools", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, a.MCP.Tools()) })
	authed.HandleFunc("POST /v1/tools/{name}", func(w http.ResponseWriter, r *http.Request) {
		a.call(w, r, r.PathValue("name"), body(r))
	})

	authed.HandleFunc("GET /v1/providers", a.tool("list_providers", nil))
	authed.HandleFunc("GET /v1/accounts", a.tool("list_accounts", nil))
	authed.HandleFunc("POST /v1/accounts", a.tool("connect_account", nil))
	authed.HandleFunc("DELETE /v1/accounts/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := a.Svc.Store.DeleteAccount(r.Context(), r.PathValue("id")); err != nil {
			a.fail(w, err)
			return
		}
		w.WriteHeader(204)
	})
	authed.HandleFunc("POST /v1/accounts/{id}/import", a.tool("import_history", map[string]string{"id": "account_id"}))
	// Profile pictures: {"media": "<id>"} or {"media_url": "https://..."}, or {"clear": true}.
	authed.HandleFunc("PATCH /v1/accounts/{id}/credentials", a.tool("update_account_credentials", map[string]string{"id": "account_id"}))
	authed.HandleFunc("PUT /v1/accounts/{id}/avatar", a.tool("set_account_avatar", map[string]string{"id": "account_id"}))
	authed.HandleFunc("PUT /v1/accounts/avatar", a.toolWith("set_account_avatar", nil, map[string]any{"account_id": "all"}))

	authed.HandleFunc("GET /v1/posts", a.tool("list_posts", nil))
	authed.HandleFunc("POST /v1/posts", a.tool("schedule_post", nil))
	authed.HandleFunc("GET /v1/posts/{id}", a.tool("get_post", map[string]string{"id": "id"}))
	authed.HandleFunc("PATCH /v1/posts/{id}", a.tool("update_post", map[string]string{"id": "id"}))
	authed.HandleFunc("DELETE /v1/posts/{id}", a.tool("delete_post", map[string]string{"id": "id"}))
	authed.HandleFunc("POST /v1/posts/{id}/publish", a.tool("publish_now", map[string]string{"id": "id"}))
	authed.HandleFunc("GET /v1/posts/{id}/comments", a.tool("list_comments", map[string]string{"id": "post_id"}))

	authed.HandleFunc("GET /v1/comments", a.tool("list_comments", nil))
	authed.HandleFunc("POST /v1/comments/reply", a.tool("reply_comment", nil))
	authed.HandleFunc("POST /v1/metrics/refresh", a.tool("refresh_metrics", nil))
	authed.HandleFunc("GET /v1/analytics", a.tool("analyze", nil))
	authed.HandleFunc("GET /v1/posts/{id}/timeline", a.tool("post_timeline", map[string]string{"id": "id"}))
	authed.HandleFunc("POST /v1/posts/{id}/approve", a.tool("approve_post", map[string]string{"id": "id"}))
	authed.HandleFunc("POST /v1/posts/validate", a.tool("validate_post", nil))
	authed.HandleFunc("GET /v1/accounts/growth", a.tool("account_growth", nil))
	authed.HandleFunc("GET /v1/oauth/grants", a.tool("list_oauth_grants", nil))
	authed.HandleFunc("DELETE /v1/oauth/grants/{id}", a.tool("revoke_oauth_grant", map[string]string{"id": "id"}))
	authed.HandleFunc("POST /v1/query", a.tool("sql_query", nil))

	// Team members (when the dashboard is on).
	authed.HandleFunc("GET /v1/members", a.tool("list_members", nil))
	authed.HandleFunc("POST /v1/members", a.tool("invite_member", nil))
	authed.HandleFunc("PATCH /v1/members/{id}", a.tool("update_member", map[string]string{"id": "id"}))
	authed.HandleFunc("DELETE /v1/members/{id}", a.tool("remove_member", map[string]string{"id": "id"}))

	authed.HandleFunc("GET /v1/media", func(w http.ResponseWriter, r *http.Request) {
		ms, err := a.Svc.Store.ListMedia(r.Context())
		if err != nil {
			a.fail(w, err)
			return
		}
		writeJSON(w, 200, ms)
	})
	authed.HandleFunc("POST /v1/media", a.uploadMedia)

	// Feedback: agents ask for missing capabilities, maintainer agents turn them into PRs.
	authed.HandleFunc("POST /v1/feedback", a.tool("submit_feedback", nil))
	authed.HandleFunc("GET /v1/feedback", a.tool("list_feedback", nil))
	authed.HandleFunc("GET /v1/feedback/{id}", a.tool("get_feedback", map[string]string{"id": "id"}))
	authed.HandleFunc("PATCH /v1/feedback/{id}", a.tool("update_feedback", map[string]string{"id": "id"}))

	// Browser agent (runs on the owner's computer, pulls work).
	authed.HandleFunc("GET /v1/agent/tasks", a.tool("list_browser_tasks", nil))
	authed.HandleFunc("POST /v1/agent/tasks", a.tool("create_browser_task", nil))
	authed.HandleFunc("POST /v1/agent/tasks/claim", func(w http.ResponseWriter, r *http.Request) {
		t, err := a.Svc.Store.ClaimTask(r.Context())
		if errors.Is(err, store.ErrNotFound) {
			w.WriteHeader(204)
			return
		}
		if err != nil {
			a.fail(w, err)
			return
		}
		writeJSON(w, 200, t)
	})
	authed.HandleFunc("POST /v1/agent/tasks/{id}/result", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			OK     bool            `json:"ok"`
			Result json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal(body(r), &in); err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		if err := a.Svc.Store.CompleteTask(r.Context(), r.PathValue("id"), in.OK, string(in.Result)); err != nil {
			a.fail(w, err)
			return
		}
		w.WriteHeader(204)
	})

	mux.Handle("/", a.auth(authed))
	return a.logging(mux)
}

func (a *API) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		var role mcp.Role
		ctx := r.Context()
		switch {
		case a.Token != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(a.Token)) == 1:
			role = mcp.RoleAdmin
		case a.DraftToken != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(a.DraftToken)) == 1:
			role = mcp.RoleDrafter
		default:
			var acc oauth.Access
			ok := false
			if a.OAuth != nil && tok != "" {
				acc, ok = a.OAuth.Authenticate(r.Context(), tok)
			}
			if ok && acc.UserID != "" {
				ctx = mcp.WithUser(ctx, acc.UserID)
			}
			switch {
			case ok && acc.Scope == oauth.ScopeAdmin:
				role = mcp.RoleAdmin
			case ok && acc.Scope == oauth.ScopeDrafts:
				role = mcp.RoleDrafter
			default:
				a.unauthorized(w, tok != "")
				return
			}
		}
		if role != mcp.RoleAdmin && !drafterRoute(r) {
			writeJSON(w, 403, map[string]string{"error": mcp.ErrForbidden.Error()})
			return
		}
		next.ServeHTTP(w, r.WithContext(mcp.WithRole(ctx, role)))
	})
}

// unauthorized points OAuth clients to the protected resource metadata (RFC 9728 §5.1).
func (a *API) unauthorized(w http.ResponseWriter, gaveToken bool) {
	if a.OAuth == nil {
		w.Header().Set("WWW-Authenticate", `Bearer realm="relay"`)
	} else {
		v := `Bearer resource_metadata="` + a.OAuth.ResourceMetadataURL() + `"`
		if gaveToken {
			v += `, error="invalid_token"`
		}
		w.Header().Set("WWW-Authenticate", v)
	}
	writeJSON(w, 401, map[string]string{"error": "unauthorized"})
}

// drafterRoute lists the non-tool routes a drafter key may use; tool routes are checked per tool.
func drafterRoute(r *http.Request) bool {
	p := r.URL.Path
	switch {
	case strings.HasPrefix(p, "/v1/accounts/") && r.Method == "DELETE":
		return false
	case strings.HasPrefix(p, "/v1/agent/tasks/"): // claim / result are for the browser agent
		return false
	}
	return true
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(c int) { s.status = c; s.ResponseWriter.WriteHeader(c) }

func (a *API) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(sw, r)
		if r.URL.Path != "/healthz" {
			a.Log.Info("http", "method", r.Method, "path", r.URL.Path, "status", sw.status)
		}
	})
}

func body(r *http.Request) []byte {
	b, _ := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	return b
}

// tool builds a handler whose arguments are the JSON body, merged with query params and path values.
func (a *API) tool(name string, pathArgs map[string]string) http.HandlerFunc {
	return a.toolWith(name, pathArgs, nil)
}

// toolWith is tool with fixed arguments that win over the request's.
func (a *API) toolWith(name string, pathArgs map[string]string, fixed map[string]any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		args := map[string]any{}
		if b := body(r); len(b) > 0 {
			if err := json.Unmarshal(b, &args); err != nil {
				writeJSON(w, 400, map[string]string{"error": "invalid JSON body: " + err.Error()})
				return
			}
		}
		for k, vs := range r.URL.Query() {
			v := vs[0]
			if n, err := strconv.Atoi(v); err == nil && (k == "limit" || k == "offset" || k == "top" || k == "max") {
				args[k] = n
			} else if v == "true" || v == "false" {
				args[k] = v == "true"
			} else {
				args[k] = v
			}
		}
		for p, arg := range pathArgs {
			args[arg] = r.PathValue(p)
		}
		for k, v := range fixed {
			args[k] = v
		}
		raw, _ := json.Marshal(args)
		a.call(w, r.WithContext(mcp.WithSource(r.Context(), "rest")), name, raw)
	}
}

func (a *API) call(w http.ResponseWriter, r *http.Request, name string, args []byte) {
	res, err := a.MCP.Call(r.Context(), name, args)
	if err != nil {
		if res != nil {
			writeJSON(w, 502, map[string]any{"error": err.Error(), "partial": res})
			return
		}
		a.fail(w, err)
		return
	}
	writeJSON(w, 200, res)
}

func (a *API) fail(w http.ResponseWriter, err error) {
	var inv *core.ErrInvalid
	var denied *team.ErrDenied
	var bad *team.ErrBadInput
	switch {
	case errors.As(err, &denied):
		writeJSON(w, 403, map[string]string{"error": err.Error()})
	case errors.As(err, &bad):
		writeJSON(w, 400, map[string]string{"error": err.Error()})
	case errors.Is(err, team.ErrNoEmail), errors.Is(err, store.ErrLastOwner), errors.Is(err, store.ErrEmailTaken):
		writeJSON(w, 409, map[string]string{"error": err.Error()})
	case errors.Is(err, store.ErrNotFound):
		writeJSON(w, 404, map[string]string{"error": "not found"})
	case errors.Is(err, mcp.ErrForbidden):
		writeJSON(w, 403, map[string]string{"error": err.Error()})
	case errors.Is(err, mcp.ErrUnknownTool):
		writeJSON(w, 404, map[string]string{"error": err.Error()})
	case errors.As(err, &inv):
		writeJSON(w, 400, map[string]string{"error": err.Error()})
	default:
		writeJSON(w, 502, map[string]string{"error": err.Error()})
	}
}

func (a *API) uploadMedia(w http.ResponseWriter, r *http.Request) {
	ct := r.Header.Get("Content-Type")
	var m *store.Media
	var err error
	if strings.HasPrefix(ct, "multipart/") {
		f, hdr, ferr := r.FormFile("file")
		if ferr != nil {
			writeJSON(w, 400, map[string]string{"error": "multipart field 'file' is required"})
			return
		}
		defer f.Close()
		m, err = a.Svc.SaveMedia(r.Context(), f, hdr.Filename, hdr.Header.Get("Content-Type"))
	} else {
		var in struct {
			URL string `json:"url"`
		}
		if jerr := json.Unmarshal(body(r), &in); jerr != nil || in.URL == "" {
			writeJSON(w, 400, map[string]string{"error": "send multipart 'file' or JSON {\"url\": ...}"})
			return
		}
		m, err = a.Svc.SaveMediaFromURL(r.Context(), in.URL)
	}
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, 201, map[string]any{"media": m, "url": a.Svc.MediaURL(m)})
}

// serveFromBucket streams a media object from the bucket, forwarding Range so video players and
// networks that download in chunks (Instagram, TikTok) work.
func (a *API) serveFromBucket(w http.ResponseWriter, r *http.Request, key string) {
	resp, err := a.Svc.Blob.Get(r.Context(), key, r.Header.Get("Range"))
	if err != nil {
		var se *blob.StatusError
		switch {
		case errors.As(err, &se) && se.Code == 404:
			http.NotFound(w, r)
		case errors.As(err, &se) && se.Code == 416:
			w.WriteHeader(416)
		default:
			a.Log.Warn("media: bucket read failed", "key", key, "err", err)
			http.Error(w, "media unavailable", http.StatusBadGateway)
		}
		return
	}
	defer resp.Body.Close()
	for _, h := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "ETag", "Last-Modified"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable") // media ids never change
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
