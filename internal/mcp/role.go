package mcp

import (
	"context"
	"errors"
)

// Role is the privilege level of the caller, set by the HTTP layer from the bearer token.
type Role string

const (
	RoleAdmin Role = "admin"
	// RoleDrafter can read everything and stage drafts, but never publish, reply or change accounts.
	// Give this key to sub-agents: a human (or an admin agent) approves drafts with approve_post.
	RoleDrafter Role = "drafter"
)

type roleKey struct{}

func WithRole(ctx context.Context, r Role) context.Context {
	return context.WithValue(ctx, roleKey{}, r)
}

// RoleFrom defaults to admin for in-process calls.
func RoleFrom(ctx context.Context) Role {
	if r, ok := ctx.Value(roleKey{}).(Role); ok {
		return r
	}
	return RoleAdmin
}

var ErrForbidden = errors.New("this API key can only create and edit drafts")
