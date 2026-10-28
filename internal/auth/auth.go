// Package auth implements passkey sign-in and the sign-in session cookie.
package auth

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/ilguzin/laavu/internal/store"
)

// CookieName is the sign-in session cookie. The __Host- prefix requires Secure,
// Path=/ and forbids Domain: the cookie never reaches view or dev-server subdomains.
const CookieName = "__Host-laavu"

// LoginTTL is how long a sign-in session lives.
const LoginTTL = 30 * 24 * time.Hour

// InviteTTL is how long a registration link lives.
const InviteTTL = 24 * time.Hour

// Auth is passkey sign-in.
type Auth struct {
	WA        *webauthn.WebAuthn
	Store     *store.Store
	PublicURL string

	mu         sync.Mutex
	ceremonies map[string]ceremony
}

type ceremony struct {
	data    webauthn.SessionData
	user    *store.User
	invite  string
	agents  []string // access for a new user from the link
	expires time.Time
}

// New configures the RP from the app URL: the RP ID is the host without the port.
func New(st *store.Store, publicURL string) (*Auth, error) {
	u, err := url.Parse(publicURL)
	if err != nil {
		return nil, err
	}
	wa, err := webauthn.New(&webauthn.Config{
		RPID:          u.Hostname(),
		RPDisplayName: "Laavu",
		RPOrigins:     []string{strings.TrimRight(publicURL, "/")},
	})
	if err != nil {
		return nil, err
	}
	return &Auth{WA: wa, Store: st, PublicURL: publicURL, ceremonies: map[string]ceremony{}}, nil
}

// waUser is a user in the shape go-webauthn needs.
type waUser struct {
	u     *store.User
	creds []webauthn.Credential
}

func (w *waUser) WebAuthnID() []byte                         { return w.u.Handle }
func (w *waUser) WebAuthnName() string                       { return w.u.Name }
func (w *waUser) WebAuthnDisplayName() string                { return w.u.Name }
func (w *waUser) WebAuthnCredentials() []webauthn.Credential { return w.creds }

func (a *Auth) loadUser(u *store.User) (*waUser, error) {
	if u.ID == 0 {
		return &waUser{u: u}, nil
	}
	cs, err := a.Store.Credentials(u.ID)
	if err != nil {
		return nil, err
	}
	wu := &waUser{u: u}
	for _, c := range cs {
		var cred webauthn.Credential
		if err := json.Unmarshal(c.Data, &cred); err == nil {
			wu.creds = append(wu.creds, cred)
		}
	}
	return wu, nil
}

func (a *Auth) putCeremony(c ceremony) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	if len(a.ceremonies) > 256 {
		for k, v := range a.ceremonies {
			if v.expires.Before(now) {
				delete(a.ceremonies, k)
			}
		}
	}
	// A stream of anonymous login/begin calls must not grow memory without bound:
	// on overflow an arbitrary old ceremony is evicted.
	for k := range a.ceremonies {
		if len(a.ceremonies) < 10000 {
			break
		}
		delete(a.ceremonies, k)
	}
	id := store.RandomToken()
	c.expires = now.Add(5 * time.Minute)
	a.ceremonies[id] = c
	return id
}

func (a *Auth) takeCeremony(id string) (ceremony, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	c, ok := a.ceremonies[id]
	delete(a.ceremonies, id)
	if !ok || c.expires.Before(time.Now()) {
		return ceremony{}, false
	}
	return c, true
}

// InviteInfo is what to show on the registration page.
type InviteInfo struct {
	NewUser bool   `json:"new_user"`
	Name    string `json:"name,omitempty"`
	Role    string `json:"role"`
}

// CheckInvite validates a registration link without consuming it.
func (a *Auth) CheckInvite(tok string) (*InviteInfo, error) {
	inv, err := a.Store.PeekInvite(tok)
	if err != nil {
		return nil, errors.New("link is invalid or expired")
	}
	info := &InviteInfo{NewUser: inv.UserID == nil, Name: inv.Name, Role: inv.Role}
	if inv.UserID != nil {
		u, err := a.Store.UserByID(*inv.UserID)
		if err != nil {
			return nil, err
		}
		info.Name, info.Role = u.Name, u.Role
	}
	return info, nil
}

