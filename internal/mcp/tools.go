package mcp

import (
	"context"
	"encoding/json"
	"time"

	"github.com/Seeyko/relay/internal/core"
	"github.com/Seeyko/relay/internal/providers"
	"github.com/Seeyko/relay/internal/store"
)

type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	// Admin tools are hidden from and refused to drafter keys.
	Admin   bool `json:"-"`
	handler func(ctx context.Context, args json.RawMessage) (any, error)
}

func obj(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
func strs(desc string) map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": desc}
}
func num(desc string) map[string]any { return map[string]any{"type": "integer", "description": desc} }

func decode[T any](raw json.RawMessage) (T, error) {
	var v T
	if len(raw) == 0 || string(raw) == "null" {
		return v, nil
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return v, &core.ErrInvalid{Msg: "invalid arguments: " + err.Error()}
	}
	return v, nil
}

func parseSince(s string, def time.Duration) (time.Time, error) {
	if s == "" {
		return time.Now().UTC().Add(-def), nil
	}
	if d, err := time.ParseDuration(s); err == nil {
		return time.Now().UTC().Add(-d), nil
	}
	return time.Parse(time.RFC3339, s)
}

// BuildTools exposes the service to agents.
func BuildTools(svc *core.Service) []Tool {
	return []Tool{
		{
			Name:        "list_providers",
			Description: "List the social networks Relay can connect to and the credential fields each one needs.",
			InputSchema: obj(map[string]any{}),
			handler: func(ctx context.Context, _ json.RawMessage) (any, error) {
				type p struct {
					Name        string   `json:"name"`
					Description string   `json:"description"`
					Credentials []string `json:"credential_fields"`
				}
				var out []p
				for _, f := range providers.All() {
					out = append(out, p{f.Name, f.Description, f.CredentialFields})
				}
				return out, nil
			},
		},
		{
			Name:        "list_accounts",
			Description: "List connected social accounts (id, network, handle). Use the ids in schedule_post.",
			InputSchema: obj(map[string]any{}),
			handler: func(ctx context.Context, _ json.RawMessage) (any, error) {
				return svc.Store.ListAccounts(ctx)
			},
		},
		{
			Name:        "connect_account",
			Admin:       true,
			Description: "Connect a new account. Credentials are verified against the network, then stored encrypted.",
			InputSchema: obj(map[string]any{
				"provider":    str("Network name from list_providers, e.g. x, website, facebook, instagram"),
				"name":        str("Optional label"),
				"credentials": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}, "description": "Credential fields listed by list_providers"},
			}, "provider", "credentials"),
			handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				a, err := decode[struct {
					Provider    string            `json:"provider"`
					Name        string            `json:"name"`
					Credentials map[string]string `json:"credentials"`
				}](raw)
				if err != nil {
					return nil, err
				}
				if a.Credentials == nil {
					a.Credentials = map[string]string{}
				}
				return svc.ConnectAccount(ctx, a.Provider, a.Name, a.Credentials)
			},
		},
		{
			Name: "schedule_post",
			Description: "Schedule a post (or a thread) on one or several accounts. scheduled_at is RFC3339 with timezone (e.g. 2026-11-20T09:00:00+01:00); " +
				"omit it or use 'now' to publish within 30 seconds, or 'draft' to keep it as a draft. Use per_account to give a network its own text.",
			InputSchema: obj(map[string]any{
				"account_ids":  strs("Target account ids"),
				"text":         str("Post text"),
				"thread":       strs("Thread parts, instead of text"),
				"media":        strs("Media ids from upload_media_from_url"),
				"scheduled_at": str("RFC3339, 'now' or 'draft'"),
				"per_account":  map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}, "description": "account id -> text override"},
				"options":      map[string]any{"type": "object", "description": "Provider options, e.g. {\"reply_to\": \"<remote id>\", \"alt\": \"image description\"}"},
			}, "account_ids"),
			handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				in, err := decode[core.ScheduleInput](raw)
				if err != nil {
					return nil, err
				}
				if RoleFrom(ctx) == RoleDrafter {
					in.ScheduledAt = "draft" // drafters stage, humans approve
				}
				return svc.Schedule(ctx, in)
			},
		},
		{
			Name:        "list_posts",
			Description: "List posts, newest first. Filter by status (draft, scheduled, published, failed), account, source (relay or imported) and date range.",
			InputSchema: obj(map[string]any{
				"status": str("draft | scheduled | publishing | published | failed"), "account_id": str(""), "source": str("relay | imported"),
				"from": str("RFC3339"), "to": str("RFC3339"), "limit": num("default 50"), "offset": num(""),
			}),
			handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				a, err := decode[struct {
					Status    string `json:"status"`
					AccountID string `json:"account_id"`
					Source    string `json:"source"`
					From      string `json:"from"`
					To        string `json:"to"`
					Limit     int    `json:"limit"`
					Offset    int    `json:"offset"`
				}](raw)
				if err != nil {
					return nil, err
				}
				f := store.PostFilter{Status: a.Status, AccountID: a.AccountID, Source: a.Source, Limit: a.Limit, Offset: a.Offset}
				if f.Limit == 0 {
					f.Limit = 50
				}
				if a.From != "" {
					t, err := time.Parse(time.RFC3339, a.From)
					if err != nil {
						return nil, err
					}
					f.From = &t
				}
				if a.To != "" {
					t, err := time.Parse(time.RFC3339, a.To)
					if err != nil {
						return nil, err
					}
					f.To = &t
				}
				return svc.Store.ListPosts(ctx, f)
			},
		},
		{
			Name:        "get_post",
			Description: "Get one post with its status, link, error and latest metrics.",
			InputSchema: obj(map[string]any{"id": str("Post id")}, "id"),
			handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				a, err := decode[struct{ ID string }](raw)
				if err != nil {
					return nil, err
				}
				return svc.Store.GetPost(ctx, a.ID)
			},
		},
		{
			Name:        "update_post",
			Description: "Edit a draft, scheduled or failed post: text, thread, media, options or scheduled_at (RFC3339, 'now', 'draft'). Rescheduling a failed post retries it.",
			InputSchema: obj(map[string]any{"id": str("Post id"), "text": str(""), "thread": strs(""), "media": strs(""), "scheduled_at": str(""), "options": map[string]any{"type": "object"}}, "id"),
			handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				a, err := decode[struct {
					ID string `json:"id"`
					core.UpdateInput
				}](raw)
				if err != nil {
					return nil, err
				}
				if RoleFrom(ctx) == RoleDrafter {
					p, err := svc.Store.GetPost(ctx, a.ID)
					if err != nil {
						return nil, err
					}
					if p.Status != store.StatusDraft || (a.ScheduledAt != nil && *a.ScheduledAt != "draft") {
						return nil, ErrForbidden
					}
				}
				return svc.UpdatePost(ctx, a.ID, a.UpdateInput)
			},
		},
		{
			Name:        "delete_post",
			Admin:       true,
			Description: "Delete a post from Relay (does not delete it from the network).",
			InputSchema: obj(map[string]any{"id": str("Post id")}, "id"),
			handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				a, err := decode[struct{ ID string }](raw)
				if err != nil {
					return nil, err
				}
				return map[string]bool{"deleted": true}, svc.Store.DeletePost(ctx, a.ID)
			},
		},
		{
			Name:        "publish_now",
			Admin:       true,
			Description: "Publish a draft or scheduled post immediately and return the result.",
			InputSchema: obj(map[string]any{"id": str("Post id")}, "id"),
			handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				a, err := decode[struct{ ID string }](raw)
				if err != nil {
					return nil, err
				}
				return svc.PublishNow(ctx, a.ID)
			},
		},
		{
			Name:        "upload_media_from_url",
			Description: "Download an image or video from a URL into Relay's media library and return its media id.",
			InputSchema: obj(map[string]any{"url": str("http(s) URL")}, "url"),
			handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				a, err := decode[struct{ URL string }](raw)
				if err != nil {
					return nil, err
				}
				return svc.SaveMediaFromURL(ctx, a.URL)
			},
		},
		{
			Name:        "refresh_metrics",
			Description: "Fetch fresh metrics (likes, reposts, replies, impressions when available) from the networks for posts published since a date or duration (default 30 days).",
			InputSchema: obj(map[string]any{"account_id": str("Optional, all accounts by default"), "since": str("RFC3339 or Go duration like 168h")}),
			handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				a, err := decode[struct {
					AccountID string `json:"account_id"`
					Since     string `json:"since"`
				}](raw)
				if err != nil {
					return nil, err
				}
				since, err := parseSince(a.Since, 30*24*time.Hour)
				if err != nil {
					return nil, err
				}
				n, err := svc.RefreshMetrics(ctx, a.AccountID, since)
				return map[string]any{"updated": n}, err
			},
		},
		{
			Name: "analyze",
			Description: "Analyse performance across all networks with normalized metrics (views, likes, comments, shares, saves, engagement, engagement_rate). " +
				"Returns totals vs the previous period of the same length, top and bottom posts, and optional groups. " +
				"group_by: network, account, day, week, month, weekday, hour, format (text/thread/reply/image/video/carousel/article), length, hashtag, has_link. " +
				"Use it to find what works: best hours, best formats, best networks. Reposts are excluded by default.",
			InputSchema: obj(map[string]any{
				"since": str("RFC3339 or duration like 720h (default 30 days)"), "until": str("RFC3339, default now"),
				"account_id": str(""), "provider": str("x, tiktok, youtube, instagram, facebook, website"),
				"group_by": str("see description"), "metric": str("views | likes | comments | shares | saves | engagement | engagement_rate | any raw key; default engagement"),
				"top": num("top/bottom posts to return, default 5"), "include_reposts": map[string]any{"type": "boolean"},
			}),
			handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				a, err := decode[struct {
					Since          string `json:"since"`
					Until          string `json:"until"`
					AccountID      string `json:"account_id"`
					Provider       string `json:"provider"`
					GroupBy        string `json:"group_by"`
					Metric         string `json:"metric"`
					Top            int    `json:"top"`
					IncludeReposts bool   `json:"include_reposts"`
				}](raw)
				if err != nil {
					return nil, err
				}
				since, err := parseSince(a.Since, 30*24*time.Hour)
				if err != nil {
					return nil, &core.ErrInvalid{Msg: "since: " + err.Error()}
				}
				in := core.AnalyzeInput{Since: since, AccountID: a.AccountID, Provider: a.Provider, GroupBy: a.GroupBy, Metric: a.Metric, Top: a.Top, IncludeReposts: a.IncludeReposts, Location: svc.Location}
				if a.Until != "" {
					if in.Until, err = time.Parse(time.RFC3339, a.Until); err != nil {
						return nil, &core.ErrInvalid{Msg: "until: " + err.Error()}
					}
				}
				return svc.Analyze(ctx, in)
			},
		},
		{
			Name:        "post_timeline",
			Description: "Growth curve of one post: every metrics snapshot with its age, plus normalized metrics at 1h, 24h, 7d and 30d.",
			InputSchema: obj(map[string]any{"id": str("Post id")}, "id"),
			handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				a, err := decode[struct{ ID string }](raw)
				if err != nil {
					return nil, err
				}
				return svc.PostTimeline(ctx, a.ID)
			},
		},
		{
			Name:        "account_growth",
			Description: "Followers / following / post counts over time per account (snapshots every 6h), with the change and the daily rate. Set refresh=true to take a snapshot now.",
			InputSchema: obj(map[string]any{"account_id": str("Optional"), "since": str("RFC3339 or duration, default 720h"), "points": map[string]any{"type": "boolean", "description": "include every snapshot"}, "refresh": map[string]any{"type": "boolean"}}),
			handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				a, err := decode[struct {
					AccountID string `json:"account_id"`
					Since     string `json:"since"`
					Points    bool   `json:"points"`
					Refresh   bool   `json:"refresh"`
				}](raw)
				if err != nil {
					return nil, err
				}
				since, err := parseSince(a.Since, 30*24*time.Hour)
				if err != nil {
					return nil, &core.ErrInvalid{Msg: "since: " + err.Error()}
				}
				if a.Refresh {
					accs, err := svc.Store.ListAccounts(ctx)
					if err != nil {
						return nil, err
					}
					for _, acc := range accs {
						if a.AccountID == "" || acc.ID == a.AccountID {
							_, _ = svc.SnapshotAccount(ctx, acc)
						}
					}
				}
				return svc.AccountGrowth(ctx, a.AccountID, since, a.Points)
			},
		},
		{
			Name:        "sql_query",
			Description: "Run one read-only SQL query (SQLite) on Relay's data for analyses the other tools do not cover. Max 1000 rows. Schema:\n" + store.Schema,
			InputSchema: obj(map[string]any{"query": str("SELECT ... or WITH ..."), "max_rows": num("default 200")}, "query"),
			handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				a, err := decode[struct {
					Query   string `json:"query"`
					MaxRows int    `json:"max_rows"`
				}](raw)
				if err != nil {
					return nil, err
				}
				res, err := svc.Store.Query(ctx, a.Query, a.MaxRows)
				if err != nil {
					return nil, &core.ErrInvalid{Msg: err.Error()}
				}
				return res, nil
			},
		},
		{
			Name:        "validate_post",
			Description: "Pre-flight check of a post against each target network (character limits with X link weighting, media count and types, required title...). Same arguments as schedule_post; nothing is saved.",
			InputSchema: obj(map[string]any{"account_ids": strs(""), "text": str(""), "thread": strs(""), "media": strs(""), "per_account": map[string]any{"type": "object"}, "options": map[string]any{"type": "object"}}, "account_ids"),
			handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				in, err := decode[core.ScheduleInput](raw)
				if err != nil {
					return nil, err
				}
				return svc.Validate(ctx, in)
			},
		},
		{
			Name:        "approve_post",
			Admin:       true,
			Description: "Approve a draft: schedule it at scheduled_at (RFC3339) or publish within one tick when omitted. Use it to release drafts staged by a drafter key.",
			InputSchema: obj(map[string]any{"id": str("Post id"), "scheduled_at": str("RFC3339 or 'now'")}, "id"),
			handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				a, err := decode[struct {
					ID          string `json:"id"`
					ScheduledAt string `json:"scheduled_at"`
				}](raw)
				if err != nil {
					return nil, err
				}
				p, err := svc.Store.GetPost(ctx, a.ID)
				if err != nil {
					return nil, err
				}
				if p.Status != store.StatusDraft {
					return nil, &core.ErrInvalid{Msg: "post is " + p.Status + ", not a draft"}
				}
				at := a.ScheduledAt
				if at == "" {
					at = "now"
				}
				return svc.UpdatePost(ctx, a.ID, core.UpdateInput{ScheduledAt: &at})
			},
		},
		{
			Name:        "list_comments",
			Description: "List replies/comments received on published posts. Set refresh=true with post_id to fetch new ones from the network first. pending_only hides comments already answered.",
			InputSchema: obj(map[string]any{"post_id": str(""), "account_id": str(""), "refresh": map[string]any{"type": "boolean"}, "pending_only": map[string]any{"type": "boolean"}, "limit": num("")}),
			handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				a, err := decode[struct {
					PostID      string `json:"post_id"`
					AccountID   string `json:"account_id"`
					Refresh     bool   `json:"refresh"`
					PendingOnly bool   `json:"pending_only"`
					Limit       int    `json:"limit"`
				}](raw)
				if err != nil {
					return nil, err
				}
				if a.Refresh && a.PostID != "" {
					if _, err := svc.RefreshComments(ctx, a.PostID); err != nil {
						return nil, err
					}
				}
				return svc.Store.ListComments(ctx, store.CommentFilter{AccountID: a.AccountID, PostID: a.PostID, OnlyPending: a.PendingOnly, Limit: a.Limit})
			},
		},
		{
			Name:        "reply_comment",
			Admin:       true,
			Description: "Reply publicly to a comment (or to any post) on the network. Use the comment's remote_id and account_id from list_comments.",
			InputSchema: obj(map[string]any{"account_id": str(""), "remote_id": str("Remote id of the comment or post to answer"), "text": str("")}, "account_id", "remote_id", "text"),
			handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				a, err := decode[struct {
					AccountID string `json:"account_id"`
					RemoteID  string `json:"remote_id"`
					Text      string `json:"text"`
				}](raw)
				if err != nil {
					return nil, err
				}
				return svc.ReplyToComment(ctx, a.AccountID, a.RemoteID, a.Text)
			},
		},
		{
			Name:        "import_history",
			Description: "Import an account's past posts (with metrics) into Relay's archive, e.g. for the 'Mon contenu' page. Re-running only refreshes metrics.",
			InputSchema: obj(map[string]any{"account_id": str(""), "since": str("RFC3339, optional"), "max": num("default: everything the API allows")}, "account_id"),
			handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				a, err := decode[struct {
					AccountID string `json:"account_id"`
					Since     string `json:"since"`
					Max       int    `json:"max"`
				}](raw)
				if err != nil {
					return nil, err
				}
				var since time.Time
				if a.Since != "" {
					if since, err = time.Parse(time.RFC3339, a.Since); err != nil {
						return nil, err
					}
				}
				c, u, err := svc.ImportHistory(ctx, a.AccountID, since, a.Max)
				return map[string]int{"created": c, "updated": u}, err
			},
		},
		{
			Name:        "create_browser_task",
			Admin:       true,
			Description: "Queue a task for the browser agent running on the owner's computer (for networks without a usable API: TikTok comments, LinkedIn stats...). kind examples: tiktok.comments, linkedin.metrics, linkedin.reply.",
			InputSchema: obj(map[string]any{"account_id": str(""), "kind": str(""), "payload": map[string]any{"type": "object"}}, "kind"),
			handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				a, err := decode[store.AgentTask](raw)
				if err != nil {
					return nil, err
				}
				t := &store.AgentTask{AccountID: a.AccountID, Kind: a.Kind, Payload: a.Payload}
				return t, svc.Store.CreateTask(ctx, t)
			},
		},
		{
			Name:        "list_browser_tasks",
			Description: "List browser-agent tasks and their results. status: pending, claimed, done, failed.",
			InputSchema: obj(map[string]any{"status": str("")}),
			handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				a, err := decode[struct{ Status string }](raw)
				if err != nil {
					return nil, err
				}
				return svc.Store.ListTasks(ctx, a.Status)
			},
		},
	}
}
