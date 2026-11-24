// Package team manages the dashboard's members: invitations by email, sign-in links, email codes for app
// approvals, roles and the audit log.
//
// Security model:
//   - Invitation links, sign-in links and email codes are random, single use and short lived (7 days, 15 minutes,
//     10 minutes). Only their SHA-256 hash is stored. An email code allows 5 attempts.
//   - Asking for a sign-in link always gets the same answer, sent at the same speed (the email leaves in the
//     background), whether the address is a member or not. It is rate limited per address and per client.
//   - Owners and admins manage the team. Only an owner may touch an owner or make one, and the last active owner
//     can be neither removed, disabled nor demoted.
//   - Tokens never reach the logs or the audit log.
package team

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/mail"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/Seeyko/relay/internal/email"
	"github.com/Seeyko/relay/internal/store"
)

const (
	InviteTTL   = 7 * 24 * time.Hour
	LoginTTL    = 15 * time.Minute
	CodeTTL     = 10 * time.Minute
	CodeTries   = 5
	limitWindow = 15 * time.Minute
)

// ErrNoEmail: email is not configured (RELAY_RESEND_API_KEY, RELAY_EMAIL_FROM).
var ErrNoEmail = errors.New("email is not configured (set RELAY_RESEND_API_KEY and RELAY_EMAIL_FROM)")

// ErrDenied is a team change the actor may not make.
type ErrDenied struct{ Msg string }

func (e *ErrDenied) Error() string { return e.Msg }

// ErrBadInput is an invalid argument.
type ErrBadInput struct{ Msg string }

func (e *ErrBadInput) Error() string { return e.Msg }

// Actor is who makes a change: a member, or the holder of the API token (UserID "", Role owner).
type Actor struct {
	UserID string
	Label  string // email or name, for the audit log and the invitation email
	Role   string
	IP     string
	// Orgs are the organizations the actor sees; ignored when Full (owners and the API token see all of them).
	Orgs []string
	Full bool
}

// APIActor is the holder of RELAY_API_TOKEN (or an admin OAuth app approved before team members existed).
func APIActor() Actor { return Actor{Label: "api", Role: store.RoleOwner, Full: true} }

// ActorFor is a member acting from ip.
func ActorFor(u *store.User, ip string) Actor {
	label := u.Email
	if label == "" {
		label = u.Name
	}
	if label == "" {
		label = u.ID
	}
	return Actor{UserID: u.ID, Label: label, Role: u.Role, IP: ip, Orgs: u.Orgs, Full: u.Role == store.RoleOwner}
}

// sees reports whether the actor sees every organization of ids.
func (a Actor) sees(ids []string) bool { return a.Full || store.Subset(ids, a.Orgs) }

// canTouch: an actor limited to some organizations only manages members who see nothing else.
func canTouch(a Actor, u *store.User) error {
	if a.Full {
		return nil
	}
	if u.Role == store.RoleOwner || !store.Subset(u.Orgs, a.Orgs) {
		return &ErrDenied{"this member sees organizations you do not manage"}
	}
	return nil
}

// orgsFor checks the organizations given to a new or changed member: an actor gives only organizations it
// sees, and a member who is not an owner needs at least one. With none given, a limited actor gives its own
// and a full one the only organization there is.
func (t *Team) orgsFor(ctx context.Context, a Actor, role string, ids []string) ([]string, error) {
	if role == store.RoleOwner {
		return nil, nil
	}
	if len(ids) == 0 {
		if !a.Full {
			return a.Orgs, nil
		}
		all, err := t.Store.AllOrgIDs(ctx)
		if err != nil {
			return nil, err
		}
		if len(all) != 1 {
			return nil, &ErrBadInput{"give orgs: the organizations this member will see (see list_organizations)"}
		}
		return all, nil
	}
	if !a.sees(ids) {
		return nil, &ErrDenied{"you can only give access to organizations you see"}
	}
	return ids, nil
}

type Team struct {
	Store *store.Store
	// Mail is nil when email is not configured: invitations, sign-in links and email codes are then off.
	Mail    email.Sender
	BaseURL string // https://relay.example.com
	Log     *slog.Logger
	Now     func() time.Time

	mu      sync.Mutex
	limiter map[string][]time.Time
	wg      sync.WaitGroup
}

