# Relay Site Protocol v2

A small HTTP contract that lets Relay, and the agents that use Relay's MCP server, **drive a website's blog**:
create, edit, publish, unpublish and delete articles, manage translations, read the taxonomy and the authors,
upload media, read metrics and comments. A site can also expose **its own features** (image import, search,
analytics, re-translation...) as *extensions* that Relay lists and calls without knowing them in advance.

Implement the routes once per site, then connect it:

```json
POST /v1/accounts
{"provider": "website", "name": "My blog", "credentials": {"base_url": "https://example.com", "token": "<site token>"}}
```

Check an implementation with the conformance checker shipped in the Relay binary (see [Conformance](#conformance)):

```bash
relay site-check https://example.com "$SITE_TOKEN"          # read-only
relay site-check https://example.com "$SITE_TOKEN" --write  # also creates, edits and deletes one draft
```

v1 sites keep working unchanged: see [Compatibility with v1](#compatibility-with-v1).

## Conventions

- **Base URL**: every route lives under `{base_url}/relay/v2`.
- **Auth**: `Authorization: Bearer <site token>` on every request. The token is a secret you generate for Relay
  and store in the site's environment. Answer `401` without it or with a wrong one.
- **JSON** in and out (`Content-Type: application/json`), except `POST /media` with a file (multipart).
- **Times** are RFC 3339 with an offset, e.g. `2026-11-21T09:00:00Z`.
- **Errors**: any non-2xx status with a body `{"error": "human readable message"}`.
  - `4xx` = permanent: Relay does not retry and shows the message to the user or agent.
    Use `400`/`422` for invalid input, `404` for an unknown id, `409` for an edit conflict.
  - `5xx` (and `429`) = temporary: Relay retries scheduled publications with backoff.
- **Unknown fields**: ignore unknown request fields; Relay ignores unknown response fields. Sites and Relay may add
  optional fields at any time without a version change.
- **Ids** are strings chosen by the site (`"42"`, `"a1"`, `"hello-world"`...). They appear in URL paths, so Relay
  path-escapes them.

## Routes

| Route | Capability | Purpose |
| --- | --- | --- |
| `GET /info` | required | Identify the site, its capabilities, languages and extensions |
| `POST /posts` | `articles` (required) | Create an article |
| `GET /posts` | `articles` (required) | List articles |
| `GET /posts/{id}` | `articles` (required) | Read one article in full |
| `PATCH /posts/{id}` | `edit` | Change an article |
| `POST /posts/{id}/publish` | `drafts` (`schedule` for a future date) | Publish (or schedule) a draft |
| `POST /posts/{id}/unpublish` | `drafts` | Take an article back to draft |
| `DELETE /posts/{id}` | `delete` | Delete an article |
| `POST /media` | `media` | Upload a file, or import one from a URL |
| `GET /taxonomy` | `taxonomy` | Categories and tags in use |
| `GET /authors` | `authors` | Authors an article can be attributed to |
| `GET /posts/{id}/translations` | `translations` | Translations of an article |
| `PUT /posts/{id}/translations/{lang}` | `translations` | Set a manual translation |
| `DELETE /posts/{id}/translations/{lang}` | `translations` | Remove a manual translation |
| `GET /metrics?ids=` | `metrics` | Counters per article |
| `GET /posts/{id}/comments` | `comments` | Comments of an article |
| `POST /comments/{id}/replies` | `comments` | Answer a comment |
| `{method} /x/{name}` | extensions | Site-defined actions listed in `/info` |

## Capabilities

`GET /info` lists what the site supports. Relay refuses an operation whose capability is missing before calling
the site, so a site only implements the routes of the capabilities it declares.

| Capability | Meaning |
| --- | --- |
| `articles` | **Required.** `POST /posts` (at least `status: "published"`), `GET /posts`, `GET /posts/{id}` |
| `drafts` | `status: "draft"` on create; `POST /posts/{id}/publish` and `/unpublish` |
| `schedule` | `status: "scheduled"` with a future `published_at`; `publish` with a future `published_at` |
| `edit` | `PATCH /posts/{id}` |
| `delete` | `DELETE /posts/{id}` |
| `media` | `POST /media`, both forms (file and URL) |
| `translations` | the three translation routes |
| `taxonomy` | `GET /taxonomy` |
| `authors` | `GET /authors`; `author_id` is accepted on create and edit |
| `metrics` | `GET /metrics` (and optional `metrics` on list items) |
| `comments` | `GET /posts/{id}/comments`, `POST /comments/{id}/replies` |

Relay ignores capability names it does not know, so a site may announce more.

## GET /info

```json
{
  "name": "RideMyPark",
  "url": "https://ridemypark.com",
  "version": "2",
  "capabilities": ["articles", "media", "drafts", "edit", "delete", "schedule", "translations", "taxonomy", "authors", "metrics"],
  "languages": ["en", "fr"],
  "extensions": [
    {
      "name": "retranslate",
      "description": "Machine-translate an article again into every language that has no manual translation.",
      "method": "POST",
      "path": "/x/retranslate",
      "input_schema": {"type": "object", "properties": {"id": {"type": "string"}}, "required": ["id"]},
      "admin": true
    },
    {
      "name": "search",
      "description": "Full-text search in the blog.",
      "method": "GET",
      "path": "/x/search",
      "input_schema": {"type": "object", "properties": {"q": {"type": "string"}, "lang": {"type": "string"}}, "required": ["q"]},
      "admin": false
    }
  ]
}
```

- `version` must be `"2"`.
- `languages` (optional): the languages of the site, the default one first. Values for `lang` everywhere.
- `extensions` (optional): see [Extensions](#extensions).

Relay caches `/info` for 5 minutes per account. Answer it fast; it is also how Relay checks the token on connect.

## The article object

`GET /posts/{id}`, `PATCH`, `publish` and `unpublish` return the full article:

```json
{
  "id": "42",
  "url": "https://example.com/blog/hello-world",
  "title": "Hello world",
  "slug": "hello-world",
  "excerpt": "Short summary",
  "body_html": "<p>Body</p>",
  "tags": ["go", "selfhosted"],
  "category": "news",
  "lang": "en",
  "cover_url": "https://cdn.example.com/img_123.jpg",
  "author_id": "7",
  "status": "draft",
  "published_at": null,
  "updated_at": "2026-11-21T09:00:00Z",
  "extra": {}
}
```

- `status` is `"draft"`, `"published"` or `"scheduled"` (published with a future `published_at`).
- `published_at` is `null` for a draft that was never published.
- `updated_at` changes on **every** change of the article (including its translations if the site wants); Relay uses
  it for optimistic concurrency (`expected_updated_at`).
- `url` is the public URL; for a draft it may be a preview URL or `""`.
- `body_html` is the canonical body. A site that stores Markdown may also return `body_markdown`.
- `extra` (optional) carries site-specific fields, returned as the site stores them.

## POST /posts

```json
{
  "title": "Hello world",
  "slug": "hello-world",
  "body_html": "<p>Body</p>",
  "excerpt": "Short summary",
  "tags": ["go", "selfhosted"],
  "category": "news",
  "lang": "en",
  "cover_url": "https://cdn.example.com/img_123.jpg",
  "author_id": "7",
  "status": "draft",
  "published_at": "2026-11-25T08:00:00Z",
  "extra": {}
}
```

- `title` and `status` are required; everything else is optional. The site generates the slug when it is missing.
- Give **one** of `body_html` or `body_markdown`. A site that accepts only one of them converts the other or answers
  `422` with a clear message.
- `status`: `"published"` (always), `"draft"` (needs `drafts`), `"scheduled"` (needs `schedule`; `published_at` is
  required and in the future). For `"published"`, `published_at` is optional (default: now; a past date back-dates the
  article).
- `cover_url` is a URL returned by `POST /media` (or any URL the site accepts).
- `extra` is free-form and site-specific (series, SEO fields, ...).

Response `201`:

```json
{"id": "42", "url": "https://example.com/blog/hello-world", "status": "draft", "updated_at": "2026-11-21T09:00:00Z"}
```

A site may return the full article instead; Relay reads these four fields.

## GET /posts

`GET /posts?status=draft&lang=en&since=2026-11-01T00:00:00Z&limit=50&cursor=...`

All parameters are optional:

- `status`: `draft`, `published` or `scheduled` (default: every status).
- `lang`: only articles in this language.
- `since`: articles updated (or published) at or after this time.
- `limit`: page size (default and maximum chosen by the site; Relay asks for at most 100).
- `cursor`: the `next_cursor` of the previous page.

```json
{
  "items": [
    {"id": "42", "url": "https://...", "title": "Hello world", "slug": "hello-world", "excerpt": "...",
     "status": "published", "lang": "en", "published_at": "2026-11-19T18:00:00Z", "updated_at": "2026-11-19T18:00:00Z",
     "metrics": {"views": 120}}
  ],
  "next_cursor": ""
}
```

Newest first. `next_cursor` is `""` (or absent) on the last page. `metrics` is optional.

## GET /posts/{id}

The [article object](#the-article-object). `404` for an unknown id.

## PATCH /posts/{id}

Any subset of the create fields **except `status`** (use `publish` / `unpublish`), plus an optional
`expected_updated_at`:

```json
{"title": "Hello again", "tags": ["go"], "expected_updated_at": "2026-11-21T09:00:00Z"}
```

- Fields absent from the body are left unchanged. `null` or `""` clears an optional field (`excerpt`, `category`,
  `cover_url`, `author_id`); `[]` clears `tags`.
- When `expected_updated_at` is given and differs from the article's current `updated_at`, answer `409` and change
  nothing. Relay passes the value it read, so two agents cannot overwrite each other.
- A body containing `status` gets `400`.
- `published_at` may move the date of a published article (back-dating, or re-scheduling a scheduled one).

Response `200`: the article.

## POST /posts/{id}/publish and /unpublish

`publish` body (optional): `{"published_at": "2026-11-25T08:00:00Z"}`.

- Without a date, or with a past one: the article becomes `published` now (or back-dated).
- With a future date: the article becomes `scheduled` (needs `schedule`, else `422`).
- Publishing an article that is already published is not an error (idempotent).

`unpublish` (no body) takes a published or scheduled article back to `draft`; the public URL stops answering.

Both return `200` with the article.

## DELETE /posts/{id}

Deletes the article and its translations. `204` with no body; `404` for an unknown id.

## POST /media

Two forms, both required with `media`:

- `multipart/form-data` with a `file` field and an optional `alt` field (Relay uploads a file it holds);
- JSON `{"url": "https://...", "alt": "..."}`: the site downloads the file itself (to its own storage or CDN).

Response `201` (or `200`):

```json
{"id": "img_123", "url": "https://cdn.example.com/img_123.jpg"}
```

`4xx` for a file the site refuses (type, size, unreachable URL).

## GET /taxonomy

```json
{"categories": [{"name": "news", "count": 12}], "tags": [{"name": "go", "count": 3}]}
```

`count` is the number of articles using it. A site without categories returns `"categories": []`.

## GET /authors

```json
{"items": [{"id": "7", "name": "Jane Doe"}]}
```

Sites may add optional fields (`avatar_url`, `bio`...).

## Translations

An article has one source language (its `lang`) and translations in the other `languages` of the site. A translation
is either `manual` (written by a person or an agent) or `machine` (generated by the site).

`GET /posts/{id}/translations`:

```json
{"items": [{"lang": "fr", "title": "Bonjour", "excerpt": "...", "body_html": "<p>...</p>", "mode": "machine", "updated_at": "2026-11-21T09:00:00Z"}]}
```

`PUT /posts/{id}/translations/{lang}` with `{"title": "...", "excerpt": "...", "body_html": "..."}` stores a manual
translation (`mode: "manual"`) and returns it (`200`, the translation item). `400` when `{lang}` is not one of the
site's languages or is the article's own language.

`DELETE /posts/{id}/translations/{lang}` removes the manual translation. A site that machine-translates goes back to
its machine translation (and `GET` then shows `mode: "machine"`); otherwise the translation is gone. `204`; `404` when
there was nothing to remove.

## Metrics and comments

Same as v1, under `/relay/v2`:

- `GET /metrics?ids=a1,a2` → `{"metrics": {"a1": {"views": 120, "reads": 80}, "a2": {"views": 4}}}`. Any integer
  counters; `views` is the conventional name.
- `GET /posts/{id}/comments` → `{"items": [{"id": "c1", "author": "Ann", "text": "Nice!", "url": "https://...#c1", "created_at": "2026-11-19T19:00:00Z"}]}`
- `POST /comments/{id}/replies` with `{"text": "Thanks!"}` → `{"id": "c2", "url": "https://...#c2"}`

## Extensions

Extensions let a site expose any feature of its blog to Relay's agents without a change to Relay. Each one is
described in `/info`:

| Field | Meaning |
| --- | --- |
| `name` | Unique, `[a-z0-9_]+`, e.g. `import_image`, `retranslate`, `blog_analytics` |
| `description` | What it does, for the agent (one or two sentences: this is all the agent knows) |
| `method` | `GET`, `POST`, `PUT`, `PATCH` or `DELETE` |
| `path` | Path under `/relay/v2`, starting with `/x/`, e.g. `/x/search` |
| `input_schema` | JSON Schema of the arguments (`{"type": "object", ...}`) |
| `admin` | `true` when only Relay's admin keys may call it (anything that publishes, deletes or spends money) |
| `read_only` | optional, default `true` for `GET` and `false` otherwise: the action changes nothing |

Relay never hard-codes extensions: `site_actions` lists them for the agent and `site_action` calls one.

- **Arguments**: for `GET` and `DELETE`, Relay sends them as query parameters (arrays comma-joined, objects as JSON);
  for the other methods, as the JSON body.
- **Path parameters** (optional): a path may contain `{param}` placeholders, e.g. `/x/posts/{id}/retranslate`.
  Relay fills them from the argument of the same name (path-escaped) and does not send that argument again.
- **Validation** is the site's job: answer `400`/`422` with `{"error"}` for bad arguments. Relay only checks that
  the arguments are an object and that the schema's `required` fields are present.
- **Response**: any JSON (`204` is fine); Relay hands it to the agent as is.
- **Permissions in Relay**: admin keys may call every extension. Drafts-only keys may call an extension only when
  it is not `admin` and is read-only.

## Compatibility with v1

v1 (below) stays supported, and v1 sites keep working without any change.

- **Detection**: Relay calls `GET {base_url}/relay/v2/info`. When it answers `2xx` with `"version": "2"`, Relay uses
  v2. Otherwise (`404`, other error, other version) it calls `GET {base_url}/relay/v1/info` and uses v1. The result is
  cached for 5 minutes, so a site that adds v2 is picked up without reconnecting the account.
- A site may serve both versions during a migration; Relay prefers v2.
- The site content tools (`site_*`) need v2; on a v1 site they answer with a clear error. Scheduling posts, metrics,
  comments and history work on both.
- Differences from v1 for implementers:
  - v2 `POST /posts` takes `body_html` **or** `body_markdown` (v1: `body_markdown` only), adds `category`,
    `author_id`, `status: "scheduled"`, and no longer sends `media` (upload them, then use their URLs).
  - v2 `POST /posts` answers `201` with `status` and `updated_at`.
  - v2 `GET /posts` items carry `slug`, `status`, `lang` and `updated_at`, and accept `status` / `lang` filters.
  - `/info` has `languages` and `extensions`; `POST /media` also accepts `{"url"}`.

## How Relay uses it

**Scheduling a post** (`schedule_post` on a website account) creates an article with `POST /posts`. The post text
(thread parts joined with blank lines) is the body. Options:

| Option | Sent as |
| --- | --- |
| `title` (required) | `title` |
| `slug`, `excerpt`, `category`, `lang`, `author_id` | the same field |
| `tags` | `tags` |
| `cover` | a media id of the post: that file is the `cover_url` (default: the first media) |
| `status` | `published` (default), `draft` or `scheduled` (with `published_at`) |
| `published_at` | `published_at` (default now for `published`) |
| `body_format` | `markdown` (default: the text goes in `body_markdown`) or `html` (`body_html`) |
| `extra` | `extra` |

Every media of the post is uploaded with `POST /media` first. History import reads `GET /posts?status=published`,
metrics and comments use the routes above.

**Site content tools** (MCP and REST, admin keys unless noted), all taking `account_id` of a website account:
`site_info`, `site_list_articles`, `site_get_article`, `site_create_article`, `site_update_article`,
`site_publish_article`, `site_unpublish_article`, `site_delete_article`, `site_taxonomy`, `site_authors`,
`site_list_translations`, `site_set_translation`, `site_delete_translation`, `site_upload_media`, `site_actions`,
`site_action`. `site_create_article` makes a draft unless `status` says otherwise. Drafts-only keys may read, create
drafts, edit articles that are drafts (content and translations), upload media and call extensions that are read-only
and not `admin`; they cannot publish, unpublish or delete. See the README for the REST routes.

**Scheduling an existing draft in Relay**: `site_publish_article` with `scheduled_at` (RFC 3339, future) creates a
Relay post with `options.article_id`. At that time the scheduler calls `POST /posts/{id}/publish` (with retries), so
the draft goes live with its translations, even on a site without `schedule`. An article already published is not
published again. Cancel with `delete_post`, move with `update_post`.

**Deleting a Relay post** with `delete_post` and `delete_on_site: true` also deletes the article on the site
(`DELETE /posts/{id}`, needs `delete`).

## Conformance

`relay site-check <base_url> <token> [--write]` (the token may also come from `RELAY_SITE_TOKEN`) calls `/info`,
then exercises every declared capability **read-only**: list (with the `status` filter and pagination), one article,
an unknown id (`404` with `{"error"}`), a wrong token (`401`), taxonomy, authors, translations, metrics, comments,
and the shape of every extension. With `--write` it also creates a draft (needs `drafts`), reads it, edits it (`edit`,
including a stale `expected_updated_at` that must get `409`), sets and removes a translation (`translations`), and
deletes it (`delete`, then expects `404`). It never publishes. It prints one line per check and exits `1` when any
check fails.

---

# Relay Site Protocol v1

The first version: create articles, list them, read metrics and comments. Kept for existing sites.

All routes live under `{base_url}/relay/v1`, speak JSON, and require `Authorization: Bearer <site token>`.
Answer `401` otherwise.

| Route | Required | Purpose |
| --- | --- | --- |
| `GET /info` | yes | Identify the site and its capabilities |
| `POST /media` | if the site takes images | Receive an image/video, return its public URL |
| `POST /posts` | yes | Create (publish or draft) an article |
| `GET /posts` | recommended | List articles, for the content archive |
| `GET /metrics` | optional (`metrics`) | Views and other counters per article |
| `GET /posts/{id}/comments` | optional (`comments`) | Comments of one article |
| `POST /comments/{id}/replies` | optional (`comments`) | Answer a comment |

## v1 GET /info

```json
{"name": "Jane Doe", "url": "https://example.com", "version": "1", "capabilities": ["articles", "media", "metrics", "comments"]}
```

## v1 POST /media

`multipart/form-data` with a `file` field and an optional `alt` field. Response:

```json
{"id": "img_123", "url": "https://example.com/uploads/img_123.jpg"}
```

## v1 POST /posts

```json
{
  "title": "Hello world",
  "slug": "hello-world",          // optional, the site may generate it
  "body_markdown": "# ...\n\nMarkdown body",
  "excerpt": "Short summary",                // optional
  "tags": ["go", "selfhosted"],                   // optional
  "lang": "en",                              // optional
  "cover_url": "https://example.com/uploads/img_123.jpg", // optional, from POST /media
  "media": [{"id": "img_123", "url": "https://..."}],
  "status": "published",                     // or "draft"
  "published_at": "2026-11-19T18:00:00Z",
  "extra": {}                                // free-form, site specific (category, series...)
}
```

Response `201` or `200`:

```json
{"id": "a1", "url": "https://example.com/blog/hello-world"}
```

Use `4xx` for content the site refuses (Relay will not retry), `5xx` for temporary failures (Relay retries
with backoff).

## v1 GET /posts?since=RFC3339&limit=100&cursor=...

```json
{
  "items": [{"id": "a1", "url": "https://...", "title": "...", "excerpt": "...", "published_at": "2026-11-19T18:00:00Z", "metrics": {"views": 120}}],
  "next_cursor": ""
}
```

## v1 GET /metrics?ids=a1,a2

```json
{"metrics": {"a1": {"views": 120, "reads": 80}, "a2": {"views": 4}}}
```

Any integer counters are accepted; `views` is the conventional name. Return `404` if unsupported.

## v1 GET /posts/{id}/comments

```json
{"items": [{"id": "c1", "author": "Ann", "text": "Nice!", "url": "https://...#c1", "created_at": "2026-11-19T19:00:00Z"}]}
```

## v1 POST /comments/{id}/replies

Body `{"text": "Thanks!"}`, response `{"id": "c2", "url": "https://...#c2"}`.
