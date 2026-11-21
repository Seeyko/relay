package core

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/Seeyko/relay/internal/providers"
	"github.com/Seeyko/relay/internal/store"
)

// Profile pictures. The automatic one comes from the network (providers.Avatarer) and is copied into
// the media library, because the networks serve it from CDN links that expire. A manual one (one
// picture for one account or for all of them) wins until it is cleared.

const (
	// maxAvatarSize caps a downloaded profile picture (the networks serve them well below 1 MB).
	maxAvatarSize = 5 << 20
	// avatarEvery is how often Relay asks the network again for an automatic picture.
	avatarEvery = 24 * time.Hour
)

// refreshAvatar downloads the account's picture from the network when it is due (once a day) or
// forced (connect, import). Best effort: a failure is logged and never fails the caller. cl is the
// client of the current operation, so a URL already read by Identify or AccountMetrics is reused.
func (s *Service) refreshAvatar(ctx context.Context, acc *store.Account, cl providers.Client, force bool) {
	if acc.AvatarSource == store.AvatarManual {
		return
	}
	now := s.now()
	if !force && acc.AvatarCheckedAt != nil && now.Sub(*acc.AvatarCheckedAt) < avatarEvery-5*time.Minute {
		return
	}
	av, ok := cl.(providers.Avatarer)
	if !ok {
		return
	}
	touch := func() {
		acc.AvatarCheckedAt = &now
		if err := s.Store.TouchAccountAvatar(ctx, acc.ID, now); err != nil {
			s.Log.Warn("avatar: save check time", "account", acc.ID, "err", err)
		}
	}
	u, err := av.AvatarURL(ctx)
	if err != nil {
		s.Log.Warn("avatar: network lookup failed", "account", acc.ID, "provider", acc.Provider, "err", err)
		touch()
		return
	}
	if u == "" {
		touch()
		return
	}
	b, mt, err := s.fetchImage(ctx, u, maxAvatarSize)
	if err != nil {
		s.Log.Warn("avatar: download failed", "account", acc.ID, "provider", acc.Provider, "err", err)
		touch()
		return
	}
	if acc.AvatarMediaID != "" && s.sameMedia(ctx, acc.AvatarMediaID, b) {
		touch()
		return
	}
	m, err := s.SaveMedia(ctx, bytes.NewReader(b), "avatar-"+acc.Provider+"-"+safeName(acc.ID)+imageExt[mt], mt)
	if err != nil {
		s.Log.Warn("avatar: save failed", "account", acc.ID, "err", err)
		touch()
		return
	}
	set, err := s.Store.SetAccountAvatar(ctx, acc.ID, m.ID, store.AvatarAuto, &now)
	if err != nil || !set {
		// A manual picture was set in the meantime: it wins.
		s.deleteMedia(ctx, m.ID)
		if err != nil {
			s.Log.Warn("avatar: save failed", "account", acc.ID, "err", err)
		}
		return
	}
	old := acc.AvatarMediaID
	acc.AvatarMediaID, acc.AvatarSource, acc.AvatarCheckedAt, acc.Avatar = m.ID, store.AvatarAuto, &now, "/media/"+m.Key()
	s.dropAutoAvatar(ctx, old)
	s.Log.Info("avatar: updated from the network", "account", acc.ID, "provider", acc.Provider, "media", m.ID)
}

// dropAutoAvatar deletes a replaced automatic picture, unless something else still uses it.
func (s *Service) dropAutoAvatar(ctx context.Context, id string) {
	if id == "" {
		return
	}
	if used, err := s.Store.MediaInUse(ctx, id); err != nil || used {
		return
	}
	s.deleteMedia(ctx, id)
}

// sameMedia reports whether a stored media file has exactly these bytes.
func (s *Service) sameMedia(ctx context.Context, id string, b []byte) bool {
	m, err := s.Store.GetMedia(ctx, id)
	if err != nil || m.Size != int64(len(b)) {
		return false
	}
	r, err := s.OpenMedia(ctx, m)
	if err != nil {
		return false
	}
	defer r.Close()
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return false
	}
	return bytes.Equal(h.Sum(nil), sha256Of(b))
}