// BeginRegister starts passkey registration from a link. A new user picks their
// own name; the user is stored only after registration succeeds.
func (a *Auth) BeginRegister(tok, name string) (any, string, error) {
	inv, err := a.Store.PeekInvite(tok)
	if err != nil {
		return nil, "", errors.New("link is invalid or expired")
	}
	var u *store.User
	if inv.UserID != nil {
		if u, err = a.Store.UserByID(*inv.UserID); err != nil {
			return nil, "", err
		}
	} else {
		if inv.Name != "" {
			name = inv.Name
		}
		name = strings.TrimSpace(name)
		if name == "" || len(name) > 64 {
			return nil, "", errors.New("name is required, up to 64 characters")
		}
		if _, err := a.Store.UserByName(name); err == nil {
			return nil, "", errors.New("name is taken")
		}
		u = &store.User{Name: name, Role: inv.Role, Handle: store.NewHandle()}
	}
	wu, err := a.loadUser(u)
	if err != nil {
		return nil, "", err
	}
	var excl []protocol.CredentialDescriptor
	for _, c := range wu.creds {
		excl = append(excl, c.Descriptor())
	}
	opts, data, err := a.WA.BeginRegistration(wu,
		webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementRequired),
		webauthn.WithExclusions(excl),
		webauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{
			ResidentKey:      protocol.ResidentKeyRequirementRequired,
			UserVerification: protocol.VerificationRequired,
		}),
	)
	if err != nil {
		return nil, "", err
	}
	return opts, a.putCeremony(ceremony{data: *data, user: u, invite: tok, agents: inv.Agents}), nil
}

// FinishRegister completes registration, consumes the link and returns the user.
func (a *Auth) FinishRegister(cid, label string, r *http.Request) (*store.User, error) {
	c, ok := a.takeCeremony(cid)
	if !ok {
		return nil, errors.New("registration expired, start over")
	}
	wu, err := a.loadUser(c.user)
	if err != nil {
		return nil, err
	}
	cred, err := a.WA.FinishRegistration(wu, c.data, r)
	if err != nil {
		return nil, fmt.Errorf("passkey rejected: %w", err)
	}
	if err := a.Store.ConsumeInvite(c.invite); err != nil {
		return nil, errors.New("link already used")
	}
	u := c.user
	if u.ID == 0 {
		if u, err = a.Store.CreateUserWithHandle(u.Name, u.Role, u.Handle); err != nil {
			return nil, err
		}
		for _, ag := range c.agents {
			if err := a.Store.GrantAccess(u.ID, ag); err != nil {
				return nil, err
			}
		}
	}
	data, _ := json.Marshal(cred)
	if err := a.Store.AddCredential(store.Credential{ID: cred.ID, UserID: u.ID, Data: data, Label: label}); err != nil {
		return nil, err
	}
	return u, nil
}

// BeginLogin starts a nameless sign-in: the browser offers this site's passkeys itself.
func (a *Auth) BeginLogin() (any, string, error) {
	opts, data, err := a.WA.BeginDiscoverableLogin(webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		return nil, "", err
	}
	return opts, a.putCeremony(ceremony{data: *data}), nil
}

// FinishLogin verifies the signature and returns the user.
func (a *Auth) FinishLogin(cid string, r *http.Request) (*store.User, error) {
	c, ok := a.takeCeremony(cid)
	if !ok {
		return nil, errors.New("sign-in expired, start over")
	}
	var found *waUser
	handler := func(rawID, userHandle []byte) (webauthn.User, error) {
		u, err := a.Store.UserByHandle(userHandle)
		if err != nil {
			return nil, errors.New("unknown passkey")
		}
		wu, err := a.loadUser(u)
		if err != nil {
			return nil, err
		}
		found = wu
		return wu, nil
	}
	cred, err := a.WA.FinishDiscoverableLogin(handler, c.data, r)
	if err != nil {
		return nil, fmt.Errorf("sign-in failed: %w", err)
	}
	// The signature counter went backwards: the key may have been cloned.
	if cred.Authenticator.CloneWarning {
		return nil, errors.New("sign-in rejected: passkey counter mismatch, the key may have been cloned")
	}
	for _, sc := range found.creds {
		if bytes.Equal(sc.ID, cred.ID) {
			sc.Authenticator.SignCount = cred.Authenticator.SignCount
			sc.Flags = cred.Flags
			data, _ := json.Marshal(sc)
			a.Store.UpdateCredential(cred.ID, data)
		}
	}
	return found.u, nil
}

// SetCookie issues the sign-in session cookie.
func SetCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name: CookieName, Value: token, Path: "/", MaxAge: int(LoginTTL.Seconds()),
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	})
}

// ClearCookie erases the cookie.
func ClearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: CookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode})
}

// UserFromRequest returns the user from the cookie.
func (a *Auth) UserFromRequest(r *http.Request) (*store.User, string, error) {
	c, err := r.Cookie(CookieName)
	if err != nil || c.Value == "" {
		return nil, "", store.ErrNotFound
	}
	u, err := a.Store.LoginUser(c.Value)
	return u, c.Value, err
}

// RegisterURL is a registration link to print to the terminal.
func (a *Auth) RegisterURL(tok string) string {
	return strings.TrimRight(a.PublicURL, "/") + "/register#" + tok
}
