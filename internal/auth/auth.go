// Package auth — вход по passkey и cookie сессии входа.
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

// CookieName — cookie сессии входа. Префикс __Host- требует Secure, Path=/ и
// запрещает Domain: cookie не уйдёт на поддомены просмотра и dev-серверов.
const CookieName = "__Host-laavu"

// LoginTTL — сколько живёт сессия входа.
const LoginTTL = 30 * 24 * time.Hour

// InviteTTL — сколько живёт ссылка регистрации.
const InviteTTL = 24 * time.Hour

// Auth — вход по passkey.
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
	expires time.Time
}

// New настраивает RP по адресу приложения: RP ID — хост без порта.
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

// waUser — пользователь в виде, который нужен go-webauthn.
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
	// Поток анонимных login/begin не должен растить память без предела:
	// при переполнении вытесняется произвольная старая церемония.
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

// InviteInfo — что показать на странице регистрации.
type InviteInfo struct {
	NewUser bool   `json:"new_user"`
	Name    string `json:"name,omitempty"`
	Role    string `json:"role"`
}

// CheckInvite проверяет ссылку регистрации, не расходуя её.
func (a *Auth) CheckInvite(tok string) (*InviteInfo, error) {
	inv, err := a.Store.PeekInvite(tok)
	if err != nil {
		return nil, errors.New("ссылка недействительна или устарела")
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

// BeginRegister начинает регистрацию passkey по ссылке. Для нового пользователя
// имя задаёт он сам; в базу пользователь попадает только после успешной регистрации.
func (a *Auth) BeginRegister(tok, name string) (any, string, error) {
	inv, err := a.Store.PeekInvite(tok)
	if err != nil {
		return nil, "", errors.New("ссылка недействительна или устарела")
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
			return nil, "", errors.New("нужно имя до 64 символов")
		}
		if _, err := a.Store.UserByName(name); err == nil {
			return nil, "", errors.New("имя занято")
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
	return opts, a.putCeremony(ceremony{data: *data, user: u, invite: tok}), nil
}

// FinishRegister завершает регистрацию, расходует ссылку и возвращает пользователя.
func (a *Auth) FinishRegister(cid, label string, r *http.Request) (*store.User, error) {
	c, ok := a.takeCeremony(cid)
	if !ok {
		return nil, errors.New("регистрация устарела, начните заново")
	}
	wu, err := a.loadUser(c.user)
	if err != nil {
		return nil, err
	}
	cred, err := a.WA.FinishRegistration(wu, c.data, r)
	if err != nil {
		return nil, fmt.Errorf("passkey не принят: %w", err)
	}
	if err := a.Store.ConsumeInvite(c.invite); err != nil {
		return nil, errors.New("ссылка уже использована")
	}
	u := c.user
	if u.ID == 0 {
		if u, err = a.Store.CreateUserWithHandle(u.Name, u.Role, u.Handle); err != nil {
			return nil, err
		}
	}
	data, _ := json.Marshal(cred)
	if err := a.Store.AddCredential(store.Credential{ID: cred.ID, UserID: u.ID, Data: data, Label: label}); err != nil {
		return nil, err
	}
	return u, nil
}

// BeginLogin — вход без имени: браузер сам предлагает passkey этого сайта.
func (a *Auth) BeginLogin() (any, string, error) {
	opts, data, err := a.WA.BeginDiscoverableLogin(webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		return nil, "", err
	}
	return opts, a.putCeremony(ceremony{data: *data}), nil
}

// FinishLogin проверяет подпись и возвращает пользователя.
func (a *Auth) FinishLogin(cid string, r *http.Request) (*store.User, error) {
	c, ok := a.takeCeremony(cid)
	if !ok {
		return nil, errors.New("вход устарел, начните заново")
	}
	var found *waUser
	handler := func(rawID, userHandle []byte) (webauthn.User, error) {
		u, err := a.Store.UserByHandle(userHandle)
		if err != nil {
			return nil, errors.New("неизвестный passkey")
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
		return nil, fmt.Errorf("вход не удался: %w", err)
	}
	// Счётчик подписей откатился — ключ, возможно, скопирован.
	if cred.Authenticator.CloneWarning {
		return nil, errors.New("вход отклонён: счётчик passkey не сходится, возможно, ключ скопирован")
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

// SetCookie выдаёт cookie сессии входа.
func SetCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name: CookieName, Value: token, Path: "/", MaxAge: int(LoginTTL.Seconds()),
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	})
}

// ClearCookie стирает cookie.
func ClearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: CookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode})
}

// UserFromRequest — пользователь по cookie.
func (a *Auth) UserFromRequest(r *http.Request) (*store.User, string, error) {
	c, err := r.Cookie(CookieName)
	if err != nil || c.Value == "" {
		return nil, "", store.ErrNotFound
	}
	u, err := a.Store.LoginUser(c.Value)
	return u, c.Value, err
}

// RegisterURL — ссылка регистрации для печати в терминал.
func (a *Auth) RegisterURL(tok string) string {
	return strings.TrimRight(a.PublicURL, "/") + "/register#" + tok
}
