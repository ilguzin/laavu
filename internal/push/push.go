// Package push — Web Push «сессия ждёт ответа». В пуше только имя сессии и
// статус: пуш идёт через серверы Google, Apple или Mozilla, содержимое рабочих
// папок туда уйти не должно.
package push

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
	"github.com/ilguzin/laavu/internal/store"
)

// Sender рассылает пуши подписчикам из базы.
type Sender struct {
	st         *store.Store
	public     string
	private    string
	subscriber string
	// Client — HTTP-клиент; в тестах подменяется.
	Client *http.Client
}

// Message — то, что видит пользователь в уведомлении.
type Message struct {
	Title   string `json:"title"`
	Body    string `json:"body"`
	Agent   string `json:"agent"`
	Session string `json:"session"`
	Tag     string `json:"tag"`
}

// New загружает или создаёт VAPID-ключи.
func New(st *store.Store, publicURL string) (*Sender, error) {
	pub, err1 := st.Get("vapid_public")
	priv, err2 := st.Get("vapid_private")
	if err1 != nil || err2 != nil {
		var err error
		priv, pub, err = webpush.GenerateVAPIDKeys()
		if err != nil {
			return nil, err
		}
		if err := st.Set("vapid_public", pub); err != nil {
			return nil, err
		}
		if err := st.Set("vapid_private", priv); err != nil {
			return nil, err
		}
	}
	return &Sender{st: st, public: pub, private: priv, subscriber: publicURL, Client: &http.Client{Timeout: 15 * time.Second}}, nil
}

// PublicKey — ключ для pushManager.subscribe в браузере.
func (s *Sender) PublicKey() string { return s.public }

// Subscription — PushSubscription из браузера.
type Subscription = webpush.Subscription

// pushHosts — push-сервисы браузеров. Другие адреса не принимаются: иначе
// подписка превращала бы laavud в отправителя POST-запросов куда угодно.
var pushHosts = []string{
	"fcm.googleapis.com", "android.googleapis.com", // Chrome, Edge на Android
	"updates.push.services.mozilla.com", // Firefox
	"web.push.apple.com",                // Safari
	".notify.windows.com",               // Edge на Windows
}

// AllowedEndpoint — адрес подписки ведёт в известный push-сервис.
func AllowedEndpoint(endpoint string) bool {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.User != nil || (u.Port() != "" && u.Port() != "443") {
		return false
	}
	h := strings.ToLower(u.Hostname())
	for _, p := range pushHosts {
		if h == p || (strings.HasPrefix(p, ".") && strings.HasSuffix(h, p)) {
			return true
		}
	}
	return false
}

// ParseSubscription проверяет подписку из клиента.
func ParseSubscription(b []byte) (*Subscription, error) {
	var sub Subscription
	if err := json.Unmarshal(b, &sub); err != nil {
		return nil, err
	}
	if sub.Endpoint == "" || sub.Keys.P256dh == "" || sub.Keys.Auth == "" {
		return nil, errors.New("неполная подписка")
	}
	return &sub, nil
}

// SendUser рассылает сообщение на все устройства пользователя. Подписки, от
// которых push-сервис отказался (404, 410), удаляются.
func (s *Sender) SendUser(userID int64, m Message) int {
	subs, err := s.st.PushSubs(userID)
	if err != nil {
		return 0
	}
	payload, _ := json.Marshal(m)
	sent := 0
	for _, raw := range subs {
		sub, err := ParseSubscription([]byte(raw))
		if err != nil {
			continue
		}
		resp, err := webpush.SendNotification(payload, sub, &webpush.Options{
			HTTPClient:      s.Client,
			Subscriber:      s.subscriber,
			VAPIDPublicKey:  s.public,
			VAPIDPrivateKey: s.private,
			TTL:             600,
			Urgency:         webpush.UrgencyHigh,
			Topic:           topic(m.Tag),
		})
		if err != nil {
			continue
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
			s.st.DeletePushSub(sub.Endpoint)
		case resp.StatusCode < 300:
			sent++
		}
	}
	return sent
}

// topic — заголовок Topic: не длиннее 32 символов base64url, новый пуш той же
// сессии заменяет старый.
func topic(tag string) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	out := make([]byte, 0, 32)
	for _, c := range []byte(tag) {
		if len(out) == 32 {
			break
		}
		out = append(out, alphabet[int(c)%len(alphabet)])
	}
	return string(out)
}
