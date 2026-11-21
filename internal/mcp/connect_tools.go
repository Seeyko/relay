package mcp

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/Seeyko/relay/internal/connect"
)

// ConnectTools start "Connect with <network>" sign-ins (admin keys only): the agent hands the link to a human.
func ConnectTools(m *connect.Manager) []Tool {
	var slugs []string
	for _, n := range m.Networks() {
		slugs = append(slugs, n.Slug)
	}
	configured := "none configured yet (set the app credentials of a network, see the README)"
	if len(slugs) > 0 {
		configured = strings.Join(slugs, ", ")
	}
	return []Tool{{
		Name:  "start_account_connection",
		Admin: true,
		Description: "Start a sign-in with a network's own consent screen, to connect a new account or to reconnect one (new " +
			"permissions, expired token) without pasting tokens. Returns url: give it to the human, who opens it in a browser " +
			"within 10 minutes (single use) and signs in. With account_id, the network must sign in the same account; its " +
			"credentials are replaced in place, so posts and history stay attached. Facebook, Instagram and LinkedIn let the " +
			"human pick the Page, Instagram account or organization. Configured networks: " + configured + ".",
		InputSchema: obj(map[string]any{
			"network":    str("Network to sign in with: " + configured + " (a connector name like linkedin-page also works). Optional with account_id."),
			"provider":   str("Alias of network"),
			"account_id": str("Reconnect this account (from list_accounts) instead of connecting a new one"),
		}),
		handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
			a, err := decode[struct {
				Network   string `json:"network"`
				Provider  string `json:"provider"`
				AccountID string `json:"account_id"`
			}](raw)
			if err != nil {
				return nil, err
			}
			if a.Network == "" {
				a.Network = a.Provider
			}
			return m.Begin(ctx, connect.Request{Network: a.Network, AccountID: a.AccountID, UserID: UserFrom(ctx)})
		},
	}}
}
