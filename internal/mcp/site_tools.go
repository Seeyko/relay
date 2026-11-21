package mcp

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/Seeyko/relay/internal/core"
	"github.com/Seeyko/relay/internal/providers/website"
)

// Site content tools: drive the blog of a website account through the Relay Site Protocol v2
// (docs/SITE_PROTOCOL.md). Drafts-only keys may read, create drafts and edit drafts; publishing,
// unpublishing and deleting need an admin key.

func articleProps(withStatus bool) map[string]any {
	p := map[string]any{
		"account_id":    str("Website account id (list_accounts, provider website)"),
		"title":         str("Title"),
		"slug":          str("URL slug (the site generates one when omitted)"),
		"body_html":     str("Body as HTML (give body_html or body_markdown)"),
		"body_markdown": str("Body as Markdown"),
		"excerpt":       str("Short summary"),
		"tags":          strs("Tags (see site_taxonomy)"),
		"category":      str("Category (see site_taxonomy)"),
		"lang":          str("Language of the article, one of the site's languages (site_info)"),
		"cover_url":     str("Cover image URL (from site_upload_media)"),
		"cover_media":   str("Or a Relay media id: uploaded to the site first, its URL becomes cover_url"),
		"author_id":     str("Author id from site_authors"),
		"published_at":  str("RFC 3339 with offset: back-date a published article, or the time of a scheduled one"),
		"extra":         map[string]any{"type": "object", "description": "Site-specific fields (see the site's documentation)"},
	}
	if withStatus {
		p["status"] = map[string]any{"type": "string", "enum": []string{"draft", "published", "scheduled"}, "description": "default draft; published and scheduled need an admin key"}
	}
	return p
}

type siteArgs struct {
	AccountID string `json:"account_id"`
	ArticleID string `json:"article_id"`
}

// articleBody decodes the arguments of a create/update call: the article fields go to the site as they are.
func articleBody(raw json.RawMessage) (siteArgs, map[string]any, error) {
	a, err := decode[siteArgs](raw)
	if err != nil {
		return a, nil, err
	}
	body := map[string]any{}
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &body); err != nil {
			return a, nil, &core.ErrInvalid{Msg: "invalid arguments: " + err.Error()}
		}
	}
	delete(body, "account_id")
	delete(body, "article_id")
	return a, body, nil
}

func site(ctx context.Context, svc *core.Service, accountID string) (*website.Client, error) {
	cl, _, err := svc.SiteClient(ctx, accountID)
	return cl, err
}

// draftOnly refuses a drafts-only key unless the article is a draft.
func draftOnly(ctx context.Context, cl *website.Client, articleID string) error {
	if RoleFrom(ctx) == RoleAdmin {
		return nil
	}
	st, err := cl.ArticleStatus(ctx, articleID)
	if err != nil {
		return core.SiteErr(err)
	}
	if st != "draft" {
		return ErrForbidden
	}
	return nil
}

// coverMedia uploads cover_media (a Relay media id) and sets cover_url.
func coverMedia(ctx context.Context, svc *core.Service, cl *website.Client, body map[string]any) error {
	id, _ := body["cover_media"].(string)
	delete(body, "cover_media")
	if id == "" {
		return nil
	}
	if _, ok := body["cover_url"]; ok {
		return &core.ErrInvalid{Msg: "give cover_url or cover_media, not both"}
	}
	m, err := svc.UploadToSite(ctx, cl, id, "")
	if err != nil {
		return err
	}
	body["cover_url"] = m.URL
	return nil
}

// result keeps the site's JSON as is (nil for 204).
func result(raw json.RawMessage, err error) (any, error) {
	if err != nil {
		return nil, core.SiteErr(err)
	}
	if raw == nil {
		return map[string]any{"ok": true}, nil
	}
	return raw, nil
}

// callable says whether the caller's key may call an extension.
func callable(ctx context.Context, e website.Extension) bool {
	if RoleFrom(ctx) == RoleAdmin {
		return true
	}
	return !e.Admin && e.IsReadOnly()
}