func sha256Of(b []byte) []byte {
	h := sha256.Sum256(b)
	return h[:]
}

// AvatarInput sets or clears the manual picture of one account or of all of them.
type AvatarInput struct {
	// AccountID is an account id, or "all".
	AccountID string `json:"account_id"`
	// Media is a media id from the library (an image). Alternative: MediaURL.
	Media string `json:"media"`
	// MediaURL is an image URL, downloaded into the library.
	MediaURL string `json:"media_url"`
	// Clear removes the manual picture: the network's own picture comes back.
	Clear bool `json:"clear"`
}

// SetAvatar applies a manual picture (or clears it) and returns the accounts it changed.
func (s *Service) SetAvatar(ctx context.Context, in AvatarInput) ([]*store.Account, error) {
	id := strings.TrimSpace(in.AccountID)
	if id == "" {
		return nil, invalid("account_id is required (an account id, or \"all\")")
	}
	media, mediaURL := strings.TrimSpace(in.Media), strings.TrimSpace(in.MediaURL)
	switch {
	case in.Clear && (media != "" || mediaURL != ""):
		return nil, invalid("clear cannot be combined with media or media_url")
	case !in.Clear && (media == "") == (mediaURL == ""):
		return nil, invalid("give exactly one of media (a media id) or media_url, or clear: true")
	}
	var accs []*store.Account
	if id == "all" {
		all, err := s.Store.ListAccounts(ctx)
		if err != nil {
			return nil, err
		}
		accs = all
	} else {
		a, err := s.Store.GetAccount(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			return nil, invalid("unknown account %q", id)
		}
		if err != nil {
			return nil, err
		}
		accs = []*store.Account{a}
	}
	if in.Clear {
		return s.clearAvatars(ctx, accs)
	}
	var m *store.Media
	if mediaURL != "" {
		b, mt, err := s.fetchImage(ctx, mediaURL, maxVisualSize)
		if err != nil {
			return nil, invalid("media_url: %v", err)
		}
		if m, err = s.SaveMedia(ctx, bytes.NewReader(b), "avatar"+imageExt[mt], mt); err != nil {
			return nil, err
		}
	} else {
		var err error
		if m, err = s.Store.GetMedia(ctx, media); err != nil {
			return nil, invalid("unknown media %q", media)
		}
		if !strings.HasPrefix(m.Mime, "image/") {
			return nil, invalid("media %s is %s: a profile picture must be an image", m.ID, m.Mime)
		}
	}
	out := make([]*store.Account, 0, len(accs))
	for _, a := range accs {
		old, oldSource := a.AvatarMediaID, a.AvatarSource
		if _, err := s.Store.SetAccountAvatar(ctx, a.ID, m.ID, store.AvatarManual, a.AvatarCheckedAt); err != nil {
			return out, err
		}
		if oldSource == store.AvatarAuto && old != m.ID {
			s.dropAutoAvatar(ctx, old)
		}
		fresh, err := s.Store.GetAccount(ctx, a.ID)
		if err != nil {
			return out, err
		}
		out = append(out, fresh)
	}
	return out, nil
}

// clearAvatars removes the manual pictures, then asks the networks for their own picture right away
// (best effort: otherwise it comes back with the next collection). The manual media stays in the library.
func (s *Service) clearAvatars(ctx context.Context, accs []*store.Account) ([]*store.Account, error) {
	out := make([]*store.Account, 0, len(accs))
	for _, a := range accs {
		if a.AvatarSource == store.AvatarManual {
			if _, err := s.Store.SetAccountAvatar(ctx, a.ID, "", "", nil); err != nil {
				return out, err
			}
			a.AvatarMediaID, a.AvatarSource, a.AvatarCheckedAt, a.Avatar = "", "", nil, ""
		}
		if cl, err := s.client(ctx, a); err == nil {
			s.refreshAvatar(ctx, a, cl, true)
		} else {
			s.Log.Warn("avatar: no client", "account", a.ID, "err", err)
		}
		fresh, err := s.Store.GetAccount(ctx, a.ID)
		if err != nil {
			return out, err
		}
		out = append(out, fresh)
	}
	return out, nil
}
