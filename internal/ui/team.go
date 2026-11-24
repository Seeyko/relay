package ui

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/Seeyko/relay/internal/email"
	"github.com/Seeyko/relay/internal/store"
	"github.com/Seeyko/relay/internal/team"
)

// registerTeam mounts the Team sheet, the invitation page and the email sign-in.
func (ui *UI) registerTeam(mux *http.ServeMux) {
	mux.Handle("POST /ui/api/email/login", ui.headers(ui.post(ui.emailLogin)))
	mux.Handle("POST /ui/api/email/signin", ui.headers(ui.post(ui.emailSignin)))
	mux.Handle("POST /ui/api/invite", ui.headers(ui.post(ui.inviteCheck)))
	mux.Handle("POST /ui/api/invite/accept", ui.headers(ui.post(ui.inviteAccept)))

	mux.Handle("GET /ui/api/team", ui.headers(ui.admin(ui.teamList)))
	mux.Handle("POST /ui/api/team/invite", ui.headers(ui.post(ui.admin(ui.teamInvite).ServeHTTP)))
	mux.Handle("POST /ui/api/team/members/{id}", ui.headers(ui.post(ui.authed(ui.teamUpdate).ServeHTTP)))
	mux.Handle("POST /ui/api/team/members/{id}/remove", ui.headers(ui.post(ui.admin(ui.teamRemove).ServeHTTP)))
	mux.Handle("POST /ui/api/team/invites/{id}/resend", ui.headers(ui.post(ui.admin(ui.teamResend).ServeHTTP)))
	mux.Handle("POST /ui/api/team/invites/{id}/revoke", ui.headers(ui.post(ui.admin(ui.teamRevoke).ServeHTTP)))

}

// teamErr maps team and store errors to HTTP answers.
func teamErr(w http.ResponseWriter, err error) {
	var denied *team.ErrDenied
	var bad *team.ErrBadInput
	switch {
	case errors.As(err, &denied):
		jsonErr(w, http.StatusForbidden, err.Error())
	case errors.As(err, &bad):
		jsonErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, team.ErrNoEmail):
		jsonErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, store.ErrLastOwner), errors.Is(err, store.ErrEmailTaken):
		jsonErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, store.ErrNotFound):
		jsonErr(w, http.StatusNotFound, "not found")
	case errors.Is(err, store.ErrOrgExists), errors.Is(err, store.ErrOrgNotEmpty), errors.Is(err, store.ErrOrgRequired):
		jsonErr(w, http.StatusConflict, err.Error())
	default:
		jsonErr(w, 500, "storage error")
	}
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid JSON")
		return false
	}
	return true
}

// ---------- email sign-in ----------

// emailLogin always answers the same, member or not.
func (ui *UI) emailLogin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email string `json:"email"`
	}
	if !decodeBody(w, r, &in) {
		return
	}
	ok, err := ui.Team.RequestLogin(r.Context(), in.Email, clientIP(r), r.Header.Get("Accept-Language"))
	if err != nil {
		teamErr(w, err)
		return
	}
	if !ok {
		jsonErr(w, http.StatusTooManyRequests, "too many attempts, wait a few minutes")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (ui *UI) emailSignin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token string `json:"token"`
	}
	if !decodeBody(w, r, &in) {
		return
	}
	u, err := ui.Team.ConsumeLogin(r.Context(), in.Token, clientIP(r))
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "this link is invalid or expired")
		return
	}
	if err := ui.startSession(w, r, u, "", "email"); err != nil {
		jsonErr(w, 500, "storage error")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---------- invitations ----------

func (ui *UI) inviteCheck(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token string `json:"token"`
	}
	if !decodeBody(w, r, &in) {
		return
	}
	t, err := ui.Team.CheckInvite(r.Context(), in.Token)
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "this invitation is invalid or expired")
		return
	}
	writeJSON(w, 200, map[string]any{"email": t.Email, "role": t.Role})
}

