package mcp

import (
	"context"
	"encoding/json"

	"github.com/Seeyko/relay/internal/team"
)

// actor is who calls a team tool: the member who approved the OAuth app, or the API token holder.
func actor(ctx context.Context, t *team.Team) (team.Actor, error) {
	id := UserFrom(ctx)
	if id == "" {
		return team.APIActor(), nil
	}
	u, err := t.Store.GetUser(ctx, id)
	if err != nil {
		return team.Actor{}, err
	}
	return team.ActorFor(u, ""), nil
}

// TeamTools manages the dashboard members (admin keys only).
func TeamTools(t *team.Team) []Tool {
	roleSchema := map[string]any{"type": "string", "enum": []string{"owner", "admin", "editor", "viewer"},
		"description": "owner/admin: everything incl. the team; editor: sees everything, drafts only (cannot publish, approve or connect accounts); viewer: read-only dashboard. Only an owner can make an owner."}
	return []Tool{
		{
			Name:        "list_members",
			Admin:       true,
			Description: "List the dashboard members (id, email, name, role, last_login_at, disabled, passkeys) and the pending invitations. The email sign-in is available when email_enabled is true.",
			InputSchema: obj(map[string]any{}),
			handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				us, inv, err := t.Members(ctx)
				if err != nil {
					return nil, err
				}
				return map[string]any{"members": us, "invites": inv, "email_enabled": t.EmailOn()}, nil
			},
		},
		{
			Name:        "invite_member",
			Admin:       true,
			Description: "Invite someone to the dashboard by email. They get a link (7 days, single use) to set their name and sign in, then sign in with an email link or a passkey. Replaces a pending invitation to the same address. Needs email (RELAY_RESEND_API_KEY, RELAY_EMAIL_FROM).",
			InputSchema: obj(map[string]any{
				"email": str("their address"),
				"role":  roleSchema,
				"lang":  str("language of the email: en (default) or fr"),
			}, "email", "role"),
			handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				a, err := decode[struct{ Email, Role, Lang string }](raw)
				if err != nil {
					return nil, err
				}
				who, err := actor(ctx, t)
				if err != nil {
					return nil, err
				}
				return t.Invite(ctx, who, a.Email, a.Role, a.Lang)
			},
		},
		{
			Name:        "update_member",
			Admin:       true,
			Description: "Change a member: role, disabled (true signs them out everywhere and disconnects the apps they approved), email or name. Use id from list_members. The last owner cannot be demoted or disabled. To resend or revoke a pending invitation, pass its id with action resend or revoke.",
			InputSchema: obj(map[string]any{
				"id":       str("member id (usr_...) or pending invitation id (inv_...)"),
				"role":     roleSchema,
				"disabled": map[string]any{"type": "boolean"},
				"email":    str("new address (lower-cased)"),
				"name":     str("display name"),
				"action":   map[string]any{"type": "string", "enum": []string{"resend", "revoke"}, "description": "pending invitations only"},
			}, "id"),
			handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				a, err := decode[struct {
					ID       string
					Role     *string
					Disabled *bool
					Email    *string
					Name     *string
					Action   string
				}](raw)
				if err != nil {
					return nil, err
				}
				who, err := actor(ctx, t)
				if err != nil {
					return nil, err
				}
				switch a.Action {
				case "resend":
					return t.ResendInvite(ctx, who, a.ID)
				case "revoke":
					if err := t.RevokeInvite(ctx, who, a.ID); err != nil {
						return nil, err
					}
					return map[string]any{"revoked": a.ID}, nil
				case "":
				default:
					return nil, &team.ErrBadInput{Msg: "action must be resend or revoke"}
				}
				return t.Update(ctx, who, a.ID, team.Patch{Role: a.Role, Disabled: a.Disabled, Email: a.Email, Name: a.Name})
			},
		},
		{
			Name:        "remove_member",
			Admin:       true,
			Description: "Remove a member: their passkeys, sessions and the apps they approved go with them. A pending invitation id (inv_...) is revoked instead. The last owner cannot be removed.",
			InputSchema: obj(map[string]any{"id": str("member id (usr_...) or pending invitation id (inv_...)")}, "id"),
			handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				a, err := decode[struct{ ID string }](raw)
				if err != nil {
					return nil, err
				}
				who, err := actor(ctx, t)
				if err != nil {
					return nil, err
				}
				if len(a.ID) > 4 && a.ID[:4] == "inv_" {
					err = t.RevokeInvite(ctx, who, a.ID)
				} else {
					err = t.Remove(ctx, who, a.ID)
				}
				if err != nil {
					return nil, err
				}
				return map[string]any{"removed": a.ID}, nil
			},
		},
	}
}
