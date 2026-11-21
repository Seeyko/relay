package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
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

// threadSchema: each part is a string, or an object with its own media.
func threadSchema() map[string]any {
	return map[string]any{"type": "array", "description": "Thread parts, instead of text. Each part is a string or {\"text\": \"...\", \"media\": [\"med_...\"]}. " +
		"thread[0].media is the media of part 1 (do not also give the top-level media). Media on later parts: X (up to 4 images or 1 video/GIF per part) and Threads " +
		"(image, video or carousel per part) only; on Facebook, Instagram and LinkedIn the later parts become text-only comments of the account on the post.",
		"items": map[string]any{"anyOf": []any{
			map[string]any{"type": "string"},
			obj(map[string]any{"text": str("Part text"), "media": strs("Media ids of this part")}),
		}}}
}

type sourceKey struct{}

// WithSource tags a call with its origin (rest or mcp), recorded on feedback.
func WithSource(ctx context.Context, src string) context.Context {
	return context.WithValue(ctx, sourceKey{}, src)
}

// list decodes a JSON array of strings or a comma separated string (REST query strings).
type list []string

func (l *list) UnmarshalJSON(b []byte) error {
	var arr []string
	if err := json.Unmarshal(b, &arr); err == nil {
		*l = arr
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("expected a list of strings or a comma separated string")
	}
	*l = nil
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			*l = append(*l, p)
		}
	}
	return nil
}

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
	return append(coreTools(svc), siteTools(svc)...)
}

