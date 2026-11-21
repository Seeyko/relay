package ui

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/Seeyko/relay/internal/connect"
	"github.com/Seeyko/relay/internal/store"
)

// registerAccounts mounts the Accounts sheet: connected accounts, Reconnect and "Connect a network" (owners and
// admins only). The sign-in itself runs on /connect/ (internal/connect).
func (ui *UI) registerAccounts(mux *http.ServeMux) {
	mux.Handle("GET /ui/api/accounts", ui.headers(ui.admin(ui.accountsList)))
	mux.Handle("POST /ui/api/accounts/connect", ui.headers(ui.post(ui.admin(ui.accountsConnect).ServeHTTP)))
}

// SessionUserID is the owner or admin signed in on r ("" otherwise): a start link they asked for from the
// dashboard goes on without a confirmation page.
func (ui *UI) SessionUserID(r *http.Request) string {
	u, _ := ui.sessionUser(r)
	if u == nil || !isAdmin(u) {
		return ""
	}
	return u.ID
}

type accountRow struct {
	ID        string `json:"id"`
	Provider  string `json:"provider"`
	Name      string `json:"name"`
	Handle    string `json:"handle"`
	Avatar    string `json:"avatar,omitempty"`
	Reconnect string `json:"reconnect,omitempty"` // network slug, "" when none is configured
}

type networkRow struct {
	Slug        string   `json:"slug"`
	Label       string   `json:"label"`
	Connector   string   `json:"connector"`
	RedirectURI string   `json:"redirect_uri"`
	Scopes      []string `json:"scopes,omitempty"`
}

func (ui *UI) accountsList(w http.ResponseWriter, r *http.Request) {
	accs, err := ui.Store.ListAccounts(r.Context())
	if err != nil {
		jsonErr(w, 500, "storage error")
		return
	}
	rows := make([]accountRow, 0, len(accs))
	for _, a := range accs {
		row := accountRow{ID: a.ID, Provider: a.Provider, Name: a.Name, Handle: a.Handle, Avatar: a.Avatar}
		if ui.Connect != nil {
			row.Reconnect = ui.Connect.ReconnectNetwork(r.Context(), a)
		}
		rows = append(rows, row)
	}
	nets := []networkRow{}
	if ui.Connect != nil {
		for _, n := range ui.Connect.Networks() {
			nets = append(nets, networkRow{Slug: n.Slug, Label: n.Label, Connector: n.Connector, RedirectURI: ui.Connect.RedirectURI(n), Scopes: n.Scopes})
		}
	}
	writeJSON(w, 200, map[string]any{"accounts": rows, "networks": nets})
}

func (ui *UI) accountsConnect(w http.ResponseWriter, r *http.Request) {
	if ui.Connect == nil {
		jsonErr(w, http.StatusNotFound, "no network sign-in is configured")
		return
	}
	var in struct {
		Network   string `json:"network"`
		AccountID string `json:"account_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	st, err := ui.Connect.Begin(r.Context(), connect.Request{Network: in.Network, AccountID: in.AccountID, UserID: userFrom(r).ID, Dashboard: true})
	var inv *connect.ErrInvalid
	switch {
	case errors.As(err, &inv):
		jsonErr(w, http.StatusBadRequest, inv.Msg)
		return
	case errors.Is(err, store.ErrNotFound):
		jsonErr(w, http.StatusNotFound, "not found")
		return
	case err != nil:
		jsonErr(w, 500, "storage error")
		return
	}
	writeJSON(w, 200, map[string]any{"url": st.URL, "expires_at": st.ExpiresAt})
}
