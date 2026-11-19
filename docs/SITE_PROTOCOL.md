# Relay Site Protocol v1

A small HTTP contract that lets Relay publish to **any website** (your own sites, client sites, anything you build)
and read back its metrics and comments. Implement these routes once per site, then connect it with:

```json
POST /v1/accounts
{"provider": "website", "name": "My blog", "credentials": {"base_url": "https://example.com", "token": "<site token>"}}
```

All routes live under `{base_url}/relay/v1`, speak JSON, and require `Authorization: Bearer <site token>`
(a secret you generate for Relay and store in the site's environment). Answer `401` otherwise.

| Route | Required | Purpose |
| --- | --- | --- |
| `GET /info` | yes | Identify the site and its capabilities |
| `POST /media` | if the site takes images | Receive an image/video, return its public URL |
| `POST /posts` | yes | Create (publish or draft) an article |
| `GET /posts` | recommended | List articles, for the content archive |
| `GET /metrics` | optional (`metrics`) | Views and other counters per article |
| `GET /posts/{id}/comments` | optional (`comments`) | Comments of one article |
| `POST /comments/{id}/replies` | optional (`comments`) | Answer a comment |

## GET /info

```json
{"name": "Jane Doe", "url": "https://example.com", "version": "1", "capabilities": ["articles", "media", "metrics", "comments"]}
```

## POST /media

`multipart/form-data` with a `file` field and an optional `alt` field. Response:

```json
{"id": "img_123", "url": "https://example.com/uploads/img_123.jpg"}
```

## POST /posts

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

## GET /posts?since=RFC3339&limit=100&cursor=...

```json
{
  "items": [{"id": "a1", "url": "https://...", "title": "...", "excerpt": "...", "published_at": "2026-11-19T18:00:00Z", "metrics": {"views": 120}}],
  "next_cursor": ""
}
```

## GET /metrics?ids=a1,a2

```json
{"metrics": {"a1": {"views": 120, "reads": 80}, "a2": {"views": 4}}}
```

Any integer counters are accepted; `views` is the conventional name. Return `404` if unsupported.

## GET /posts/{id}/comments

```json
{"items": [{"id": "c1", "author": "Ann", "text": "Nice!", "url": "https://...#c1", "created_at": "2026-11-19T19:00:00Z"}]}
```

## POST /comments/{id}/replies

Body `{"text": "Thanks!"}`, response `{"id": "c2", "url": "https://...#c2"}`.