func (ui *UI) inviteAccept(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token string `json:"token"`
		Name  string `json:"name"`
	}
	if !decodeBody(w, r, &in) {
		return
	}
	if team.CleanName(in.Name) == "" {
		jsonErr(w, http.StatusBadRequest, "name is required")
		return
	}
	u, err := ui.Team.AcceptInvite(r.Context(), in.Token, in.Name, clientIP(r))
	if errors.Is(err, store.ErrEmailTaken) {
		jsonErr(w, http.StatusConflict, "this address is already a member: sign in instead")
		return
	}
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "this invitation is invalid or expired")
		return
	}
	if err := ui.startSession(w, r, u, "", "invite"); err != nil {
		jsonErr(w, 500, "storage error")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---------- the Team sheet ----------

func (ui *UI) actor(r *http.Request) team.Actor { return team.ActorFor(userFrom(r), clientIP(r)) }

func (ui *UI) teamList(w http.ResponseWriter, r *http.Request) {
	us, inv, err := ui.Team.Members(r.Context(), ui.actor(r))
	if err != nil {
		teamErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"members": us, "invites": inv, "email": ui.Team.EmailOn(), "me": userFrom(r).ID})
}

func (ui *UI) teamInvite(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email string   `json:"email"`
		Role  string   `json:"role"`
		Lang  string   `json:"lang"`
		Orgs  []string `json:"orgs"`
	}
	if !decodeBody(w, r, &in) {
		return
	}
	if in.Lang == "" {
		in.Lang = email.Lang(r.Header.Get("Accept-Language"))
	}
	ids, err := ui.Store.ResolveOrgs(r.Context(), in.Orgs)
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	inv, err := ui.Team.Invite(r.Context(), ui.actor(r), in.Email, in.Role, in.Lang, ids)
	if err != nil {
		teamErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"invite": inv})
}

func (ui *UI) teamUpdate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Role     *string   `json:"role"`
		Disabled *bool     `json:"disabled"`
		Email    *string   `json:"email"`
		Name     *string   `json:"name"`
		Orgs     *[]string `json:"orgs"`
	}
	if !decodeBody(w, r, &in) {
		return
	}
	p := team.Patch{Role: in.Role, Disabled: in.Disabled, Email: in.Email, Name: in.Name}
	if in.Orgs != nil {
		ids, err := ui.Store.ResolveOrgs(r.Context(), *in.Orgs)
		if err != nil {
			jsonErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if ids == nil {
			ids = []string{}
		}
		p.Orgs = &ids
	}
	u, err := ui.Team.Update(r.Context(), ui.actor(r), r.PathValue("id"), p)
	if err != nil {
		teamErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"member": u})
}

func (ui *UI) teamRemove(w http.ResponseWriter, r *http.Request) {
	if err := ui.Team.Remove(r.Context(), ui.actor(r), r.PathValue("id")); err != nil {
		teamErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (ui *UI) teamResend(w http.ResponseWriter, r *http.Request) {
	inv, err := ui.Team.ResendInvite(r.Context(), ui.actor(r), r.PathValue("id"))
	if err != nil {
		teamErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"invite": inv})
}

func (ui *UI) teamRevoke(w http.ResponseWriter, r *http.Request) {
	if err := ui.Team.RevokeInvite(r.Context(), ui.actor(r), r.PathValue("id")); err != nil {
		teamErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------- organizations (owners) ----------

// owner is authed for owners only.
func (ui *UI) owner(fn http.HandlerFunc) http.Handler {
	return ui.authed(func(w http.ResponseWriter, r *http.Request) {
		if u := userFrom(r); u == nil || u.Role != store.RoleOwner {
			jsonErr(w, http.StatusForbidden, "owners only")
			return
		}
		fn(w, r)
	})
}

func (ui *UI) orgCreate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
		Slug string `json:"slug"`
	}
	if !decodeBody(w, r, &in) {
		return
	}
	o, err := ui.Store.CreateOrg(r.Context(), in.Name, in.Slug)
	if err != nil && !errors.Is(err, store.ErrOrgExists) {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err != nil {
		teamErr(w, err)
		return
	}
	ui.audit(r, "org.create", o.ID, o.Name)
	writeJSON(w, 200, map[string]any{"org": o})
}

func (ui *UI) orgUpdate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
		Slug string `json:"slug"`
	}
	if !decodeBody(w, r, &in) {
		return
	}
	o, err := ui.Store.UpdateOrg(r.Context(), r.PathValue("id"), in.Name, in.Slug)
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrOrgExists):
		teamErr(w, err)
		return
	case err != nil:
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ui.audit(r, "org.update", o.ID, o.Name)
	writeJSON(w, 200, map[string]any{"org": o})
}

// audit records a dashboard change (when the team is configured).
func (ui *UI) audit(r *http.Request, action, target, detail string) {
	if ui.Team != nil {
		ui.Team.Audit(r.Context(), ui.actor(r), action, target, detail)
	}
}
