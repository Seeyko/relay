package mcp

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/Seeyko/relay/internal/store"
)

// fullAccess: organizations are created, renamed and removed by owners and the API token only.
func fullAccess(ctx context.Context, st *store.Store) error {
	if RoleFrom(ctx) != RoleAdmin {
		return ErrForbidden
	}
	id := UserFrom(ctx)
	if id == "" {
		if _, limited := store.OrgsFrom(ctx); !limited {
			return nil
		}
	} else if u, err := st.GetUser(ctx, id); err == nil && u.Role == store.RoleOwner {
		return nil
	}
	return errors.New("only owners (and the API token) manage organizations")
}

// OrgTools lists and manages organizations.
func OrgTools(st *store.Store) []Tool {
	return []Tool{
		{
			Name: "list_organizations",
			Description: "List the organizations you can use (id, slug, name, number of accounts). Each groups the accounts of one brand or app, " +
				"with any number of accounts per network. Pass org (id, slug or name) to any tool to work in one of them.",
			InputSchema: obj(map[string]any{}),
			OwnOrg:      true,
			handler: func(ctx context.Context, _ json.RawMessage) (any, error) {
				return st.ListOrgs(ctx)
			},
		},
		{
			Name:        "create_organization",
			Admin:       true,
			OwnOrg:      true,
			Description: "Create an organization (owners and the API token). Then connect accounts in it with start_account_connection or connect_account and org, and give members access with invite_member or update_member (orgs).",
			InputSchema: obj(map[string]any{"name": str("Display name, e.g. My Little Garden"), "slug": str("Optional short id, default from the name (my-little-garden)")}, "name"),
			handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				if err := fullAccess(ctx, st); err != nil {
					return nil, err
				}
				a, err := decode[struct{ Name, Slug string }](raw)
				if err != nil {
					return nil, err
				}
				return st.CreateOrg(ctx, a.Name, a.Slug)
			},
		},
		{
			Name:        "update_organization",
			Admin:       true,
			OwnOrg:      true,
			Description: "Rename an organization or change its slug (owners and the API token).",
			InputSchema: obj(map[string]any{"org": str("Organization id, slug or name"), "name": str("New name"), "slug": str("New slug")}, "org"),
			handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				if err := fullAccess(ctx, st); err != nil {
					return nil, err
				}
				a, err := decode[struct{ Org, Name, Slug string }](raw)
				if err != nil {
					return nil, err
				}
				return st.UpdateOrg(ctx, a.Org, a.Name, a.Slug)
			},
		},
		{
			Name:        "delete_organization",
			Admin:       true,
			OwnOrg:      true,
			Description: "Remove an organization that has no accounts left (owners and the API token). The last one cannot be removed.",
			InputSchema: obj(map[string]any{"org": str("Organization id, slug or name")}, "org"),
			handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				if err := fullAccess(ctx, st); err != nil {
					return nil, err
				}
				a, err := decode[struct{ Org string }](raw)
				if err != nil {
					return nil, err
				}
				if err := st.DeleteOrg(ctx, a.Org); err != nil {
					return nil, err
				}
				return map[string]any{"deleted": a.Org}, nil
			},
		},
		{
			Name:        "move_account",
			Admin:       true,
			OwnOrg:      true,
			Description: "Move an account, with its posts, comments and metrics, to another organization. You must see both.",
			InputSchema: obj(map[string]any{"account_id": str("Account id"), "org": str("Target organization id, slug or name")}, "account_id", "org"),
			handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				a, err := decode[struct {
					AccountID string `json:"account_id"`
					Org       string
				}](raw)
				if err != nil {
					return nil, err
				}
				return st.MoveAccount(ctx, a.AccountID, a.Org)
			},
		},
	}
}