func coreTools(svc *core.Service) []Tool {
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
			Name: "list_accounts",
			Description: "List connected social accounts (id, network, handle, avatar). Use the ids in schedule_post. avatar is the profile picture " +
				"(/media/<file> on this Relay), avatar_source says whether it comes from the network (auto) or from set_account_avatar (manual).",
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
				"provider":    str("Network name from list_providers, e.g. x, website, facebook, instagram, threads"),
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
			Name:        "update_account_credentials",
			Admin:       true,
			Description: "Change some credential fields of a connected account (new token after a reconnection, a refresh token...). Other fields are kept; a field set to \"\" is removed. The result is verified with the network and must be the same remote account, so posts and history stay attached. To have a human sign in again with the network instead (new permissions), use start_account_connection.",
			InputSchema: obj(map[string]any{
				"account_id":  str(""),
				"credentials": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}, "description": "Fields to set, as in connect_account"},
			}, "account_id", "credentials"),
			handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				a, err := decode[struct {
					AccountID   string            `json:"account_id"`
					Credentials map[string]string `json:"credentials"`
				}](raw)
				if err != nil {
					return nil, err
				}
				return svc.UpdateCredentials(ctx, a.AccountID, a.Credentials)
			},
		},
		{
			Name:  "set_account_avatar",
			Admin: true,
			Description: "Set the profile picture Relay shows for an account in its previews, or for every account (account_id \"all\"). " +
				"Relay downloads each network's own picture by itself (on connect, import_history and once a day); a picture set here wins " +
				"over it until cleared with clear: true, which brings the network's picture back. Give media (an image id from the media library) " +
				"or media_url (an image URL, downloaded into the library).",
			InputSchema: obj(map[string]any{
				"account_id": str("Account id, or \"all\" for every account"),
				"media":      str("Image media id (from upload_media_from_url or the media library)"),
				"media_url":  str("Image URL to download, instead of media"),
				"clear":      map[string]any{"type": "boolean", "description": "Remove the manual picture: the network's own picture comes back"},
			}, "account_id"),
			handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				in, err := decode[core.AvatarInput](raw)
				if err != nil {
					return nil, err
				}
				return svc.SetAvatar(ctx, in)
			},
		},
		{
			Name: "schedule_post",
			Description: "Schedule a post (or a thread) on one or several accounts. scheduled_at is RFC3339 with timezone (e.g. 2026-11-20T09:00:00+01:00); " +
				"omit it or use 'now' to publish within 30 seconds, or 'draft' to keep it as a draft. A draft can carry planned_at, the time it is meant to go out: " +
				"it shows in the owner's calendar and approve_post schedules it then. Use per_account to give a network its own text. " +
				"Threads: on X and Threads each part is a reply to the previous one; on Facebook, Instagram and LinkedIn part 1 is the post and parts 2..n " +
				"are published right after as comments of the account on it. Options: LinkedIn images_as_document (+ document_title) sends 2-300 images as one " +
				"swipeable PDF document; Instagram trial_reel (+ trial_graduation MANUAL or SS_PERFORMANCE) publishes a Reel as a trial Reel.",
			InputSchema: obj(map[string]any{
				"account_ids":  strs("Target account ids"),
				"text":         str("Post text"),
				"thread":       threadSchema(),
				"media":        strs("Media ids from upload_media_from_url (media of the first part)"),
				"scheduled_at": str("RFC3339, 'now' or 'draft'"),
				"planned_at":   str("Drafts only: intended publish time, RFC3339 with offset. Nothing is published until approve_post."),
				"per_account":  map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}, "description": "account id -> text override"},
				"options":      map[string]any{"type": "object", "description": "Provider options, e.g. {\"reply_to\": \"<remote id>\", \"alt\": \"image description\"}"},
			}, "account_ids"),
			handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				in, err := decode[core.ScheduleInput](raw)
				if err != nil {
					return nil, err
				}
				if RoleFrom(ctx) == RoleDrafter {
					// Drafters stage, humans approve. A date they ask for becomes the draft's planned time.
					if _, err := time.Parse(time.RFC3339, strings.TrimSpace(in.ScheduledAt)); err == nil && in.PlannedAt == "" {
						in.PlannedAt = in.ScheduledAt
					}
					in.ScheduledAt = "draft"
				}
				return svc.Schedule(ctx, in)
			},
		},
		{
			Name: "list_posts",
			Description: "List posts, newest first. Filter by status (draft, scheduled, published, failed), account, source (relay or imported) and date range. " +
				"Dates are published_at, else scheduled_at, else planned_at (drafts), else created_at.",
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
			Description: "Get one post with its status, link, error and latest metrics. Media are ids: get_media with post=<id> gives their public urls.",
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
			Name: "update_post",
			Description: "Edit a draft, scheduled or failed post: text, thread, media, options, scheduled_at (RFC3339, 'now', 'draft') or planned_at (drafts only, RFC3339; '' or 'none' clears it). " +
				"Rescheduling a failed post retries it. Moving a scheduled post back to 'draft' keeps its time as planned_at.",
			InputSchema: obj(map[string]any{"id": str("Post id"), "text": str(""), "thread": threadSchema(), "media": strs("Media of the first part"), "scheduled_at": str(""), "planned_at": str("Drafts only: RFC3339, or '' / 'none' to clear"), "options": map[string]any{"type": "object"}}, "id"),
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
			Name:  "delete_post",
			Admin: true,
			Description: "Delete a post from Relay. By default the published post stays on the network. With remote=true, Relay first deletes it " +
				"from the network, with every part of its thread (X, Threads, Facebook, LinkedIn); the Relay copy is kept if the network refuses, " +
				"and the call can be repeated. Ask the user before using remote=true: it cannot be undone. For a post of a website account, " +
				"delete_on_site: true also deletes the article on the site (needs its delete capability; an article already gone is fine).",
			InputSchema: obj(map[string]any{"id": str("Post id"),
				"remote":         map[string]any{"type": "boolean", "description": "Also delete the published post from the network (default false)"},
				"delete_on_site": map[string]any{"type": "boolean", "description": "website posts only: also delete the article on the site"}}, "id"),
			handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				a, err := decode[struct {
					ID           string `json:"id"`
					Remote       bool   `json:"remote"`
					DeleteOnSite bool   `json:"delete_on_site"`
				}](raw)
				if err != nil {
					return nil, err
				}
				if a.DeleteOnSite {
					p, err := svc.Store.GetPost(ctx, a.ID)
					if err != nil {
						return nil, err
					}
					if err := svc.DeleteOnSite(ctx, p); err != nil {
						return nil, err
					}
				}
				res, err := svc.DeletePost(ctx, a.ID, a.Remote)
				if err == nil && a.DeleteOnSite {
					res.DeletedOnSite = true
				}
				return res, err
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
			Name: "get_media",
			Description: "Get media files to show them to a person: filename, mime, size and a public url that opens in a browser " +
				"without a Relay login (download it with a plain GET, e.g. to attach it to a chat message). Give media ids, " +
				"or a post id to get every media of that post in publishing order (carousel slides, then the media of each thread part).",
			InputSchema: obj(map[string]any{
				"ids":  strs("Media ids (med_...)"),
				"post": str("Post id: list the media of this post, in order"),
			}),
			handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				a, err := decode[struct {
					IDs  list   `json:"ids"`
					Post string `json:"post"`
				}](raw)
				if err != nil {
					return nil, err
				}
				return svc.MediaLinks(ctx, a.IDs, a.Post)
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
			Description: "Answer analytics questions across networks with normalized metrics (views, likes, comments, shares, saves, engagement, engagement_rate). " +
				"Examples: views on every network except X last month -> {period:\"last_month\", exclude_providers:[\"x\"], metric:\"views\"}; " +
				"likes per network per week this quarter -> {period:\"this_quarter\", group_by:\"network,week\", metric:\"likes\"}; " +
				"best Instagram and TikTok videos with #skatepark this year -> {period:\"this_year\", providers:[\"instagram\",\"tiktok\"], formats:[\"video\",\"reel\"], hashtags:[\"skatepark\"], metric:\"views\", top:10}. " +
				"Period: period (named) or since/until (RFC 3339, a date 2026-10-01 in the instance timezone, or a duration ago like 720h; until is exclusive); default last 30 days. " +
				"Filters (AND between fields, OR inside a list; lists may also be comma separated strings): providers, exclude_providers, account_ids, exclude_account_ids, " +
				"formats, exclude_formats (text, thread, reply, image, video, carousel, reel, story, short, article, document, quote, repost...), hashtags (any of), text_contains, has_link, has_media. " +
				"basis: published (default: posts published in the period, lifetime counters) or activity (what all posts gained during the period, from hourly/daily snapshots; " +
				"only counts posts Relay was already measuring at the start). compare: previous (default), previous_year or none. " +
				"group_by: one or several comma separated of network, account, day, week, month, weekday, hour, format, length, hashtag, has_link. " +
				"Returns totals, change vs the comparison period, groups (avg/median of metric, totals, engagement rate), top and bottom posts, and notes. Reposts are excluded unless include_reposts.",
			InputSchema: obj(map[string]any{
				"period": str("One of " + strings.Join(core.Periods, ", ") + " (calendar periods use the instance timezone, weeks start Monday)"),
				"since":  str("RFC 3339, date 2006-01-02 or duration ago like 720h"), "until": str("Same formats, exclusive, default now"),
				"providers": strs("Only these networks: x, threads, tiktok, youtube, instagram, facebook, linkedin-page, website"), "exclude_providers": strs("All networks except these"),
				"account_ids": strs("Only these accounts"), "exclude_account_ids": strs("All accounts except these"),
				"formats": strs("Only these formats"), "exclude_formats": strs("Exclude these formats"),
				"hashtags": strs("Posts with any of these hashtags"), "text_contains": str("Case-insensitive text search"),
				"has_link": map[string]any{"type": "boolean"}, "has_media": map[string]any{"type": "boolean"},
				"basis": str("published (default) or activity"), "compare": str("previous (default), previous_year or none"),
				"account_id": str("Deprecated: use account_ids"), "provider": str("Deprecated: use providers"),
				"group_by": str("see description"), "metric": str("views | likes | comments | shares | saves | engagement | engagement_rate | any raw key; default engagement"),
				"top": num("top/bottom posts to return, default 5"), "include_reposts": map[string]any{"type": "boolean"},
			}),
			handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				a, err := decode[struct {
					Period            string `json:"period"`
					Since             string `json:"since"`
					Until             string `json:"until"`
					AccountID         string `json:"account_id"`
					Provider          string `json:"provider"`
					Providers         list   `json:"providers"`
					ExcludeProviders  list   `json:"exclude_providers"`
					AccountIDs        list   `json:"account_ids"`
					ExcludeAccountIDs list   `json:"exclude_account_ids"`
					Formats           list   `json:"formats"`
					ExcludeFormats    list   `json:"exclude_formats"`
					Hashtags          list   `json:"hashtags"`
					TextContains      string `json:"text_contains"`
					HasLink           *bool  `json:"has_link"`
					HasMedia          *bool  `json:"has_media"`
					Basis             string `json:"basis"`
					Compare           string `json:"compare"`
					GroupBy           string `json:"group_by"`
					Metric            string `json:"metric"`
					Top               int    `json:"top"`
					IncludeReposts    bool   `json:"include_reposts"`
				}](raw)
				if err != nil {
					return nil, err
				}
				now := time.Now().UTC()
				if svc.Now != nil {
					now = svc.Now()
				}
				in := core.AnalyzeInput{AccountID: a.AccountID, Provider: a.Provider, GroupBy: a.GroupBy, Metric: a.Metric, Top: a.Top, IncludeReposts: a.IncludeReposts,
					Location: svc.Location, Basis: a.Basis, Compare: a.Compare,
					Filter: core.Filter{Providers: a.Providers, ExcludeProviders: a.ExcludeProviders, AccountIDs: a.AccountIDs, ExcludeAccountIDs: a.ExcludeAccountIDs,
						Formats: a.Formats, ExcludeFormats: a.ExcludeFormats, Hashtags: a.Hashtags, TextContains: a.TextContains, HasLink: a.HasLink, HasMedia: a.HasMedia}}
				if a.Period != "" {
					if a.Since != "" || a.Until != "" {
						return nil, &core.ErrInvalid{Msg: "use period or since/until, not both"}
					}
					if in.Since, in.Until, err = core.ResolvePeriod(a.Period, now, svc.Location); err != nil {
						return nil, err
					}
				} else {
					in.Since = now.Add(-30 * 24 * time.Hour)
					if a.Since != "" {
						if in.Since, err = core.ParseTime(a.Since, now, svc.Location); err != nil {
							return nil, &core.ErrInvalid{Msg: "since: " + err.Error()}
						}
					}
					if a.Until != "" {
						if in.Until, err = core.ParseTime(a.Until, now, svc.Location); err != nil {
							return nil, &core.ErrInvalid{Msg: "until: " + err.Error()}
						}
					}
				}
				return svc.Analyze(ctx, in)
			},
		},
		{
			Name: "submit_feedback",
			Description: "Tell Relay's maintainers what is missing or broken. Use it whenever the user asks for something this API/MCP cannot do " +
				"(missing tool, filter, network, post option, metric...) or when a tool misbehaves. Write the full context: the user's request in their words, " +
				"what you tried (tools and arguments), what happened, and what would have solved it. Attach screenshots or examples with media (ids from " +
				"upload_media_from_url / POST /v1/media) or media_urls. Agents that maintain Relay read open feedback and open pull requests. " +
				"Returns the feedback id; check it later with get_feedback.",
			InputSchema: obj(map[string]any{
				"kind":       str("missing_feature (default), bug, improvement or other"),
				"title":      str("One line, max 200 characters"),
				"body":       str("Full text (Markdown): user request, what you tried, what happened, what is needed"),
				"media":      strs("Media ids (screenshots, example files)"),
				"media_urls": strs("http(s) URLs Relay downloads into its media library"),
				"context":    map[string]any{"type": "object", "description": "Optional structured context: tool, arguments, provider, error, client name..."},
			}, "title", "body"),
			handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				a, err := decode[struct {
					Kind      string         `json:"kind"`
					Title     string         `json:"title"`
					Body      string         `json:"body"`
					Media     list           `json:"media"`
					MediaURLs list           `json:"media_urls"`
					Context   map[string]any `json:"context"`
				}](raw)
				if err != nil {
					return nil, err
				}
				src := "mcp"
				if s, ok := ctx.Value(sourceKey{}).(string); ok {
					src = s
				}
				return svc.SubmitFeedback(ctx, core.FeedbackInput{Kind: a.Kind, Title: a.Title, Body: a.Body, Media: a.Media, MediaURLs: a.MediaURLs, Context: a.Context, Source: src, Role: string(RoleFrom(ctx))})
			},
		},
		{
			Name:        "get_feedback",
			Description: "Read one feedback with its status (open, triaged, in_progress, done, rejected, duplicate), resolution and pull request URL.",
			InputSchema: obj(map[string]any{"id": str("Feedback id")}, "id"),
			handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				a, err := decode[struct{ ID string }](raw)
				if err != nil {
					return nil, err
				}
				return svc.Store.GetFeedback(ctx, a.ID)
			},
		},
		{
			Name:        "list_feedback",
			Description: "List feedback, oldest first, for the agents that maintain Relay. Default: status open.",
			Admin:       true,
			InputSchema: obj(map[string]any{"status": strs("Statuses to include (open, triaged, in_progress, done, rejected, duplicate) or [\"all\"]; default open"), "limit": num("default 100, max 500")}),
			handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				a, err := decode[struct {
					Status list `json:"status"`
					Limit  int  `json:"limit"`
				}](raw)
				if err != nil {
					return nil, err
				}
				st := []string(a.Status)
				if len(st) == 0 {
					st = []string{"open"}
				}
				if len(st) == 1 && st[0] == "all" {
					st = nil
				}
				fs, err := svc.Store.ListFeedback(ctx, st, a.Limit)
				if fs == nil && err == nil {
					fs = []*store.Feedback{}
				}
				return fs, err
			},
		},
		{
			Name:        "update_feedback",
			Description: "Move a feedback through its workflow (triaged, in_progress, done, rejected, duplicate) with a resolution note and the pull request URL.",
			Admin:       true,
			InputSchema: obj(map[string]any{"id": str(""), "status": str(""), "resolution": str("What was done or why not"), "pr_url": str("https URL of the pull request")}, "id"),
			handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				a, err := decode[struct {
					ID         string `json:"id"`
					Status     string `json:"status"`
					Resolution string `json:"resolution"`
					PRURL      string `json:"pr_url"`
				}](raw)
				if err != nil {
					return nil, err
				}
				return svc.UpdateFeedback(ctx, a.ID, a.Status, a.Resolution, a.PRURL)
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
			InputSchema: obj(map[string]any{"account_ids": strs(""), "text": str(""), "thread": threadSchema(), "media": strs(""), "per_account": map[string]any{"type": "object"}, "options": map[string]any{"type": "object"}}, "account_ids"),
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
			Description: "Approve a draft: schedule it at scheduled_at (RFC3339) or, when omitted, at its planned_at if that is still in the future, else publish within one tick. Use it to release drafts staged by a drafter key.",
			InputSchema: obj(map[string]any{"id": str("Post id"), "scheduled_at": str("RFC3339 or 'now'")}, "id"),
			handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				a, err := decode[struct {
					ID          string `json:"id"`
					ScheduledAt string `json:"scheduled_at"`
				}](raw)
				if err != nil {
					return nil, err
				}
				return svc.Approve(ctx, a.ID, a.ScheduledAt)
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
			Name: "import_history",
			Description: "Import an account's past posts (with metrics and visuals) into Relay's archive, e.g. for the 'Mon contenu' page. " +
				"Each post's images (photos, video covers) are copied into the media library. Re-running refreshes metrics and fills the visuals " +
				"of imported posts that have none yet (backfill); posts created in Relay are never changed.",
			InputSchema: obj(map[string]any{"account_id": str(""), "since": str("RFC3339, optional"), "max": num("default: everything the API allows"),
				"visuals": map[string]any{"type": "boolean", "description": "download the posts' images (default true)"}}, "account_id"),
			handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				a, err := decode[struct {
					AccountID string `json:"account_id"`
					Since     string `json:"since"`
					Max       int    `json:"max"`
					Visuals   *bool  `json:"visuals"`
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
				return svc.ImportHistory(ctx, a.AccountID, since, a.Max, core.ImportOptions{SkipVisuals: a.Visuals != nil && !*a.Visuals})
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
		{
			Name:        "list_oauth_grants",
			Admin:       true,
			Description: "List the apps connected with OAuth (claude.ai, ChatGPT, Claude Code...): id, app name, scope (relay:admin = full access, relay:drafts = drafts only), created_at, last_used_at, expires_at. Tokens are never shown.",
			InputSchema: obj(map[string]any{}),
			handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				return svc.Store.ListOAuthGrants(ctx)
			},
		},
		{
			Name:        "revoke_oauth_grant",
			Admin:       true,
			Description: "Disconnect an app connected with OAuth: every token issued to it stops working at once. The app must be approved again (with the owner's passkey) to reconnect.",
			InputSchema: obj(map[string]any{"id": str("grant id from list_oauth_grants")}, "id"),
			handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				a, err := decode[struct{ ID string }](raw)
				if err != nil {
					return nil, err
				}
				if err := svc.Store.DeleteOAuthGrant(ctx, a.ID); err != nil {
					return nil, err
				}
				return map[string]any{"revoked": a.ID}, nil
			},
		},
	}
}
