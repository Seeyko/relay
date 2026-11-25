# Relay

**A headless, agent-first social media scheduler.** No dashboard: you (or your AI agent) drive it through an
[MCP](https://modelcontextprotocol.io) server or a REST API. One static Go binary, one SQLite file,
about 11 MB of RAM at idle. A lightweight, self-hosted alternative to tools like Postiz or Buffer.

- **Organizations**: one instance for several brands or apps, each with as many accounts per network as needed
  (three TikTok accounts for one app, one for another...). Members see only their organizations
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
- A small open protocol to plug in **any website** as a channel and drive its blog: drafts, edits, publish/unpublish,
  translations, taxonomy, media and the site's own features as extensions ([Relay Site Protocol v2](docs/SITE_PROTOCOL.md))

## Connectors

| Provider | Publish | Metrics | Comments | Credentials |
| --- | --- | --- | --- | --- |
| `x` | text, threads (media per part), images, videos | public + impressions (< 30 days) | read + reply | OAuth 1.0a user keys |
| `linkedin-page` | text, link/article, image, multi-image (2-20), video, document (PDF...), images as a swipeable document carousel (2-300, `images_as_document`); extra thread parts as comments | impressions, unique impressions, clicks, likes, comments, shares (organic, 12 months) + followers | read + reply | `organization_id`, `access_token`, `refresh_token`, `client_id`, `client_secret` |
| `facebook` | text, link, photos, video, Reels (Pages); extra thread parts as comments | reactions, comments, shares | read + reply | `page_id`, `page_access_token` |
| `instagram` | image, carousel (up to 20), Reels, trial Reels, stories; extra thread parts as comments | likes, comments, views, reach, saves, shares | read + reply | `access_token` (Instagram Login, auto-refreshed) |
| `threads` | text (500), image, video, carousel (2-20), threads (reply chains, media per part), replies, quotes, link attachment | views, likes, replies, reposts, quotes, shares; followers | read + reply | `access_token` (long-lived, auto-refreshed), optional `user_id` |
| `youtube` | videos and Shorts (resumable upload) | views, likes, comments (+ YouTube Analytics) | read + reply | `client_id`, `client_secret`, `refresh_token` |
| `tiktok` | videos, photo posts (Content Posting API) | views, likes, comments, shares | not available in the public API | `client_key`, `client_secret`, `access_token`, `refresh_token` |
| `website` | Markdown or HTML articles (published, draft or scheduled on the site); full blog management with the `site_*` tools (v2) | if the site supports it | if the site supports it | `base_url`, `token` |

TikTok requires `options.privacy_level` on every post (one of the values the creator's account allows), and
unaudited TikTok apps can only post privately. Meta connectors use Graph API v26.0; Threads uses the Threads API v1.0. The LinkedIn connector posts as a company page with Community
Management API scopes and sends `LinkedIn-Version: 202611` (override with the `api_version` credential). Contributions for other
networks are welcome, see [CONTRIBUTING.md](CONTRIBUTING.md).

### Threads, comments and per-part media

A post with several parts (`thread`) is published the way each network can carry it:

- **X and Threads**: a chain of replies. Each part can carry its own media: `thread` items are strings or
  `{"text": "...", "media": ["med_..."]}` (X: up to 4 images or 1 video/GIF per part; Threads: one image, one video or a
  carousel per part). The top-level `media` is the media of part 1; `thread[0].media` may give it instead, not both.
- **Facebook, Instagram and LinkedIn**: part 1 is the post; parts 2..n are published right after it as **comments of
  the account on the post**, in order (the "first comment"). Comments are text only (Facebook 8000, Instagram 2200,
  LinkedIn 1250 characters); a later part with media is refused before publishing. Instagram stories take one part.
  **Behaviour change:** Facebook and Instagram used to join every part into one caption. A multi-part post now
  publishes extra parts as comments; send a single part (`text`) to keep everything in the caption.
- **Resume**: when a thread or a comment fails half-way, the post is marked failed with the ids already published in
  `remote_ids`. Publishing it again (`publish_now`, or `update_post` with a new `scheduled_at`) sends only the missing
  parts, with their own media, chained to the last reply (X, Threads) or commented on the post (Facebook, Instagram, LinkedIn).

Other options worth knowing:

- LinkedIn `images_as_document: true` (optional `document_title`, default the first line of the text, 100 characters
  max): Relay turns 2 to 300 images into one PDF (one page per image, page size = image size) and posts it as a
  document, which LinkedIn shows as a swipeable carousel. 100 MB max.
- Instagram `trial_reel: true` with `trial_graduation` `MANUAL` (default) or `SS_PERFORMANCE`: the Reel is first shown
  to non-followers (`trial_params` on the Reels container). Single video Reels only.

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
| `RELAY_DEFAULT_ORG` | `Default` | Name of the first [organization](#organizations), created on first start (existing accounts go there). Rename it later with `update_organization` |
| `RELAY_OWNER_EMAIL` | | Email of the first owner (see [Team](#team)). Fills an empty owner email; never replaces one |
| `RELAY_RESEND_API_KEY`, `RELAY_EMAIL_FROM` | | Send invitations, sign-in links and approval codes with [Resend](https://resend.com), from e.g. `RideMyPark <no-reply@ridemypark.com>`. Both or neither. Without them, email features are off |
| `RELAY_OAUTH` | `on` | `off` disables OAuth for agents (see [Connect an agent with OAuth](#connect-an-agent-with-oauth)). It is also off when the dashboard is off |
| `RELAY_DRAFT_TOKEN` | | Optional second key that can only create and edit drafts |
| `RELAY_WEBHOOK_URL` / `RELAY_WEBHOOK_SECRET` | | Webhook target and HMAC secret |
| `RELAY_AGENT_INSTRUCTIONS` | | Extra instructions sent to MCP clients (timezone, tone, languages...) |
| `X_CONSUMER_KEY`, `X_CONSUMER_SECRET`, `X_ACCESS_TOKEN`, `X_ACCESS_TOKEN_SECRET` | | Optional X bootstrap |
| `RELAY_THREADS_APP_ID`/`_SECRET`, `RELAY_META_APP_ID`/`_SECRET`, `RELAY_INSTAGRAM_APP_ID`/`_SECRET`, `RELAY_LINKEDIN_CLIENT_ID`/`_SECRET`, `RELAY_YOUTUBE_CLIENT_ID`/`_SECRET`, `RELAY_TIKTOK_CLIENT_KEY`/`_SECRET`, `RELAY_X_CONSUMER_KEY`/`_SECRET` | | App credentials for "Connect with ..." sign-ins (see [Connect accounts](#connect-accounts-with-each-networks-sign-in)) |

## Deploy

Any Docker host works (Dokploy, Coolify, Portainer, plain Compose):

1. Build from the `Dockerfile` (or use `docker-compose.yml`).
2. Set the environment variables above.
3. Mount a persistent volume on `/data`.
4. Expose port `8080` behind HTTPS and set `RELAY_BASE_URL` to that URL.

Back up `/data/relay.db` and `/data/media` (or your bucket). Media uploaded before a bucket was configured stay on disk
and keep working.

## Dashboard

A minimal, read-only dashboard at `https://<your-host>/ui/`, for you and your [team](#team):

- **Calendar**: week and day views of every post across networks. Drafts with a planned time (`planned_at`) show at
  that time with a dashed outline and a "Draft" label.
- **Drafts**: every draft, dated or not, newest first. One card per `schedule_post` call, with each target network
  and the number of pre-flight problems.
- **Preview** (tap any post): the whole group, one tab per account, laid out roughly the way each network shows it
  (thread with a counter and the media of each post, Instagram caption cut, YouTube title and description, LinkedIn,
  Facebook and Instagram post and comments, LinkedIn document carousel),
  with every media in order (images, playable videos), the options, the planned or scheduled time in
  `RELAY_TIMEZONE`, the status and errors, and the provider's pre-flight problems, run server-side.

It cannot publish, approve or edit anything: approve drafts with `approve_post` (or `POST /v1/posts/{id}/approve`).
What it changes: the **Accounts** sheet connects and reconnects social accounts (see
[Connect accounts](#connect-accounts-with-each-networks-sign-in)), the **Apps** sheet (header button) lists the agents connected with OAuth (name, access, who
approved, date, last use) and revokes them; the **Team** sheet manages members.
Media are served through Relay at `/media/`, so previews work with a private bucket.

Login is a passkey (WebAuthn, with user verification: Touch ID, Windows Hello, a security key...) or, when email is
configured, a sign-in link sent by email. To add the first passkey, open the page, expand "Add a key" and enter
`RELAY_API_TOKEN` once (it registers the passkey for the first owner). Once signed in, any member adds passkeys for
themselves from the account sheet, without the token. `RELAY_PASSKEY_REGISTRATION=off` closes registration for
everyone. Sessions last 12 hours and use a `__Host-` cookie (Secure, HttpOnly, SameSite=Strict). The dashboard needs
`RELAY_BASE_URL` on https; set `RELAY_UI=off` to disable it.

### Team

Invite people by email and give each a role:

| Role | Dashboard | OAuth apps they approve | Team |
| --- | --- | --- | --- |
| `owner` | everything | full access or drafts only | manage everyone, make owners |
| `admin` | everything | full access or drafts only | manage admins, editors, viewers |
| `editor` | everything (read) | drafts only, like `RELAY_DRAFT_TOKEN`: create and edit drafts, never publish, approve or connect accounts | |
| `viewer` | everything (read) | cannot approve apps | |

1. Set `RELAY_RESEND_API_KEY` and `RELAY_EMAIL_FROM` (a domain verified in Resend), and `RELAY_OWNER_EMAIL`.
2. Owners and admins invite from the **Team** sheet (email + role), or with `invite_member`. The invitee gets a
   link (7 days, single use) to `/ui/accept`, sets a name and is signed in.
3. Members then sign in with **Email me a link** on the login page (15 minutes, single use), and can add a passkey
   from the account sheet. The Team sheet changes roles, removes members, resends or cancels invitations.

Rules and protections:

- The last active owner can be neither removed, disabled nor demoted. Only an owner can change an owner or make one.
- The role is checked on every request. A demoted member's OAuth apps are capped at once (an editor's apps are drafts
  only, a viewer's stop working); removing or disabling a member signs them out and disconnects their apps.
- Asking for a sign-in link always gets the same answer, member or not, and is rate limited per address (3 per
  15 minutes) and per client (10 per 15 minutes). The link page only signs in when you press the button, so mail
  scanners that open links cannot use them.
- Links and codes are random, single use, stored as SHA-256 only, and never logged. Emails are short HTML + text,
  in English or French, with no images or tracking pixels, and never contain the API token. Resend's own click
  tracking, if enabled on the sending domain, rewrites links: keep it off for the Relay sender.
- Member changes, invitations and logins (with the method) are logged and kept in the `audit_log` table.

Upgrading from a single-owner instance: on start, existing passkeys, open sessions and OAuth apps are attached to a
new `owner` member (with `RELAY_OWNER_EMAIL` as its email, if set; else set it later with `update_member`).
Existing passkeys keep working. Without email configured, the Team sheet still lists members and changes roles.

### Organizations

An organization groups the accounts of one brand or app. It can hold any number of accounts per network, and
everything hanging off an account follows it: posts, drafts, comments, metrics, browser tasks. Media belong to the
organization they were uploaded for (their links stay public).

- **Who sees what**: owners, `RELAY_API_TOKEN`, `RELAY_DRAFT_TOKEN` and the scheduler see every organization. Other
  members see the organizations listed for them (`orgs` of `invite_member` / `update_member`, or the chips of the
  Team sheet). Everything else answers "not found", exactly like an id that does not exist.
- **Agents**: when a member approves an app with OAuth, they can limit it to one organization. Every tool takes an
  optional `org` (id, slug or name) that limits the call; `X-Relay-Org: <org>` (or `/mcp?org=<org>` with a static
  token) does it for a whole connection. `list_organizations` lists them. `sql_query` reads every organization, so
  it only works for callers who see all of them, without `org`.
- **New accounts** go to the organization of the call: `org`, or the only one the caller sees. With several and none
  given, Relay asks for one. The same remote account cannot be connected in two organizations.
- **Dashboard**: a switcher in the header filters the calendar, the drafts and the accounts (it shows when you see
  at least two organizations). Owners create and rename organizations in the Team sheet; owners and admins move an
  account from the Accounts sheet or with `move_account`.
- **Team**: an admin limited to some organizations only manages members who see nothing else, and only gives access
  to their own organizations. Organizations are created, renamed and removed by owners (`create_organization`,
  `update_organization`, `delete_organization`, which needs it to be empty).
- **Upgrading**: on start, an instance from before organizations gets one (`RELAY_DEFAULT_ORG`) holding every
  account and media; members who are not owners get access to it, so nobody's view changes.

## Connect accounts with each network's sign-in

Instead of pasting tokens, owners and admins connect an account with the network's own consent screen, and
**reconnect** it in one click when a token expires or the app gets a new permission (a token refresh keeps the
permissions of the first consent, so a new permission needs a new consent).

- **Dashboard**: the **Accounts** sheet (link icon in the header, owners and admins only) lists the accounts with a
  **Reconnect** button, and a "Connect a network" button per configured network. It also lists the redirect URIs.
- **Agents**: `start_account_connection` (admin keys) with `network`, or `account_id` to reconnect, returns a link
  (single use, 1 hour) to hand to a human. REST: `POST /v1/accounts/connect` `{"network": "threads"}`,
  `POST /v1/accounts/{id}/reconnect`.
- **Reconnect** must sign in the same remote account: another one is refused and nothing changes. The credentials are
  replaced in place (same checks as `update_account_credentials`), so posts and history stay attached. Connecting an
  account that is already connected updates it in place too.
- Facebook, Instagram (Facebook Login) and LinkedIn show a picker when the sign-in gives several Pages, Instagram
  accounts or organizations.

A network shows up only when its app credentials are set. Register the redirect URI
`{RELAY_BASE_URL}/connect/{network}/callback` in each developer console:

| Network | Variables | Redirect URI | Where to register it | Scopes requested |
| --- | --- | --- | --- | --- |
| `threads` | `RELAY_THREADS_APP_ID`, `RELAY_THREADS_APP_SECRET` | `/connect/threads/callback` | Meta app > Threads API use case > Settings: Redirect Callback URLs | `threads_basic`, `threads_content_publish`, `threads_manage_replies`, `threads_read_replies`, `threads_manage_insights`, `threads_delete` (then a 60-day long-lived token) |
| `facebook` | `RELAY_META_APP_ID`, `RELAY_META_APP_SECRET` | `/connect/facebook/callback` | Meta app > Facebook Login for Business > Settings: Valid OAuth Redirect URIs | `pages_show_list`, `pages_manage_posts`, `pages_read_engagement`, `pages_read_user_content`, `pages_manage_engagement`, `read_insights`, `business_management` (Page tokens from a long-lived user token: they do not expire) |
| `instagram` (via Facebook) | `RELAY_META_APP_ID`, `RELAY_META_APP_SECRET` | `/connect/instagram/callback` | same Meta app, same setting | `instagram_basic`, `instagram_content_publish`, `instagram_manage_comments`, `instagram_manage_insights`, `pages_show_list`, `pages_read_engagement`, `business_management` |
| `instagram-login` | `RELAY_INSTAGRAM_APP_ID`, `RELAY_INSTAGRAM_APP_SECRET` (optional) | `/connect/instagram-login/callback` | Meta app > Instagram > API setup with Instagram login > Business login settings | `instagram_business_basic`, `instagram_business_content_publish`, `instagram_business_manage_comments`, `instagram_business_manage_insights` (then a 60-day long-lived token) |
| `linkedin` | `RELAY_LINKEDIN_CLIENT_ID`, `RELAY_LINKEDIN_CLIENT_SECRET` | `/connect/linkedin/callback` | LinkedIn developer portal > Auth: Authorized redirect URLs | `r_organization_social`, `w_organization_social`, `rw_organization_admin` (Community Management API; `r_organization_admin` belongs to the Advertising API) |
| `youtube` | `RELAY_YOUTUBE_CLIENT_ID`, `RELAY_YOUTUBE_CLIENT_SECRET` | `/connect/youtube/callback` | Google Cloud > Credentials > OAuth client (Web application): Authorized redirect URIs | `youtube.upload`, `youtube.readonly`, `youtube.force-ssl`, `yt-analytics.readonly` (`access_type=offline`, `prompt=consent`, PKCE) |
| `tiktok` | `RELAY_TIKTOK_CLIENT_KEY`, `RELAY_TIKTOK_CLIENT_SECRET` | `/connect/tiktok/callback` | TikTok for Developers > Login Kit: Redirect URI (Web) | `user.info.basic`, `user.info.profile`, `user.info.stats`, `video.publish`, `video.upload`, `video.list` |
| `x` | `RELAY_X_CONSUMER_KEY`, `RELAY_X_CONSUMER_SECRET` | `/connect/x/callback` | X developer portal > User authentication settings: Callback URI (Read and write) | OAuth 1.0a three-legged (app permissions decide the access) |

Override a scope list with `RELAY_CONNECT_<NETWORK>_SCOPES` (e.g. `RELAY_CONNECT_LINKEDIN_SCOPES`, comma or space
separated). PKCE is used where the network documents it for web apps (Google); Meta, LinkedIn and TikTok document
it only for other flows or native apps. Instagram accounts connected with Facebook Login reconnect through
`instagram`, the others through `instagram-login`.

Security: the start link and the network's `state` are random and single use; the start link expires after 1 hour, the `state` after 10 minutes. The state is
bound to the browser that started the sign-in (an HttpOnly, SameSite=Lax cookie) and to the member who asked, who
must still be an owner or admin when the network answers. A link handed out by an agent shows a **Continue** button
(a same-origin POST), so chat link previews cannot use it. Tokens never appear in URLs shown to people, in pages or in
logs; pending sign-ins live in memory (a restart cancels them).

## Use it from an agent (MCP)

The MCP endpoint is `https://<your-host>/mcp` (Streamable HTTP), authenticated with `Authorization: Bearer <RELAY_API_TOKEN>`.
With Claude Code:

```bash
claude mcp add --transport http relay https://<your-host>/mcp --header "Authorization: Bearer $RELAY_API_TOKEN"
```

Tools:

- Organizations: `list_organizations`, `create_organization`, `update_organization`, `delete_organization`, `move_account`; every tool also takes `org`
- Accounts: `list_providers`, `list_accounts`, `connect_account`, `import_history`, `set_account_avatar`
- Posts: `validate_post`, `schedule_post`, `list_posts`, `get_post`, `update_post`, `approve_post`, `delete_post`, `publish_now`, `upload_media_from_url`, `get_media` (public links of media, or of every media of a post, to show them to a person)
- Deleting: `delete_post` removes the post from Relay only. `delete_post` with `remote: true` (REST:
  `DELETE /v1/posts/{id}?remote=true`) first deletes the published post from the network, with every part of its
  thread: X and Threads delete each reply, last first; Facebook and LinkedIn delete the post, which removes the
  page's comments with it. If the network refuses, the post stays in Relay and the call can be repeated (ids that are
  already gone count as deleted). Threads needs the `threads_delete` permission. Instagram, TikTok, YouTube and
  websites answer 400: delete the post on the network, then call `delete_post` without `remote`.
- Analytics: `analyze`, `post_timeline`, `account_growth`, `refresh_metrics`, `sql_query`
- Conversations: `list_comments`, `reply_comment`
- Browser agent: `create_browser_task`, `list_browser_tasks`
- Connected apps (admin): `list_oauth_grants`, `revoke_oauth_grant`
- Website content (Site Protocol v2, `account_id` of a website account): `site_info`, `site_list_articles`, `site_get_article`,
  `site_create_article`, `site_update_article`, `site_publish_article`, `site_unpublish_article`, `site_delete_article`,
  `site_taxonomy`, `site_authors`, `site_list_translations`, `site_set_translation`, `site_delete_translation`,
  `site_upload_media`, `site_actions`, `site_action`. `site_publish_article` with `scheduled_at` lets Relay publish an
  existing draft (and its translations) at a set time. See [Websites](#websites)
- Team (admin): `list_members`, `invite_member`, `update_member` (role, disabled, email, name; `action: resend|revoke`
  for a pending invitation), `remove_member`. Called through an OAuth app, they act as the member who approved it

### Connect an agent with OAuth

Agents that support MCP authorization (claude.ai, Claude Desktop, Claude Code, ChatGPT...) connect with the URL
alone: a dashboard member approves them, no token to copy.

- **claude.ai / Claude Desktop**: Settings → Connectors → Add custom connector → URL `https://<your-host>/mcp`.
- **Claude Code**: `claude mcp add --transport http relay https://<your-host>/mcp`, then `/mcp` → relay → Authenticate.

The app opens `https://<your-host>/oauth/authorize`. Sign in (passkey or email link) if needed, pick **Full access**
(`relay:admin`) or **Drafts only** (`relay:drafts`, the same rights as `RELAY_DRAFT_TOKEN`), then **Approve**:
every approval asks for a fresh check, even inside a dashboard session: your passkey again (with PIN or
biometrics), or, if you have no passkey, a 6-digit code sent to your email (10 minutes, 5 attempts, single use).
Your role caps the choice (editors: drafts only; viewers cannot approve), and the Apps sheet shows who approved
each app. Revoke an app from the Apps sheet (owners and admins: any app; others: their own), with
`revoke_oauth_grant` or `DELETE /v1/oauth/grants/{id}`.

How it works (MCP authorization spec 2025-06-18 and later):

| Endpoint | |
| --- | --- |
| `GET /.well-known/oauth-protected-resource` (and `.../mcp`) | protected resource metadata (RFC 9728) |
| `GET /.well-known/oauth-authorization-server` (and `/.well-known/openid-configuration`) | server metadata (RFC 8414) |
| `POST /oauth/register` | dynamic client registration (RFC 7591), rate limited, at most 200 apps |
| `GET /oauth/authorize` | consent page (code flow, PKCE S256 required, `resource` = `https://<your-host>/mcp`) |
| `POST /oauth/token` | `authorization_code` and `refresh_token` |
| `POST /oauth/revoke` | revocation (RFC 7009): disconnects the app |

- Clients are public: no secret is issued (`token_endpoint_auth_method: none`). A client that sends a secret
  anyway (`client_secret_post` or `client_secret_basic`) is identified by its `client_id` and the secret is
  ignored: PKCE proves the request.
- Redirect URIs must be https, or http on `127.0.0.1`, `[::1]` or `localhost` (any port at authorization time,
  RFC 8252). They are matched exactly otherwise.
- Codes live 60 seconds and work once: a code used twice revokes everything issued from it.
- Access tokens live 1 hour. Refresh tokens live 30 days, are replaced on every use, and a replaced refresh
  token used again revokes the whole connection. A connection lasts at most 90 days, then the app asks again.
- Tokens are opaque and bound to `https://<your-host>/mcp`; Relay stores only their SHA-256. They work on `/mcp`
  and `/v1`, next to `RELAY_API_TOKEN` and `RELAY_DRAFT_TOKEN`, which keep working.
- OAuth needs the dashboard (`RELAY_BASE_URL` on https). With `RELAY_UI=off` or `RELAY_OAUTH=off`, the metadata
  answers 404 and agents use the static tokens.

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
| GET / DELETE | `/v1/oauth/grants`, `/v1/oauth/grants/{id}` | `list_oauth_grants` / `revoke_oauth_grant` |
| POST | `/v1/query` | `sql_query` |
| GET / POST | `/v1/members` | `list_members` / `invite_member` |
| POST | `/v1/accounts/connect`, `/v1/accounts/{id}/reconnect` | `start_account_connection` |
| PATCH / DELETE | `/v1/members/{id}` | `update_member` / `remove_member` (an `inv_` id revokes the invitation) |
| POST | `/v1/posts/validate` | `validate_post` |
| POST | `/v1/posts/{id}/approve` | `approve_post` |
| GET / POST | `/v1/media` | list (each with its public `url`) / upload (multipart `file` or JSON `{"url"}`) |
| GET | `/v1/media/{id}` | `get_media` (metadata and public `url`; comma-separated ids work) |
| GET | `/v1/posts/{id}/media` | `get_media` with `post`: the post's media in publishing order |
| GET / POST | `/v1/agent/tasks` | `list_browser_tasks` / `create_browser_task` |
| POST | `/v1/agent/tasks/claim`, `/v1/agent/tasks/{id}/result` | used by the browser agent |
| POST | `/v1/feedback` | `submit_feedback` |
| GET | `/v1/feedback`, `/v1/feedback/{id}` | `list_feedback` (admin) / `get_feedback` |
| PATCH | `/v1/feedback/{id}` | `update_feedback` (admin) |
| GET | `/v1/sites/{account}` | `site_info` |
| GET / POST | `/v1/sites/{account}/articles` | `site_list_articles` / `site_create_article` |
| GET / PATCH / DELETE | `/v1/sites/{account}/articles/{article}` | `site_get_article` / `site_update_article` / `site_delete_article` |
| POST | `/v1/sites/{account}/articles/{article}/publish`, `.../unpublish` | `site_publish_article` / `site_unpublish_article` |
| GET | `/v1/sites/{account}/articles/{article}/translations` | `site_list_translations` |
| PUT / DELETE | `/v1/sites/{account}/articles/{article}/translations/{lang}` | `site_set_translation` / `site_delete_translation` |
| GET | `/v1/sites/{account}/taxonomy`, `/v1/sites/{account}/authors` | `site_taxonomy` / `site_authors` |
| POST | `/v1/sites/{account}/media` | `site_upload_media` (`{"media": "med_..."}` or `{"url": "..."}`) |
| GET | `/v1/sites/{account}/actions` | `site_actions` |
| POST | `/v1/sites/{account}/actions/{action}` | `site_action` (the body is the action's arguments) |
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

## Websites

Any site that implements the [Relay Site Protocol](docs/SITE_PROTOCOL.md) is an account like the others
(`provider: website`, credentials `base_url` and `token`). With **v2**, agents drive the site's blog:

- `schedule_post` on a website account creates an article when the post goes out (options `title`, `slug`, `excerpt`,
  `tags`, `category`, `lang`, `cover`, `author_id`, `status` published/draft/scheduled, `published_at`,
  `body_format` markdown/html, `extra`), with Relay's retries and calendar.
- The `site_*` tools read and change the site's content directly: list and read articles (drafts included), create,
  edit (`expected_updated_at` prevents overwriting someone else's change), publish, schedule, unpublish, delete,
  translations (manual or machine), categories, tags, authors and media (from Relay's library or a URL the site
  downloads).
- **Extensions**: a site lists its own features in `/info` (search, image import to its CDN, blog analytics,
  re-translation...). `site_actions` shows them with their JSON Schema and `site_action` calls one; Relay has no code
  for any of them.
- Every tool checks the site's capabilities first and refuses cleanly when one is missing. `/info` is cached
  5 minutes per account (`site_info` with `refresh: true` reads it again).
- Drafts-only keys may read, create drafts, edit drafts and their translations, upload media and call extensions that
  are read-only and not `admin`. Publishing, unpublishing and deleting need an admin key.
- `delete_post` with `delete_on_site: true` also deletes the article of a website post on the site.
- v1 sites keep working for publishing, metrics, comments and history; the `site_*` tools need v2.

Check a site's implementation with the conformance checker built into the binary:

```bash
relay site-check https://example.com "$SITE_TOKEN"          # read-only checks of every declared capability
relay site-check https://example.com "$SITE_TOKEN" --write  # also creates, edits and deletes one draft (never publishes)
```

It prints `PASS` / `FAIL` / `WARN` / `SKIP` per check and exits `1` when a check fails.

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