func New(st *store.Store, mail email.Sender, baseURL string, log *slog.Logger) *Team {
	return &Team{Store: st, Mail: mail, BaseURL: strings.TrimRight(baseURL, "/"), Log: log, Now: time.Now, limiter: map[string][]time.Time{}}
}

// EmailOn reports whether email features are available.
func (t *Team) EmailOn() bool { return t != nil && t.Mail != nil }

// Wait blocks until the emails sent in the background are out (tests).
func (t *Team) Wait() { t.wg.Wait() }

func HashToken(tok string) string {
	h := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(h[:])
}

func randomToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// allow is a sliding window: at most n events per limitWindow for key.
func (t *Team) allow(key string, n int) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.Now()
	cut := now.Add(-limitWindow)
	if len(t.limiter) > 8192 {
		t.limiter = map[string][]time.Time{}
	}
	kept := t.limiter[key][:0]
	for _, at := range t.limiter[key] {
		if at.After(cut) {
			kept = append(kept, at)
		}
	}
	if len(kept) >= n {
		t.limiter[key] = kept
		return false
	}
	t.limiter[key] = append(kept, now)
	return true
}

// Audit records an event in the log and the audit table. detail never holds a token.
func (t *Team) Audit(ctx context.Context, a Actor, action, target, detail string) {
	_ = t.Store.AddAudit(ctx, &store.AuditEntry{At: t.Now(), Actor: a.Label, Action: action, Target: target, Detail: detail, IP: a.IP})
	t.Log.Info("team: "+action, "actor", a.Label, "target", target, "detail", detail, "ip", a.IP)
}

func (t *Team) send(m email.Message, what string) {
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := t.Mail.Send(ctx, m); err != nil {
			t.Log.Warn("team: email not sent", "what", what, "err", err)
		}
	}()
}

func cleanEmail(e string) (string, error) {
	e = store.NormEmail(e)
	a, err := mail.ParseAddress(e)
	if err != nil || a.Address != e || len(e) > 254 || !strings.Contains(e[strings.LastIndex(e, "@")+1:], ".") {
		return "", &ErrBadInput{"invalid email address"}
	}
	return e, nil
}

// CleanName trims a display name to 60 printable characters.
func CleanName(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.TrimSpace(s))
	if r := []rune(s); len(r) > 60 {
		s = string(r[:60])
	}
	return s
}

// ---------- team management ----------

func canManage(a Actor) error {
	if a.Role != store.RoleOwner && a.Role != store.RoleAdmin {
		return &ErrDenied{"only owners and admins manage the team"}
	}
	return nil
}

// canGive: only an owner may make an owner.
func canGive(a Actor, role string) error {
	if !store.ValidRole(role) {
		return &ErrBadInput{"role must be owner, admin, editor or viewer"}
	}
	if role == store.RoleOwner && a.Role != store.RoleOwner {
		return &ErrDenied{"only an owner can make an owner"}
	}
	return nil
}

// Members lists the members and the pending invitations. An actor limited to some organizations sees the
// owners and the members who share one of them, with only those organizations listed.
func (t *Team) Members(ctx context.Context, a Actor) ([]*store.User, []*store.Invite, error) {
	us, err := t.Store.ListUsers(ctx)
	if err != nil {
		return nil, nil, err
	}
	inv, err := t.Store.ListInvites(ctx, t.Now())
	if err != nil || a.Full {
		return us, inv, err
	}
	mine := func(ids []string) []string {
		out := []string{}
		for _, id := range ids {
			if store.Subset([]string{id}, a.Orgs) {
				out = append(out, id)
			}
		}
		return out
	}
	keptU := []*store.User{}
	for _, u := range us {
		if u.Role == store.RoleOwner || u.ID == a.UserID {
			keptU = append(keptU, u)
		} else if m := mine(u.Orgs); len(m) > 0 {
			u.Orgs = m
			keptU = append(keptU, u)
		}
	}
	keptI := []*store.Invite{}
	for _, i := range inv {
		if m := mine(i.Orgs); len(m) > 0 {
			i.Orgs = m
			keptI = append(keptI, i)
		}
	}
	return keptU, keptI, nil
}

