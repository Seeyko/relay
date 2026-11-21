package core

import (
	"context"
	"errors"

	"github.com/Seeyko/relay/internal/providers"
	"github.com/Seeyko/relay/internal/providers/website"
	"github.com/Seeyko/relay/internal/store"
)

// SiteClient returns the Site Protocol client of a website account.
func (s *Service) SiteClient(ctx context.Context, accountID string) (*website.Client, *store.Account, error) {
	if accountID == "" {
		return nil, nil, invalid("account_id is required (a website account from list_accounts)")
	}
	acc, err := s.Store.GetAccount(ctx, accountID)
	if err != nil {
		return nil, nil, err
	}
	if acc.Provider != "website" {
		return nil, nil, invalid("account %s is a %s account: the site tools need a website account", acc.ID, acc.Provider)
	}
	cl, err := s.client(ctx, acc)
	if err != nil {
		return nil, nil, err
	}
	wc, ok := cl.(*website.Client)
	if !ok {
		return nil, nil, errors.New("website account without a website client")
	}
	return wc, acc, nil
}

// SiteErr turns the site client's refusals into caller errors: a missing capability or invalid input
// becomes ErrInvalid (400); answers of the site stay *website.SiteError (the REST API keeps their status).
func SiteErr(err error) error {
	if err == nil {
		return nil
	}
	var ce *website.CapError
	if errors.As(err, &ce) || errors.Is(err, providers.ErrInvalidPost) {
		return &ErrInvalid{Msg: err.Error()}
	}
	return err
}

// DeleteOnSite deletes the article of a website post on its site. An article already gone (404) is fine.
func (s *Service) DeleteOnSite(ctx context.Context, p *store.Post) error {
	acc, err := s.Store.GetAccount(ctx, p.AccountID)
	if err != nil {
		return err
	}
	if acc.Provider != "website" {
		return invalid("delete_on_site only applies to posts of website accounts (this one is %s)", acc.Provider)
	}
	if p.RemoteID == "" {
		return invalid("post %s has no article on the site (never published)", p.ID)
	}
	cl, _, err := s.SiteClient(ctx, acc.ID)
	if err != nil {
		return err
	}
	if err := cl.DeleteArticle(ctx, p.RemoteID); err != nil && website.StatusOf(err) != 404 {
		return SiteErr(err)
	}
	s.Log.Info("article deleted on site", "post", p.ID, "account", acc.ID, "remote_id", p.RemoteID)
	return nil
}

// UploadToSite sends a file of Relay's media library to the site (POST /media).
func (s *Service) UploadToSite(ctx context.Context, cl *website.Client, mediaID, alt string) (*website.Media, error) {
	files, err := s.mediaFiles(ctx, []string{mediaID})
	if err != nil {
		return nil, err
	}
	files[0].Alt = alt
	m, err := cl.UploadMedia(ctx, files[0])
	return m, SiteErr(err)
}