func siteTools(svc *core.Service) []Tool {
	acc := str("Website account id (list_accounts, provider website)")
	art := str("Article id on the site (site_list_articles)")
	return []Tool{
		{
			Name: "site_info",
			Description: "Describe a website account's site: name, url, protocol version, capabilities (articles, media, drafts, edit, delete, schedule, " +
				"translations, taxonomy, authors, metrics, comments), languages and extensions (site-specific actions, see site_actions). " +
				"Cached 5 minutes; refresh=true reads it again.",
			InputSchema: obj(map[string]any{"account_id": acc, "refresh": map[string]any{"type": "boolean"}}, "account_id"),
			handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				a, err := decode[struct {
					AccountID string `json:"account_id"`
					Refresh   bool   `json:"refresh"`
				}](raw)
				if err != nil {
					return nil, err
				}
				cl, err := site(ctx, svc, a.AccountID)
				if err != nil {
					return nil, err
				}
				if a.Refresh {
					return cl.Refresh(ctx)
				}
				return cl.Info(ctx)
			},
		},
		{
			Name: "site_list_articles",
			Description: "List the articles of a website (newest first), drafts included: id, url, title, slug, excerpt, status, lang, published_at, updated_at. " +
				"Filters: status (draft, published, scheduled), lang, since (RFC 3339, updated since). Page with next_cursor.",
			InputSchema: obj(map[string]any{"account_id": acc, "status": str("draft | published | scheduled"), "lang": str(""), "since": str("RFC 3339"),
				"limit": num("default site's, max 100"), "cursor": str("next_cursor of the previous page")}, "account_id"),
			handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				a, err := decode[struct {
					AccountID string `json:"account_id"`
					Status    string `json:"status"`
					Lang      string `json:"lang"`
					Since     string `json:"since"`
					Limit     int    `json:"limit"`
					Cursor    string `json:"cursor"`
				}](raw)
				if err != nil {
					return nil, err
				}
				cl, err := site(ctx, svc, a.AccountID)
				if err != nil {
					return nil, err
				}
				return result(cl.ListArticles(ctx, website.ListQuery{Status: a.Status, Lang: a.Lang, Since: a.Since, Limit: a.Limit, Cursor: a.Cursor}))
			},
		},
		{
			Name:        "site_get_article",
			Description: "Read one article of a website in full: body_html, tags, category, cover_url, author_id, status, updated_at (pass it as expected_updated_at to site_update_article).",
			InputSchema: obj(map[string]any{"account_id": acc, "article_id": art}, "account_id", "article_id"),
			handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				a, err := decode[siteArgs](raw)
				if err != nil {
					return nil, err
				}
				cl, err := site(ctx, svc, a.AccountID)
				if err != nil {
					return nil, err
				}
				return result(cl.GetArticle(ctx, a.ArticleID))
			},
		},
		{
			Name: "site_create_article",
			Description: "Create an article on a website, as a draft by default. status published publishes it at once, scheduled (with published_at) lets the site publish it " +
				"later (needs the schedule capability); both need an admin key. Give body_html or body_markdown. Unknown fields are passed to the site. " +
				"To publish at a time chosen in Relay with retries instead, use schedule_post on the website account.",
			InputSchema: obj(articleProps(true), "account_id", "title"),
			handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				a, body, err := articleBody(raw)
				if err != nil {
					return nil, err
				}
				if s, _ := body["status"].(string); s == "" {
					body["status"] = "draft"
				}
				if body["status"] != "draft" && RoleFrom(ctx) != RoleAdmin {
					return nil, ErrForbidden
				}
				cl, err := site(ctx, svc, a.AccountID)
				if err != nil {
					return nil, err
				}
				if err := coverMedia(ctx, svc, cl, body); err != nil {
					return nil, err
				}
				return result(cl.CreateArticle(ctx, body))
			},
		},
		{
			Name: "site_update_article",
			Description: "Edit an article of a website: any of the create fields except status (use site_publish_article / site_unpublish_article). " +
				"Absent fields are kept; \"\" or null clears an optional field. Pass expected_updated_at (the updated_at you read) to fail with a conflict " +
				"if someone changed it meanwhile. Drafts-only keys may edit drafts only. Needs the edit capability.",
			InputSchema: obj(func() map[string]any {
				p := articleProps(false)
				p["article_id"] = art
				p["expected_updated_at"] = str("updated_at read with site_get_article; the site answers 409 if the article changed since")
				return p
			}(), "account_id", "article_id"),
			handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				a, body, err := articleBody(raw)
				if err != nil {
					return nil, err
				}
				cl, err := site(ctx, svc, a.AccountID)
				if err != nil {
					return nil, err
				}
				if _, err := cl.Need(ctx, website.CapEdit); err != nil {
					return nil, core.SiteErr(err)
				}
				if err := draftOnly(ctx, cl, a.ArticleID); err != nil {
					return nil, err
				}
				if err := coverMedia(ctx, svc, cl, body); err != nil {
					return nil, err
				}
				return result(cl.UpdateArticle(ctx, a.ArticleID, body))
			},
		},
		{
			Name:  "site_publish_article",
			Admin: true,
			Description: "Publish a draft article of a website now, or at published_at (RFC 3339): a past date back-dates it, a future one schedules it on the site " +
				"(schedule capability). Needs the drafts capability.",
			InputSchema: obj(map[string]any{"account_id": acc, "article_id": art, "published_at": str("optional, RFC 3339 with offset")}, "account_id", "article_id"),
			handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				a, err := decode[struct {
					siteArgs
					PublishedAt string `json:"published_at"`
				}](raw)
				if err != nil {
					return nil, err
				}
				cl, err := site(ctx, svc, a.AccountID)
				if err != nil {
					return nil, err
				}
				return result(cl.PublishArticle(ctx, a.ArticleID, a.PublishedAt))
			},
		},
		{
			Name:        "site_unpublish_article",
			Admin:       true,
			Description: "Take a published or scheduled article of a website back to draft (its public page goes away). Needs the drafts capability.",
			InputSchema: obj(map[string]any{"account_id": acc, "article_id": art}, "account_id", "article_id"),
			handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				a, err := decode[siteArgs](raw)
				if err != nil {
					return nil, err
				}
				cl, err := site(ctx, svc, a.AccountID)
				if err != nil {
					return nil, err
				}
				return result(cl.UnpublishArticle(ctx, a.ArticleID))
			},
		},
		{
			Name:        "site_delete_article",
			Admin:       true,
			Description: "Delete an article (and its translations) from a website. Cannot be undone. Needs the delete capability. Relay's own posts are not changed (see delete_post with delete_on_site).",
			InputSchema: obj(map[string]any{"account_id": acc, "article_id": art}, "account_id", "article_id"),
			handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				a, err := decode[siteArgs](raw)
				if err != nil {
					return nil, err
				}
				cl, err := site(ctx, svc, a.AccountID)
				if err != nil {
					return nil, err
				}
				if err := cl.DeleteArticle(ctx, a.ArticleID); err != nil {
					return nil, core.SiteErr(err)
				}
				return map[string]any{"deleted": a.ArticleID}, nil
			},
		},
		{
			Name:        "site_taxonomy",
			Description: "Categories and tags in use on a website, with their article counts. Needs the taxonomy capability.",
			InputSchema: obj(map[string]any{"account_id": acc}, "account_id"),
			handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				a, err := decode[siteArgs](raw)
				if err != nil {
					return nil, err
				}
				cl, err := site(ctx, svc, a.AccountID)
				if err != nil {
					return nil, err
				}
				return result(cl.Taxonomy(ctx))
			},
		},
		{
			Name:        "site_authors",
			Description: "Authors an article of a website can be attributed to (id, name): use the id as author_id. Needs the authors capability.",
			InputSchema: obj(map[string]any{"account_id": acc}, "account_id"),
			handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				a, err := decode[siteArgs](raw)
				if err != nil {
					return nil, err
				}
				cl, err := site(ctx, svc, a.AccountID)
				if err != nil {
					return nil, err
				}
				return result(cl.Authors(ctx))
			},
		},
		{
			Name:        "site_list_translations",
			Description: "Translations of an article of a website: lang, title, excerpt, body_html, mode (manual or machine), updated_at. Needs the translations capability.",
			InputSchema: obj(map[string]any{"account_id": acc, "article_id": art}, "account_id", "article_id"),
			handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				a, err := decode[siteArgs](raw)
				if err != nil {
					return nil, err
				}
				cl, err := site(ctx, svc, a.AccountID)
				if err != nil {
					return nil, err
				}
				return result(cl.Translations(ctx, a.ArticleID))
			},
		},
		{
			Name: "site_set_translation",
			Description: "Write a manual translation of an article of a website in another of its languages (title, excerpt, body_html). It replaces the machine " +
				"translation for that language. Drafts-only keys may only translate drafts. Needs the translations capability.",
			InputSchema: obj(map[string]any{"account_id": acc, "article_id": art, "lang": str("Target language (site_info languages)"),
				"title": str(""), "excerpt": str(""), "body_html": str("")}, "account_id", "article_id", "lang", "title"),
			handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				a, body, err := articleBody(raw)
				if err != nil {
					return nil, err
				}
				lang, _ := body["lang"].(string)
				delete(body, "lang")
				cl, err := site(ctx, svc, a.AccountID)
				if err != nil {
					return nil, err
				}
				if _, err := cl.Need(ctx, website.CapTranslations); err != nil {
					return nil, core.SiteErr(err)
				}
				if err := draftOnly(ctx, cl, a.ArticleID); err != nil {
					return nil, err
				}
				return result(cl.SetTranslation(ctx, a.ArticleID, lang, body))
			},
		},
		{
			Name: "site_delete_translation",
			Description: "Remove the manual translation of an article of a website in one language: a site that machine-translates goes back to its machine translation. " +
				"Drafts-only keys may only change drafts. Needs the translations capability.",
			InputSchema: obj(map[string]any{"account_id": acc, "article_id": art, "lang": str("")}, "account_id", "article_id", "lang"),
			handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				a, err := decode[struct {
					siteArgs
					Lang string `json:"lang"`
				}](raw)
				if err != nil {
					return nil, err
				}
				cl, err := site(ctx, svc, a.AccountID)
				if err != nil {
					return nil, err
				}
				if _, err := cl.Need(ctx, website.CapTranslations); err != nil {
					return nil, core.SiteErr(err)
				}
				if err := draftOnly(ctx, cl, a.ArticleID); err != nil {
					return nil, err
				}
				if err := cl.DeleteTranslation(ctx, a.ArticleID, a.Lang); err != nil {
					return nil, core.SiteErr(err)
				}
				return map[string]any{"deleted": a.Lang}, nil
			},
		},
		{
			Name: "site_upload_media",
			Description: "Put an image or a video on a website (its own storage or CDN) and return {id, url}: give media (a Relay media id) or url (the site downloads it). " +
				"Use the url as cover_url or in body_html. Needs the media capability.",
			InputSchema: obj(map[string]any{"account_id": acc, "media": str("Relay media id (upload_media_from_url, POST /v1/media)"), "url": str("http(s) URL the site downloads"), "alt": str("Alternative text")}, "account_id"),
			handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				a, err := decode[struct {
					AccountID string `json:"account_id"`
					Media     string `json:"media"`
					URL       string `json:"url"`
					Alt       string `json:"alt"`
				}](raw)
				if err != nil {
					return nil, err
				}
				if (a.Media == "") == (a.URL == "") {
					return nil, &core.ErrInvalid{Msg: "give media or url (one of them)"}
				}
				cl, err := site(ctx, svc, a.AccountID)
				if err != nil {
					return nil, err
				}
				if a.Media != "" {
					return svc.UploadToSite(ctx, cl, a.Media, a.Alt)
				}
				m, err := cl.ImportMedia(ctx, a.URL, a.Alt)
				return m, core.SiteErr(err)
			},
		},
		{
			Name: "site_actions",
			Description: "List the site-specific actions (extensions) of a website: name, description, method, input_schema, admin, read_only, and allowed " +
				"(whether this key may call it). Call one with site_action.",
			InputSchema: obj(map[string]any{"account_id": acc}, "account_id"),
			handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				a, err := decode[siteArgs](raw)
				if err != nil {
					return nil, err
				}
				cl, err := site(ctx, svc, a.AccountID)
				if err != nil {
					return nil, err
				}
				i, err := cl.Need(ctx)
				if err != nil {
					return nil, core.SiteErr(err)
				}
				type action struct {
					Name        string         `json:"name"`
					Description string         `json:"description"`
					Method      string         `json:"method"`
					InputSchema map[string]any `json:"input_schema"`
					Admin       bool           `json:"admin"`
					ReadOnly    bool           `json:"read_only"`
					Allowed     bool           `json:"allowed"`
				}
				out := []action{}
				for _, e := range i.Extensions {
					if website.CheckExtension(e) != "" {
						continue
					}
					out = append(out, action{e.Name, e.Description, strings.ToUpper(e.Method), e.InputSchema, e.Admin, e.IsReadOnly(), callable(ctx, e)})
				}
				return out, nil
			},
		},
		{
			Name: "site_action",
			Description: "Call a site-specific action (extension) of a website with its arguments (args must follow the action's input_schema from site_actions). " +
				"Returns the site's answer as is. admin actions need an admin key; drafts-only keys may call read-only actions only.",
			InputSchema: obj(map[string]any{"account_id": acc, "action": str("Action name from site_actions"),
				"args": map[string]any{"type": "object", "description": "Arguments, as described by the action's input_schema"}}, "account_id", "action"),
			handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				a, err := decode[struct {
					AccountID string         `json:"account_id"`
					Action    string         `json:"action"`
					Args      map[string]any `json:"args"`
				}](raw)
				if err != nil {
					return nil, err
				}
				cl, err := site(ctx, svc, a.AccountID)
				if err != nil {
					return nil, err
				}
				i, err := cl.Need(ctx)
				if err != nil {
					return nil, core.SiteErr(err)
				}
				e, ok := i.Extension(a.Action)
				if !ok {
					names := []string{}
					for _, x := range i.Extensions {
						names = append(names, x.Name)
					}
					msg := "this site has no extensions"
					if len(names) > 0 {
						msg = "available: " + strings.Join(names, ", ")
					}
					return nil, &core.ErrInvalid{Msg: "unknown site action " + a.Action + " (" + msg + ")"}
				}
				if !callable(ctx, e) {
					return nil, ErrForbidden
				}
				return result(cl.CallExtension(ctx, e, a.Args))
			},
		},
	}
}
