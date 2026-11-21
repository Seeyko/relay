# Relay

**A headless, agent-first social media scheduler.** No dashboard: you (or your AI agent) drive it through an
[MCP](https://modelcontextprotocol.io) server or a REST API. One static Go binary, one SQLite file,
about 11 MB of RAM at idle. A lightweight, self-hosted alternative to tools like Postiz or Buffer.

- Schedule posts and threads on several accounts at once, with images and videos
- Publish on time (built-in scheduler, automatic retries on temporary errors)
- **Analytics agents can act on**: metric snapshots over time (growth curves at 1h / 24h / 7d), follower growth,
  normalized metrics across networks (views, likes, comments, shares, saves, engagement rate), comparisons with
  the previous period or the previous year, breakdowns by network, hour, weekday, format or hashtag (or two of them,
  e.g. network × month), named periods (`last_month`, `this_quarter`...), filters (include or exclude networks,
  accounts and formats, hashtags, text, links, media), "what happened during the period" from snapshots, and read-only SQL
- Read and reply to comments
- Pre-flight validation (character limits, media rules) and a **drafts-only API key** for sub-agents
- Import an account's history (build a "my content" archive), with each post's images and video covers
- Signed webhooks (`post.scheduled`, `post.published`, `post.failed`, `comment.new`, `feedback.new`, `feedback.updated`)
- **Agent feedback loop**: when a user asks for something Relay cannot do, the agent files it with `submit_feedback`
  (full text + screenshots); maintainer agents read open feedback and open pull requests
- A task queue for a local browser agent, for networks without a usable API
- A small open protocol to plug in **any website** as a channel ([Relay Site Protocol](docs/SITE_PROTOCOL.md))

## Connectors

| Provider | Publish | Metrics | Comments | Credentials |
| --- | --- | --- | --- | --- |
| `x` | text, threads, images, videos | public + impressions (< 30 days) | read + reply | OAuth 1.0a user keys |
| `linkedin-page` | text, link/article, image, multi-image (2-20), video, document (PDF...); extra thread parts as comments | impressions, unique impressions, clicks, likes, comments, shares (organic, 12 months) + followers | read + reply | `organization_id`, `access_token`, `refresh_token`, `client_id`, `client_secret` |
| `facebook` | text, link, photos, video, Reels (Pages) | reactions, comments, shares | read + reply | `page_id`, `page_access_token` |
| `instagram` | image, carousel (up to 20), Reels, stories | likes, comments, views, reach, saves, shares | read + reply | `access_token` (Instagram Login, auto-refreshed) |
| `threads` | text (500), image, video, carousel (2-20), threads (reply chains), replies, quotes, link attachment | views, likes, replies, reposts, quotes, shares; followers | read + reply | `access_token` (long-lived, auto-refreshed), optional `user_id` |
| `youtube` | videos and Shorts (resumable upload) | views, likes, comments (+ YouTube Analytics) | read + reply | `client_id`, `client_secret`, `refresh_token` |
| `tiktok` | videos, photo posts (Content Posting API) | views, likes, comments, shares | not available in the public API | `client_key`, `client_secret`, `access_token`, `refresh_token` |
| `website` | Markdown articles | if the site supports it | if the site supports it | `base_url`, `token` |

TikTok requires `options.privacy_level` on every post (one of the values the creator's account allows), and
unaudited TikTok apps can only post privately. Meta connectors use Graph API v26.0; Threads uses the Threads API v1.0. The LinkedIn connector posts as a company page with Community
Management API scopes and sends `LinkedIn-Version: 202611` (override with the `api_version` credential). Contributions for other
networks are welcome, see [CONTRIBUTING.md](CONTRIBUTING.md).

You create your own developer apps on each network (X Developer Portal, Meta for Developers...) and give Relay
the resulting tokens. Relay never ships with shared API keys.

## Quick start

```bash
cp .env.example .env        # set RELAY_API_TOKEN and RELAY_ENCRYPTION_KEY at least
go run ./cmd/relay
```

or with Docker:

```bash
docker compose up -d
```

If the `X_*` variables are set, the X account is connected automatically on first start. Other accounts are
connected through the `connect_account` tool or `POST /v1/accounts`.

## Configuration

| Variable | Default | Description |
| --- | --- | --- |
| `RELAY_API_TOKEN` | required | Bearer token for the REST API and `/mcp` (16+ chars) |
| `RELAY_ENCRYPTION_KEY` | required | Encrypts stored credentials (AES-256-GCM). Never change it once accounts exist. |
| `RELAY_BASE_URL` | | Public URL of this instance. Required for Instagram and Threads, which download media from Relay. |
| `RELAY_ADDR` | `:8080` | Listen address |
| `RELAY_DATA_DIR` | `./data` (`/data` in Docker) | SQLite database and media files |
| `RELAY_TICK` | `30s` | How often due posts are checked |
| `RELAY_COLLECT_INTERVAL` | `1h` | Collector tick. Posts are refreshed hourly for 48h, every 6h until 7 days, then daily (`0s` disables) |
| `RELAY_TIMEZONE` | `UTC` | Timezone used to group analytics by day and hour |
| `RELAY_S3_ENDPOINT`, `RELAY_S3_BUCKET`, `RELAY_S3_ACCESS_KEY_ID`, `RELAY_S3_SECRET_ACCESS_KEY` | | Store media in an S3-compatible bucket (Cloudflare R2, S3, MinIO...) instead of `/data/media` |
| `RELAY_S3_REGION` | `auto` | Bucket region (`auto` for R2) |
| `RELAY_S3_PUBLIC_URL` | | Public base URL of the bucket (custom domain or r2.dev). Empty: the bucket stays private and Relay serves media at `/media/` |
| `RELAY_UI` | `on` | `off` disables the read-only dashboard at `/ui/` |
| `RELAY_PASSKEY_REGISTRATION` | `on` | `off` refuses new passkeys, even with the API token (existing ones keep working) |
| `RELAY_DRAFT_TOKEN` | | Optional second key that can only create and edit drafts |
| `RELAY_WEBHOOK_URL` / `RELAY_WEBHOOK_SECRET` | | Webhook target and HMAC secret |
| `RELAY_AGENT_INSTRUCTIONS` | | Extra instructions sent to MCP clients (timezone, tone, languages...) |
| `X_CONSUMER_KEY`, `X_CONSUMER_SECRET`, `X_ACCESS_TOKEN`, `X_ACCESS_TOKEN_SECRET` | | Optional X bootstrap |

## Deploy

Any Docker host works (Dokploy, Coolify, Portainer, plain Compose):

1. Build from the `Dockerfile` (or use `docker-compose.yml`).
2. Set the environment variables above.
3. Mount a persistent volume on `/data`.
4. Expose port `8080` behind HTTPS and set `RELAY_BASE_URL` to that URL.

Back up `/data/relay.db` and `/data/media` (or your bucket). Media uploaded before a bucket was configured stay on disk
and keep working.

## Dashboard

A minimal, read-only dashboard at `https://<your-host>/ui/`:

- **Calendar**: week and day views of every post across networks. Drafts with a planned time (`planned_at`) show at
  that time with a dashed outline and a "Draft" label.
- **Drafts**: every draft, dated or not, newest first. One card per `schedule_post` call, with each target network
  and the number of pre-flight problems.
- **Preview** (tap any post): the whole group, one tab per account, laid out roughly the way each network shows it
  (thread with a counter per post, Instagram caption cut, YouTube title and description, LinkedIn post and comments),
  with every media in order (images, playable videos), the options, the planned or scheduled time in
  `RELAY_TIMEZONE`, the status and errors, and the provider's pre-flight problems, run server-side.

It cannot publish, approve or edit anything: approve drafts with `approve_post` (or `POST /v1/posts/{id}/approve`).
Media are served through Relay at `/media/`, so previews work with a private bucket.

Login is passkey-only (WebAuthn, with user verification: Touch ID, Windows Hello, a security key...). To add a
passkey, open the page, expand "Add a passkey" and enter `RELAY_API_TOKEN` once. Once your keys are added, set `RELAY_PASSKEY_REGISTRATION=off` to close registration. Sessions last 12 hours and use a
`__Host-` cookie (Secure, HttpOnly, SameSite=Strict). The dashboard needs `RELAY_BASE_URL` on https; set
`RELAY_UI=off` to disable it.

## Use it from an agent (MCP)

The MCP endpoint is `https://<your-host>/mcp` (Streamable HTTP), authenticated with `Authorization: Bearer <RELAY_API_TOKEN>`.
With Claude Code:

```bash
claude mcp add --transport http relay https://<your-host>/mcp --header "Authorization: Bearer $RELAY_API_TOKEN"
```

Tools:

- Accounts: `list_providers`, `list_accounts`, `connect_account`, `import_history`, `set_account_avatar`
- Posts: `validate_post`, `schedule_post`, `list_posts`, `get_post`, `update_post`, `approve_post`, `delete_post`, `publish_now`, `upload_media_from_url`
- Analytics: `analyze`, `post_timeline`, `account_growth`, `refresh_metrics`, `sql_query`
- Conversations: `list_comments`, `reply_comment`
- Browser agent: `create_browser_task`, `list_browser_tasks`

### Profile pictures

Relay copies each account's profile picture (Page logo for LinkedIn and Facebook) from the network into
its media library on connect, on `import_history` and at most once a day with the account counters.
`set_account_avatar` (or `PUT /v1/accounts/{id}/avatar`, `PUT /v1/accounts/avatar` for every account) sets
one picture by hand: `{"media": "<media id>"}` or `{"media_url": "https://..."}`. It wins over the
network's picture until `{"clear": true}`. The dashboard draws it in every post preview.

### Drafts-only keys

Set `RELAY_DRAFT_TOKEN` and give that key to sub-agents. With it, `schedule_post` always creates drafts,
`update_post` only edits drafts, and publishing, replying, approving and account changes are hidden and refused.
An admin key (or a human) releases drafts with `approve_post`.

A date a drafter passes in `scheduled_at` becomes the draft's `planned_at`, so the owner sees it in the calendar.

### Planned drafts

A draft can carry `planned_at`, the time it is meant to go out. It never publishes anything by itself:

- `schedule_post` with `"scheduled_at": "draft", "planned_at": "2026-12-01T09:00:00+01:00"` creates a dated draft.
  `planned_at` with a real `scheduled_at` is refused.
- `update_post` sets it (`"planned_at": "<RFC 3339>"`) or clears it (`""` or `"none"`), on drafts only. Moving a
  scheduled post back to `"draft"` keeps its time as `planned_at`.
- `approve_post` without `scheduled_at` schedules the draft at its `planned_at` when that is still in the future.
  Otherwise (no planned time, or a past one) it publishes within one tick, as before. An explicit `scheduled_at` wins.
- `get_post`, `list_posts` and the REST API return `planned_at`; `from`/`to` filters date drafts by it.

### History import and visuals

`import_history` (or `POST /v1/accounts/{id}/import`) archives an account's past posts with their text and metrics,
and copies each post's **visuals** into the media library, so `post.media` lists them and the dashboard shows them
like posts made in Relay. A visual is a photo of a photo post or carousel, or the cover of a video. Video files are
never downloaded. The networks serve visuals through signed CDN links that expire, so Relay downloads them during
the import and never keeps the remote link. Media go to the bucket when `RELAY_S3_*` is set, else to `/data/media`.

- At most 10 visuals per post; an image over 15 MB, or a link that does not answer with an image, is skipped.
- Best effort: a failed download is logged (`import: visual download failed`) and counted in `visuals_failed`. The post
  is still imported.
- Re-running `import_history` refreshes metrics and fills the visuals of imported posts that have none yet (backfill).
  It never downloads again for a post that has visuals and never changes posts created in Relay.
- `"visuals": false` imports text and metrics only.
- The result is `{"created", "updated", "visuals", "visuals_failed"}`.

Where each connector finds the visuals (in the same request as the posts, except LinkedIn):

| Provider | Field | Size |
| --- | --- | --- |
| `x` | `media.url` (photos, `?name=medium`, at most 1200 px), `preview_image_url` (videos, GIFs) | 100-300 KB |
| `instagram` | `media_url` (images), `thumbnail_url` (videos, Reels), `children{...}` (carousels) | 100-400 KB (1080 px) |
| `facebook` | `attachments{media{image{src}},subattachments}`, else `full_picture` (link previews) | 50-150 KB (720 px) |
| `threads` | `media_url` (images), `thumbnail_url` (videos), `children{...}` (carousels) | 100-400 KB |
| `tiktok` | `cover_image_url` | 30-100 KB |
| `youtube` | `snippet.thumbnails`: maxres, then standard, high, medium | 50-200 KB (1280x720) |
| `linkedin-page` | image URNs (also article thumbnails) via `GET /rest/images?ids=List(...)` (`downloadUrl`), video URNs via `GET /rest/videos?ids=List(...)` (`thumbnail`), 50 per call | the original upload: 0.2-5 MB |

Expect about 0.1-0.3 MB per single-image or video post, and up to 10 times that for a carousel. LinkedIn serves the
original file, so its posts are the heaviest. Not imported: LinkedIn documents (the API gives the PDF, not a preview
image), polls and text posts, reposts (the visual belongs to someone else), and images the network withholds
(Instagram omits `media_url` for media with copyrighted music).

## REST API

Every route except `/healthz`, `/media/*` and the dashboard (`/ui/`, its own passkey login) requires `Authorization: Bearer <RELAY_API_TOKEN>`.

| Method | Route | Equivalent tool |
| --- | --- | --- |
| GET | `/v1/providers` | `list_providers` |
| GET / POST | `/v1/accounts` | `list_accounts` / `connect_account` |
| DELETE | `/v1/accounts/{id}` | |
| POST | `/v1/accounts/{id}/import` | `import_history` |
| PUT | `/v1/accounts/{id}/avatar`, `/v1/accounts/avatar` (all accounts) | `set_account_avatar` |
| GET / POST | `/v1/posts` | `list_posts` / `schedule_post` |
| GET / PATCH / DELETE | `/v1/posts/{id}` | `get_post` / `update_post` / `delete_post` |
| POST | `/v1/posts/{id}/publish` | `publish_now` |
| GET | `/v1/posts/{id}/comments`, `/v1/comments` | `list_comments` |
| POST | `/v1/comments/reply` | `reply_comment` |
| POST | `/v1/metrics/refresh` | `refresh_metrics` |
| GET | `/v1/analytics` | `analyze` |
| GET | `/v1/posts/{id}/timeline` | `post_timeline` |
| GET | `/v1/accounts/growth` | `account_growth` |
| POST | `/v1/query` | `sql_query` |
| POST | `/v1/posts/validate` | `validate_post` |
| POST | `/v1/posts/{id}/approve` | `approve_post` |
| GET / POST | `/v1/media` | list / upload (multipart `file` or JSON `{"url"}`) |
| GET / POST | `/v1/agent/tasks` | `list_browser_tasks` / `create_browser_task` |
| POST | `/v1/agent/tasks/claim`, `/v1/agent/tasks/{id}/result` | used by the browser agent |
| POST | `/v1/feedback` | `submit_feedback` |
| GET | `/v1/feedback`, `/v1/feedback/{id}` | `list_feedback` (admin) / `get_feedback` |
| PATCH | `/v1/feedback/{id}` | `update_feedback` (admin) |
| POST | `/v1/tools/{name}` | any tool, JSON arguments |

```bash
curl -X POST https://<your-host>/v1/posts \
  -H "Authorization: Bearer $RELAY_API_TOKEN" -H "Content-Type: application/json" \
  -d '{"account_ids":["acc_..."],"text":"Hello","scheduled_at":"2026-12-01T09:00:00+01:00"}'
```

`scheduled_at` is RFC 3339 with an offset, `now` (or omitted) to publish within one tick, or `draft` (optionally with
`planned_at`, see [Planned drafts](#planned-drafts)).
`per_account` gives an account its own text; `options` carries network settings (`reply_to`, `title`, `tags`,
`media_type`, `link`...).

## Analytics queries

`analyze` (MCP) and `GET /v1/analytics` (REST, same arguments as query parameters, lists comma separated) answer
questions like these:

| Question | Arguments |
| --- | --- |
| Views on every network except X last month | `period=last_month&exclude_providers=x&metric=views` |
| Likes per network per week this quarter | `period=this_quarter&group_by=network,week&metric=likes` |
| Best Instagram and TikTok videos with #skatepark this year | `period=this_year&providers=instagram,tiktok&formats=video,reel&hashtags=skatepark&metric=views&top=10` |
| October vs October last year on LinkedIn | `since=2026-10-01&until=2026-11-01&providers=linkedin-page&compare=previous_year` |
| Views gained by all posts (old ones too) during last week | `period=last_week&basis=activity&metric=views` |

- **Period**: `period` (`today`, `yesterday`, `this_week`, `last_week`, `this_month`, `last_month`, `this_quarter`,
  `last_quarter`, `this_year`, `last_year`, `last_7d`, `last_14d`, `last_30d`, `last_90d`, `last_365d`, `all`) or
  `since`/`until` (RFC 3339, a date, or a duration ago such as `720h`; `until` is exclusive). Calendar periods use
  `RELAY_TIMEZONE`, weeks start on Monday. Default: the last 30 days.
- **Filters**: `providers`, `exclude_providers`, `account_ids`, `exclude_account_ids`, `formats`, `exclude_formats`,
  `hashtags` (any of), `text_contains`, `has_link`, `has_media`, `include_reposts`. AND between fields, OR inside a list.
  The applied filters are echoed in `filters`.
- **basis**: `published` (default) counts the posts published in the period with their lifetime counters.
  `activity` counts what every post gained during the period, from the metric snapshots; posts Relay was not yet
  measuring at the start are left out and reported in `notes`.
- **compare**: `previous` (default, the period just before, same length), `previous_year` or `none`.

## Feedback from agents

Agents hit limits: a filter that does not exist, a network option Relay does not send, a bug. The MCP instructions
tell them to call `submit_feedback` instead of giving up:

```json
{"kind": "missing_feature", "title": "Filter analyze by language",
 "body": "User asked: views of English posts only. analyze has no language filter; text_contains is not enough.",
 "media_urls": ["https://example.com/screenshot.png"], "context": {"tool": "analyze"}}
```

`kind` is `missing_feature` (default), `bug`, `improvement` or `other`. `media` takes media ids (upload first with
`POST /v1/media` or `upload_media_from_url`), `media_urls` makes Relay download them; at most 10. Drafts-only keys can
submit and read their own feedback by id.

Maintainer agents (admin key) poll `list_feedback` (or listen to the `feedback.new` webhook), move each item through
`open → triaged → in_progress → done | rejected | duplicate` with `update_feedback`, and attach the pull request URL.

## Webhooks

`POST` with JSON `{"event", "at", "data"}` to `RELAY_WEBHOOK_URL`, signed with
`X-Relay-Signature: sha256=<hex hmac of the body>` using `RELAY_WEBHOOK_SECRET`.

## Development

```bash
go test ./...
```

Tests replay X, Facebook, Instagram, Threads, LinkedIn, YouTube, TikTok and the site protocol against fake HTTP servers, so they need no network
or credentials.

## License

[MIT](LICENSE)