// Invite emails an invitation to join with role. A pending invitation to the same address is replaced.
// orgs are organization ids (see orgsFor).
func (t *Team) Invite(ctx context.Context, a Actor, addr, role, lang string, orgs []string) (*store.Invite, error) {
	if err := canManage(a); err != nil {
		return nil, err
	}
	if err := canGive(a, role); err != nil {
		return nil, err
	}
	orgs, err := t.orgsFor(ctx, a, role, orgs)
	if err != nil {
		return nil, err
	}
	if !t.EmailOn() {
		return nil, ErrNoEmail
	}
	addr, err = cleanEmail(addr)
	if err != nil {
		return nil, err
	}
	if _, err := t.Store.UserByEmail(ctx, addr); err == nil {
		return nil, &ErrBadInput{"this address is already a member"}
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	if err := t.Store.DeleteInvites(ctx, "", addr); err != nil {
		return nil, err
	}
	tok := randomToken()
	lang = email.Lang(lang)
	row := &store.AuthToken{ID: store.NewID("inv"), Kind: store.TokenInvite, Hash: HashToken(tok), Email: addr, Role: role, Lang: lang,
		InvitedBy: a.UserID, ExpiresAt: t.Now().Add(InviteTTL), Orgs: orgs}
	if err := t.Store.CreateAuthToken(ctx, row); err != nil {
		return nil, err
	}
	t.send(email.Invite(lang, addr, inviterName(a), role, t.BaseURL+"/ui/accept?token="+tok), "invite")
	t.Audit(ctx, a, "invite", addr, "role="+role+" orgs="+strings.Join(orgs, ","))
	return &store.Invite{ID: row.ID, Email: addr, Role: role, Lang: lang, InvitedBy: a.UserID, CreatedAt: row.CreatedAt, ExpiresAt: row.ExpiresAt, Orgs: orgs}, nil
}

func inviterName(a Actor) string {
	if a.UserID == "" {
		return ""
	}
	return a.Label
}

// ResendInvite sends a pending invitation again with a new link (the previous one stops working) and 7 more days.
func (t *Team) ResendInvite(ctx context.Context, a Actor, id string) (*store.Invite, error) {
	if err := canManage(a); err != nil {
		return nil, err
	}
	if !t.EmailOn() {
		return nil, ErrNoEmail
	}
	inv, err := t.Store.GetInvite(ctx, id, t.Now())
	if err != nil {
		return nil, err
	}
	if err := canGive(a, inv.Role); err != nil {
		return nil, err
	}
	if !a.sees(inv.Orgs) {
		return nil, store.ErrNotFound
	}
	tok := randomToken()
	exp := t.Now().Add(InviteTTL)
	if err := t.Store.RenewInvite(ctx, id, HashToken(tok), exp); err != nil {
		return nil, err
	}
	t.send(email.Invite(inv.Lang, inv.Email, inviterName(a), inv.Role, t.BaseURL+"/ui/accept?token="+tok), "invite")
	t.Audit(ctx, a, "invite.resend", inv.Email, "role="+inv.Role)
	return &store.Invite{ID: inv.ID, Email: inv.Email, Role: inv.Role, Lang: inv.Lang, InvitedBy: inv.InvitedBy, ExpiresAt: exp, Orgs: inv.Orgs}, nil
}

// RevokeInvite deletes a pending invitation.
func (t *Team) RevokeInvite(ctx context.Context, a Actor, id string) error {
	if err := canManage(a); err != nil {
		return err
	}
	inv, err := t.Store.GetInvite(ctx, id, t.Now())
	if err != nil {
		return err
	}
	if err := canGive(a, inv.Role); err != nil {
		return err
	}
	if !a.sees(inv.Orgs) {
		return store.ErrNotFound
	}
	if err := t.Store.DeleteInvites(ctx, id, ""); err != nil {
		return err
	}
	t.Audit(ctx, a, "invite.revoke", inv.Email, "")
	return nil
}

// Patch is a change to a member; nil fields are kept.
type Patch struct {
	Role     *string
	Disabled *bool
	Email    *string
	Name     *string
	// Orgs replaces the organizations (ids) of a member who is not an owner.
	Orgs *[]string
}

// Update changes a member's role, status, email or name.
func (t *Team) Update(ctx context.Context, a Actor, id string, p Patch) (*store.User, error) {
	u, err := t.Store.GetUser(ctx, id)
	if err != nil {
		return nil, err
	}
	self := a.UserID != "" && a.UserID == id
	// Anyone may rename themselves; everything else is team management.
	if !(self && p.Role == nil && p.Disabled == nil && p.Email == nil && p.Orgs == nil) {
		if err := canManage(a); err != nil {
			return nil, err
		}
		if u.Role == store.RoleOwner && a.Role != store.RoleOwner {
			return nil, &ErrDenied{"only an owner can change an owner"}
		}
		if err := canTouch(a, u); err != nil {
			return nil, err
		}
	}
	sp := store.UserPatch{Disabled: p.Disabled}
	var changes []string
	if p.Role != nil {
		if err := canGive(a, *p.Role); err != nil {
			return nil, err
		}
		sp.Role = p.Role
		changes = append(changes, "role="+*p.Role)
	}
	if p.Email != nil {
		e := ""
		if strings.TrimSpace(*p.Email) != "" {
			if e, err = cleanEmail(*p.Email); err != nil {
				return nil, err
			}
		}
		sp.Email = &e
		changes = append(changes, "email="+e)
	}
	if p.Name != nil {
		n := CleanName(*p.Name)
		sp.Name = &n
		changes = append(changes, "name")
	}
	if p.Disabled != nil {
		changes = append(changes, fmt.Sprintf("disabled=%v", *p.Disabled))
	}
	role := u.Role
	if p.Role != nil {
		role = *p.Role
	}
	if p.Orgs != nil || (u.Role == store.RoleOwner && role != store.RoleOwner) {
		var want []string
		if p.Orgs != nil {
			want = *p.Orgs
		}
		if p.Orgs != nil || !a.Full {
			ids, err := t.orgsFor(ctx, a, role, want)
			if err != nil {
				return nil, err
			}
			if role != store.RoleOwner {
				sp.Orgs = &ids
				changes = append(changes, "orgs="+strings.Join(ids, ","))
			}
		}
	}
	out, err := t.Store.UpdateUser(ctx, id, sp)
	if err != nil {
		return nil, err
	}
	if p.Disabled != nil && *p.Disabled {
		_ = t.revokeGrants(ctx, id)
	}
	t.Audit(ctx, a, "member.update", memberLabel(u), strings.Join(changes, " "))
	return out, nil
}

// Remove deletes a member with its passkeys, sessions and the apps it approved.
func (t *Team) Remove(ctx context.Context, a Actor, id string) error {
	if err := canManage(a); err != nil {
		return err
	}
	u, err := t.Store.GetUser(ctx, id)
	if err != nil {
		return err
	}
	if u.Role == store.RoleOwner && a.Role != store.RoleOwner {
		return &ErrDenied{"only an owner can remove an owner"}
	}
	if err := canTouch(a, u); err != nil {
		return err
	}
	if err := t.Store.DeleteUser(ctx, id); err != nil {
		return err
	}
	t.Audit(ctx, a, "member.remove", memberLabel(u), "role="+u.Role)
	return nil
}

func (t *Team) revokeGrants(ctx context.Context, userID string) error {
	gs, err := t.Store.ListOAuthGrants(ctx)
	if err != nil {
		return err
	}
	for _, g := range gs {
		if g.UserID == userID {
			_ = t.Store.DeleteOAuthGrant(ctx, g.ID)
		}
	}
	return nil
}

func memberLabel(u *store.User) string {
	if u.Email != "" {
		return u.Email
	}
	return u.ID
}

// ---------- invitations ----------

// CheckInvite returns the pending invitation of a link (for the accept page).
func (t *Team) CheckInvite(ctx context.Context, tok string) (*store.AuthToken, error) {
	if tok == "" {
		return nil, store.ErrNotFound
	}
	return t.Store.PeekAuthToken(ctx, store.TokenInvite, HashToken(tok), t.Now())
}

// AcceptInvite turns an invitation link into a member (the link is burned).
func (t *Team) AcceptInvite(ctx context.Context, tok, name, ip string) (*store.User, error) {
	if tok == "" {
		return nil, store.ErrNotFound
	}
	name = CleanName(name)
	if name == "" {
		return nil, &ErrBadInput{"name is required"}
	}
	u, err := t.Store.AcceptInvite(ctx, HashToken(tok), name, t.Now())
	if err != nil {
		t.Log.Warn("team: invitation refused", "ip", ip, "err", err)
		return nil, err
	}
	t.Audit(ctx, ActorFor(u, ip), "invite.accept", u.Email, "role="+u.Role)
	return u, nil
}

// ---------- sign-in links ----------

// RequestLogin emails a sign-in link when addr is an active member. It reports nothing about the address:
// the only error is ErrNoEmail, or a rate limit on the client (false).
func (t *Team) RequestLogin(ctx context.Context, addr, ip, lang string) (bool, error) {
	if !t.EmailOn() {
		return false, ErrNoEmail
	}
	if !t.allow("login-ip:"+ip, 10) {
		return false, nil
	}
	addr = store.NormEmail(addr)
	if addr == "" || len(addr) > 254 || !t.allow("login-email:"+addr, 3) {
		return true, nil
	}
	u, err := t.Store.UserByEmail(ctx, addr)
	if err != nil || u.Disabled {
		t.Log.Info("team: sign-in link not sent (no active member)", "ip", ip)
		return true, nil
	}
	tok := randomToken()
	if err := t.Store.CreateAuthToken(ctx, &store.AuthToken{Kind: store.TokenLogin, Hash: HashToken(tok), UserID: u.ID, Email: u.Email,
		ExpiresAt: t.Now().Add(LoginTTL)}); err != nil {
		t.Log.Warn("team: sign-in link not stored", "err", err)
		return true, nil
	}
	t.send(email.SignIn(lang, u.Email, t.BaseURL+"/ui/signin?token="+tok), "signin")
	t.Audit(ctx, ActorFor(u, ip), "login.link", u.Email, "")
	return true, nil
}

// ConsumeLogin burns a sign-in link and returns its member.
func (t *Team) ConsumeLogin(ctx context.Context, tok, ip string) (*store.User, error) {
	if tok == "" {
		return nil, store.ErrNotFound
	}
	row, err := t.Store.UseAuthToken(ctx, store.TokenLogin, HashToken(tok), t.Now())
	if err != nil {
		t.Log.Warn("team: sign-in link refused", "ip", ip, "err", err)
		return nil, err
	}
	u, err := t.Store.GetUser(ctx, row.UserID)
	if err != nil {
		return nil, err
	}
	if u.Disabled {
		return nil, store.ErrNotFound
	}
	return u, nil
}

// ---------- email codes (OAuth approval without a passkey) ----------

func codeHash(id, code string) string { return HashToken(id + ":" + code) }

// SendCode emails a fresh 6-digit code to the member. app names the app being approved.
func (t *Team) SendCode(ctx context.Context, u *store.User, app, lang, ip string) error {
	if !t.EmailOn() {
		return ErrNoEmail
	}
	if u.Email == "" {
		return &ErrBadInput{"this member has no email address"}
	}
	if !t.allow("code:"+u.ID, 5) {
		return &ErrDenied{"too many codes, wait a few minutes"}
	}
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return err
	}
	code := fmt.Sprintf("%06d", n.Int64())
	id := store.NewID("code")
	if err := t.Store.CreateAuthToken(ctx, &store.AuthToken{ID: id, Kind: store.TokenCode, Hash: codeHash(id, code), UserID: u.ID, Email: u.Email,
		ExpiresAt: t.Now().Add(CodeTTL)}); err != nil {
		return err
	}
	t.send(email.Code(lang, u.Email, app, code), "code")
	t.Audit(ctx, ActorFor(u, ip), "code.sent", u.Email, "app="+app)
	return nil
}

// CheckCode verifies the member's latest code (single use, 5 attempts, 10 minutes).
func (t *Team) CheckCode(ctx context.Context, userID, code string) error {
	code = strings.TrimSpace(code)
	if len(code) != 6 {
		return errors.New("wrong code")
	}
	return t.Store.CheckCode(ctx, userID, func(id string) string { return codeHash(id, code) }, CodeTries, t.Now())
}
